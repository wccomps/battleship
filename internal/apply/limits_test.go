package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// slowStatus wraps the fake so CurrentStatus takes a moment and records the
// most calls in flight at once.
type slowStatus struct {
	*podstest.Fake
	mu       sync.Mutex
	inFlight int
	max      int
}

func (s *slowStatus) CurrentStatus(ctx context.Context, node string, vmid int) (string, error) {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.max {
		s.max = s.inFlight
	}
	s.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	return s.Fake.CurrentStatus(ctx, node, vmid)
}

// runTwoPowerJobs runs power plans for teams 01 and 02 at the same time and
// returns the most CurrentStatus calls that were in flight together.
func runTwoPowerJobs(t *testing.T, shared bool) int {
	t.Helper()
	f := newCluster()
	for team, vmid := range map[string]int{"01": 10121, "02": 10221} {
		f.Add(proxmox.VM{VMID: vmid, Name: "team" + team + "-teak", Node: "cedar", Status: "running"}, nil)
	}
	api := &slowStatus{Fake: f}
	cfg := testExecutor(f, &recorder{}).Cfg
	cfg.Concurrency.ConfigCalls = 1
	lim := NewLimits(cfg.Concurrency)

	var wg sync.WaitGroup
	for _, team := range []string{"01", "02"} {
		plan, err := pods.NewPlanner(f, cfg).Power(context.Background(), []string{team}, nil, "start")
		if err != nil {
			t.Fatal(err)
		}
		e := &Executor{API: api, Cfg: cfg, Sleep: func(context.Context, time.Duration) error { return nil }}
		if shared {
			e.Limits = lim
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := e.Run(context.Background(), plan); len(res.Failed) != 0 {
				t.Errorf("failed: %v", res.Failed)
			}
		}()
	}
	wg.Wait()
	return api.max
}

func TestSharedLimitsCapCallsAcrossExecutors(t *testing.T) {
	if got := runTwoPowerJobs(t, true); got != 1 {
		t.Errorf("shared limits: %d calls in flight at once, want 1", got)
	}
}

func TestSeparateLimitsDoNotCapEachOther(t *testing.T) {
	// Control for the test above: without sharing, each executor has its own
	// budget of one call, so both run at once.
	if got := runTwoPowerJobs(t, false); got != 2 {
		t.Errorf("separate limits: %d calls in flight at once, want 2", got)
	}
}

func TestBlockedItemsEmitBlockedEvents(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "someone-elses-vm", Node: "cedar"}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), pods.DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	testExecutor(f, rec).Run(context.Background(), plan)
	ev := rec.find("team01-teak", EventBlocked)
	if len(ev) != 1 || ev[0].Message != "VMID 10121 is used by someone-elses-vm" {
		t.Errorf("blocked events = %+v", ev)
	}
}

func TestLimitsCallHoldsOneSlot(t *testing.T) {
	lim := NewLimits(config.Concurrency{ConfigCalls: 2, ClonesPerNode: 1})
	// Inside a call one slot is taken, so exactly one more call fits.
	err := lim.Call(context.Background(), func() error {
		if got := len(lim.calls); got != 1 {
			t.Errorf("slots held inside Call = %d, want 1", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(lim.calls); got != 0 {
		t.Errorf("slots held after Call = %d, want 0", got)
	}
	want := errors.New("boom")
	if err := lim.Call(context.Background(), func() error { return want }); err != want {
		t.Errorf("Call error = %v, want the callback's", err)
	}
	if got := len(lim.calls); got != 0 {
		t.Errorf("slots held after a failed Call = %d, want 0", got)
	}
}

func TestLimitsCallWaitsForASlot(t *testing.T) {
	lim := NewLimits(config.Concurrency{ConfigCalls: 1, ClonesPerNode: 1})
	holding, release := make(chan struct{}), make(chan struct{})
	done := make(chan error)
	go func() {
		done <- lim.Call(context.Background(), func() error { close(holding); <-release; return nil })
	}()
	<-holding
	ctx, cancel := context.WithCancel(context.Background())
	ran := false
	waited := make(chan error)
	go func() { waited <- lim.Call(ctx, func() error { ran = true; return nil }) }()
	cancel()
	if err := <-waited; !errors.Is(err, context.Canceled) || ran {
		t.Errorf("Call with no free slot and a cancelled ctx = %v (ran %v), want context.Canceled without running", err, ran)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A free slot doesn't win against a context that is already done.
	if err := lim.Call(ctx, func() error { ran = true; return nil }); !errors.Is(err, context.Canceled) || ran {
		t.Errorf("Call with a done ctx = %v (ran %v), want context.Canceled without running", err, ran)
	}
}

// slotWins is a done context whose Done select doesn't pick: the free
// slot won the select, as it may when both are ready.
type slotWins struct{ context.Context }

func (slotWins) Done() <-chan struct{} { return nil }
func (slotWins) Err() error            { return context.Canceled }

func TestSemDoesNotRunWhenAFreeSlotWinsOverADoneCtx(t *testing.T) {
	s := make(sem, 1)
	ran := false
	err := s.do(slotWins{context.Background()}, func() error { ran = true; return nil })
	if !errors.Is(err, context.Canceled) || ran {
		t.Errorf("do when a free slot won over a done ctx = %v (ran %v), want context.Canceled without running", err, ran)
	}
	if len(s) != 0 {
		t.Errorf("slots held after = %d, want 0", len(s))
	}
}

// memSlots is Slots in memory, standing in for the database's advisory locks
// that every process shares.
type memSlots struct {
	mu   sync.Mutex
	sems map[string]chan struct{}
}

func (m *memSlots) AcquireSlot(ctx context.Context, name string, n int) (func(), error) {
	m.mu.Lock()
	if m.sems == nil {
		m.sems = map[string]chan struct{}{}
	}
	ch, ok := m.sems[name]
	if !ok {
		ch = make(chan struct{}, n)
		m.sems[name] = ch
	}
	m.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// runTwoProcesses builds teak's and oak's templates at once from two
// executors with their own Limits, as two replicas would, and returns the
// most conversions that ran together.
func runTwoProcesses(t *testing.T, slots Slots) int {
	t.Helper()
	f := newCluster()
	w := watchStorageTasks(f)
	var wg sync.WaitGroup
	for _, pattern := range []string{"teak.*", "oak.*"} {
		plan, err := testPlanner(f).Deploy(context.Background(), pods.DeployRequest{Pattern: pattern, Teams: []string{"01"}})
		if err != nil {
			t.Fatal(err)
		}
		ex := testExecutor(f, &recorder{})
		ex.Limits = NewClusterLimits(ex.Cfg.Concurrency, slots)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := ex.Run(context.Background(), plan); len(res.Failed) != 0 {
				t.Errorf("failed: %v", res.Failed)
			}
		}()
	}
	wg.Wait()
	return w.peak()
}

func TestStorageOpsAreSerializedAcrossProcesses(t *testing.T) {
	if p := runTwoProcesses(t, &memSlots{}); p != 1 {
		t.Errorf("%d conversions ran at once across processes, want 1", p)
	}
}

func TestStorageOpsWithoutSharedSlotsArePerProcess(t *testing.T) {
	// Control for the test above: without shared slots each process has its
	// own storage-ops slot, so both conversions run at once.
	if p := runTwoProcesses(t, nil); p != 2 {
		t.Errorf("%d conversions ran at once, want 2", p)
	}
}

type recordingSlots struct {
	memSlots
	mu    sync.Mutex
	taken []string
}

func (r *recordingSlots) AcquireSlot(ctx context.Context, name string, n int) (func(), error) {
	r.mu.Lock()
	r.taken = append(r.taken, fmt.Sprintf("%s/%d", name, n))
	r.mu.Unlock()
	return r.memSlots.AcquireSlot(ctx, name, n)
}

func TestDeletesTakeClusterSlots(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, map[string]string{"name": "team01-teak"})
	plan := teardownTeam01(t, f)
	ex := testExecutor(f, &recorder{})
	slots := &recordingSlots{}
	ex.Limits = NewClusterLimits(ex.Cfg.Concurrency, slots)

	if res := ex.Run(context.Background(), plan); len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if want := []string{fmt.Sprintf("deletes/%d", ex.Cfg.Concurrency.Deletes)}; !slices.Equal(slots.taken, want) {
		t.Errorf("slots taken = %v, want %v", slots.taken, want)
	}
}

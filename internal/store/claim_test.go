package store_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

func mustClaim(t *testing.T, s *store.Store, worker string) *store.Job {
	t.Helper()
	j, err := s.Claim(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestClaimWaitsForOverlappingLocks(t *testing.T) {
	s := storetest.New(t)
	j1 := create(t, s, "team:01")
	j2 := create(t, s, "team:01", "team:02")
	j3 := create(t, s, "team:03")

	if got := mustClaim(t, s, "w1"); got == nil || got.ID != j1 || got.Status != store.StatusRunning || got.ClaimedBy != "w1" {
		t.Fatalf("first claim = %+v, want job %d", got, j1)
	}
	if got := mustClaim(t, s, "w2"); got == nil || got.ID != j3 {
		t.Fatalf("second claim = %+v, want job %d (job %d waits for %d)", got, j3, j2, j1)
	}
	if got := mustClaim(t, s, "w2"); got != nil {
		t.Fatalf("third claim = %+v, want none", got)
	}
	if b, _ := s.Blockers(ctx, j2); !reflect.DeepEqual(b, []int64{j1}) {
		t.Errorf("Blockers(%d) = %v, want [%d]", j2, b, j1)
	}
	if err := s.Finish(ctx, j1, "w1", store.Outcome{Status: store.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	if got := mustClaim(t, s, "w1"); got == nil || got.ID != j2 {
		t.Fatalf("claim after finish = %+v, want job %d", got, j2)
	}
}

func TestClaimKeepsOverlappingJobsInOrder(t *testing.T) {
	s := storetest.New(t)
	j1 := create(t, s, "team:01", "template:teak.tpl")
	j2 := create(t, s, "team:02", "template:teak.tpl")
	j3 := create(t, s, "team:02")

	if got := mustClaim(t, s, "w"); got.ID != j1 {
		t.Fatalf("claim = %d, want %d", got.ID, j1)
	}
	// j3 shares team:02 with the older pending j2, so it must not jump ahead.
	if got := mustClaim(t, s, "w"); got != nil {
		t.Fatalf("claim = %d, want none (j2 waits for j1, j3 waits for j2)", got.ID)
	}
	if b, _ := s.Blockers(ctx, j3); !reflect.DeepEqual(b, []int64{j2}) {
		t.Errorf("Blockers(%d) = %v, want [%d]", j3, b, j2)
	}
}

func TestConcurrentClaimsNeverOverlap(t *testing.T) {
	s := storetest.New(t)
	for i := 0; i < 5; i++ {
		create(t, s, "team:01")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := s.Claim(ctx, "w")
			if err != nil {
				t.Error(err)
				return
			}
			if j != nil {
				mu.Lock()
				claimed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claimed != 1 {
		t.Errorf("%d jobs sharing team:01 were claimed at once, want 1", claimed)
	}
}

func TestHeartbeatReportsCancel(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	mustClaim(t, s, "w1")

	if cancel, err := s.Heartbeat(ctx, id, "w1"); err != nil || cancel {
		t.Fatalf("Heartbeat = %v, %v", cancel, err)
	}
	if err := s.RequestCancel(ctx, id, "bob"); err != nil {
		t.Fatal(err)
	}
	if cancel, err := s.Heartbeat(ctx, id, "w1"); err != nil || !cancel {
		t.Fatalf("Heartbeat after cancel = %v, %v; want true", cancel, err)
	}
	if err := s.Finish(ctx, id, "w1", store.Outcome{Status: store.StatusCancelled}); err != nil {
		t.Fatal(err)
	}
	j, _ := s.Job(ctx, id)
	if j.Status != store.StatusCancelled || j.CancelledBy != "bob" || j.FinishedAt == nil || j.Active() {
		t.Errorf("job = %+v", j)
	}
	if err := s.RequestCancel(ctx, id, "bob"); !errors.Is(err, store.ErrNotActive) {
		t.Errorf("cancel finished job: err = %v", err)
	}
	if err := s.RequestCancel(ctx, 999, "bob"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cancel missing job: err = %v", err)
	}
}

func TestCancelPendingJobIsImmediate(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	if err := s.RequestCancel(ctx, id, "bob"); err != nil {
		t.Fatal(err)
	}
	j, _ := s.Job(ctx, id)
	if j.Status != store.StatusCancelled {
		t.Errorf("status = %s, want cancelled", j.Status)
	}
	if got := mustClaim(t, s, "w"); got != nil {
		t.Errorf("claimed a cancelled job: %+v", got)
	}
}

func TestOtherWorkersCannotHeartbeatOrFinish(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	mustClaim(t, s, "w1")
	if _, err := s.Heartbeat(ctx, id, "w2"); !errors.Is(err, store.ErrLostClaim) {
		t.Errorf("Heartbeat by w2: err = %v", err)
	}
	if err := s.Finish(ctx, id, "w2", store.Outcome{Status: store.StatusSucceeded}); !errors.Is(err, store.ErrLostClaim) {
		t.Errorf("Finish by w2: err = %v", err)
	}
}

func TestCancelDuringClaimWins(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")

	// Set up a hook that cancels the job after Claim selects it but before updating.
	store.SetBeforeClaimUpdate(func(jobID int64) {
		if jobID == id {
			if err := s.RequestCancel(ctx, id, "bob"); err != nil {
				t.Error(err)
			}
		}
	})
	t.Cleanup(func() { store.SetBeforeClaimUpdate(nil) })

	// Claim must return nil because the job was cancelled before the update.
	got, err := s.Claim(ctx, "w")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("Claim = %+v, want nil (job was cancelled during claim)", got)
	}

	// Verify the job is cancelled.
	j, _ := s.Job(ctx, id)
	if j.Status != store.StatusCancelled || j.CancelledBy != "bob" {
		t.Errorf("job = %+v, want cancelled by bob", j)
	}

	// A second claim must also return nil.
	if got, _ := s.Claim(ctx, "w"); got != nil {
		t.Errorf("second claim = %+v, want nil", got)
	}
}

func TestReapStaleInterruptsSilentJobs(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	mustClaim(t, s, "w1")

	if ids, err := s.ReapStale(ctx, time.Hour); err != nil || len(ids) != 0 {
		t.Fatalf("ReapStale(1h) = %v, %v; want none", ids, err)
	}
	time.Sleep(20 * time.Millisecond)
	ids, err := s.ReapStale(ctx, 10*time.Millisecond)
	if err != nil || !reflect.DeepEqual(ids, []int64{id}) {
		t.Fatalf("ReapStale = %v, %v; want [%d]", ids, err, id)
	}
	j, _ := s.Job(ctx, id)
	if j.Status != store.StatusInterrupted || j.Error == "" {
		t.Errorf("job = %+v", j)
	}
	// Unfinished items end too: one that never reached a step didn't run;
	// blocked ones stay blocked.
	items, _ := s.Items(ctx, id)
	if items[0].Status != store.ItemNotRun || items[1].Status != store.ItemBlocked {
		t.Errorf("items after reap = %+v", items)
	}
	// Late progress from the silent worker is refused and doesn't revive them.
	if err := s.AddEvent(ctx, id, store.Event{At: time.Now(), Item: "team01-teak", Step: "stop", Status: "done"}); !errors.Is(err, store.ErrNotActive) {
		t.Errorf("AddEvent after reap: %v, want ErrNotActive", err)
	}
	if items, _ := s.Items(ctx, id); items[0].Status != store.ItemNotRun {
		t.Errorf("item after late event = %+v, want not run", items[0])
	}
	if _, err := s.Heartbeat(ctx, id, "w1"); !errors.Is(err, store.ErrLostClaim) {
		t.Errorf("Heartbeat after reap: err = %v", err)
	}
	// The team is free again.
	id2 := create(t, s, "team:01")
	if got := mustClaim(t, s, "w2"); got == nil || got.ID != id2 {
		t.Errorf("claim after reap = %+v, want %d", got, id2)
	}
}

func TestEventsUpdateItems(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	mustClaim(t, s, "w")
	now := time.Now()
	for _, ev := range []store.Event{
		{At: now, Item: "team01-teak", Step: "stop", Status: "done"},
		{At: now, Item: "team01-teak", Step: "delete", Status: "failed", Message: "boom"},
		{At: now, Item: "team01-dc", Status: "blocked", Message: "no snapshot"},
		{At: now, Item: "", Status: "info", Message: "retry round 1"},
	} {
		if err := s.AddEvent(ctx, id, ev); err != nil {
			t.Fatal(err)
		}
	}
	items, _ := s.Items(ctx, id)
	if items[0].Step != "delete" || items[0].Status != store.ItemRunning || items[0].Error != "boom" {
		t.Errorf("teak = %+v", items[0])
	}
	if items[1].Status != store.ItemBlocked {
		t.Errorf("dc = %+v, want still blocked", items[1])
	}
	evs, err := s.Events(ctx, id, 0, 2)
	if err != nil || len(evs) != 2 || evs[0].Step != "stop" {
		t.Fatalf("first page = %+v, %v", evs, err)
	}
	rest, _ := s.Events(ctx, id, evs[1].ID, 100)
	if len(rest) != 2 || rest[1].Message != "retry round 1" {
		t.Errorf("second page = %+v", rest)
	}
}

func TestFinishSetsItemOutcomes(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	mustClaim(t, s, "w")
	if err := s.Finish(ctx, id, "w", store.Outcome{Status: store.StatusRunning}); err == nil {
		t.Error("Finish accepted a non-final status (running)")
	}
	if err := s.Finish(ctx, id, "w", store.Outcome{Status: "succeded"}); err == nil {
		t.Error("Finish accepted an invalid status (typo)")
	}
	err := s.Finish(ctx, id, "w", store.Outcome{
		Status:  store.StatusCompletedWithFailures,
		Summary: json.RawMessage(`{"failed":1}`),
		Items: map[string]store.ItemOutcome{
			"team01-teak": {Status: store.ItemFailed, Error: "delete: boom"},
			"team01-dc":   {Status: store.ItemDone}, // blocked items keep their status
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := s.Items(ctx, id)
	if items[0].Status != store.ItemFailed || items[0].Error != "delete: boom" || items[1].Status != store.ItemBlocked {
		t.Errorf("items = %+v", items)
	}
	j, _ := s.Job(ctx, id)
	if j.Status != store.StatusCompletedWithFailures || string(j.Summary) != `{"failed": 1}` {
		t.Errorf("job = %s %s", j.Status, j.Summary)
	}
}

func TestClaimJobClaimsThatJob(t *testing.T) {
	s := storetest.New(t)
	j1 := create(t, s, "team:01")
	j2 := create(t, s, "team:02")
	got, err := s.ClaimJob(ctx, j2, "cli")
	if err != nil || got == nil || got.ID != j2 || got.Status != store.StatusRunning || got.ClaimedBy != "cli" {
		t.Fatalf("ClaimJob(%d) = %+v, %v", j2, got, err)
	}
	if j, _ := s.Job(ctx, j1); j.Status != store.StatusPending {
		t.Errorf("job %d = %s, want still pending", j1, j.Status)
	}
}

func TestClaimJobWaitsForOlderOverlappingJob(t *testing.T) {
	s := storetest.New(t)
	j1 := create(t, s, "team:01")
	j2 := create(t, s, "team:01", "team:02")
	if got, err := s.ClaimJob(ctx, j2, "cli"); err != nil || got != nil {
		t.Fatalf("ClaimJob behind pending job = %+v, %v; want nil, nil", got, err)
	}
	mustClaim(t, s, "w1") // j1 is now running
	if got, err := s.ClaimJob(ctx, j2, "cli"); err != nil || got != nil {
		t.Fatalf("ClaimJob behind running job = %+v, %v; want nil, nil", got, err)
	}
	if err := s.Finish(ctx, j1, "w1", store.Outcome{Status: store.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ClaimJob(ctx, j2, "cli"); err != nil || got == nil || got.ID != j2 {
		t.Fatalf("ClaimJob after blocker finished = %+v, %v; want job %d", got, err, j2)
	}
}

func TestClaimJobDoesNotTakeOtherWork(t *testing.T) {
	s := storetest.New(t)
	j1 := create(t, s, "team:01")
	j2 := create(t, s, "team:01")
	// Claiming the older job must not start the newer one, and claiming the
	// newer one must not start the older one in its place.
	if got, err := s.ClaimJob(ctx, j2, "cli"); err != nil || got != nil {
		t.Fatalf("ClaimJob(%d) = %+v, %v; want nil, nil", j2, got, err)
	}
	if got, err := s.ClaimJob(ctx, j1, "cli"); err != nil || got == nil || got.ID != j1 {
		t.Fatalf("ClaimJob(%d) = %+v, %v", j1, got, err)
	}
	if j, _ := s.Job(ctx, j2); j.Status != store.StatusPending {
		t.Errorf("job %d = %s, want pending", j2, j.Status)
	}
	if got := mustClaim(t, s, "w"); got != nil {
		t.Errorf("Claim = %+v, want none while %d runs", got, j1)
	}
}

func TestClaimJobErrors(t *testing.T) {
	s := storetest.New(t)
	if _, err := s.ClaimJob(ctx, 999, "cli"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing job: err = %v, want ErrNotFound", err)
	}
	id := create(t, s, "team:01")
	if err := s.RequestCancel(ctx, id, "bob"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ClaimJob(ctx, id, "cli"); !errors.Is(err, store.ErrNotActive) || got != nil {
		t.Errorf("cancelled job: ClaimJob = %+v, %v; want ErrNotActive", got, err)
	}
}

func TestClaimJobLosesToCancel(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	store.SetBeforeClaimUpdate(func(jobID int64) {
		if jobID == id {
			if err := s.RequestCancel(ctx, id, "bob"); err != nil {
				t.Error(err)
			}
		}
	})
	t.Cleanup(func() { store.SetBeforeClaimUpdate(nil) })
	if got, err := s.ClaimJob(ctx, id, "cli"); !errors.Is(err, store.ErrNotActive) || got != nil {
		t.Fatalf("ClaimJob = %+v, %v; want ErrNotActive (cancelled during claim)", got, err)
	}
}

// A job that ends early leaves its unfinished items interrupted, or not run
// if they never reached a step, as ReapStale does, so no item of a finished
// job still reads as pending or running.
func TestFinishInterruptsUnfinishedItems(t *testing.T) {
	for _, status := range []string{store.StatusInterrupted, store.StatusCancelled} {
		t.Run(status, func(t *testing.T) {
			s := storetest.New(t)
			nj := newJob("team:01")
			nj.Items = append(nj.Items, store.NewItem{Name: "team01-web", Team: "01", VMID: 10107})
			id, err := s.CreateJob(ctx, nj)
			if err != nil {
				t.Fatal(err)
			}
			mustClaim(t, s, "w")
			// teak is under way; web never starts.
			if err := s.AddEvent(ctx, id, store.Event{At: time.Now(), Item: "team01-teak", Step: "stop", Status: "done"}); err != nil {
				t.Fatal(err)
			}
			if err := s.Finish(ctx, id, "w", store.Outcome{Status: status}); err != nil {
				t.Fatal(err)
			}
			items, _ := s.Items(ctx, id)
			got := map[string]string{}
			for _, it := range items {
				got[it.Name] = it.Status
			}
			want := map[string]string{"team01-teak": store.ItemInterrupted, "team01-dc": store.ItemBlocked, "team01-web": store.ItemNotRun}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("items = %v, want %v", got, want)
			}
		})
	}
}

// Items with an outcome keep it; only the rest become interrupted.
func TestFinishKeepsItemOutcomesWhenInterrupted(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	mustClaim(t, s, "w")
	err := s.Finish(ctx, id, "w", store.Outcome{Status: store.StatusCancelled,
		Items: map[string]store.ItemOutcome{"team01-teak": {Status: store.ItemDone}}})
	if err != nil {
		t.Fatal(err)
	}
	if items, _ := s.Items(ctx, id); items[0].Status != store.ItemDone || items[1].Status != store.ItemBlocked {
		t.Errorf("items = %+v", items)
	}
}

// Whatever a job's outcome, an item it never started (a stale or failed
// job's, say) is recorded as not run.
func TestFinishMarksItemsThatNeverRan(t *testing.T) {
	for _, status := range []string{store.StatusSucceeded, store.StatusStale, store.StatusFailed} {
		t.Run(status, func(t *testing.T) {
			s := storetest.New(t)
			id := create(t, s, "team:01")
			mustClaim(t, s, "w")
			if err := s.Finish(ctx, id, "w", store.Outcome{Status: status}); err != nil {
				t.Fatal(err)
			}
			if items, _ := s.Items(ctx, id); items[0].Status != store.ItemNotRun || items[1].Status != store.ItemBlocked {
				t.Errorf("items = %+v, want not run and blocked", items)
			}
		})
	}
}

// A pending job cancelled before it starts has items that never ran.
func TestCancelPendingJobMarksItemsNotRun(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	if err := s.RequestCancel(ctx, id, "bob"); err != nil {
		t.Fatal(err)
	}
	if items, _ := s.Items(ctx, id); items[0].Status != store.ItemNotRun || items[1].Status != store.ItemBlocked {
		t.Errorf("items = %+v, want not run and blocked", items)
	}
}

// Asking a running job to cancel changes nothing about its items yet: the
// worker still owns them.
func TestCancelRunningJobLeavesItems(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	mustClaim(t, s, "w")
	if err := s.RequestCancel(ctx, id, "bob"); err != nil {
		t.Fatal(err)
	}
	if items, _ := s.Items(ctx, id); items[0].Status != store.ItemPending {
		t.Errorf("item = %+v, want still pending", items[0])
	}
}

// BlockersOf answers Blockers for many jobs in one query.
func TestBlockersOf(t *testing.T) {
	s := storetest.New(t)
	running := create(t, s, "team:01")
	mustClaim(t, s, "w1")
	a := create(t, s, "team:01", "team:02") // waits for running
	b := create(t, s, "team:02")            // waits for a
	c := create(t, s, "team:03")            // waits for nothing
	got, err := s.BlockersOf(ctx, []int64{running, a, b, c, 9999})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64][]int64{a: {running}, b: {a}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BlockersOf = %v, want %v", got, want)
	}
	for _, id := range []int64{running, a, b, c} {
		one, err := s.Blockers(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(one, got[id]) && !(len(one) == 0 && len(got[id]) == 0) {
			t.Errorf("job %d: Blockers %v, BlockersOf %v", id, one, got[id])
		}
	}
	if got, err := s.BlockersOf(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("BlockersOf(nil) = %v, %v", got, err)
	}
}

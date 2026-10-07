package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

func teamVMs() *fakeAPI {
	return newFake(
		proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "n1", Status: "running"},
		proxmox.VM{VMID: 10105, Name: "team01-dc", Node: "n1", Status: "running"},
	)
}

// submit previews in against f, as the web UI will, and stores the job.
func submit(t *testing.T, st *store.Store, f *fakeAPI, in Inputs) int64 {
	t.Helper()
	cfg := testCfg()
	p, err := BuildPlan(bg, pods.NewPlanner(f, cfg), in)
	if err != nil {
		t.Fatal(err)
	}
	id, err := Submit(bg, st, in, p, Submitter{User: "alice", Credential: aliceTicket(), Seal: mustCreds()})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// mustCreds seals job credentials with the tests' key.
func mustCreds() Credentials {
	cfg := testCfg()
	cfg.Database.SealKey = testSealKey
	c, err := NewCredentials(cfg)
	if err != nil {
		panic(err)
	}
	return c
}

func newWorker(st *store.Store, f *fakeAPI) *Worker {
	cfg := testCfg()
	cfg.Jobs.Heartbeat = 20 * time.Millisecond
	cfg.Jobs.Poll = 10 * time.Millisecond
	cfg.Jobs.CancelGrace = 50 * time.Millisecond
	return &Worker{Store: st, Cfg: cfg, Bind: f.as, Credentials: mustCreds(), ID: "w1"}
}

func claim(t *testing.T, st *store.Store) *store.Job {
	t.Helper()
	j, err := st.Claim(bg, "w1")
	if err != nil || j == nil {
		t.Fatalf("Claim = %v, %v", j, err)
	}
	return j
}

func job(t *testing.T, st *store.Store, id int64) store.Job {
	t.Helper()
	j, err := st.Job(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestWorkerRunsJob(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})

	newWorker(st, f).RunJob(bg, claim(t, st))

	j := job(t, st, id)
	if j.Status != store.StatusSucceeded || j.FinishedAt == nil {
		t.Fatalf("job = %s (%s)", j.Status, j.Error)
	}
	if len(f.DeletedIDs()) != 2 {
		t.Errorf("deleted %v, want both VMs", f.DeletedIDs())
	}
	items, _ := st.Items(bg, id)
	for _, it := range items {
		if it.Status != store.ItemDone || it.Step != string(pods.StepDelete) {
			t.Errorf("item = %+v", it)
		}
	}
	var sum Summary
	if err := json.Unmarshal(j.Summary, &sum); err != nil || len(sum.Succeeded) != 2 {
		t.Errorf("summary = %s, %v", j.Summary, err)
	}
	evs, _ := st.Events(bg, id, 0, 100)
	if len(evs) == 0 || !strings.HasPrefix(evs[len(evs)-1].Message, "finished:") {
		t.Errorf("events = %+v", evs)
	}
}

func TestWorkerRefusesStalePlan(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	// Someone adds a VM to team 01 after the preview.
	f.Mu.Lock()
	f.addVM(proxmox.VM{VMID: 10107, Name: "team01-web", Node: "n1", Status: "running"})
	f.Mu.Unlock()

	newWorker(st, f).RunJob(bg, claim(t, st))

	j := job(t, st, id)
	if j.Status != store.StatusStale || !strings.Contains(j.Error, "changed since the preview") {
		t.Fatalf("job = %s: %s", j.Status, j.Error)
	}
	if len(f.DeletedIDs()) != 0 {
		t.Errorf("a stale job deleted %v", f.DeletedIDs())
	}
}

func TestWorkerStopsOnCancel(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	f.gate = make(chan struct{}) // tasks hang until cancelled
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	j := claim(t, st)

	done := make(chan struct{})
	go func() { w.RunJob(bg, j); close(done) }()
	time.Sleep(50 * time.Millisecond)
	if err := st.RequestCancel(bg, id, "bob"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("job didn't stop after cancel")
	}
	got := job(t, st, id)
	if got.Status != store.StatusCancelled || got.CancelledBy != "bob" {
		t.Errorf("job = %s by %q", got.Status, got.CancelledBy)
	}
	// The cancel cut both VMs off, so they read as interrupted, not failed.
	items, _ := st.Items(bg, id)
	for _, it := range items {
		if it.Status != store.ItemInterrupted {
			t.Errorf("item %s = %s (%s), want interrupted", it.Name, it.Status, it.Error)
		}
	}
}

// A worker told of cancels stops at once, not at its next heartbeat.
func TestWorkerStopsOnCancelBeforeItsNextHeartbeat(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	f.gate = make(chan struct{}) // tasks hang until cancelled
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	w.Cfg.Jobs.Heartbeat = time.Hour
	w.Cfg.Jobs.StaleAfter = 4 * time.Hour
	hubCtx, stopHub := context.WithCancel(bg)
	defer stopHub()
	hub := status.NewHub(st.Notifications, status.HubOptions{Logf: t.Logf})
	go hub.Run(hubCtx)
	subscribed := make(chan struct{})
	w.Cancels = func(id int64) (<-chan struct{}, func()) {
		defer close(subscribed)
		return hub.Wake(status.CancelTopic(id))
	}
	j := claim(t, st)

	done := make(chan struct{})
	go func() { w.RunJob(bg, j); close(done) }()
	<-subscribed
	// The hub's first message is a resync once it listens; wait for that
	// so the cancel's notice can't be sent before it does.
	for hub.Seq() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	if err := st.RequestCancel(bg, id, "bob"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("job didn't stop after cancel")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("cancel took %s", took)
	}
	if got := job(t, st, id); got.Status != store.StatusCancelled {
		t.Errorf("job = %s", got.Status)
	}
}

// A stop that arrives after every VM finished changes nothing, so the job
// records what really happened.
func TestWorkerKeepsOutcomeWhenStopArrivesAfterWork(t *testing.T) {
	for _, stop := range []string{"cancel", "shutdown"} {
		t.Run(stop, func(t *testing.T) {
			st := storetest.New(t)
			f := teamVMs()
			id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
			w := newWorker(st, f)
			ctx, shutdown := context.WithCancel(bg)
			defer shutdown()
			// Beats after the one that saw the cancel prove the run's
			// context was cancelled.
			var sawCancel atomic.Bool
			applied := make(chan struct{})
			var once sync.Once
			w.beat = func(ctx context.Context, jobID int64, worker string) (bool, error) {
				if sawCancel.Load() {
					once.Do(func() { close(applied) })
				}
				cancel, err := w.Store.Heartbeat(ctx, jobID, worker)
				if cancel {
					sawCancel.Store(true)
				}
				return cancel, err
			}
			w.OnEvent = func(ev apply.Event) {
				if !strings.HasPrefix(ev.Message, "finished:") {
					return
				}
				if stop == "shutdown" {
					shutdown()
					return
				}
				if err := st.RequestCancel(bg, id, "bob"); err != nil {
					t.Error(err)
				}
				select {
				case <-applied:
				case <-time.After(10 * time.Second):
					t.Error("the heartbeat never saw the cancel")
				}
			}

			w.RunJob(ctx, claim(t, st))

			got := job(t, st, id)
			if got.Status != store.StatusSucceeded || got.Error != "" {
				t.Errorf("job = %s: %s, want succeeded", got.Status, got.Error)
			}
			items, _ := st.Items(bg, id)
			for _, it := range items {
				if it.Status != store.ItemDone {
					t.Errorf("item %s = %s, want done", it.Name, it.Status)
				}
			}
		})
	}
}

func TestWorkerFailsJobThatCannotBePlanned(t *testing.T) {
	st := storetest.New(t)
	id, err := st.CreateJob(bg, store.NewJob{
		Kind: "teardown", Inputs: json.RawMessage(`{"kind":"teardown","teams":"5-1"}`),
		Plan: json.RawMessage(`{}`), Fingerprint: "x", LockKeys: []string{"team:01"}, CreatedBy: "alice",
		Credential: mustCreds().Seal(aliceTicket()),
	})
	if err != nil {
		t.Fatal(err)
	}
	newWorker(st, teamVMs()).RunJob(bg, claim(t, st))
	j := job(t, st, id)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "planning:") {
		t.Errorf("job = %s: %s", j.Status, j.Error)
	}
}

// A stop while the job is being planned is no planning failure: a shutdown
// interrupts the job and a lead's cancel cancels it, so it can be re-run.
func TestWorkerStopDuringPlanning(t *testing.T) {
	for _, stop := range []string{"shutdown", "cancel"} {
		t.Run(stop, func(t *testing.T) {
			st := storetest.New(t)
			f := teamVMs()
			id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
			f.listGate, f.listing = make(chan struct{}), make(chan struct{}, 1)
			w := newWorker(st, f)
			j := claim(t, st)
			ctx, shutdown := context.WithCancel(bg)
			defer shutdown()
			done := make(chan struct{})
			go func() { w.RunJob(ctx, j); close(done) }()
			<-f.listing // planning is reading the cluster
			if stop == "shutdown" {
				shutdown()
			} else if err := st.RequestCancel(bg, id, "lena"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the job didn't stop")
			}
			got := job(t, st, id)
			switch {
			case stop == "shutdown" && (got.Status != store.StatusInterrupted || !strings.Contains(got.Error, "shut down")):
				t.Errorf("job = %s: %s; want interrupted by the shutdown", got.Status, got.Error)
			case stop == "cancel" && got.Status != store.StatusCancelled:
				t.Errorf("job = %s: %s; want cancelled", got.Status, got.Error)
			}
			if strings.Contains(got.Error, "planning") {
				t.Errorf("error %q blames planning", got.Error)
			}
		})
	}
}

// holdBeats makes w's heartbeats succeed without touching the database
// until release is called; after that they reach the store again. While
// they are held, the job's heartbeat_at stays at its claim time, so a reap
// can't race a live heartbeat that would make the job look fresh.
func holdBeats(w *Worker) (release func()) {
	released := make(chan struct{})
	w.beat = func(ctx context.Context, jobID int64, worker string) (bool, error) {
		select {
		case <-released:
			return w.Store.Heartbeat(ctx, jobID, worker)
		default:
			return false, nil
		}
	}
	return sync.OnceFunc(func() { close(released) })
}

// reapNow has another replica reap job id, and fails unless exactly that job
// was reaped. Call it while the job's heartbeats are held (holdBeats).
func reapNow(t *testing.T, st *store.Store, id int64) {
	t.Helper()
	ids, err := st.ReapStale(bg, time.Nanosecond)
	if err != nil || !slices.Equal(ids, []int64{id}) {
		t.Fatalf("ReapStale = %v, %v; want [%d]", ids, err, id)
	}
}

func TestWorkerStopsWhenClaimIsLost(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	f.gate = make(chan struct{})
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	f.waiting = make(chan struct{}, 10)
	w := newWorker(st, f)
	release := holdBeats(w)
	j := claim(t, st)

	done := make(chan struct{})
	go func() { w.RunJob(bg, j); close(done) }()
	<-f.waiting // the job is waiting for a Proxmox task
	// Another replica decides this worker went silent.
	reapNow(t, st, id)
	release()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("job kept running after its claim was lost")
	}
	if got := job(t, st, id); got.Status != store.StatusInterrupted {
		t.Errorf("status = %s, want interrupted (the reaper's verdict stands)", got.Status)
	}
}

// Events after another replica reaped the job can't be stored; the worker
// logs them instead, so what happened to each VM isn't lost.
func TestWorkerLogsEventsItCannotRecord(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	f.gate = make(chan struct{})
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	f.waiting = make(chan struct{}, 10)
	w := newWorker(st, f)
	var mu sync.Mutex
	var logs []string
	w.Logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	var unstored []apply.Event
	stored := map[string]bool{}
	w.OnEvent = func(ev apply.Event) {
		mu.Lock()
		defer mu.Unlock()
		unstored = append(unstored, ev)
	}
	release := holdBeats(w)
	j := claim(t, st)
	done := make(chan struct{})
	go func() { w.RunJob(bg, j); close(done) }()
	<-f.waiting
	reapNow(t, st, id)
	release()
	<-done

	events, _ := st.Events(bg, id, 0, 1000)
	for _, ev := range events {
		stored[ev.Item+"|"+ev.Status+"|"+ev.Message] = true
	}
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(logs, "\n")
	n := 0
	for _, ev := range unstored {
		if stored[ev.Item+"|"+string(ev.Status)+"|"+ev.Message] {
			continue
		}
		n++
		if !strings.Contains(all, ev.Item) || !strings.Contains(all, string(ev.Status)) || !strings.Contains(all, ev.Message) {
			t.Errorf("event %+v was neither stored nor logged; logs:\n%s", ev, all)
		}
	}
	if n == 0 {
		t.Fatal("no event came after the reap")
	}
}

func TestWorkerShutdownInterruptsJob(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	f.gate = make(chan struct{})
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	j := claim(t, st)

	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { w.RunJob(ctx, j); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	got := job(t, st, id)
	if got.Status != store.StatusInterrupted || !strings.Contains(got.Error, "shut down") {
		t.Errorf("job = %s: %s", got.Status, got.Error)
	}
	items, _ := st.Items(bg, id)
	for _, it := range items {
		if it.Status != store.ItemInterrupted {
			t.Errorf("item %s = %s (%s), want interrupted", it.Name, it.Status, it.Error)
		}
	}
}

func TestWorkerRunLoopClaimsJobs(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	id := submit(t, st, f, Inputs{Kind: pods.KindPower, Teams: "1", Action: "stop"})
	w := newWorker(st, f)

	ctx, cancel := context.WithCancel(bg)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); w.Run(ctx) }() //nolint:errcheck
	deadline := time.Now().Add(10 * time.Second)
	for job(t, st, id).Active() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if got := job(t, st, id); got.Status != store.StatusSucceeded {
		t.Errorf("status = %s (%s)", got.Status, got.Error)
	}
}

func TestWorkerStopsWhenHeartbeatsKeepFailing(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	f.gate = make(chan struct{}) // tasks hang until cancelled
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	w.Cfg.Jobs.StaleAfter = 100 * time.Millisecond
	w.beat = func(context.Context, int64, string) (bool, error) { return false, errors.New("db down") }
	j := claim(t, st)

	done := make(chan struct{})
	go func() { w.RunJob(bg, j); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("job kept running with no heartbeats")
	}
	got := job(t, st, id)
	if got.Status != store.StatusInterrupted || !strings.Contains(got.Error, "lost contact with the database") {
		t.Errorf("job = %s: %s", got.Status, got.Error)
	}
	if len(f.DeletedIDs()) != 0 {
		t.Errorf("deleted %v while cut off", f.DeletedIDs())
	}
}

func TestWorkerSurvivesOneFailedHeartbeat(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	f.gate = make(chan struct{}) // held until three heartbeats have been attempted
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	w.Cfg.Jobs.StaleAfter = 10 * time.Second // one failure is far under the threshold
	var calls atomic.Int32
	w.beat = func(ctx context.Context, jobID int64, worker string) (bool, error) {
		switch calls.Add(1) {
		case 1:
			return false, errors.New("blip")
		case 3:
			close(f.gate)
		}
		return w.Store.Heartbeat(ctx, jobID, worker)
	}
	w.RunJob(bg, claim(t, st))
	if got := job(t, st, id); got.Status != store.StatusSucceeded {
		t.Errorf("job = %s: %s", got.Status, got.Error)
	}
}

func TestWorkerStopsWhenHeartbeatHangs(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	f.gate = make(chan struct{})
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	w.Cfg.Jobs.StaleAfter = 100 * time.Millisecond
	// A partition that drops packets: the call only ends when its ctx does.
	w.beat = func(ctx context.Context, _ int64, _ string) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}
	j := claim(t, st)

	start := time.Now()
	done := make(chan struct{})
	go func() { w.RunJob(bg, j); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("job kept running while heartbeats hung")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v to stop", d)
	}
	got := job(t, st, id)
	if got.Status != store.StatusInterrupted || !strings.Contains(got.Error, "lost contact with the database") {
		t.Errorf("job = %s: %s", got.Status, got.Error)
	}
	if len(f.DeletedIDs()) != 0 {
		t.Errorf("deleted %v while cut off", f.DeletedIDs())
	}
}

// A machine that sleeps stops the monotonic clock, so a timer alone would
// never fire; the wall clock shows the gap when it wakes, even if its beats
// then succeed.
func TestWorkerStopsAfterWallClockGap(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	f.gate = make(chan struct{}) // tasks hang until cancelled
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	w.Cfg.Jobs.StaleAfter = 10 * time.Second // the timer won't fire during the test
	var skew atomic.Int64
	w.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	var calls atomic.Int32
	w.beat = func(ctx context.Context, jobID int64, worker string) (bool, error) {
		stop, err := w.Store.Heartbeat(ctx, jobID, worker)
		if calls.Add(1) == 2 {
			skew.Store(int64(time.Hour)) // the laptop slept after this beat
		}
		return stop, err
	}
	j := claim(t, st)

	done := make(chan struct{})
	go func() { w.RunJob(bg, j); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("job kept running after the wall clock jumped past the threshold")
	}
	got := job(t, st, id)
	if got.Status != store.StatusInterrupted || !strings.Contains(got.Error, "lost contact with the database") {
		t.Errorf("job = %s: %s", got.Status, got.Error)
	}
}

func TestWorkerOnEventSeesProgress(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	var mu sync.Mutex
	var seen []apply.Event
	w.OnEvent = func(ev apply.Event) { mu.Lock(); seen = append(seen, ev); mu.Unlock() }
	w.RunJob(bg, claim(t, st))

	stored, _ := st.Events(bg, id, 0, 1000)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || len(seen) != len(stored) {
		t.Errorf("OnEvent saw %d events, store has %d", len(seen), len(stored))
	}
}

func TestWorkerReportsJobsItRuns(t *testing.T) {
	type report struct {
		event  string
		id     int64
		status string
	}
	watch := func(w *Worker) func() []report {
		var mu sync.Mutex
		var got []report
		w.OnStart = func(j *store.Job) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, report{"start", j.ID, j.Status})
		}
		w.OnFinish = func(j *store.Job, out store.Outcome) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, report{"finish", j.ID, out.Status})
		}
		return func() []report { mu.Lock(); defer mu.Unlock(); return append([]report(nil), got...) }
	}

	t.Run("succeeded", func(t *testing.T) {
		st := storetest.New(t)
		f := teamVMs()
		id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
		w := newWorker(st, f)
		reports := watch(w)
		w.RunJob(bg, claim(t, st))
		want := []report{{"start", id, store.StatusRunning}, {"finish", id, store.StatusSucceeded}}
		if got := reports(); !slices.Equal(got, want) {
			t.Errorf("reports = %+v, want %+v", got, want)
		}
	})

	t.Run("shut down", func(t *testing.T) {
		st := storetest.New(t)
		f := teamVMs()
		f.gate, f.waiting = make(chan struct{}), make(chan struct{}, 10)
		id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
		w := newWorker(st, f)
		reports := watch(w)
		j := claim(t, st)
		ctx, cancel := context.WithCancel(bg)
		done := make(chan struct{})
		go func() { w.RunJob(ctx, j); close(done) }()
		<-f.waiting // the job is waiting for a Proxmox task
		cancel()
		<-done
		want := []report{{"start", id, store.StatusRunning}, {"finish", id, store.StatusInterrupted}}
		if got := reports(); !slices.Equal(got, want) {
			t.Errorf("reports = %+v, want %+v", got, want)
		}
	})

	t.Run("claim lost", func(t *testing.T) {
		st := storetest.New(t)
		f := teamVMs()
		f.gate, f.waiting = make(chan struct{}), make(chan struct{}, 10)
		id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
		w := newWorker(st, f)
		release := holdBeats(w)
		reports := watch(w)
		j := claim(t, st)
		done := make(chan struct{})
		go func() { w.RunJob(bg, j); close(done) }()
		<-f.waiting
		// Another replica decides this worker went silent.
		reapNow(t, st, id)
		release()
		<-done
		// The worker recorded nothing, so it reports no finish.
		want := []report{{"start", id, store.StatusRunning}}
		if got := reports(); !slices.Equal(got, want) {
			t.Errorf("reports = %+v, want %+v", got, want)
		}
	})
}

// A stop during the pause before a retry round leaves a failed VM without its
// retry, so the job didn't finish: it reads cancelled or interrupted, not
// completed with failures.
func TestWorkerStopDuringRetryPauseIsUnfinished(t *testing.T) {
	for _, stop := range []string{"cancel", "shutdown"} {
		t.Run(stop, func(t *testing.T) {
			st := storetest.New(t)
			f := teamVMs()
			f.deleteErr = map[int]error{10105: &proxmox.APIError{Status: 400, Message: "Parameter verification failed."}} // retried, unlike a 403
			id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
			w := newWorker(st, f)
			w.Cfg.Retry.Rounds = 1
			w.Cfg.Retry.RoundPause = time.Minute
			ctx, shutdown := context.WithCancel(bg)
			defer shutdown()
			w.OnEvent = func(ev apply.Event) {
				if !strings.HasPrefix(ev.Message, "retry round 1") {
					return
				}
				if stop == "shutdown" {
					shutdown()
				} else if err := st.RequestCancel(bg, id, "bob"); err != nil {
					t.Error(err)
				}
			}

			done := make(chan struct{})
			go func() { w.RunJob(ctx, claim(t, st)); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("job didn't stop during the retry pause")
			}

			got := job(t, st, id)
			want := store.StatusCancelled
			if stop == "shutdown" {
				want = store.StatusInterrupted
			}
			if got.Status != want {
				t.Errorf("job = %s: %s, want %s", got.Status, got.Error, want)
			}
			if stop == "cancel" && got.CancelledBy != "bob" {
				t.Errorf("cancelled by %q", got.CancelledBy)
			}
		})
	}
}

// Summary.Failed holds only VMs that failed; VMs a stop cut off are listed as
// interrupted, matching their item status.
func TestOutcomeSummaryListsInterruptedApart(t *testing.T) {
	res := apply.Result{
		Succeeded: []string{"team01-a"},
		Failed: map[string]error{
			"team01-b": &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/10102, VM.Allocate)"},
		},
		Interrupted: map[string]error{
			"team01-c": errors.New("not run: context canceled"),
			"team01-d": context.Canceled,
		},
		CleanupFailed: map[string]error{},
	}

	out := outcomeOf(res)
	var sum Summary
	if err := json.Unmarshal(out.Summary, &sum); err != nil {
		t.Fatal(err)
	}
	if len(sum.Failed) != 1 || sum.Failed["team01-b"] == "" {
		t.Errorf("Failed = %v, want only team01-b", sum.Failed)
	}
	if want := []string{"team01-c", "team01-d"}; !slices.Equal(sum.Interrupted, want) {
		t.Errorf("Interrupted = %v, want %v", sum.Interrupted, want)
	}
	for name, want := range map[string]string{"team01-b": store.ItemFailed, "team01-c": store.ItemInterrupted, "team01-d": store.ItemInterrupted} {
		if got := out.Items[name].Status; got != want {
			t.Errorf("item %s = %s, want %s", name, got, want)
		}
	}
}

// A run whose only unfinished VMs were cut off by a stop still completed
// with failures.
func TestInterruptedOnlyOutcomeHasFailures(t *testing.T) {
	out := outcomeOf(apply.Result{Interrupted: map[string]error{"team01-a": context.Canceled}})
	if out.Status != store.StatusCompletedWithFailures {
		t.Errorf("status = %s, want %s", out.Status, store.StatusCompletedWithFailures)
	}
}

// A job runs with the config of the process that claims it. A preview made
// under other settings for what a run does to VMs (here, how long a
// teardown waits for a clean shutdown) is stale: nothing runs.
func TestWorkerRefusesPlanUnderOtherConfig(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	w.Cfg.Teardown.ShutdownTimeout = 5 * time.Minute

	w.RunJob(bg, claim(t, st))

	j := job(t, st, id)
	if j.Status != store.StatusStale || !strings.Contains(j.Error, "config") {
		t.Fatalf("job = %s: %s", j.Status, j.Error)
	}
	if len(f.DeletedIDs()) != 0 {
		t.Errorf("a stale job deleted %v", f.DeletedIDs())
	}
}

// A job stored before plans carried their config's hash can't show that it
// was previewed under this config, so it is stale, and says so.
func TestWorkerRefusesJobStoredWithoutConfigHash(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	in := Inputs{Kind: pods.KindTeardown, Teams: "1"}
	cfg := testCfg()
	p, err := BuildPlan(bg, pods.NewPlanner(f, cfg), in)
	if err != nil {
		t.Fatal(err)
	}
	p.Config = ""
	id, err := Submit(bg, st, in, p, Submitter{User: "alice", Credential: aliceTicket(), Seal: mustCreds()})
	if err != nil {
		t.Fatal(err)
	}

	newWorker(st, f).RunJob(bg, claim(t, st))

	j := job(t, st, id)
	if j.Status != store.StatusStale || !strings.Contains(j.Error, "config") {
		t.Fatalf("job = %s: %s, want stale for a config change", j.Status, j.Error)
	}
	if len(f.DeletedIDs()) != 0 {
		t.Errorf("a stale job deleted %v", f.DeletedIDs())
	}
}

// Each of the executor's verdicts is recorded as the store names it.
func TestLeftConfigRecordsTheVerdict(t *testing.T) {
	for l, want := range map[apply.Left]string{apply.LeftChanged: "", apply.LeftUntouched: store.LeftUntouched, apply.LeftConverged: store.LeftConverged} {
		if got := leftConfig(l); got != want {
			t.Errorf("leftConfig(%v) = %q, want %q", l, got, want)
		}
	}
}

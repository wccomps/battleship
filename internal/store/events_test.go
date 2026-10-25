package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// A follower using Events(after=last) must never miss a line, so a
// late-committing event must not get an ID below a visible one.
func TestAddEventCommitsInIDOrder(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	mustClaim(t, s, "w")

	held := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	store.SetBeforeEventCommit(func(jobID int64, ev store.Event) {
		if jobID == id && ev.Message == "first" {
			once.Do(func() { close(held) })
			<-release
		}
	})
	t.Cleanup(func() { store.SetBeforeEventCommit(nil) })

	// The two events touch different items (the second none), so only the
	// job's row can order them.
	add := func(item, msg string) <-chan error {
		done := make(chan error, 1)
		go func() {
			done <- s.AddEvent(ctx, id, store.Event{At: time.Now(), Item: item, Step: "stop", Status: "failed", Message: msg})
		}()
		return done
	}
	firstDone := add("team01-teak", "first")
	<-held

	// While "first" waits to commit, add "second": it either commits
	// straight away (the bug) or waits for the job's row.
	secondDone := add("", "second")
	waiter, err := store.Acquire(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Release()
	var secondErr error
	secondFinished := false
	deadline := time.Now().Add(20 * time.Second)
	for !secondFinished {
		select {
		case secondErr = <-secondDone:
			secondFinished = true
			continue
		default:
		}
		var waiting int
		if err := waiter.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break // "second" waits for "first"
		}
		if time.Now().After(deadline) {
			t.Fatal(`"second" neither finished nor waited for a lock`)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A reader polls now: it sees what has committed so far.
	seen, err := s.Events(ctx, id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var last int64
	var got []string
	for _, ev := range seen {
		last = ev.ID
		got = append(got, ev.Message)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf(`adding "first": %v`, err)
	}
	if !secondFinished {
		secondErr = <-secondDone
	}
	if secondErr != nil {
		t.Fatalf(`adding "second": %v`, secondErr)
	}

	// The reader polls again from where it was.
	rest, err := s.Events(ctx, id, last, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range rest {
		got = append(got, ev.Message)
	}
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("a reader following the log with after=last saw %q, want [first second]", got)
	}
}

// Running AddEvent beside the other job-row writers neither deadlocks nor
// starves the pool. This stress run is probabilistic;
// TestRequestCancelOnFinishedJobDoesNotStarveThePool pins the starvation.
func TestAddEventAlongsideJobRowWriters(t *testing.T) {
	s := storetest.New(t)
	ids := []int64{create(t, s, "team:01"), create(t, s, "team:02")}
	for range ids {
		if j, err := s.Claim(ctx, "w", time.Nanosecond); err != nil || j == nil { // the reaper races the rest
			t.Fatalf("claim: %v %v", j, err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 1000)
	for _, id := range ids {
		for range 4 {
			wg.Go(func() {
				for range 25 {
					err := s.AddEvent(ctx, id, store.Event{At: time.Now(), Item: "team01-teak", Step: "stop", Status: "done"})
					if err != nil && !errors.Is(err, store.ErrNotActive) {
						errs <- fmt.Errorf("addevent: %w", err)
					}
				}
			})
		}
		wg.Go(func() {
			for range 25 {
				if _, err := s.Heartbeat(ctx, id, "w"); err != nil && !errors.Is(err, store.ErrLostClaim) {
					errs <- fmt.Errorf("heartbeat: %w", err)
				}
			}
		})
		wg.Go(func() {
			if err := s.RequestCancel(ctx, id, "lead"); err != nil && !errors.Is(err, store.ErrNotActive) {
				errs <- fmt.Errorf("cancel: %w", err)
			}
			err := s.Finish(ctx, id, "w", store.Outcome{Status: store.StatusCancelled,
				Items: map[string]store.ItemOutcome{"team01-teak": {Status: store.ItemDone}}})
			if err != nil && !errors.Is(err, store.ErrLostClaim) {
				errs <- fmt.Errorf("finish: %w", err)
			}
		})
	}
	wg.Go(func() {
		for range 25 {
			if _, err := s.ReapStale(ctx, 0); err != nil {
				errs <- fmt.Errorf("reap: %w", err)
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent job writers: %v", err)
	}
}

// waitForLockWaits waits until n sessions on s's database wait for a lock.
// The 10s limit only guards against a hang.
func waitForLockWaits(t *testing.T, s *store.Store, n int) {
	t.Helper()
	conn, err := store.Acquire(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions wait for a lock after 10s, want %d", waiting, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Deterministic pool starvation: a RequestCancel that waited on a Finish
// still holds the row lock while an AddEvent holds the other of two
// connections waiting on it. RequestCancel must finish on its own
// connection, or the pool starves until lock_timeout (55P03).
func TestRequestCancelOnFinishedJobDoesNotStarveThePool(t *testing.T) {
	big, dbURL := storetest.NewWithURL(t)
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("pool_max_conns", "2")
	u.RawQuery = q.Encode()
	s, err := store.Open(ctx, u.String(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id := create(t, big, "team:01")
	if j, err := big.Claim(ctx, "w", time.Hour); err != nil || j == nil {
		t.Fatalf("claim: %v %v", j, err)
	}
	// A Finish in progress, on a connection of its own: it holds the row.
	fin, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer fin.Close(ctx)
	tx, err := fin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status = 'succeeded', finished_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	held, release := make(chan struct{}), make(chan struct{})
	store.SetBeforeCancelRecheck(func(jobID int64) {
		if jobID == id {
			close(held)
			<-release
		}
	})
	defer store.SetBeforeCancelRecheck(nil)
	cancelled := make(chan error, 1)
	go func() { cancelled <- s.RequestCancel(ctx, id, "lead") }()
	waitForLockWaits(t, big, 1) // RequestCancel waits for the Finish
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held: // RequestCancel found the job finished, holding the row
	case <-time.After(10 * time.Second):
		t.Fatal("RequestCancel didn't find the job finished within 10s")
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 4 {
		wg.Go(func() {
			// The job has finished by the time it gets the row.
			if err := s.AddEvent(ctx, id, store.Event{At: time.Now(), Item: "team01-teak", Step: "stop", Status: "done"}); !errors.Is(err, store.ErrNotActive) {
				errs <- fmt.Errorf("addevent: %w, want ErrNotActive", err)
			}
		})
	}
	waitForLockWaits(t, big, 1) // an AddEvent holds the pool's other connection
	close(release)
	if err := <-cancelled; !errors.Is(err, store.ErrNotActive) {
		t.Errorf("RequestCancel = %v, want ErrNotActive", err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("AddEvent alongside RequestCancel: %v", err)
	}
	evs, err := big.Events(ctx, id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Errorf("%d events stored for a finished job, want 0", len(evs))
	}
}

// Only a running job records events; followers rely on a finished job's
// log and item statuses being final.
func TestAddEventRefusesJobsNotRunning(t *testing.T) {
	s := storetest.New(t)
	pending := create(t, s, "team:01")
	ev := store.Event{At: time.Now(), Item: "team01-teak", Step: "stop", Status: "failed", Message: "late"}
	if err := s.AddEvent(ctx, pending, ev); !errors.Is(err, store.ErrNotActive) {
		t.Errorf("AddEvent on a pending job: %v, want ErrNotActive", err)
	}

	id := create(t, s, "team:02")
	if _, err := storetest.ClaimJob(s, id, "w", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, id, "w", store.Outcome{Status: store.StatusInterrupted}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Items(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddEvent(ctx, id, ev); !errors.Is(err, store.ErrNotActive) {
		t.Errorf("AddEvent on a finished job: %v, want ErrNotActive", err)
	}
	if evs, err := s.Events(ctx, id, 0, 10); err != nil || len(evs) != 0 {
		t.Errorf("finished job's events = %v, %v; want none", evs, err)
	}
	after, err := s.Items(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for i := range after {
		if after[i].Status != before[i].Status || after[i].Step != before[i].Step || after[i].Error != before[i].Error {
			t.Errorf("item %s changed: %+v, was %+v", after[i].Name, after[i], before[i])
		}
	}
	if err := s.AddEvent(ctx, 999999, ev); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("AddEvent on an unknown job: %v, want ErrNotFound", err)
	}
}

// An item keeps how each step last ended; info events and job-level events
// leave it alone, and a step run again (a retry round) is overwritten.
func TestAddEventRecordsStepOutcomes(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	if _, err := storetest.ClaimJob(s, id, "w", time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []store.Event{
		{Item: "team01-teak", Step: "stop", Status: "done"},
		{Item: "team01-teak", Step: "delete", Status: "failed", Message: "locked"},
		{Item: "team01-teak", Step: "delete", Status: "info", Message: "freed disks"},
		{Item: "", Step: "clone", Status: "done", Message: "template created"},
		{Item: "team01-teak", Step: "delete", Status: "skipped"},
	} {
		ev.At = time.Now()
		if err := s.AddEvent(ctx, id, ev); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.Items(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"stop": "done", "delete": "skipped"}
	if len(items) == 0 || !maps.Equal(items[0].Steps, want) {
		t.Errorf("items = %+v, want steps %v", items, want)
	}
}

// An item's StepAt is its last step event's time; an info line sets its
// Note and leaves StepAt, and the next step event clears the note. A start
// clears the outcomes of an earlier run.
func TestItemStepAtAndNote(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	mustClaim(t, s, "w")
	at := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	item := func() store.Item {
		t.Helper()
		items, err := s.Items(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Name == "team01-teak" {
				return it
			}
		}
		t.Fatalf("no team01-teak in %v", items)
		return store.Item{}
	}
	for _, c := range []struct {
		ev       store.Event
		stepAt   time.Time
		wantNote string
	}{
		{store.Event{At: at, Step: "stop", Status: "started"}, at, ""},
		{store.Event{At: at.Add(time.Minute), Step: "stop", Status: "info", Message: "waiting: VM is locked"}, at, "waiting: VM is locked"},
		{store.Event{At: at.Add(2 * time.Minute), Step: "stop", Status: "info", Message: "still locked"}, at, "still locked"},
		{store.Event{At: at.Add(3 * time.Minute), Step: "stop", Status: "done"}, at.Add(3 * time.Minute), ""},
		{store.Event{At: at.Add(4 * time.Minute), Step: "delete", Status: "failed", Message: "locked"}, at.Add(4 * time.Minute), ""},
	} {
		c.ev.Item = "team01-teak"
		if err := s.AddEvent(ctx, id, c.ev); err != nil {
			t.Fatal(err)
		}
		if it := item(); !it.StepAt.Equal(c.stepAt) || it.Note != c.wantNote {
			t.Errorf("after %s %q: StepAt %v, note %q; want %v, %q", c.ev.Status, c.ev.Message, it.StepAt, it.Note, c.stepAt, c.wantNote)
		}
	}

	// A retry round starts it afresh: its earlier run's outcomes go.
	if it := item(); len(it.Steps) != 2 {
		t.Fatalf("steps before the retry round: %v", it.Steps)
	}
	if err := s.AddEvent(ctx, id, store.Event{At: at.Add(5 * time.Minute), Item: "team01-teak", Step: "stop", Status: "started"}); err != nil {
		t.Fatal(err)
	}
	if it := item(); len(it.Steps) != 0 || !it.StepAt.Equal(at.Add(5*time.Minute)) {
		t.Errorf("after a restart: steps %v, StepAt %v", it.Steps, it.StepAt)
	}
}

// A template build's info lines set Note on the pending items that clone
// from it, leaving them pending and untouched when the template finishes.
func TestTemplateEventsNotePendingItems(t *testing.T) {
	s := storetest.New(t)
	nj := newJob("team:01")
	nj.Plan = json.RawMessage(`{"kind":"deploy","items":[` +
		`{"name":"team01-teak","template":"teak.tango.delta.tpl","steps":["clone","start"]},` +
		`{"name":"team01-dc","template":"teak.tango.delta.tpl","steps":["network"]}]}`)
	nj.Items = []store.NewItem{{Name: "team01-teak", Team: "01", VMID: 10121}, {Name: "team01-dc", Team: "01", VMID: 10101}}
	id, err := s.CreateJob(ctx, nj)
	if err != nil {
		t.Fatal(err)
	}
	mustClaim(t, s, "w")
	at := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	if err := s.AddEvent(ctx, id, store.Event{At: at, Item: "teak.tango.delta.tpl", Step: "clone", Status: "info", Message: "cloning master teak.tango.delta"}); err != nil {
		t.Fatal(err)
	}
	items, err := s.Items(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Status != store.ItemPending || items[0].Step != "" || items[0].Note != "cloning master teak.tango.delta" {
		t.Errorf("team01-teak while its template builds = %+v", items[0])
	}
	if items[1].Note != "" {
		t.Errorf("team01-dc (does not clone the template) note = %q, want empty", items[1].Note)
	}
	if err := s.AddEvent(ctx, id, store.Event{At: at.Add(time.Minute), Item: "teak.tango.delta.tpl", Step: "clone", Status: "done", Message: "template created"}); err != nil {
		t.Fatal(err)
	}
	items, err = s.Items(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Status != store.ItemPending || items[0].Note != "" {
		t.Errorf("team01-teak after its template is created = %+v, want pending with empty note", items[0])
	}
}

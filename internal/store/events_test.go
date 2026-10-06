package store_test

import (
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

// A reader that follows a job's log with Events(after=last) must never miss
// a line. The line that commits late (here "first", held back before its
// commit) must not get an ID below one that is already visible: AddEvent
// serializes on the job's row, so event IDs are handed out in commit order.
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

// AddEvent locks the job's row before its items, as Finish, RequestCancel
// and ReapStale do, so running them all at once never deadlocks. Nor does
// the pool starve: a RequestCancel that finds the job already finished
// still holds the row's lock, so it must not wait for another pool
// connection (these used to end in lock timeouts). This stress run catches
// that only sometimes; TestRequestCancelOnFinishedJobDoesNotStarveThePool
// pins it down every time.
func TestAddEventAlongsideJobRowWriters(t *testing.T) {
	s := storetest.New(t)
	ids := []int64{create(t, s, "team:01"), create(t, s, "team:02")}
	for range ids {
		if j, err := s.Claim(ctx, "w"); err != nil || j == nil {
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

// The deterministic form of the pool-starvation case above. A RequestCancel
// whose UPDATE waited for a Finish, then found the job no longer active,
// still holds the job row's lock. On a two-connection pool, an AddEvent
// holds the other connection while it waits for that lock, so RequestCancel
// must finish on its own connection: waiting for another one would starve
// the pool until AddEvent's lock_timeout (55P03).
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
	if j, err := big.Claim(ctx, "w"); err != nil || j == nil {
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

// Only a running job records events. A finished job has every event it
// will ever have, which its page and the CLI's follower rely on, and its
// items keep the statuses it finished with.
func TestAddEventRefusesJobsNotRunning(t *testing.T) {
	s := storetest.New(t)
	pending := create(t, s, "team:01")
	ev := store.Event{At: time.Now(), Item: "team01-teak", Step: "stop", Status: "failed", Message: "late"}
	if err := s.AddEvent(ctx, pending, ev); !errors.Is(err, store.ErrNotActive) {
		t.Errorf("AddEvent on a pending job: %v, want ErrNotActive", err)
	}

	id := create(t, s, "team:02")
	if _, err := s.ClaimJob(ctx, id, "w"); err != nil {
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
	if _, err := s.ClaimJob(ctx, id, "w"); err != nil {
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

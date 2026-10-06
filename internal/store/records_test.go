package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

func TestMigrateIsIdempotent(t *testing.T) {
	s := storetest.New(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	create(t, s, "team:01")
}

func TestCreateAndReadJob(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")

	j, err := s.Job(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != store.StatusPending || j.CreatedBy != "alice" || !reflect.DeepEqual(j.LockKeys, []string{"team:01"}) || !j.Active() {
		t.Errorf("job = %+v", j)
	}
	items, err := s.Items(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Status != store.ItemPending || items[1].Status != store.ItemBlocked || items[1].Error != "no snapshot" {
		t.Errorf("items = %+v", items)
	}
	id2 := create(t, s, "team:02")
	jobs, err := s.Jobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != id2 || jobs[1].ID != id {
		t.Errorf("Jobs order = %v, %v; want newest first", jobs[0].ID, jobs[1].ID)
	}
	if _, err := s.Job(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing job err = %v", err)
	}
}

func TestCreateJobRequiresFields(t *testing.T) {
	s := storetest.New(t)
	nj := newJob()
	if _, err := s.CreateJob(ctx, nj); !errors.Is(err, store.ErrEmptyInput) {
		t.Errorf("no lock keys: err = %v", err)
	}
}

// A listener has a connection of its own, so closing the store doesn't wait
// for it to stop.
func TestCloseDoesntWaitForListeners(t *testing.T) {
	s := storetest.New(t)
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := s.Notifications(lctx)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close waited for the listener")
	}
	cancel()
	for range ch { // ends once the listener has stopped
	}
}

func TestNotificationsDeliverChangedJobs(t *testing.T) {
	s := storetest.New(t)
	lctx, cancel := context.WithCancel(ctx)
	ch, err := s.Notifications(lctx)
	if err != nil {
		t.Fatal(err)
	}
	next := func(what string) store.Notice {
		t.Helper()
		select {
		case n := <-ch:
			return n
		case <-time.After(5 * time.Second):
			t.Fatalf("no notification for %s", what)
			return store.Notice{}
		}
	}
	// A log line is told apart from changes to the job's status or items:
	// only the job's own page shows it.
	id := create(t, s, "team:01")
	if n := next("a new job"); n != (store.Notice{JobID: id}) {
		t.Errorf("new job: %+v, want %d", n, id)
	}
	mustClaim(t, s, "w")
	if n := next("a claim"); n != (store.Notice{JobID: id}) {
		t.Errorf("claim: %+v, want %d", n, id)
	}
	if err := s.AddEvent(ctx, id, store.Event{At: time.Now(), Item: "team01-teak", Step: "stop", Status: "started"}); err != nil {
		t.Fatal(err)
	}
	if n := next("a log line"); n != (store.Notice{JobID: id, Log: true}) {
		t.Errorf("log line: %+v, want a log notice for %d", n, id)
	}
	// A cancel request is marked, so its worker can act on it at once.
	if err := s.RequestCancel(ctx, id, "bob"); err != nil {
		t.Fatal(err)
	}
	if n := next("a cancel request"); n != (store.Notice{JobID: id, Cancel: true}) {
		t.Errorf("cancel: %+v, want a cancel notice for %d", n, id)
	}
	if err := s.Finish(ctx, id, "w", store.Outcome{Status: store.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	if n := next("the job's end"); n != (store.Notice{JobID: id}) {
		t.Errorf("finish: %+v, want %d", n, id)
	}
	cancel()
	for range ch { // must close after cancel
	}
}

package store_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// secondProcess opens another Store on the same database, as another replica
// or a direct CLI run would.
func secondProcess(t *testing.T, url string) *store.Store {
	t.Helper()
	s, err := store.Open(ctx, url, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func acquireWithin(s *store.Store, name string, n int, d time.Duration) (func(), error) {
	c, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return s.AcquireSlot(c, name, n)
}

func TestSlotsAreSharedAcrossProcesses(t *testing.T) {
	a, url := storetest.NewWithURL(t)
	b := secondProcess(t, url)

	release, err := acquireWithin(a, "storage-ops", 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireWithin(b, "storage-ops", 1, 300*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second process took the only slot while the first held it: err = %v", err)
	}
	if r, err := acquireWithin(b, "deletes", 1, time.Second); err != nil {
		t.Fatalf("a slot of another name was not free: %v", err)
	} else {
		r()
	}
	release()
	r, err := acquireWithin(b, "storage-ops", 1, 5*time.Second)
	if err != nil {
		t.Fatalf("slot not free after release: %v", err)
	}
	r()
}

func TestSlotsCountUpToN(t *testing.T) {
	a, url := storetest.NewWithURL(t)
	b := secondProcess(t, url)
	var releases []func()
	for _, s := range []*store.Store{a, a} {
		r, err := acquireWithin(s, "deletes", 2, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, r)
	}
	for i, s := range []*store.Store{a, b} {
		if _, err := acquireWithin(s, "deletes", 2, 300*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("process %d took a third of two slots: err = %v", i, err)
		}
	}
	releases[0]()
	r, err := acquireWithin(b, "deletes", 2, 5*time.Second)
	if err != nil {
		t.Fatalf("freed slot not taken: %v", err)
	}
	r()
	releases[1]()
	releases[0]() // a second release is harmless
}

// A slot's lock lives in a database session; when that session dies the
// slot is free again, and the store opens a new one for its next slot.
func TestSlotsRecoverFromALostSession(t *testing.T) {
	a, url := storetest.NewWithURL(t)
	b := secondProcess(t, url)
	if _, err := acquireWithin(a, "storage-ops", 1, time.Second); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory' AND granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database()) AND pid <> pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	r, err := acquireWithin(b, "storage-ops", 1, 5*time.Second)
	if err != nil {
		t.Fatalf("slot of a dead session not free: %v", err)
	}
	r()
	r, err = acquireWithin(a, "deletes", 1, 5*time.Second)
	if err != nil {
		t.Fatalf("store did not replace its dead slot session: %v", err)
	}
	r()
}

// killSlotSessions ends every session of the test's database that holds an
// advisory lock, which in these tests is the slot session of a store.
func killSlotSessions(t *testing.T, url string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory' AND granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database()) AND pid <> pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
}

// A holder whose session died must not, on release, free a slot that the
// replacement session took since.
func TestStaleReleaseKeepsNewSessionsSlot(t *testing.T) {
	a, url := storetest.NewWithURL(t)
	b := secondProcess(t, url)
	stale, err := acquireWithin(a, "deletes", 2, time.Second) // slot 0, first session
	if err != nil {
		t.Fatal(err)
	}
	killSlotSessions(t, url)
	r1, err := acquireWithin(a, "deletes", 2, 5*time.Second) // reconnects, slot 1
	if err != nil {
		t.Fatal(err)
	}
	defer r1()
	r0, err := acquireWithin(a, "deletes", 2, 5*time.Second) // slot 0, new session
	if err != nil {
		t.Fatal(err)
	}
	defer r0()
	stale()
	if r, err := acquireWithin(b, "deletes", 2, 300*time.Millisecond); err == nil {
		r()
		t.Fatal("stale release freed the new session's slot: another process took a third of two")
	}
}

// Replacing a dead slot session must not make releases wait for the new
// connection.
func TestReleaseDoesNotWaitForReconnect(t *testing.T) {
	a, url := storetest.NewWithURL(t)
	stale, err := acquireWithin(a, "deletes", 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.SlotConnConfig(a)
	dial, gate := cfg.DialFunc, make(chan struct{})
	dialing := make(chan struct{}, 1)
	cfg.DialFunc = func(c context.Context, network, addr string) (net.Conn, error) {
		select {
		case dialing <- struct{}{}:
		default:
		}
		<-gate
		return dial(c, network, addr)
	}
	killSlotSessions(t, url)
	done := make(chan error, 1)
	go func() {
		r, err := acquireWithin(a, "deletes", 2, 10*time.Second)
		if err == nil {
			r()
		}
		done <- err
	}()
	<-dialing
	released := make(chan struct{})
	go func() { stale(); close(released) }()
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Error("release waited for the slot session's reconnect")
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-released
}

// Processes that pass different n for one name run up to the largest n at
// once, as config.Concurrency documents for deletes.
func TestSlotsLargestNWins(t *testing.T) {
	a, url := storetest.NewWithURL(t)
	b := secondProcess(t, url)
	ra, err := acquireWithin(a, "deletes", 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer ra()
	rb, err := acquireWithin(b, "deletes", 2, time.Second)
	if err != nil {
		t.Fatalf("a process with n = 2 found no slot while one with n = 1 held one: %v", err)
	}
	defer rb()
	if _, err := acquireWithin(a, "deletes", 1, 300*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("n = 1 took a slot while two were held: err = %v", err)
	}
}

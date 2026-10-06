package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

func TestSessionsHaveTimeouts(t *testing.T) {
	s := storetest.New(t)
	conn, err := store.Acquire(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for setting, want := range map[string]string{
		"idle_in_transaction_session_timeout": "30s",
		"lock_timeout":                        "10s",
		"tcp_keepalives_idle":                 "30",
		"tcp_keepalives_interval":             "10",
		"tcp_keepalives_count":                "3",
	} {
		var got string
		if err := conn.QueryRow(ctx, "SHOW "+setting).Scan(&got); err != nil {
			t.Fatal(err)
		}
		// Over a Unix socket the server reports keepalives as 0.
		if got != want && !(got == "0" && setting != "lock_timeout" && setting != "idle_in_transaction_session_timeout") {
			t.Errorf("%s = %q, want %q", setting, got, want)
		}
	}
}

// A session that holds the claim lock and never lets go (e.g. its client
// vanished mid-transaction) must not hang every other worker's Claim.
func TestClaimGivesUpOnAHeldLock(t *testing.T) {
	t.Parallel()
	s := storetest.New(t)
	create(t, s, "team:01")
	holder, err := store.Acquire(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, store.ClaimLockID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, store.ClaimLockID) //nolint:errcheck
		holder.Release()
	})

	start := time.Now()
	done := make(chan error, 1)
	go func() { _, err := s.Claim(ctx, "w"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Claim succeeded while another session held the claim lock")
		}
		if d := time.Since(start); d > 12*time.Second {
			t.Errorf("Claim took %v to give up", d)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Claim hung on the held claim lock")
	}
}

// Replicas wait for each other's migrations for as long as it takes, past
// lock_timeout.
func TestMigrateWaitsPastLockTimeout(t *testing.T) {
	t.Parallel()
	s := storetest.New(t)
	holder, err := store.Acquire(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, store.MigrateLockID); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, store.MigrateLockID) //nolint:errcheck
			holder.Release()
		}
	}
	t.Cleanup(release)

	done := make(chan error, 1)
	go func() { done <- s.Migrate(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("Migrate returned %v while another replica held the migrate lock", err)
	case <-time.After(11 * time.Second):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Migrate after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Migrate hung after the lock was released")
	}
	// The pool's sessions keep their lock_timeout afterwards.
	for i := 0; i < 3; i++ {
		conn, err := store.Acquire(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		err = conn.QueryRow(ctx, "SHOW lock_timeout").Scan(&got)
		conn.Release()
		if err != nil || got != "10s" {
			t.Errorf("lock_timeout after Migrate = %q, %v; want 10s", got, err)
		}
	}
}

func TestMigrationsTableIsNamespaced(t *testing.T) {
	s := storetest.New(t)
	conn, err := store.Acquire(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM battleship_schema_migrations`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("battleship_schema_migrations has %d rows, %v", n, err)
	}
	var generic bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&generic); err != nil || generic {
		t.Errorf("a generic schema_migrations table exists (%v); it may clash with other apps", err)
	}
}

func TestOpenDoesNotEchoTheURL(t *testing.T) {
	_, err := store.Open(ctx, "postgres://battleship:hunter2@db.example:notaport/battleship", store.Options{})
	if err == nil {
		t.Fatal("Open accepted a bad URL")
	}
	if msg := err.Error(); strings.Contains(msg, "db.example") || strings.Contains(msg, "hunter2") {
		t.Errorf("error echoes the URL: %s", msg)
	}
}

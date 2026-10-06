package store_test

import (
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// Item 13: a refused renewal of an old ticket clears it only if it is
// still the one stored; a newer ticket another request stored stays.
func TestClearSessionTicketOnlyClearsTheTicketItRead(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("sess-1"))
	newer := store.SessionTicket{User: "u", Ticket: "v1:new", CSRF: "v1:c", IssuedAt: t0.Add(time.Hour), LoginAt: t0}
	if err := s.SetSessionTicket(ctx, "sess-1", time.Time{}, newer); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearSessionTicket(ctx, "sess-1", t0); err != nil { // the old ticket's issue time
		t.Fatal(err)
	}
	got, _ := s.Session(ctx, "sess-1")
	if got.PVE.Ticket != "v1:new" {
		t.Fatalf("a stale clear wiped the newer ticket: %+v", got.PVE)
	}
	if err := s.ClearSessionTicket(ctx, "sess-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Session(ctx, "sess-1"); got.PVE.Ticket != "" {
		t.Fatalf("the current ticket wasn't cleared: %+v", got.PVE)
	}
}

// Item 16: the schema itself refuses a ticket without a renewal time and
// a token with one.
func TestJobCredentialKindMatchesRenewAfter(t *testing.T) {
	s := storetest.New(t)
	conn, err := store.Acquire(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, tc := range []struct {
		kind  string
		renew *time.Time
	}{{"ticket", nil}, {"token", &t0}} {
		id := create(t, s, "team:01")
		_, err := conn.Exec(ctx, `INSERT INTO job_credentials (job_id, kind, pve_user, sealed, issued_at, login_at, renew_after)
			VALUES ($1, $2, 'u', 'v1:x', $3, $3, $4)`, id, tc.kind, t0, tc.renew)
		if err == nil {
			t.Errorf("a %s with renew_after %v was stored", tc.kind, tc.renew)
		}
	}
}

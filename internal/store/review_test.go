package store_test

import (
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// Clearing a refused old ticket leaves a newer stored one alone.
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

func TestJobCredentialNeedsRenewAfter(t *testing.T) {
	s := storetest.New(t)
	conn, err := store.Acquire(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	id := create(t, s, "team:01")
	if _, err := conn.Exec(ctx, `INSERT INTO job_credentials (job_id, pve_user, sealed, issued_at, login_at, renew_after)
		VALUES ($1, 'u', 'v1:x', $2, $2, NULL)`, id, t0); err == nil {
		t.Error("a credential without renew_after was stored")
	}
}

package store_test

import (
	"testing"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

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

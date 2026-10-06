package store_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// t0 has no sub-microsecond part, so it survives Postgres's timestamptz.
var t0 = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

func newSession(id string) store.Session {
	return store.Session{
		ID:           id,
		Subject:      "sub-alice",
		Name:         "Alice",
		Email:        "alice@example.org",
		Groups:       []string{"battleship-operators", "staff"},
		RefreshToken: "refresh-1",
		CSRF:         "csrf-1",
		CreatedAt:    t0,
		LastSeen:     t0,
		RefreshedAt:  t0,
		ExpiresAt:    t0.Add(30 * time.Minute),
	}
}

func createSession(t *testing.T, s *store.Store, sess store.Session) {
	t.Helper()
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
}

// sameSession compares sessions with times compared by instant, since
// Postgres returns them in the local zone.
func sameSession(t *testing.T, got, want store.Session) {
	t.Helper()
	for _, p := range []struct {
		name      string
		got, want time.Time
	}{
		{"CreatedAt", got.CreatedAt, want.CreatedAt},
		{"LastSeen", got.LastSeen, want.LastSeen},
		{"RefreshedAt", got.RefreshedAt, want.RefreshedAt},
		{"ExpiresAt", got.ExpiresAt, want.ExpiresAt},
		{"RefreshRetryAt", got.RefreshRetryAt, want.RefreshRetryAt},
		{"RefreshFailedAt", got.RefreshFailedAt, want.RefreshFailedAt},
	} {
		if !p.got.Equal(p.want) {
			t.Errorf("%s = %v, want %v", p.name, p.got, p.want)
		}
	}
	for _, x := range []*store.Session{&got, &want} {
		x.CreatedAt, x.LastSeen, x.RefreshedAt, x.ExpiresAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
		x.RefreshRetryAt, x.RefreshFailedAt = time.Time{}, time.Time{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("session = %+v, want %+v", got, want)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	s := storetest.New(t)
	want := newSession("hash-1")
	createSession(t, s, want)
	got, err := s.Session(ctx, "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	sameSession(t, got, want)
}

func TestSessionNotFound(t *testing.T) {
	s := storetest.New(t)
	if _, err := s.Session(ctx, "nope"); !errors.Is(err, store.ErrSessionNotFound) {
		t.Errorf("Session(unknown) err = %v, want ErrSessionNotFound", err)
	}
}

func TestCreateSessionRejects(t *testing.T) {
	s := storetest.New(t)
	for name, mut := range map[string]func(*store.Session){
		"no id":            func(x *store.Session) { x.ID = "" },
		"no subject":       func(x *store.Session) { x.Subject = "" },
		"no refresh token": func(x *store.Session) { x.RefreshToken = "" },
		"no csrf":          func(x *store.Session) { x.CSRF = "" },
		"no expiry":        func(x *store.Session) { x.ExpiresAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			sess := newSession("hash-" + name)
			mut(&sess)
			if err := s.CreateSession(ctx, sess); !errors.Is(err, store.ErrEmptySession) {
				t.Errorf("CreateSession err = %v, want ErrEmptySession", err)
			}
		})
	}
	createSession(t, s, newSession("dup"))
	if err := s.CreateSession(ctx, newSession("dup")); err == nil {
		t.Error("CreateSession with a duplicate ID succeeded")
	}
}

func TestSessionNilGroupsReadBackEmpty(t *testing.T) {
	s := storetest.New(t)
	sess := newSession("hash-1")
	sess.Groups = nil
	createSession(t, s, sess)
	got, err := s.Session(ctx, "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Groups == nil || len(got.Groups) != 0 {
		t.Errorf("Groups = %#v, want empty, non-nil", got.Groups)
	}
}

func TestTouchSession(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("hash-1"))
	seen, exp := t0.Add(10*time.Minute), t0.Add(40*time.Minute)
	if err := s.TouchSession(ctx, "hash-1", seen, exp); err != nil {
		t.Fatal(err)
	}
	want := newSession("hash-1")
	want.LastSeen, want.ExpiresAt = seen, exp
	got, err := s.Session(ctx, "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	sameSession(t, got, want)

	if err := s.TouchSession(ctx, "gone", seen, exp); !errors.Is(err, store.ErrSessionNotFound) {
		t.Errorf("TouchSession(unknown) err = %v, want ErrSessionNotFound", err)
	}
}

func TestClaimSessionRefreshIsExclusive(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("hash-1"))
	now := t0.Add(6 * time.Minute)
	until := now.Add(time.Minute)
	won, err := s.ClaimSessionRefresh(ctx, "hash-1", t0, now, until)
	if err != nil || !won {
		t.Fatalf("first claim = %v, %v; want true", won, err)
	}
	// Another replica that read the same session loses until the claim
	// lapses.
	for _, at := range []time.Time{now, until.Add(-time.Second)} {
		if won, err := s.ClaimSessionRefresh(ctx, "hash-1", t0, at, at.Add(time.Minute)); err != nil || won {
			t.Errorf("claim at %v = %v, %v; want false", at, won, err)
		}
	}
	got, err := s.Session(ctx, "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.RefreshedAt.Equal(t0) || !got.RefreshRetryAt.Equal(until) {
		t.Errorf("RefreshedAt %v RefreshRetryAt %v, want %v (unchanged) and %v", got.RefreshedAt, got.RefreshRetryAt, t0, until)
	}
	// A claim that lapsed (its refresher died) can be taken over.
	if won, err := s.ClaimSessionRefresh(ctx, "hash-1", t0, until, until.Add(time.Minute)); err != nil || !won {
		t.Errorf("claim after it lapsed = %v, %v; want true", won, err)
	}
	// A reader that saw an older refreshed_at loses once a refresh landed.
	if err := s.UpdateSessionGroups(ctx, "hash-1", store.SessionUpdate{RefreshToken: "r2", RefreshedAt: until}); err != nil {
		t.Fatal(err)
	}
	if won, err := s.ClaimSessionRefresh(ctx, "hash-1", t0, until.Add(time.Hour), until.Add(2*time.Hour)); err != nil || won {
		t.Errorf("claim with a stale refreshed_at = %v, %v; want false", won, err)
	}
	if won, err := s.ClaimSessionRefresh(ctx, "gone", t0, now, until); err != nil || won {
		t.Errorf("claim on unknown session = %v, %v; want false, nil", won, err)
	}
}

func TestRecordSessionRefreshFailure(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("hash-1"))
	failed, retry := t0.Add(5*time.Minute), t0.Add(6*time.Minute)
	// No token from the provider: the refresh token is kept.
	if err := s.RecordSessionRefreshFailure(ctx, "hash-1", "", failed, retry); err != nil {
		t.Fatal(err)
	}
	got, err := s.Session(ctx, "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.RefreshFailedAt.Equal(failed) || !got.RefreshRetryAt.Equal(retry) || !got.RefreshedAt.Equal(t0) ||
		got.RefreshToken != "refresh-1" {
		t.Errorf("failed %v retry %v refreshed %v token %q; want %v, %v, unchanged, refresh-1",
			got.RefreshFailedAt, got.RefreshRetryAt, got.RefreshedAt, got.RefreshToken, failed, retry)
	}
	// The provider rotated the token before a later step failed: keep the
	// new one, or the retry would spend a dead token.
	if err := s.RecordSessionRefreshFailure(ctx, "hash-1", "refresh-rotated", failed, retry); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Session(ctx, "hash-1"); got.RefreshToken != "refresh-rotated" {
		t.Errorf("refresh token = %q, want the rotated one", got.RefreshToken)
	}
	// A successful refresh clears both.
	if err := s.UpdateSessionGroups(ctx, "hash-1", store.SessionUpdate{RefreshToken: "r2", RefreshedAt: retry}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Session(ctx, "hash-1")
	if !got.RefreshFailedAt.IsZero() || !got.RefreshRetryAt.IsZero() {
		t.Errorf("after a refresh: failed %v retry %v, want both zero", got.RefreshFailedAt, got.RefreshRetryAt)
	}
	if err := s.RecordSessionRefreshFailure(ctx, "gone", "", failed, retry); !errors.Is(err, store.ErrSessionNotFound) {
		t.Errorf("unknown session err = %v, want ErrSessionNotFound", err)
	}
}

func TestUpdateSessionGroups(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("hash-1"))
	up := store.SessionUpdate{
		Name:         "Alice B",
		Email:        "ab@example.org",
		Groups:       []string{"battleship-leads"},
		RefreshToken: "refresh-2",
		RefreshedAt:  t0.Add(5 * time.Minute),
	}
	if err := s.UpdateSessionGroups(ctx, "hash-1", up); err != nil {
		t.Fatal(err)
	}
	want := newSession("hash-1")
	want.Name, want.Email, want.Groups, want.RefreshToken, want.RefreshedAt =
		up.Name, up.Email, up.Groups, up.RefreshToken, up.RefreshedAt
	got, err := s.Session(ctx, "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	sameSession(t, got, want)

	if err := s.UpdateSessionGroups(ctx, "gone", up); !errors.Is(err, store.ErrSessionNotFound) {
		t.Errorf("UpdateSessionGroups(unknown) err = %v, want ErrSessionNotFound", err)
	}
	up.RefreshToken = ""
	if err := s.UpdateSessionGroups(ctx, "hash-1", up); !errors.Is(err, store.ErrEmptySession) {
		t.Errorf("UpdateSessionGroups without a refresh token err = %v, want ErrEmptySession", err)
	}
}

func TestDeleteSession(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("hash-1"))
	createSession(t, s, newSession("hash-2"))
	if err := s.DeleteSession(ctx, "hash-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, "hash-1"); !errors.Is(err, store.ErrSessionNotFound) {
		t.Errorf("deleted session err = %v, want ErrSessionNotFound", err)
	}
	if _, err := s.Session(ctx, "hash-2"); err != nil {
		t.Errorf("other session: %v", err)
	}
	// Logging out twice is not an error.
	if err := s.DeleteSession(ctx, "hash-1"); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

func TestDeleteExpiredSessions(t *testing.T) {
	s := storetest.New(t)
	for i, exp := range []time.Duration{-time.Minute, 0, time.Second, time.Hour} {
		sess := newSession("hash-" + string(rune('a'+i)))
		sess.ExpiresAt = t0.Add(exp)
		createSession(t, s, sess)
	}
	n, err := s.DeleteExpiredSessions(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("deleted %d sessions, want 2 (expired before or at now)", n)
	}
	for id, live := range map[string]bool{"hash-a": false, "hash-b": false, "hash-c": true, "hash-d": true} {
		_, err := s.Session(ctx, id)
		if live && err != nil {
			t.Errorf("%s: %v, want kept", id, err)
		}
		if !live && !errors.Is(err, store.ErrSessionNotFound) {
			t.Errorf("%s: err = %v, want deleted", id, err)
		}
	}
}

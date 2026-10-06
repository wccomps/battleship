package auth

import (
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/store"
)

func TestSessionTimes(t *testing.T) {
	w := config.Web{SessionIdle: 30 * time.Minute, SessionMax: 12 * time.Hour, SessionRefresh: 5 * time.Minute}
	t0 := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	sess := store.Session{CreatedAt: t0, LastSeen: t0, RefreshedAt: t0, ExpiresAt: t0.Add(30 * time.Minute)}

	cases := []struct {
		name    string
		mut     func(*store.Session)
		at      time.Duration
		expired bool
	}{
		{"fresh", nil, 0, false},
		{"just before idle", nil, 30*time.Minute - time.Nanosecond, false},
		{"at idle", nil, 30 * time.Minute, true},
		{"stored expiry earlier than config", func(s *store.Session) { s.ExpiresAt = t0.Add(time.Minute) }, time.Minute, true},
		{"active but past max", func(s *store.Session) {
			s.LastSeen = t0.Add(12*time.Hour - time.Minute)
			s.ExpiresAt = t0.Add(13 * time.Hour)
		}, 12 * time.Hour, true},
		{"last seen in the future", func(s *store.Session) { s.LastSeen = t0.Add(time.Hour); s.ExpiresAt = t0.Add(2 * time.Hour) }, 0, false},
	}
	for _, tc := range cases {
		s := sess
		if tc.mut != nil {
			tc.mut(&s)
		}
		if got := expired(w, s, t0.Add(tc.at)); got != tc.expired {
			t.Errorf("%s: expired = %v, want %v", tc.name, got, tc.expired)
		}
	}

	if got := expiresAt(w, t0, t0.Add(time.Hour)); !got.Equal(t0.Add(90 * time.Minute)) {
		t.Errorf("expiresAt = %v, want last seen + idle", got)
	}
	if got := expiresAt(w, t0, t0.Add(12*time.Hour-10*time.Minute)); !got.Equal(t0.Add(12 * time.Hour)) {
		t.Errorf("expiresAt = %v, want capped at created + max", got)
	}
	if got := touchEvery(w); got != time.Minute {
		t.Errorf("touchEvery(30m idle) = %v, want 1m", got)
	}
	if got := touchEvery(config.Web{SessionIdle: 2 * time.Minute}); got != 12*time.Second {
		t.Errorf("touchEvery(2m idle) = %v, want 12s", got)
	}
}

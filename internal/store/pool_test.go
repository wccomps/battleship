package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// Page traffic that takes every connection of the main pool must not starve
// job heartbeats or flip readiness: both use their own small pool.
func TestSaturatedMainPoolSparesPingAndHeartbeat(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	mustClaim(t, s, "w1")

	release, err := store.HoldMainPool(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// The main pool really is saturated.
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err = s.Job(short, id)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Job with the main pool held = %v, want a timeout", err)
	}

	short, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.Ping(short); err != nil {
		t.Errorf("Ping with the main pool held = %v, want nil", err)
	}
	if c, err := s.Heartbeat(short, id, "w1"); err != nil || c {
		t.Errorf("Heartbeat with the main pool held = %v, %v; want false, nil", c, err)
	}
}

func TestOpenPoolSizes(t *testing.T) {
	_, url := storetest.NewWithURL(t)
	// The URL's pool_max_conns loses to MaxConns.
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	url += sep + "pool_max_conns=99"
	for _, tc := range []struct{ opt, main int }{{0, store.DefaultMaxConns}, {5, 5}} {
		s, err := store.Open(ctx, url, store.Options{MaxConns: tc.opt})
		if err != nil {
			t.Fatal(err)
		}
		main, liveness := store.PoolSizes(s)
		s.Close()
		if main != tc.main || liveness != store.LivenessConns {
			t.Errorf("MaxConns %d: pools of %d and %d, want %d and %d", tc.opt, main, liveness, tc.main, store.LivenessConns)
		}
	}
	if store.DefaultMaxConns != 16 || store.LivenessConns != 2 {
		t.Errorf("DefaultMaxConns, LivenessConns = %d, %d; want 16, 2", store.DefaultMaxConns, store.LivenessConns)
	}
}

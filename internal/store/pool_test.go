package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// Page traffic that takes every connection of the main pool must not starve
// job heartbeats or flip readiness: each has its own small pool.
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
		main, liveness, ping := store.PoolSizes(s)
		s.Close()
		if main != tc.main || liveness != store.LivenessConns || ping != 1 {
			t.Errorf("MaxConns %d: pools of %d, %d and %d, want %d, %d and 1", tc.opt, main, liveness, ping, tc.main, store.LivenessConns)
		}
	}
	if store.DefaultMaxConns != 16 || store.LivenessConns != 2 {
		t.Errorf("DefaultMaxConns, LivenessConns = %d, %d; want 16, 2", store.DefaultMaxConns, store.LivenessConns)
	}
}

// slowPing makes s's shared ping hold its connection until release is
// called, and reports each one on started.
func slowPing(s *store.Store) (started chan struct{}, release func()) {
	started, held := make(chan struct{}, 100), make(chan struct{})
	store.SetBeforePing(s, func() {
		started <- struct{}{}
		<-held
	})
	return started, sync.OnceFunc(func() { close(held) })
}

// Pings never take a heartbeat's connection.
func TestPingsLeaveHeartbeatPool(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	mustClaim(t, s, "w1")
	before := store.LivenessAcquires(s)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() { _ = s.Ping(ctx) })
	}
	wg.Wait()
	if n := store.LivenessAcquires(s) - before; n != 0 {
		t.Errorf("100 pings took %d heartbeat connections, want 0", n)
	}
	if c, err := s.Heartbeat(ctx, id, "w1"); err != nil || c {
		t.Errorf("Heartbeat = %v, %v; want false, nil", c, err)
	}
}

// A flood of pings waits on one shared ping, not in line for the one
// connection.
func TestPingFloodSharesOne(t *testing.T) {
	s := storetest.New(t)
	started, release := slowPing(s)
	defer release()
	errs := make(chan error, 100)
	go func() { errs <- s.Ping(ctx) }()
	<-started
	for range 99 {
		go func() {
			short, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			errs <- s.Ping(short)
		}()
	}
	time.Sleep(100 * time.Millisecond) // the 99 join the ping
	release()
	for range 100 {
		if err := <-errs; err != nil {
			t.Errorf("Ping = %v, want nil", err)
		}
	}
	if n := len(started); n != 0 {
		t.Errorf("100 concurrent pings made %d pings, want 1", n+1)
	}
}

// A caller that gives up doesn't end the ping others share.
func TestPingCallerGivingUpSparesOthers(t *testing.T) {
	s := storetest.New(t)
	started, release := slowPing(s)
	defer release()
	short, cancel := context.WithCancel(ctx)
	first := make(chan error, 1)
	go func() { first <- s.Ping(short) }()
	<-started
	second := make(chan error, 1)
	go func() { second <- s.Ping(ctx) }()
	time.Sleep(50 * time.Millisecond) // the second caller joins the ping
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Errorf("the starter's Ping = %v, want context.Canceled", err)
	}
	release()
	if err := <-second; err != nil {
		t.Errorf("Ping sharing a ping whose starter gave up = %v, want nil", err)
	}
	if n := len(started); n != 0 {
		t.Errorf("the second Ping made a ping of its own, so it shared nothing")
	}
}

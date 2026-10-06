package proxmox

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testRetrier(slept *[]time.Duration) Retrier {
	return Retrier{
		Retryable: transient(testPatterns...),
		Attempts:  4,
		Initial:   time.Second,
		Max:       3 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) error {
			*slept = append(*slept, d)
			return nil
		},
	}
}

func TestRetrierRetriesTransientWithBackoff(t *testing.T) {
	var slept []time.Duration
	calls := 0
	err := testRetrier(&slept).Do(context.Background(), func() error {
		calls++
		if calls < 4 {
			return &APIError{Status: 500, Message: "File exists"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}
	if len(slept) != len(want) {
		t.Fatalf("slept %v, want %v", slept, want)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v", i, slept[i], want[i])
		}
	}
}

func TestRetrierStopsOnPermanentError(t *testing.T) {
	var slept []time.Duration
	calls := 0
	want := &APIError{Status: 403, Message: "Permission check failed (/vms/1, VM.Audit)"}
	err := testRetrier(&slept).Do(context.Background(), func() error {
		calls++
		return want
	})
	if err == nil || calls != 1 || len(slept) != 0 || !errors.Is(err, want) {
		t.Errorf("calls=%d slept=%v err=%v, want one call, no sleeps, and original error", calls, slept, err)
	}
}

func TestRetrierGivesUpAfterAttempts(t *testing.T) {
	var slept []time.Duration
	calls := 0
	err := testRetrier(&slept).Do(context.Background(), func() error {
		calls++
		return &APIError{Status: 500, Message: "got no worker upid"}
	})
	var apiErr *APIError
	if err == nil || calls != 4 || len(slept) != 3 || !errors.As(err, &apiErr) || apiErr.Message != "got no worker upid" {
		t.Errorf("calls=%d slept=%v err=%v, want 4 calls, 3 sleeps, and last error with 'got no worker upid'", calls, slept, err)
	}
}

func TestRetrierStopsWhenContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	r := Retrier{Retryable: transient(testPatterns...), Attempts: 5, Initial: time.Hour, Max: time.Hour}
	start := time.Now()
	err := r.Do(ctx, func() error {
		calls++
		return &APIError{Status: 500, Message: "got no worker upid"}
	})
	var apiErr *APIError
	if err == nil || calls != 1 || time.Since(start) > time.Second || !errors.As(err, &apiErr) || errors.Is(err, context.Canceled) {
		t.Errorf("calls=%d err=%v elapsed=%v, want one call, APIError (not context.Canceled), and immediate return", calls, err, time.Since(start))
	}
}

func TestRetrierWithoutRetryableTriesOnce(t *testing.T) {
	var slept []time.Duration
	r := testRetrier(&slept)
	r.Retryable = nil
	calls := 0
	err := r.Do(context.Background(), func() error { calls++; return &APIError{Status: 500, Message: "boom"} })
	if err == nil || calls != 1 || len(slept) != 0 {
		t.Errorf("calls=%d slept=%v err=%v, want one call", calls, slept, err)
	}
}

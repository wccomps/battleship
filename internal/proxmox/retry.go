package proxmox

import (
	"context"
	"time"
)

// Retrier retries errors with exponential backoff.
type Retrier struct {
	// Retryable decides which errors are tried again; nil retries none.
	Retryable func(error) bool
	Attempts  int // total tries, including the first
	Initial   time.Duration
	Max       time.Duration
	// Sleep waits for d or until ctx is done. Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Do calls fn until it succeeds, returns an error Retryable refuses, or has been tried
// Attempts times. It returns fn's last error, including when ctx is cancelled
// during a backoff; callers that need to tell cancellation apart check
// ctx.Err(). Config validation guarantees Attempts >= 1 and Initial <= Max.
func (r Retrier) Do(ctx context.Context, fn func() error) error {
	sleep := r.Sleep
	if sleep == nil {
		sleep = SleepContext
	}
	delay := r.Initial
	var err error
	for attempt := 1; ; attempt++ {
		if err = fn(); err == nil || attempt >= r.Attempts || r.Retryable == nil || !r.Retryable(err) {
			return err
		}
		if serr := sleep(ctx, delay); serr != nil {
			return err
		}
		if delay *= 2; delay > r.Max {
			delay = r.Max
		}
	}
}

// SleepContext waits for d or until ctx is done.
func SleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

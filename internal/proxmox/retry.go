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

// Do calls fn until it succeeds, fails non-retryably, or runs out of
// Attempts, returning fn's last error even on cancel (check ctx.Err()).
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

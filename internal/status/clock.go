package status

import "time"

// Clock is the time source of the poller and the hub, so tests can drive
// them with a fake one.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives the time once d has passed.
	After(d time.Duration) <-chan time.Time
}

// SystemClock is the real clock.
var SystemClock Clock = systemClock{}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

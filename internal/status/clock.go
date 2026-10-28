package status

import "time"

// Clock is the time source of the poller and the hub, so tests can drive
// them with a fake one.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

var SystemClock Clock = systemClock{}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

//go:build unix

package budget

import "time"

// Clock is the gate's time: the snapshot period, the wedge and full-budget bounds, the Open
// backoff and the waits it reports. Tests advance it by hand.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

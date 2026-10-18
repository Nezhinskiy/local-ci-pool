package runner

import (
	"context"
	"errors"
)

// Mode is how a gate admitted a runner.
type Mode string

const (
	// ModeOff: the budget is off; the runner starts as it would without one.
	ModeOff Mode = "off"
	// ModeQueued: the runner waited for its share of the budget and got it.
	ModeQueued Mode = "queued"
	// ModeAdopted: a runner already running took what was free, without queueing.
	ModeAdopted Mode = "adopted"
	// ModeFailOpen: the budget could not be consulted, and the runner starts anyway.
	ModeFailOpen Mode = "fail_open"
)

// Admission names a runner to the gate. Need 0, or a need outside 1..256, means the
// gate's configured need.
type Admission struct {
	Runner, Project string
	Need            int
}

// Admitted is what the gate gave. Release is idempotent, never nil, never blocks, and is
// never called under a pool lock. Need is the cores the runner may use: the cores granted for
// ModeQueued, which can be fewer than it asked; the need asked for ModeAdopted and ModeFailOpen;
// 0 for ModeOff. The runner's job gets it as CORESLOT_CPU.
type Admitted struct {
	Release func()
	Mode    Mode
	Need    int
}

// NoRelease is the Release of an admission that holds nothing.
func NoRelease() {}

// Gate admits runners into a machine-wide budget. Enabled reports whether the next runner
// goes through Admit. Admit blocks until the runner may start; it returns an error only when
// ctx ended or the request was cancelled through coreslot (ErrCancelledByBudget); any other
// failure is decided inside the gate (ModeFailOpen); settings turned off since Enabled give
// ModeOff. Adopt takes what is free for a runner already running, without queueing, and
// never fails.
type Gate interface {
	Enabled() bool
	Admit(ctx context.Context, a Admission) (Admitted, error)
	Adopt(ctx context.Context, a Admission) Admitted
}

// ErrCancelledByBudget is Admit's error when the runner's request was cancelled
// through coreslot.
var ErrCancelledByBudget = errors.New("cancelled through coreslot")

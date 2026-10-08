package runner

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/actions/scaleset"
)

// stillRunning is how the scale set client reports a runner with a job:
// wrapped, as newRequestResponseError wraps it.
func stillRunning(int64) error {
	return fmt.Errorf("request DELETE /runners/1 failed(status=\"400\"): %w: the runner is running a job", scaleset.JobStillRunningError)
}

func TestReapKeepsARunnerThatIsRunningAJob(t *testing.T) {
	slots := NewSlots(2)
	h := newHarness(slots)
	desired(t, h.scaler, 2)
	names := h.starter.started()
	// Both runners took jobs; neither JobStarted has reached the listener.
	h.registry.removeErr = stillRunning
	if got := desired(t, h.scaler, 0); got != 2 {
		t.Fatalf("Desired(0) with two runners on jobs = %d, want 2", got)
	}
	if got := h.docker.removed(); len(got) != 0 {
		t.Fatalf("containers removed %v: a runner with a job was killed", got)
	}
	if len(h.registry.unregistered()) != 2 {
		t.Fatalf("unregister attempts %v, want one per idle runner", h.registry.unregistered())
	}
	if slots.InUse() != 2 || slots.Busy() != 2 || h.scaler.BusyCount() != 2 || h.scaler.Idle() != 0 {
		t.Fatalf("in use %d busy %d / %d idle %d, want 2 2 2 0", slots.InUse(), slots.Busy(), h.scaler.BusyCount(), h.scaler.Idle())
	}
	// The late JobStarted does not count the runner twice.
	h.scaler.Started(names[0])
	if slots.Busy() != 2 {
		t.Fatalf("Busy %d after the late JobStarted, want 2", slots.Busy())
	}
	h.scaler.Completed(names[0])
	if slots.Busy() != 1 || slots.InUse() != 2 {
		t.Fatalf("busy %d in use %d after Completed, want 1 and 2", slots.Busy(), slots.InUse())
	}
}

func TestReapUnregistersBeforeRemovingTheContainer(t *testing.T) {
	slots := NewSlots(1)
	h := newHarness(slots)
	desired(t, h.scaler, 1)
	name := h.starter.started()[0]
	before := len(h.docker.callLog())
	desired(t, h.scaler, 0)
	if got := h.docker.callLog()[before:]; !slices.Equal(got, []string{"lookup", "unregister", "remove"}) {
		t.Fatalf("reap calls %v, want lookup, unregister, then the container remove", got)
	}
	if got := h.registry.lookedUp(); !slices.Equal(got, []string{name}) {
		t.Fatalf("looked up %v, want %s", got, name)
	}
	if got, id := h.registry.unregistered(), h.registry.idOf(name); !slices.Equal(got, []int64{id}) {
		t.Fatalf("unregistered %v, want [%d]", got, id)
	}
	if got := h.docker.removed(); !slices.Equal(got, []string{name}) {
		t.Fatalf("removed %v, want %s", got, name)
	}
	if slots.InUse() != 0 || h.scaler.Idle() != 0 {
		t.Fatalf("in use %d idle %d, want 0 0", slots.InUse(), h.scaler.Idle())
	}
}

func TestReapRemovesTheContainerOfAnUnregisteredRunner(t *testing.T) {
	for _, c := range []struct {
		name       string
		setup      func(h *harness, runner string)
		unregister int
	}{
		{"lookup finds none", func(h *harness, runner string) { h.registry.missing = map[string]bool{runner: true} }, 0},
		{"gone before the remove", func(h *harness, _ string) {
			h.registry.removeErr = func(int64) error {
				return fmt.Errorf("request DELETE failed: %w: gone", scaleset.RunnerNotFoundError)
			}
		}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			slots := NewSlots(1)
			h := newHarness(slots)
			desired(t, h.scaler, 1)
			runner := h.starter.started()[0]
			c.setup(h, runner)
			desired(t, h.scaler, 0)
			if got := h.registry.lookedUp(); !slices.Equal(got, []string{runner}) {
				t.Fatalf("looked up %v, want %s", got, runner)
			}
			if got := len(h.registry.unregistered()); got != c.unregister {
				t.Fatalf("%d unregister calls, want %d", got, c.unregister)
			}
			if got := h.docker.removed(); !slices.Equal(got, []string{runner}) {
				t.Fatalf("removed %v, want %s", got, runner)
			}
			if slots.InUse() != 0 {
				t.Fatalf("InUse %d, want 0", slots.InUse())
			}
		})
	}
}

func TestReapRetriesAfterARegistryFailure(t *testing.T) {
	for _, c := range []struct {
		name  string
		fail  func(h *harness)
		clear func(h *harness)
	}{
		{"lookup", func(h *harness) { h.registry.lookupErr = errors.New("api unavailable") }, func(h *harness) { h.registry.lookupErr = nil }},
		{"remove", func(h *harness) { h.registry.removeErr = func(int64) error { return errors.New("api unavailable") } }, func(h *harness) { h.registry.removeErr = nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			slots := NewSlots(1)
			h := newHarness(slots)
			desired(t, h.scaler, 1)
			runner := h.starter.started()[0]
			c.fail(h)
			before := len(h.logs.lines())
			desired(t, h.scaler, 0)
			if got := h.docker.removed(); len(got) != 0 {
				t.Fatalf("removed %v although the runner could not be unregistered", got)
			}
			if slots.InUse() != 1 || h.scaler.Idle() != 1 || h.scaler.BusyCount() != 0 {
				t.Fatalf("in use %d idle %d busy %d, want 1 1 0", slots.InUse(), h.scaler.Idle(), h.scaler.BusyCount())
			}
			var warns []string
			for _, l := range h.logs.lines()[before:] {
				if strings.Contains(l, "level=WARN") {
					warns = append(warns, l)
				}
			}
			if len(warns) != 1 {
				t.Fatalf("%d warning lines, want 1: %v", len(warns), warns)
			}
			c.clear(h)
			desired(t, h.scaler, 0)
			if got := h.docker.removed(); !slices.Equal(got, []string{runner}) {
				t.Fatalf("next call removed %v, want %s", got, runner)
			}
			if slots.InUse() != 0 {
				t.Fatalf("InUse %d after the retry, want 0", slots.InUse())
			}
		})
	}
}

func TestFailedStartUnregistersTheRunner(t *testing.T) {
	slots := NewSlots(1)
	h := newHarness(slots)
	h.starter.err = errors.New("create failed")
	desired(t, h.scaler, 1)
	runner := h.starter.started()[0]
	if got := h.registry.lookedUp(); !slices.Equal(got, []string{runner}) {
		t.Fatalf("looked up %v, want the runner whose start failed (%s)", got, runner)
	}
	if got, id := h.registry.unregistered(), h.registry.idOf(runner); !slices.Equal(got, []int64{id}) {
		t.Fatalf("unregistered %v, want [%d]", got, id)
	}
	if slots.InUse() != 0 {
		t.Fatalf("InUse %d, want 0", slots.InUse())
	}

	// Unregistering is best effort: a failure still releases the slot.
	h2 := newHarness(slots)
	h2.starter.err = errors.New("create failed")
	h2.registry.lookupErr = errors.New("api unavailable")
	desired(t, h2.scaler, 1)
	if len(h2.registry.lookedUp()) != 1 || slots.InUse() != 0 {
		t.Fatalf("lookups %v, in use %d; want one attempt and 0", h2.registry.lookedUp(), slots.InUse())
	}

	// Nothing was registered when the mint itself failed.
	h3 := newHarness(slots)
	h3.jit.err = errors.New("jit refused")
	desired(t, h3.scaler, 1)
	if len(h3.registry.lookedUp()) != 0 || slots.InUse() != 0 {
		t.Fatalf("lookups %v, in use %d; want none and 0", h3.registry.lookedUp(), slots.InUse())
	}
}

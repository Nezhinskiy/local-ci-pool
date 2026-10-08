package runner

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// An exit that lands while a runner is being unregistered must not bring the
// runner back: its slot is already released, so a revived entry would count a
// runner that no longer exists.
func TestExitDuringReapIsNotResurrected(t *testing.T) {
	for _, c := range []struct {
		name string
		arm  func(h *harness)
	}{
		{"job still running", func(h *harness) {
			h.registry.removeErr = func(id int64) error {
				for n, i := range h.registry.ids {
					if int64(i) == id {
						h.scaler.Exited(n)
					}
				}
				return stillRunning(id)
			}
		}},
		{"registry failure", func(h *harness) {
			h.registry.onLookup = func(_ context.Context, name string) error {
				h.scaler.Exited(name)
				return errors.New("api unavailable")
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			slots := NewSlots(1)
			h := newHarness(slots)
			desired(t, h.scaler, 1)
			c.arm(h)
			if got := desired(t, h.scaler, 0); got != 0 {
				t.Fatalf("Desired(0) = %d, want 0", got)
			}
			if slots.InUse() != 0 || h.scaler.Idle() != 0 || slots.Busy() != 0 || h.scaler.BusyCount() != 0 {
				t.Fatalf("in use %d idle %d busy %d / %d, want all 0", slots.InUse(), h.scaler.Idle(), slots.Busy(), h.scaler.BusyCount())
			}
			if got := h.docker.removed(); len(got) != 0 {
				t.Fatalf("removed %v", got)
			}
		})
	}
}

func TestDrainDuringMintStartsNothing(t *testing.T) {
	slots := NewSlots(2)
	h := newHarness(slots)
	h.jit.onMint = func(string) { h.scaler.Drain() }
	if got := desired(t, h.scaler, 2); got != 0 {
		t.Fatalf("Desired(2) = %d, want 0", got)
	}
	if got := h.starter.started(); len(got) != 0 {
		t.Fatalf("started %v after Drain", got)
	}
	minted := h.jit.names
	if len(minted) != 1 {
		t.Fatalf("minted %v, want exactly the one in flight", minted)
	}
	if got, id := h.registry.unregistered(), h.registry.idOf(minted[0]); !slices.Equal(got, []int64{id}) {
		t.Fatalf("unregistered %v, want the minted runner [%d]", got, id)
	}
	if slots.InUse() != 0 || h.scaler.Idle() != 0 {
		t.Fatalf("in use %d idle %d, want 0 0", slots.InUse(), h.scaler.Idle())
	}
}

// shortTimeouts bounds the scaler's calls for one test.
func shortTimeouts(t *testing.T) time.Duration {
	t.Helper()
	oldLaunch, oldReap, oldForget := launchTimeout, reapTimeout, forgetTimeout
	launchTimeout, reapTimeout, forgetTimeout = 50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { launchTimeout, reapTimeout, forgetTimeout = oldLaunch, oldReap, oldForget })
	// Generous against a loaded race-detector run, far below the minutes a
	// hang without a bound would take.
	return 5 * time.Second
}

// desiredWithin calls Desired with a context that is never cancelled, as the
// listener's is while it handles a message, so only the scaler's own bounds
// can end a hung call. It fails the test if the call outlives limit.
func desiredWithin(t *testing.T, s *Scaler, count int, limit time.Duration) int {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		n, _ := s.Desired(context.WithoutCancel(context.Background()), count)
		done <- n
	}()
	select {
	case n := <-done:
		return n
	case <-time.After(limit):
		t.Fatalf("Desired(%d) did not return within %s", count, limit)
		return 0
	}
}

func TestHungStartIsBoundedAndReleasesItsSlot(t *testing.T) {
	limit := shortTimeouts(t)
	slots := NewSlots(1)
	h := newHarness(slots)
	h.starter.hangs = true
	n := desiredWithin(t, h.scaler, 1, limit)
	if n != 0 || slots.InUse() != 0 || h.scaler.Idle() != 0 {
		t.Fatalf("Desired = %d, in use %d, idle %d; want 0 0 0", n, slots.InUse(), h.scaler.Idle())
	}
	if len(h.registry.unregistered()) != 1 {
		t.Fatalf("unregistered %v, want the timed-out runner", h.registry.unregistered())
	}
}

func TestHungReapIsBoundedAndKeepsTheRunner(t *testing.T) {
	for _, c := range []struct {
		name  string
		hang  func(h *harness)
		clear func(h *harness)
	}{
		{"container remove", func(h *harness) { h.docker.removeHangs = true }, func(h *harness) { h.docker.removeHangs = false }},
		{"registry lookup", func(h *harness) {
			h.registry.onLookup = func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }
		}, func(h *harness) { h.registry.onLookup = nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			limit := shortTimeouts(t)
			slots := NewSlots(1)
			h := newHarness(slots)
			desired(t, h.scaler, 1)
			c.hang(h)
			n := desiredWithin(t, h.scaler, 0, limit)
			// The container may still exist, so the runner keeps its slot.
			if n != 1 || slots.InUse() != 1 || h.scaler.Idle() != 1 {
				t.Fatalf("Desired = %d, in use %d, idle %d; want 1 1 1", n, slots.InUse(), h.scaler.Idle())
			}
			c.clear(h)
			desired(t, h.scaler, 0)
			if slots.InUse() != 0 || h.scaler.Idle() != 0 {
				t.Fatalf("after the retry: in use %d idle %d, want 0 0", slots.InUse(), h.scaler.Idle())
			}
		})
	}
}

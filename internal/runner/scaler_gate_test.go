package runner

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// scripted names runners from names, in order.
func scripted(names ...string) func() (string, error) {
	return func() (string, error) {
		n := names[0]
		names = names[1:]
		return n, nil
	}
}

// stateOf is the state of the runner's record, or 0 when there is none.
func stateOf(s *Scaler, name string) runnerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runners[name]; ok {
		return r.state
	}
	return 0
}

// countLines counts the log lines that contain every part.
func countLines(l *logBuffer, parts ...string) int {
	n := 0
	for _, line := range l.lines() {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			n++
		}
	}
	return n
}

func ended(ctx context.Context) bool { return ctx.Err() != nil }

func TestNoMintBeforeAdmission(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	if got := desired(t, h.scaler, 1); got != 1 {
		t.Fatalf("Desired(1) = %d, want 1 waiting runner", got)
	}
	r := h.gate.next(t)
	if r.project != "alpha" {
		t.Fatalf("Admit for project %q, want alpha", r.project)
	}
	if got := h.jit.minted(); len(got) != 0 {
		t.Fatalf("minted %v while Admit is held, want nothing", got)
	}
	if got := h.starter.started(); len(got) != 0 {
		t.Fatalf("started %v while Admit is held, want nothing", got)
	}
	r.reply(h.gate.admitted(r.name, ModeQueued, 3), nil)
	eventually(t, "the admitted runner is idle", func() bool { return stateOf(h.scaler, r.name) == stateIdle })
	if got := h.jit.minted(); !slices.Equal(got, []string{r.name}) {
		t.Fatalf("minted %v, want [%s]", got, r.name)
	}
	if got := h.starter.started(); !slices.Equal(got, []string{r.name}) {
		t.Fatalf("started %v, want [%s]", got, r.name)
	}
	if got := h.starter.needOf(r.name); got != 3 {
		t.Fatalf("started with need %d, want 3", got)
	}
}

func TestEnabledReadOncePerDesiredOutsideTheLock(t *testing.T) {
	// The harness's Enabled calls s.Held() and fails the test if that blocks.
	h := newGateHarness(t, NewSlots(4))
	for range 3 {
		desired(t, h.scaler, 2)
	}
	if got := h.gate.enabledCalls.Load(); got != 3 {
		t.Fatalf("Enabled called %d times for three Desired calls, want 3", got)
	}
}

func TestRepeatedDesiredWhileWaiting(t *testing.T) {
	h := newGateHarness(t, NewSlots(8))
	for range 5 {
		desired(t, h.scaler, 1)
	}
	h.gate.next(t)
	if got := h.scaler.slots.InUse(); got != 1 {
		t.Fatalf("InUse %d after five Desired(1) while waiting, want 1", got)
	}
	if got := h.gate.admitCalls(); got != 1 {
		t.Fatalf("%d Admit calls, want 1", got)
	}
}

func TestWaitingIsHeldAsStarting(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	desired(t, h.scaler, 1)
	r := h.gate.next(t)
	if got, want := h.scaler.Held(), []HeldRunner{{Name: r.name, Starting: true}}; !slices.Equal(got, want) {
		t.Fatalf("Held() = %v, want %v", got, want)
	}
}

func TestCancelledIsHeldAsStarting(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	desired(t, h.scaler, 1)
	r := h.gate.next(t)
	desired(t, h.scaler, 0)
	if !ended(r.ctx) {
		t.Fatal("the waiter's ctx is live after Desired(0)")
	}
	if got, want := h.scaler.Held(), []HeldRunner{{Name: r.name, Starting: true}}; !slices.Equal(got, want) {
		t.Fatalf("Held() = %v, want %v", got, want)
	}
}

func TestCancelNewestFirst(t *testing.T) {
	// Creation order b, a, c: newest first gives up c and a; name order would
	// give up a and b (or c and b), and map order either.
	h := newGateHarness(t, NewSlots(4))
	h.scaler.names = scripted("b", "a", "c")
	reqs := map[string]*admitReq{}
	for i := 1; i <= 3; i++ {
		desired(t, h.scaler, i)
		r := h.gate.next(t)
		reqs[r.name] = r
	}
	desired(t, h.scaler, 1)
	if !ended(reqs["c"].ctx) || !ended(reqs["a"].ctx) || ended(reqs["b"].ctx) {
		t.Fatalf("ctx ended: a %v, b %v, c %v; want the newest two (c, a) given up, b kept",
			ended(reqs["a"].ctx), ended(reqs["b"].ctx), ended(reqs["c"].ctx))
	}
	if st := stateOf(h.scaler, "b"); st != stateWaiting {
		t.Fatalf("b is in state %d, want waiting", st)
	}
}

func TestCancelledWaiterIsNotANoJobExit(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	desired(t, h.scaler, 3)
	for range 3 {
		answerOnCancel(h.gate.next(t))
	}
	desired(t, h.scaler, 0)
	eventually(t, "every waiter given up", func() bool { return len(h.scaler.Held()) == 0 })
	if got := h.scaler.NoJobExits(); got != 0 {
		t.Fatalf("NoJobExits %d after three cancelled waiters, want 0", got)
	}
	if got := h.scaler.slots.InUse(); got != 0 {
		t.Fatalf("InUse %d, want 0", got)
	}
	if got := countLines(h.logs, "runner given up before admission", "reason=cancelled"); got != 3 {
		t.Fatalf("%d 'given up' lines with reason=cancelled, want 3", got)
	}
}

func TestGrantAfterGiveUpIsReleased(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		h := newGateHarness(t, NewSlots(4))
		desired(t, h.scaler, 1)
		r := h.gate.next(t)
		desired(t, h.scaler, 0)
		r.reply(h.gate.admitted(r.name, ModeQueued, 3), nil)
		eventually(t, "the grant released", func() bool { return h.gate.releasedOf(r.name) == 1 })
		if !waitGroupWithin(&h.scaler.wg, 2*time.Second) {
			t.Fatal("the waiter goroutine did not return")
		}
		if got := h.jit.minted(); len(got) != 0 {
			t.Fatalf("minted %v for a cancelled runner", got)
		}
		if got := h.scaler.slots.InUse(); got != 0 || len(h.scaler.Held()) != 0 {
			t.Fatalf("InUse %d, held %v; want nothing", got, h.scaler.Held())
		}
		if got := h.gate.releasedOf(r.name); got != 1 {
			t.Fatalf("released %d times, want 1", got)
		}
	})
	t.Run("exited", func(t *testing.T) {
		h := newGateHarness(t, NewSlots(4))
		desired(t, h.scaler, 1)
		r := h.gate.next(t)
		h.scaler.Exited(r.name)
		if got := h.scaler.slots.InUse(); got != 0 || len(h.scaler.Held()) != 0 {
			t.Fatalf("after Exited: InUse %d, held %v; want nothing", got, h.scaler.Held())
		}
		// A new runner takes the freed slot before the old grant arrives.
		if got := desired(t, h.scaler, 1); got != 1 {
			t.Fatalf("Desired(1) after the exit = %d, want 1", got)
		}
		next := h.gate.next(t)
		r.reply(h.gate.admitted(r.name, ModeQueued, 3), nil)
		eventually(t, "the late grant released", func() bool { return h.gate.releasedOf(r.name) == 1 })
		if got := h.scaler.slots.InUse(); got != 1 {
			t.Fatalf("InUse %d after the late grant, want 1 (the new runner's)", got)
		}
		if got, want := h.scaler.Held(), []HeldRunner{{Name: next.name, Starting: true}}; !slices.Equal(got, want) {
			t.Fatalf("Held() = %v, want %v", got, want)
		}
		if got := h.jit.minted(); len(got) != 0 {
			t.Fatalf("minted %v, want nothing", got)
		}
	})
}

func TestCancelledByBudgetIsNotFailOpen(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	desired(t, h.scaler, 1)
	r := h.gate.next(t)
	r.reply(Admitted{}, ErrCancelledByBudget)
	eventually(t, "the runner given up", func() bool { return len(h.scaler.Held()) == 0 })
	if got := h.scaler.slots.InUse(); got != 0 {
		t.Fatalf("InUse %d, want 0", got)
	}
	if got := h.jit.minted(); len(got) != 0 {
		t.Fatalf("minted %v after a cancel through coreslot", got)
	}
	if got := countLines(h.logs, "runner given up before admission", "runner="+r.name, "reason=coreslot_cancel"); got != 1 {
		t.Fatalf("%d 'given up' lines with reason=coreslot_cancel, want 1:\n%s", got, strings.Join(h.logs.lines(), "\n"))
	}
	if got := h.scaler.NoJobExits(); got != 0 {
		t.Fatalf("NoJobExits %d, want 0", got)
	}
	desired(t, h.scaler, 1)
	if again := h.gate.next(t); again.name == r.name {
		t.Fatalf("the next Admit reused %s, want a fresh runner", r.name)
	}
}

func TestGateOffIsTodaysPath(t *testing.T) {
	t.Run("no gate", func(t *testing.T) {
		h := newHarness(NewSlots(4))
		if got := desired(t, h.scaler, 1); got != 1 {
			t.Fatalf("Desired(1) = %d, want 1", got)
		}
		names := h.starter.started()
		if len(names) != 1 || h.starter.needOf(names[0]) != 0 {
			t.Fatalf("started %v with need %d, want one runner with need 0", names, h.starter.needOf(names[0]))
		}
		if st := stateOf(h.scaler, names[0]); st != stateIdle {
			t.Fatalf("state %d after Desired returned, want idle", st)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		h := newGateHarness(t, NewSlots(4))
		h.gate.enabled.Store(false)
		if got := desired(t, h.scaler, 1); got != 1 {
			t.Fatalf("Desired(1) = %d, want 1", got)
		}
		names := h.starter.started()
		if len(names) != 1 || h.starter.needOf(names[0]) != 0 {
			t.Fatalf("started %v, want one runner with need 0", names)
		}
		if st := stateOf(h.scaler, names[0]); st != stateIdle {
			t.Fatalf("state %d after Desired returned, want idle", st)
		}
		if got := h.gate.admitCalls(); got != 0 {
			t.Fatalf("%d Admit calls with the gate disabled, want 0", got)
		}
		if got, want := h.scaler.Held(), []HeldRunner{{Name: names[0]}}; !slices.Equal(got, want) {
			t.Fatalf("Held() = %v, want %v", got, want)
		}
	})
}

func TestBudgetCounts(t *testing.T) {
	h := newGateHarness(t, NewSlots(8))
	desired(t, h.scaler, 5)
	var reqs []*admitReq
	for range 5 {
		reqs = append(reqs, h.gate.next(t))
	}
	for i, mode := range []Mode{ModeQueued, ModeFailOpen, ModeOff} {
		need := 3
		if mode == ModeOff {
			need = 0
		}
		reqs[i].reply(h.gate.admitted(reqs[i].name, mode, need), nil)
	}
	eventually(t, "three runners started", func() bool {
		n := 0
		for _, r := range reqs[:3] {
			if stateOf(h.scaler, r.name) == stateIdle {
				n++
			}
		}
		return n == 3
	})
	waiting, admitted := h.scaler.BudgetCounts()
	if want := map[Mode]int{ModeQueued: 1, ModeFailOpen: 1}; waiting != 2 || !maps.Equal(admitted, want) {
		t.Fatalf("BudgetCounts() = %d, %v; want 2, %v", waiting, admitted, want)
	}
}

func TestOscillatingCountLeaksNothing(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	desired(t, h.scaler, 2)
	w1, w2 := h.gate.next(t), h.gate.next(t)
	w1.reply(h.gate.admitted(w1.name, ModeQueued, 3), nil)
	eventually(t, "w1 idle", func() bool { return stateOf(h.scaler, w1.name) == stateIdle })

	desired(t, h.scaler, 0) // w2 cancelled, w1 reaped
	if !ended(w2.ctx) {
		t.Fatal("w2's ctx is live after Desired(0)")
	}
	desired(t, h.scaler, 2)
	w3, w4 := h.gate.next(t), h.gate.next(t)
	w2.reply(h.gate.admitted(w2.name, ModeQueued, 3), nil)
	answerOnCancel(w3)
	desired(t, h.scaler, 0)
	w4.reply(h.gate.admitted(w4.name, ModeFailOpen, 3), nil)

	for _, r := range []*admitReq{w1, w2, w3, w4} {
		h.scaler.Exited(r.name)
	}
	if !waitGroupWithin(&h.scaler.wg, 2*time.Second) {
		t.Fatal("waiter goroutines still running after 2s")
	}
	if got := h.scaler.slots.InUse(); got != 0 || len(h.scaler.Held()) != 0 {
		t.Fatalf("InUse %d, held %v; want nothing", got, h.scaler.Held())
	}
	for _, r := range []*admitReq{w1, w2, w4} {
		if got := h.gate.releasedOf(r.name); got != 1 {
			t.Errorf("grant of %s released %d times, want 1", r.name, got)
		}
	}
	if got := h.gate.releasedOf(w3.name); got != 0 {
		t.Errorf("w3, never admitted, released %d times", got)
	}
}

// admitIdle admits one runner with ModeQueued and waits until it is idle.
func admitIdle(t *testing.T, h *gateHarness) string {
	t.Helper()
	desired(t, h.scaler, 1)
	r := h.gate.next(t)
	r.reply(h.gate.admitted(r.name, ModeQueued, 3), nil)
	eventually(t, "the admitted runner is idle", func() bool { return stateOf(h.scaler, r.name) == stateIdle })
	return r.name
}

func TestReleaseOnEveryPath(t *testing.T) {
	// Every Release runs the harness's onRelease, which calls s.Held() and
	// fails the test if that blocks: Release must run after unlocking.
	t.Run("die event", func(t *testing.T) {
		h := newGateHarness(t, NewSlots(4))
		name := admitIdle(t, h)
		h.scaler.Exited(name)
		if got := h.gate.releasedOf(name); got != 1 {
			t.Fatalf("released %d times, want 1", got)
		}
	})
	t.Run("failed launch", func(t *testing.T) {
		h := newGateHarness(t, NewSlots(4))
		h.starter.err = errors.New("no such image")
		desired(t, h.scaler, 1)
		r := h.gate.next(t)
		r.reply(h.gate.admitted(r.name, ModeQueued, 3), nil)
		eventually(t, "the failed start released", func() bool { return len(h.scaler.Held()) == 0 })
		if got := h.gate.releasedOf(r.name); got != 1 {
			t.Fatalf("released %d times, want 1", got)
		}
	})
	t.Run("reap", func(t *testing.T) {
		h := newGateHarness(t, NewSlots(4))
		name := admitIdle(t, h)
		desired(t, h.scaler, 0)
		if got := h.docker.removed(); !slices.Equal(got, []string{name}) {
			t.Fatalf("removed %v, want [%s]", got, name)
		}
		if got := h.gate.releasedOf(name); got != 1 {
			t.Fatalf("released %d times, want 1", got)
		}
	})
	t.Run("reconcile", func(t *testing.T) {
		h := newGateHarness(t, NewSlots(4))
		name := admitIdle(t, h)
		// Reconcile releases a held runner that is not Starting and whose
		// container is gone.
		if got, want := h.scaler.Held(), []HeldRunner{{Name: name}}; !slices.Equal(got, want) {
			t.Fatalf("Held() = %v, want %v", got, want)
		}
		h.scaler.Exited(name)
		if got := h.gate.releasedOf(name); got != 1 {
			t.Fatalf("released %d times, want 1", got)
		}
	})
	t.Run("drain removes a waiting runner", func(t *testing.T) {
		h := newGateHarness(t, NewSlots(4))
		desired(t, h.scaler, 1)
		r := h.gate.next(t)
		h.scaler.Drain()
		h.scaler.Exited(r.name)
		r.reply(h.gate.admitted(r.name, ModeQueued, 3), nil)
		if !h.scaler.Join(2 * time.Second) {
			t.Fatal("the waiter goroutine did not return")
		}
		if got := h.gate.releasedOf(r.name); got != 1 {
			t.Fatalf("released %d times, want 1", got)
		}
		if got := h.scaler.slots.InUse(); got != 0 {
			t.Fatalf("InUse %d, want 0", got)
		}
	})
}

func TestDrainCancelsWaitersPromptly(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	desired(t, h.scaler, 3)
	var reqs []*admitReq
	for range 3 {
		r := h.gate.next(t)
		answerOnCancel(r)
		reqs = append(reqs, r)
	}
	h.scaler.Drain()
	deadline := time.Now().Add(5 * time.Second)
	for len(h.scaler.Held()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Held() = %v 5s after Drain, want empty", h.scaler.Held())
		}
		time.Sleep(time.Millisecond)
	}
	for _, r := range reqs {
		if !ended(r.ctx) {
			t.Fatalf("the Admit ctx of %s is live after Drain", r.name)
		}
	}
}

func TestAdmittedLaunchSurvivesDrain(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	block := make(chan struct{})
	h.starter.block = block
	desired(t, h.scaler, 1)
	r := h.gate.next(t)
	r.reply(h.gate.admitted(r.name, ModeQueued, 3), nil)
	eventually(t, "the start began", func() bool { return h.starter.ctxOf(r.name) != nil })

	h.scaler.mu.Lock()
	h.scaler.runners[r.name].cancel()
	h.scaler.mu.Unlock()
	h.scaler.Drain()
	if err := h.starter.ctxOf(r.name).Err(); err != nil {
		t.Fatalf("the start's ctx ended (%v) with the wait ctx and the drain", err)
	}
	close(block)
	eventually(t, "the runner idle", func() bool { return stateOf(h.scaler, r.name) == stateIdle })
}

func TestJoinWaitsAbortDoesNot(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	desired(t, h.scaler, 2)
	for range 2 {
		r := h.gate.next(t)
		go func() {
			<-r.ctx.Done()
			time.Sleep(300 * time.Millisecond)
			r.reply(Admitted{}, r.ctx.Err())
		}()
	}
	start := time.Now()
	h.scaler.Abort()
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("Abort took %v, want it to return at once", d)
	}
	if h.scaler.Join(10 * time.Millisecond) {
		t.Fatal("Join(10ms) = true while both Admits wait 300ms")
	}
	if !h.scaler.Join(5 * time.Second) {
		t.Fatal("Join(5s) = false")
	}
	if got := h.gate.returned.Load(); got != 2 {
		t.Fatalf("%d Admit calls returned after Join, want 2", got)
	}
}

// Turning the settings off (or deleting the file) while a runner waits leaves it waiting: it
// keeps its ticket, and the next Desired neither gives it up, nor admits it again, nor starts
// another runner in its place.
func TestGateOffWhileWaitingKeepsTheTicket(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	desired(t, h.scaler, 1)
	r := h.gate.next(t)
	inUse := h.scaler.slots.InUse()

	h.gate.enabled.Store(false)
	if got := desired(t, h.scaler, 1); got != 1 {
		t.Fatalf("Desired(1) = %d with the gate off, want 1", got)
	}
	if st := stateOf(h.scaler, r.name); st != stateWaiting {
		t.Fatalf("state %d after the gate turned off, want waiting", st)
	}
	if ended(r.ctx) {
		t.Fatal("the waiter's ctx ended after the gate turned off")
	}
	if got := h.gate.admitCalls(); got != 1 {
		t.Fatalf("%d Admit calls, want 1", got)
	}
	if got := h.scaler.slots.InUse(); got != inUse {
		t.Fatalf("InUse %d after the gate turned off, want %d", got, inUse)
	}
	if got := h.starter.started(); len(got) != 0 {
		t.Fatalf("started %v, want nothing", got)
	}
}

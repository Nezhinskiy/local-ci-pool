package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVet answers the scaler's Vet with err, and counts the calls.
type fakeVet struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (v *fakeVet) vet(context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	return v.err
}

func (v *fakeVet) set(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.err = err
}

func (v *fakeVet) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

// A refused vet mints nothing and starts nothing; the slot comes back, and the
// refusal is no no-job exit.
func TestVetRefusalMintsNothing(t *testing.T) {
	v := &fakeVet{err: errors.New("the repository is no longer private")}
	h := newHarnessWith(NewSlots(2), func(c *ScalerConfig) { c.Vet = v.vet })
	if got := desired(t, h.scaler, 1); got != 0 {
		t.Fatalf("Desired(1) with a refused vet = %d, want 0", got)
	}
	if m := h.jit.minted(); len(m) != 0 {
		t.Fatalf("minted %v after a refused vet", m)
	}
	if s := h.starter.started(); len(s) != 0 {
		t.Fatalf("started %v after a refused vet", s)
	}
	if n := h.scaler.slots.InUse(); n != 0 {
		t.Fatalf("in use %d after a refused vet, want 0", n)
	}
	if n := h.scaler.NoJobExits(); n != 0 {
		t.Fatalf("no-job exits %d after a refused vet, want 0", n)
	}
	if !strings.Contains(strings.Join(h.logs.lines(), "\n"), "no longer private") {
		t.Fatalf("the refusal is not logged: %v", h.logs.lines())
	}

	// Once the vet passes, the next call starts the runner.
	v.set(nil)
	if got := desired(t, h.scaler, 1); got != 1 || len(h.jit.minted()) != 1 {
		t.Fatalf("Desired(1) after the vet passed = %d, minted %v; want 1 runner", got, h.jit.minted())
	}
}

// A runner admitted by the gate is vetted again right before its mint: the
// vet that passed when it was queued does not let it through once it refuses.
func TestVetRunsBeforeEachMint(t *testing.T) {
	v := &fakeVet{}
	g := newFakeGate()
	h := newHarnessWith(NewSlots(2), func(c *ScalerConfig) {
		c.Gate = g
		c.Vet = v.vet
	})
	t.Cleanup(func() {
		h.scaler.Abort()
		if !h.scaler.Join(2 * time.Second) {
			t.Error("waiter goroutines still running after cleanup")
		}
	})
	desired(t, h.scaler, 1)
	req := g.next(t)
	v.set(errors.New("the repository is no longer private"))
	req.reply(g.admitted(req.name, ModeQueued, 2), nil)
	eventually(t, "the refused runner released", func() bool { return h.scaler.slots.InUse() == 0 })
	if m := h.jit.minted(); len(m) != 0 {
		t.Fatalf("minted %v for a runner whose vet refused at the mint", m)
	}
	if g.releasedOf(req.name) != 1 {
		t.Fatalf("the refused runner's grant released %d times, want 1", g.releasedOf(req.name))
	}
}

// Desired vets while the scale set has assigned jobs, so a runner already
// idle cannot take a job unnoticed; with none assigned it asks nothing.
func TestDesiredVetsOnlyWithAssignedJobs(t *testing.T) {
	v := &fakeVet{}
	h := newHarnessWith(NewSlots(2), func(c *ScalerConfig) { c.Vet = v.vet })
	desired(t, h.scaler, 0)
	if n := v.count(); n != 0 {
		t.Fatalf("%d vets with no assigned job, want 0", n)
	}
	desired(t, h.scaler, 1)
	before := v.count()
	desired(t, h.scaler, 1) // the runner is idle: nothing to mint
	if n := v.count(); n != before+1 {
		t.Fatalf("%d vets on a call with an assigned job and nothing to mint, want 1", n-before)
	}
}

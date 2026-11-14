package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
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

// The vet runs again right before each mint: a refusal that comes after
// Desired's own vet passed still mints nothing, and gives the slot back.
func TestVetRunsBeforeEachMint(t *testing.T) {
	v := &fakeVet{}
	h := newHarnessWith(NewSlots(2), func(c *ScalerConfig) {
		c.Vet = func(ctx context.Context) error {
			err := v.vet(ctx)
			v.set(errors.New("the repository is no longer private"))
			return err
		}
	})
	desired(t, h.scaler, 1)
	if n := v.count(); n != 2 {
		t.Fatalf("%d vets, want Desired's and the mint's", n)
	}
	if m := h.jit.minted(); len(m) != 0 || h.scaler.slots.InUse() != 0 {
		t.Fatalf("minted %v, %d slots in use, for a runner whose vet refused at the mint", m, h.scaler.slots.InUse())
	}
}

// A job counted as a survivor's still makes Desired vet: an idle survivor of
// the previous run could take a job the count holds.
func TestDesiredVetsJobsItCountsAsSurvivors(t *testing.T) {
	v := &fakeVet{}
	h := newHarnessWith(NewSlots(2), func(c *ScalerConfig) {
		c.Vet = v.vet
		c.Survivors = func() int { return 1 }
	})
	desired(t, h.scaler, 1)
	if n := v.count(); n != 1 {
		t.Fatalf("%d vets with one assigned job, a survivor's, want 1", n)
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

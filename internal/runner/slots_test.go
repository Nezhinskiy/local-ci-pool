package runner

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestSlotsNeverExceedCap(t *testing.T) {
	s := NewSlots(2)
	var won atomic.Int32
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			if s.TryAcquire() {
				won.Add(1)
			}
		}()
	}
	close(gate)
	wg.Wait()
	if got := won.Load(); got != 2 {
		t.Fatalf("%d goroutines acquired a slot, want exactly 2", got)
	}
	if s.InUse() != 2 || s.Cap() != 2 {
		t.Fatalf("InUse %d Cap %d, want 2 and 2", s.InUse(), s.Cap())
	}
	s.Release()
	if !s.TryAcquire() {
		t.Fatal("a released slot could not be acquired again")
	}
	if s.TryAcquire() {
		t.Fatal("a third slot was acquired with cap 2")
	}
}

func TestSlotsReleaseAndBusyNeverGoNegative(t *testing.T) {
	s := NewSlots(1)
	s.Release()
	if s.InUse() != 0 {
		t.Fatalf("InUse %d after a release with nothing held, want 0", s.InUse())
	}
	if !s.TryAcquire() || s.TryAcquire() {
		t.Fatal("a stray release must not create capacity beyond the cap")
	}
	s.SetBusy(1)
	s.SetBusy(-2)
	if s.Busy() != 0 {
		t.Fatalf("Busy %d, want 0", s.Busy())
	}
	s.SetBusy(2)
	if s.Busy() != 2 {
		t.Fatalf("Busy %d, want 2", s.Busy())
	}
}

func TestSlotsNegativeCapIsZero(t *testing.T) {
	s := NewSlots(-3)
	if s.Cap() != 0 || s.TryAcquire() {
		t.Fatalf("cap %d, want 0 and no acquire", s.Cap())
	}
}

// OnChange runs after every acquire and release that changed the slots in
// use, outside the lock, and never for one that changed nothing.
func TestSlotsOnChange(t *testing.T) {
	s := NewSlots(1)
	var seen []int
	s.OnChange(func() { seen = append(seen, s.InUse()) })
	s.Release() // nothing taken
	s.TryAcquire()
	s.TryAcquire() // full
	s.Release()
	if want := []int{1, 0}; len(seen) != len(want) || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("OnChange saw %v, want %v", seen, want)
	}
}

// Two Slots within one parent never hold more than the parent's limit
// together, whichever takes first, and a slot given back by either is free
// for the other.
func TestSlotsWithinAParentShareItsLimit(t *testing.T) {
	jobs := NewSlots(3)
	containers, vms := NewSlots(3), NewSlots(1)
	containers.Within(jobs)
	vms.Within(jobs)
	if !vms.TryAcquire() {
		t.Fatal("the VM slot could not be taken")
	}
	if containers.Cap() != 3 || containers.Free() != 2 {
		t.Fatalf("containers Cap %d Free %d with the VM running, want 3 and 2", containers.Cap(), containers.Free())
	}
	for range 3 {
		containers.TryAcquire()
	}
	if containers.InUse() != 2 {
		t.Fatalf("containers took %d slots beside the VM, want 2", containers.InUse())
	}
	vms.Release()
	if jobs.InUse() != 2 {
		t.Fatalf("the parent holds %d after the VM ended, want 2", jobs.InUse())
	}
	if !containers.TryAcquire() || vms.TryAcquire() {
		t.Fatalf("containers %d, VMs %d after the VM ended; want 3 and 0", containers.InUse(), vms.InUse())
	}
	if containers.InUse()+vms.InUse() != 3 {
		t.Fatalf("children hold %d together, want the parent's 3", containers.InUse()+vms.InUse())
	}
}

// A child refused by its parent takes nothing from either, and a lowered
// parent keeps what is taken but gives nothing new until enough is released.
func TestSlotsWithinALoweredParent(t *testing.T) {
	jobs := NewSlots(4)
	containers := NewSlots(4)
	containers.Within(jobs)
	for range 3 {
		containers.TryAcquire()
	}
	jobs.SetCap(2)
	if containers.Cap() != 2 || containers.Free() != 0 || containers.TryAcquire() {
		t.Fatalf("Cap %d Free %d after the parent was lowered to 2, want 2, 0 and no acquire", containers.Cap(), containers.Free())
	}
	if containers.InUse() != 3 || jobs.InUse() != 3 {
		t.Fatalf("in use %d and %d, want the 3 taken before", containers.InUse(), jobs.InUse())
	}
	containers.Release()
	containers.Release()
	if !containers.TryAcquire() || containers.TryAcquire() {
		t.Fatalf("in use %d once below the lowered cap, want 2", containers.InUse())
	}
	jobs.SetCap(-1)
	if jobs.Cap() != 0 {
		t.Fatalf("parent cap %d, want 0", jobs.Cap())
	}
}

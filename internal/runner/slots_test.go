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

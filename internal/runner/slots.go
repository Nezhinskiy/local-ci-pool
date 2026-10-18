// Package runner starts the pool's ephemeral runner containers and keeps the
// count of them within the machine's slots. One Slots value is shared by every
// scale set on the machine; each scale set has its own Scaler, driven by the
// scale set listener through the Adapter. A slot is taken when a runner is
// started and given back only when its container is gone.
package runner

import "sync"

// Slots is the machine-wide limit on runner containers. It also counts the
// runners that are running a job, for the supervisor's sleep assertion.
type Slots struct {
	mu       sync.Mutex
	cap      int
	inUse    int
	busy     int
	onChange func()
}

// NewSlots returns a limit of n runners; a negative n is 0.
func NewSlots(n int) *Slots {
	return &Slots{cap: max(n, 0)}
}

// OnChange sets f to run after every TryAcquire that took a slot and every
// Release that gave one back. f runs after the slots' lock is released, but
// often under a Scaler's lock: it must not block, and must not call into a
// Scaler.
func (s *Slots) OnChange(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = f
}

// TryAcquire takes a slot if one is free. It never waits.
func (s *Slots) TryAcquire() bool {
	s.mu.Lock()
	if s.inUse >= s.cap {
		s.mu.Unlock()
		return false
	}
	s.inUse++
	f := s.onChange
	s.mu.Unlock()
	if f != nil {
		f()
	}
	return true
}

// Release gives a slot back. A release with no slot taken is ignored, so a
// stray release can never raise the limit.
func (s *Slots) Release() {
	s.mu.Lock()
	if s.inUse == 0 {
		s.mu.Unlock()
		return
	}
	s.inUse--
	f := s.onChange
	s.mu.Unlock()
	if f != nil {
		f()
	}
}

// InUse is the number of slots taken.
func (s *Slots) InUse() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inUse
}

// Cap is the limit.
func (s *Slots) Cap() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cap
}

// Busy is the number of runners known to be running a job.
func (s *Slots) Busy() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

// SetBusy adjusts the busy count by delta; it never goes below zero.
func (s *Slots) SetBusy(delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = max(s.busy+delta, 0)
}

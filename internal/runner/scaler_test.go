package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

func desired(t *testing.T, s *Scaler, count int) int {
	t.Helper()
	n, err := s.Desired(context.Background(), count)
	if err != nil {
		t.Fatalf("Desired(%d): %v", count, err)
	}
	return n
}

func TestDesiredStartsRunnersWithMintedJIT(t *testing.T) {
	h := newHarness(NewSlots(4))
	if got := desired(t, h.scaler, 2); got != 2 {
		t.Fatalf("Desired(2) = %d, want 2", got)
	}
	names := h.starter.started()
	if len(names) != 2 || names[0] == names[1] {
		t.Fatalf("started %v, want two distinct runners", names)
	}
	if !slices.Equal(h.jit.names, names) {
		t.Fatalf("JIT minted for %v, runners started %v", h.jit.names, names)
	}
	for i, n := range names {
		if h.jit.ids[i] != 7 {
			t.Fatalf("JIT minted for scale set %d, want 7", h.jit.ids[i])
		}
		if h.starter.jits[n] != "jit-for-"+n {
			t.Fatalf("runner %s started with JIT %q", n, h.starter.jits[n])
		}
	}
	if h.scaler.Idle() != 2 || h.scaler.BusyCount() != 0 || h.scaler.slots.InUse() != 2 {
		t.Fatalf("idle %d busy %d in use %d, want 2 0 2", h.scaler.Idle(), h.scaler.BusyCount(), h.scaler.slots.InUse())
	}
	// A repeated call with the same count starts nothing more.
	if got := desired(t, h.scaler, 2); got != 2 || len(h.starter.started()) != 2 {
		t.Fatalf("second Desired(2) = %d after %d starts", got, len(h.starter.started()))
	}
	// The cap bounds the target.
	if got := desired(t, h.scaler, 9); got != 4 {
		t.Fatalf("Desired(9) with 4 slots = %d, want 4", got)
	}
}

func TestGlobalLimitAcrossScalers(t *testing.T) {
	slots := NewSlots(2)
	a, b := newHarness(slots), newHarness(slots)
	if got := desired(t, a.scaler, 2); got != 2 {
		t.Fatalf("first scaler Desired(2) = %d, want 2", got)
	}
	if got := desired(t, b.scaler, 2); got != 0 {
		t.Fatalf("second scaler Desired(2) = %d, want 0", got)
	}
	if total := len(a.starter.started()) + len(b.starter.started()); total != 2 {
		t.Fatalf("%d runners started in total, want 2", total)
	}
	if slots.InUse() != 2 {
		t.Fatalf("InUse %d, want 2", slots.InUse())
	}
	// The blocked scaler gets a slot on its next call once one is freed.
	a.scaler.Exited(a.starter.started()[0])
	if got := desired(t, b.scaler, 2); got != 1 {
		t.Fatalf("second scaler after one exit = %d, want 1", got)
	}
}

func TestGlobalLimitAcrossConcurrentScalers(t *testing.T) {
	slots := NewSlots(2)
	hs := make([]*harness, 6)
	for i := range hs {
		hs[i] = newHarness(slots)
	}
	var wg sync.WaitGroup
	gate := make(chan struct{})
	got := make([]int, len(hs))
	for i, h := range hs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			got[i] = desired(t, h.scaler, 2)
		}()
	}
	close(gate)
	wg.Wait()
	sum, starts := 0, 0
	for i, h := range hs {
		sum += got[i]
		starts += len(h.starter.started())
	}
	if sum != 2 || starts != 2 || slots.InUse() != 2 {
		t.Fatalf("scalers report %d runners, %d started, %d slots in use; want 2, 2, 2", sum, starts, slots.InUse())
	}
}

func TestContainerExitReleasesSlot(t *testing.T) {
	h := newHarness(NewSlots(2))
	desired(t, h.scaler, 1)
	name := h.starter.started()[0]
	if h.scaler.slots.InUse() != 1 {
		t.Fatalf("InUse %d after a start, want 1", h.scaler.slots.InUse())
	}

	// The runner's container dies (a bad JIT configuration) without ever
	// taking a job; the die event reaches the scaler through WatchExits.
	msgs := make(chan events.Message, 1)
	msgs <- events.Message{
		Type: events.ContainerEventType, Action: events.ActionDie,
		Actor: events.Actor{ID: "id-1", Attributes: map[string]string{LabelInstance: "main", LabelRunner: name, "exitCode": "1"}},
	}
	h.docker.events = func(ctx context.Context, _ client.EventsListOptions) client.EventsResult {
		return client.EventsResult{Messages: msgs, Err: make(chan error)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		WatchExits(ctx, h.docker, "main", func(n string) {
			h.scaler.Exited(n)
			cancel()
		})
	}()
	<-done

	if h.scaler.slots.InUse() != 0 || h.scaler.Idle() != 0 || h.scaler.BusyCount() != 0 {
		t.Fatalf("after the exit: in use %d idle %d busy %d, want all 0", h.scaler.slots.InUse(), h.scaler.Idle(), h.scaler.BusyCount())
	}
}

func TestExitedIsIdempotent(t *testing.T) {
	slots := NewSlots(2)
	h := newHarness(slots)
	desired(t, h.scaler, 2)
	a := h.starter.started()[0]
	h.scaler.Started(a)
	if slots.Busy() != 1 || h.scaler.BusyCount() != 1 {
		t.Fatalf("busy %d / %d after Started, want 1", slots.Busy(), h.scaler.BusyCount())
	}
	h.scaler.Exited(a)
	h.scaler.Exited(a)
	h.scaler.Completed(a)
	if slots.InUse() != 1 {
		t.Fatalf("InUse %d, want 1: one runner exited (twice), the other still holds its slot", slots.InUse())
	}
	if slots.Busy() != 0 || h.scaler.BusyCount() != 0 || h.scaler.Idle() != 1 {
		t.Fatalf("busy %d / %d idle %d, want 0 0 1", slots.Busy(), h.scaler.BusyCount(), h.scaler.Idle())
	}
}

func TestCompletedClearsBusyButKeepsTheSlotUntilExit(t *testing.T) {
	slots := NewSlots(2)
	h := newHarness(slots)
	desired(t, h.scaler, 1)
	a := h.starter.started()[0]
	h.scaler.Started(a)
	h.scaler.Started(a)
	if slots.Busy() != 1 {
		t.Fatalf("Busy %d after Started twice, want 1", slots.Busy())
	}
	h.scaler.Completed(a)
	if slots.Busy() != 0 || h.scaler.BusyCount() != 0 || h.scaler.Idle() != 0 {
		t.Fatalf("busy %d / %d idle %d after Completed, want 0 0 0", slots.Busy(), h.scaler.BusyCount(), h.scaler.Idle())
	}
	if slots.InUse() != 1 {
		t.Fatalf("InUse %d after Completed, want 1 until the container exits", slots.InUse())
	}
	// The finished runner is not counted, so the scaler may start another
	// runner if a slot is free; the global limit still holds.
	if got := desired(t, h.scaler, 1); got != 1 || slots.InUse() != 2 {
		t.Fatalf("Desired(1) = %d with %d in use, want 1 and 2", got, slots.InUse())
	}
	h.scaler.Exited(a)
	if slots.InUse() != 1 {
		t.Fatalf("InUse %d after the exit, want 1", slots.InUse())
	}
}

func TestIdleRunnerReapedWhenDesiredDrops(t *testing.T) {
	slots := NewSlots(2)
	h := newHarness(slots)
	desired(t, h.scaler, 2)
	if got := desired(t, h.scaler, 0); got != 0 {
		t.Fatalf("Desired(0) = %d, want 0", got)
	}
	want := h.starter.started()
	slices.Sort(want)
	if got := h.docker.removed(); !slices.Equal(got, want) {
		t.Fatalf("removed %v, want forced removes of %v", got, want)
	}
	if slots.InUse() != 0 || h.scaler.Idle() != 0 {
		t.Fatalf("in use %d idle %d after the reap, want 0 0", slots.InUse(), h.scaler.Idle())
	}
	// The die events of the reaped containers change nothing more.
	for _, n := range want {
		h.scaler.Exited(n)
	}
	if slots.InUse() != 0 {
		t.Fatalf("InUse %d after the die events, want 0", slots.InUse())
	}
}

// Desired counts the project's survivors against the assigned jobs: a
// survivor's job gets no runner, the runner beyond the rest is reaped, and
// once the survivor is gone the full count applies again.
func TestSurvivorsCountAgainstDesired(t *testing.T) {
	var survivors atomic.Int32
	survivors.Store(1)
	slots := NewSlots(3)
	h := newHarnessWith(slots, func(c *ScalerConfig) {
		c.Survivors = func() int { return int(survivors.Load()) }
	})
	if got := desired(t, h.scaler, 2); got != 1 {
		t.Fatalf("Desired(2) with one survivor = %d, want 1", got)
	}
	if got := desired(t, h.scaler, 1); got != 0 || len(h.docker.removed()) != 1 {
		t.Fatalf("Desired(1) with one survivor = %d after %d removes, want 0 and 1", got, len(h.docker.removed()))
	}
	if got := desired(t, h.scaler, 0); got != 0 {
		t.Fatalf("Desired(0) with one survivor = %d, want 0", got)
	}
	survivors.Store(0)
	if got := desired(t, h.scaler, 1); got != 1 {
		t.Fatalf("Desired(1) once the survivor is gone = %d, want 1", got)
	}
}

func TestReapNeverRemovesBusyRunners(t *testing.T) {
	slots := NewSlots(3)
	h := newHarness(slots)
	desired(t, h.scaler, 3)
	names := h.starter.started()
	h.scaler.Started(names[0])
	h.scaler.Started(names[1])
	if got := desired(t, h.scaler, 1); got != 2 {
		t.Fatalf("Desired(1) with 2 busy and 1 idle = %d, want 2", got)
	}
	if got := h.docker.removed(); !slices.Equal(got, []string{names[2]}) {
		t.Fatalf("removed %v, want only the idle %s", got, names[2])
	}
	if got := desired(t, h.scaler, 0); got != 2 || len(h.docker.removed()) != 1 {
		t.Fatalf("Desired(0) with only busy runners = %d after %d removes, want 2 and 1", got, len(h.docker.removed()))
	}
}

func TestRemoveNotFoundIsNotAnError(t *testing.T) {
	d := newFakeDocker()
	d.removeErr = func(string) error { return cerrdefs.ErrNotFound.WithMessage("No such container") }
	if err := RemoveContainer(context.Background(), d, "alpha-1"); err != nil {
		t.Fatalf("RemoveContainer on a missing container: %v", err)
	}
	// What the daemon answers for an exited AutoRemove container (measured).
	d.removeErr = func(string) error {
		return cerrdefs.ErrConflict.WithMessage("removal of container alpha-1 is already in progress")
	}
	if err := RemoveContainer(context.Background(), d, "alpha-1"); err != nil {
		t.Fatalf("RemoveContainer on a container being removed: %v", err)
	}
	for _, failure := range []error{
		errors.New("daemon unavailable"),
		cerrdefs.ErrConflict.WithMessage("some other conflict"),
	} {
		d.removeErr = func(string) error { return failure }
		if err := RemoveContainer(context.Background(), d, "alpha-1"); err == nil {
			t.Fatalf("RemoveContainer hid a real failure: %v", failure)
		}
	}
	if err := RemoveContainer(context.Background(), d, ""); err == nil {
		t.Fatal("RemoveContainer accepted an empty name")
	}
	if got := d.removed(); len(got) != 4 {
		t.Fatalf("removes %v, want four (none for the empty name)", got)
	}

	// Through the scaler: a reaped runner whose container is already gone
	// frees its slot without an error line.
	slots := NewSlots(1)
	h := newHarness(slots)
	h.docker.removeErr = func(string) error { return cerrdefs.ErrNotFound.WithMessage("No such container") }
	desired(t, h.scaler, 1)
	desired(t, h.scaler, 0)
	if slots.InUse() != 0 || h.scaler.Idle() != 0 {
		t.Fatalf("in use %d idle %d, want 0 0", slots.InUse(), h.scaler.Idle())
	}
	for _, l := range h.logs.lines() {
		if strings.Contains(l, "level=ERROR") || strings.Contains(l, "level=WARN") {
			t.Fatalf("a missing container was logged as a failure: %s", l)
		}
	}
}

func TestFailedRemoveKeepsTheRunnerForTheNextCall(t *testing.T) {
	slots := NewSlots(1)
	h := newHarness(slots)
	desired(t, h.scaler, 1)
	h.docker.removeErr = func(string) error { return errors.New("daemon unavailable") }
	desired(t, h.scaler, 0)
	if slots.InUse() != 1 || h.scaler.Idle() != 1 {
		t.Fatalf("in use %d idle %d after a failed remove, want 1 1", slots.InUse(), h.scaler.Idle())
	}
	h.docker.removeErr = nil
	desired(t, h.scaler, 0)
	if slots.InUse() != 0 || len(h.docker.removed()) != 2 {
		t.Fatalf("in use %d after %d removes, want 0 after 2", slots.InUse(), len(h.docker.removed()))
	}
}

func TestUnknownRunnerEventIgnored(t *testing.T) {
	slots := NewSlots(2)
	h := newHarness(slots)
	desired(t, h.scaler, 1)
	before := len(h.logs.lines())
	for _, name := range []string{"nope", ""} {
		h.scaler.Started(name)
		h.scaler.Completed(name)
		h.scaler.Exited(name)
	}
	if slots.InUse() != 1 || slots.Busy() != 0 || h.scaler.Idle() != 1 || h.scaler.BusyCount() != 0 {
		t.Fatalf("in use %d busy %d idle %d busy count %d, want 1 0 1 0", slots.InUse(), slots.Busy(), h.scaler.Idle(), h.scaler.BusyCount())
	}
	if got := len(h.logs.lines()) - before; got != 6 {
		t.Fatalf("%d log lines for 6 unknown-name events, want one each:\n%s", got, strings.Join(h.logs.lines()[before:], "\n"))
	}
	if len(h.docker.removed()) != 0 {
		t.Fatalf("removes %v for unknown names", h.docker.removed())
	}
}

func TestDrainStartsNothing(t *testing.T) {
	slots := NewSlots(2)
	h := newHarness(slots)
	h.scaler.Drain()
	if got := desired(t, h.scaler, 2); got != 0 {
		t.Fatalf("Desired(2) while draining = %d, want 0", got)
	}
	if len(h.starter.started()) != 0 || len(h.jit.names) != 0 || slots.InUse() != 0 {
		t.Fatalf("draining scaler started %v, minted %v, holds %d slots", h.starter.started(), h.jit.names, slots.InUse())
	}
}

func TestFailedStartReleasesItsSlot(t *testing.T) {
	slots := NewSlots(2)
	h := newHarness(slots)
	h.starter.err = errors.New("create failed")
	if got := desired(t, h.scaler, 2); got != 0 {
		t.Fatalf("Desired(2) with failing starts = %d, want 0", got)
	}
	if slots.InUse() != 0 || h.scaler.Idle() != 0 {
		t.Fatalf("in use %d idle %d after a failed start, want 0 0", slots.InUse(), h.scaler.Idle())
	}
	if n := len(h.starter.started()); n != 1 {
		t.Fatalf("%d start attempts in one call, want 1 (the next call retries)", n)
	}

	h2 := newHarness(slots)
	h2.jit.err = errors.New("jit refused")
	if got := desired(t, h2.scaler, 2); got != 0 || slots.InUse() != 0 || len(h2.starter.started()) != 0 {
		t.Fatalf("Desired with a failing JIT = %d, %d in use, %d starts", got, slots.InUse(), len(h2.starter.started()))
	}
}

// A start that Start refused (ErrRefused) frees its slot like a failed one,
// but is no failed start: it does not count towards the no-job exits.
func TestARefusedStartIsNoFailedStart(t *testing.T) {
	slots := NewSlots(1)
	h := newHarness(slots)
	h.starter.err = fmt.Errorf("%w: the base is being built", ErrRefused)
	if got := desired(t, h.scaler, 1); got != 0 || slots.InUse() != 0 {
		t.Fatalf("Desired(1) with a refused start = %d, %d in use; want 0, 0", got, slots.InUse())
	}
	if n := h.scaler.NoJobExits(); n != 0 {
		t.Fatalf("no-job exits %d after a refused start, want 0", n)
	}
	h.starter.err = errors.New("create failed")
	desired(t, h.scaler, 1)
	if n := h.scaler.NoJobExits(); n != 1 {
		t.Fatalf("no-job exits %d after a failed start, want 1", n)
	}
}

func TestExitDuringStartIsNotResurrected(t *testing.T) {
	slots := NewSlots(1)
	h := newHarness(slots)
	// The container dies, and its die event arrives, before Start returns.
	h.starter.onStart = func(name string) { h.scaler.Exited(name) }
	desired(t, h.scaler, 1)
	if slots.InUse() != 0 || h.scaler.Idle() != 0 {
		t.Fatalf("in use %d idle %d, want 0 0: the runner exited while starting", slots.InUse(), h.scaler.Idle())
	}
}

func TestHeldListsEveryRunnerHoldingASlot(t *testing.T) {
	h := newHarness(NewSlots(3))
	var during []HeldRunner
	h.starter.onStart = func(string) {
		if during == nil {
			during = h.scaler.Held()
		}
	}
	desired(t, h.scaler, 2)
	names := h.starter.started()
	if len(during) != 1 || !during[0].Starting {
		t.Fatalf("Held during the first start = %+v, want that one runner marked Starting", during)
	}
	h.scaler.Started(names[0])
	h.scaler.Completed(names[0])
	got := h.scaler.Held()
	if len(got) != 2 || got[0].Starting || got[1].Starting {
		t.Fatalf("Held = %+v, want two runners, none starting (one done, one idle)", got)
	}
	want := slices.Clone(names)
	slices.Sort(want)
	if got[0].Name != want[0] || got[1].Name != want[1] {
		t.Fatalf("Held = %+v, want %v", got, want)
	}
	h.scaler.Exited(names[0])
	h.scaler.Exited(names[1])
	if got := h.scaler.Held(); len(got) != 0 {
		t.Fatalf("Held after both exits = %+v, want none", got)
	}
}

func TestSetScaleSetIDRetargetsLaterStarts(t *testing.T) {
	h := newHarness(NewSlots(3))
	desired(t, h.scaler, 1)
	h.scaler.SetScaleSetID(9)
	desired(t, h.scaler, 2)
	if !slices.Equal(h.jit.ids, []int{7, 9}) {
		t.Fatalf("JIT minted for scale sets %v, want 7 then 9", h.jit.ids)
	}
}

// Scaling down is not a broken runner: reaped idle runners, a completed job's
// exit and a start stopped by a drain never count towards NoJobExits; an idle
// runner's exit does, and a job report clears the count.
func TestNoJobExitsCountOnlyRunnersThatNeverTookAJob(t *testing.T) {
	h := newHarness(NewSlots(4))
	desired(t, h.scaler, 3)
	desired(t, h.scaler, 0) // reaps all three
	if n := h.scaler.NoJobExits(); n != 0 || len(h.docker.removed()) != 3 {
		t.Fatalf("no-job exits %d after reaping %v, want 0", n, h.docker.removed())
	}
	desired(t, h.scaler, 2)
	names := h.starter.started()[3:]
	h.scaler.Started(names[0])
	h.scaler.Completed(names[0])
	h.scaler.Exited(names[0])
	if n := h.scaler.NoJobExits(); n != 0 {
		t.Fatalf("no-job exits %d after a completed job's exit, want 0", n)
	}
	h.scaler.Exited(names[1])
	if n := h.scaler.NoJobExits(); n != 1 {
		t.Fatalf("no-job exits %d after an idle runner's exit, want 1", n)
	}
	h.scaler.Completed("local-ci-000000000000") // late, for a runner already gone
	if n := h.scaler.NoJobExits(); n != 0 {
		t.Fatalf("no-job exits %d after a job report, want 0", n)
	}
}

// fakeAdmit is the coreslot gate as a scaler sees it. With answers nil each
// Bind returns true at once; otherwise each waits for its answer, and for its
// context too unless deaf.
type fakeAdmit struct {
	bound chan string

	mu       sync.Mutex
	budgeted bool
	answers  chan bool
	deaf     bool
	binds    []string
	unbinds  []string
	ctxs     map[string]context.Context
}

func newFakeAdmit(answers chan bool) *fakeAdmit {
	return &fakeAdmit{bound: make(chan string, 16), budgeted: true, answers: answers, ctxs: map[string]context.Context{}}
}

func (a *fakeAdmit) Budgeted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.budgeted
}

func (a *fakeAdmit) setBudgeted(b bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.budgeted = b
}

func (a *fakeAdmit) Bind(ctx context.Context, name string) bool {
	a.mu.Lock()
	a.binds = append(a.binds, name)
	a.ctxs[name] = ctx
	answers, deaf := a.answers, a.deaf
	a.mu.Unlock()
	a.bound <- name
	if answers == nil {
		return true
	}
	if deaf {
		return <-answers
	}
	select {
	case v := <-answers:
		return v
	case <-ctx.Done():
		return false
	}
}

func (a *fakeAdmit) Unbind(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unbinds = append(a.unbinds, name)
}

func (a *fakeAdmit) bindsSoFar() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.binds)
}

func (a *fakeAdmit) unbound() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.unbinds)
}

func (a *fakeAdmit) ctxOf(name string) context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ctxs[name]
}

// nextBind is the name of the next Bind, within 1s.
func (a *fakeAdmit) nextBind(t *testing.T) string {
	t.Helper()
	select {
	case n := <-a.bound:
		return n
	case <-time.After(time.Second):
		t.Fatal("no Bind within 1s")
		return ""
	}
}

// admitHarness is a harness whose scaler waits on a; at the end the waiting
// Binds are answered false and the scaler's goroutines joined.
func admitHarness(t *testing.T, slots int, a *fakeAdmit, configure func(*ScalerConfig)) *harness {
	t.Helper()
	h := newHarnessWith(NewSlots(slots), func(c *ScalerConfig) {
		c.Admit = a
		if configure != nil {
			configure(c)
		}
	})
	t.Cleanup(func() {
		if a.answers != nil {
			close(a.answers)
		}
		h.scaler.Abort()
		h.scaler.Wait()
	})
	return h
}

// desiredSoon runs Desired in the background and fails unless it returns
// within 1s.
func desiredSoon(t *testing.T, s *Scaler, ctx context.Context, count int) int {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		n, _ := s.Desired(ctx, count)
		done <- n
	}()
	select {
	case n := <-done:
		return n
	case <-time.After(time.Second):
		t.Fatalf("Desired(%d) did not return within 1s", count)
		return 0
	}
}

// Mutation: calling admitThenLaunch inline in Desired (no goroutine) turns it
// red: Desired waits for the permit.
func TestAdmitWaitsOutsideTheLock(t *testing.T) {
	a := newFakeAdmit(make(chan bool))
	a.setBudgeted(false)
	h := admitHarness(t, 3, a, nil)
	desired(t, h.scaler, 1)
	other := h.starter.started()[0]
	a.setBudgeted(true)
	if n := desiredSoon(t, h.scaler, context.Background(), 2); n != 2 {
		t.Fatalf("Desired(2) = %d, want 2 (one idle, one waiting)", n)
	}
	waiting := a.nextBind(t)
	if st := stateOf(h.scaler, waiting); st != stateWaiting {
		t.Fatalf("state %d, want waiting", st)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.scaler.Exited(other)
		h.scaler.Held()
		_, _ = h.scaler.Desired(context.Background(), 2)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Exited, Held and Desired did not complete within 1s while a Bind waits")
	}
}

// Mutation: activeLocked not counting waiting runners turns it red: the
// second Desired reserves a second runner.
func TestAdmitSecondDesiredStartsNoSecondRunner(t *testing.T) {
	a := newFakeAdmit(make(chan bool))
	h := admitHarness(t, 2, a, nil)
	desiredSoon(t, h.scaler, context.Background(), 1)
	a.nextBind(t)
	if n := desiredSoon(t, h.scaler, context.Background(), 1); n != 1 {
		t.Fatalf("second Desired(1) = %d, want 1", n)
	}
	time.Sleep(50 * time.Millisecond)
	if b := a.bindsSoFar(); len(b) != 1 || h.scaler.slots.InUse() != 1 {
		t.Fatalf("binds %v, %d slots in use; want one of each", b, h.scaler.slots.InUse())
	}
}

// Mutation: vetting only in launch (not before Bind) turns it red: the
// refused runner spends a Bind.
func TestAdmitVetBeforeBind(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	vet := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls > 1 {
			return errors.New("the base is being built")
		}
		return nil
	}
	a := newFakeAdmit(nil)
	h := admitHarness(t, 2, a, func(c *ScalerConfig) { c.Vet = vet })
	desiredSoon(t, h.scaler, context.Background(), 1)
	eventually(t, "the refused runner's slot is released", func() bool { return h.scaler.slots.InUse() == 0 })
	if b := a.bindsSoFar(); len(b) != 0 {
		t.Fatalf("binds %v after a refused vet, want none", b)
	}
	if m := h.jit.minted(); len(m) != 0 || h.scaler.NoJobExits() != 0 {
		t.Fatalf("minted %v, no-job exits %d; want none, 0", m, h.scaler.NoJobExits())
	}
}

// Mutation: releasing a runner whose Bind returned false as a failed start
// turns it red.
func TestAdmitFalseReleasesTheSlot(t *testing.T) {
	a := newFakeAdmit(make(chan bool))
	h := admitHarness(t, 2, a, nil)
	desiredSoon(t, h.scaler, context.Background(), 1)
	a.nextBind(t)
	a.answers <- false
	eventually(t, "the slot is released", func() bool { return h.scaler.slots.InUse() == 0 && len(h.scaler.Held()) == 0 })
	if n := h.scaler.NoJobExits(); n != 0 {
		t.Fatalf("no-job exits %d after a withdrawn permit, want 0", n)
	}
	if m := h.jit.minted(); len(m) != 0 {
		t.Fatalf("minted %v", m)
	}
}

// Mutation: reap skipping waiting runners (no withdrawLocked) turns it red:
// the Bind keeps waiting and the slot stays taken.
func TestAdmitWithdrawnWhenCountDrops(t *testing.T) {
	a := newFakeAdmit(make(chan bool))
	h := admitHarness(t, 2, a, nil)
	desiredSoon(t, h.scaler, context.Background(), 1)
	name := a.nextBind(t)
	if n := desiredSoon(t, h.scaler, context.Background(), 0); n != 0 {
		t.Fatalf("Desired(0) = %d, want 0", n)
	}
	select {
	case <-a.ctxOf(name).Done():
	case <-time.After(time.Second):
		t.Fatal("the waiting Bind's context did not end")
	}
	eventually(t, "the slot is released", func() bool { return h.scaler.slots.InUse() == 0 })
	if m := h.jit.minted(); len(m) != 0 {
		t.Fatalf("minted %v for a withdrawn runner", m)
	}
}

// Mutation: counting stateWithdrawn in activeLocked turns it red: the second
// Desired(1) withdraws the older runner too.
func TestAdmitWithdrawsNewestOnce(t *testing.T) {
	a := newFakeAdmit(make(chan bool))
	a.deaf = true // a withdrawn runner stays withdrawn until its Bind answers
	h := admitHarness(t, 2, a, nil)
	h.scaler.names = scripted("local-ci-older", "local-ci-newer")
	desiredSoon(t, h.scaler, context.Background(), 2)
	a.nextBind(t)
	a.nextBind(t)
	desiredSoon(t, h.scaler, context.Background(), 1)
	desiredSoon(t, h.scaler, context.Background(), 1)
	if a.ctxOf("local-ci-newer").Err() == nil {
		t.Fatal("the newer runner was not withdrawn")
	}
	if a.ctxOf("local-ci-older").Err() != nil {
		t.Fatal("the older runner was withdrawn too")
	}
	if st := stateOf(h.scaler, "local-ci-older"); st != stateWaiting {
		t.Fatalf("the older runner's state %d, want waiting", st)
	}
}

// Mutation: launching a runner withdrawn while its admission came turns it
// red: it is minted.
func TestAdmitAdmissionRacingWithdrawal(t *testing.T) {
	a := newFakeAdmit(make(chan bool))
	a.deaf = true
	h := admitHarness(t, 2, a, nil)
	desiredSoon(t, h.scaler, context.Background(), 1)
	name := a.nextBind(t)
	desiredSoon(t, h.scaler, context.Background(), 0)
	if st := stateOf(h.scaler, name); st != stateWithdrawn {
		t.Fatalf("state %d, want withdrawn", st)
	}
	a.answers <- true
	eventually(t, "the withdrawn runner is released", func() bool { return h.scaler.slots.InUse() == 0 })
	if u := a.unbound(); !slices.Equal(u, []string{name}) {
		t.Fatalf("unbinds %v, want %s once", u, name)
	}
	if m := h.jit.minted(); len(m) != 0 {
		t.Fatalf("minted %v for a withdrawn runner", m)
	}
}

// Mutation: Bind under the caller's ctx instead of WithoutCancel(ctx) turns
// it red.
func TestAdmitBindSurvivesCallerContext(t *testing.T) {
	a := newFakeAdmit(make(chan bool))
	h := admitHarness(t, 2, a, nil)
	ctx, cancel := context.WithCancel(context.Background())
	desiredSoon(t, h.scaler, ctx, 1)
	name := a.nextBind(t)
	cancel()
	time.Sleep(20 * time.Millisecond)
	if err := a.ctxOf(name).Err(); err != nil {
		t.Fatalf("the Bind's context ended with its caller's: %v", err)
	}
	a.answers <- true
	eventually(t, "the runner starts", func() bool { return len(h.starter.started()) == 1 })
}

func TestAdmitDrainBetweenBindAndMint(t *testing.T) {
	a := newFakeAdmit(make(chan bool))
	h := admitHarness(t, 2, a, nil)
	desiredSoon(t, h.scaler, context.Background(), 1)
	name := a.nextBind(t)
	h.scaler.Drain()
	a.answers <- true
	eventually(t, "the drained runner is released", func() bool { return h.scaler.slots.InUse() == 0 })
	if s := h.starter.started(); len(s) != 0 {
		t.Fatalf("started %v while draining", s)
	}
	if u := a.unbound(); !slices.Equal(u, []string{name}) {
		t.Fatalf("unbinds %v, want %s once", u, name)
	}
	if !strings.Contains(strings.Join(h.logs.lines(), "\n"), "draining; not starting the runner") {
		t.Fatalf("no errDrained line: %v", h.logs.lines())
	}
}

// Mutation: calling Unbind only from Exited (not from release) turns it red:
// a launch that fails after its admission keeps its hold.
func TestAdmitUnbindOnEveryRelease(t *testing.T) {
	a := newFakeAdmit(nil)
	h := admitHarness(t, 2, a, nil)
	desiredSoon(t, h.scaler, context.Background(), 1)
	eventually(t, "the runner starts", func() bool { return len(h.starter.started()) == 1 })
	first := h.starter.started()[0]
	eventually(t, "the runner is idle", func() bool { return stateOf(h.scaler, first) == stateIdle })
	h.scaler.Exited(first)
	if u := a.unbound(); !slices.Equal(u, []string{first}) {
		t.Fatalf("unbinds %v after Exited, want %s", u, first)
	}
	h.starter.mu.Lock()
	h.starter.err = errors.New("create failed")
	h.starter.mu.Unlock()
	desiredSoon(t, h.scaler, context.Background(), 1)
	eventually(t, "the failed launch is released", func() bool {
		return len(h.starter.started()) == 2 && h.scaler.slots.InUse() == 0
	})
	second := h.starter.started()[1]
	eventually(t, "the failed launch is unbound", func() bool { return len(a.unbound()) == 2 })
	if u := a.unbound(); !slices.Equal(u, []string{first, second}) {
		t.Fatalf("unbinds %v, want %s then %s, once each", u, first, second)
	}
}

// Mutation: waiting asynchronously regardless of Budgeted turns it red.
func TestAdmitInlineWhenUnbudgeted(t *testing.T) {
	a := newFakeAdmit(make(chan bool))
	a.setBudgeted(false)
	h := admitHarness(t, 2, a, nil)
	desired(t, h.scaler, 1)
	if s := h.starter.started(); len(s) != 1 {
		t.Fatalf("started %v when Desired returned, want one runner inline", s)
	}
	if b := a.bindsSoFar(); len(b) != 0 {
		t.Fatalf("binds %v while unbudgeted", b)
	}
}

func TestScalerWaitJoins(t *testing.T) {
	a := newFakeAdmit(make(chan bool))
	h := admitHarness(t, 2, a, nil)
	desiredSoon(t, h.scaler, context.Background(), 2)
	a.nextBind(t)
	a.nextBind(t)
	h.scaler.Abort()
	done := make(chan struct{})
	go func() {
		h.scaler.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return within 1s of Abort")
	}
	if n := h.scaler.slots.InUse(); n != 0 {
		t.Fatalf("%d slots in use after Wait, want 0", n)
	}
}

func TestNilAdmitUnchanged(t *testing.T) {
	h := newHarness(NewSlots(2))
	desired(t, h.scaler, 1)
	if s := h.starter.started(); len(s) != 1 {
		t.Fatalf("started %v, want one runner inline", s)
	}
	h.scaler.Exited(h.starter.started()[0])
	h.scaler.Wait()
	if n := h.scaler.slots.InUse(); n != 0 {
		t.Fatalf("%d slots in use, want 0", n)
	}
}

package runner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

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

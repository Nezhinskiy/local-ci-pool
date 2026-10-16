//go:build unix

package budget

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/core-slot/coreslot"

	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
)

// stepUntilWedged steps the monitor until the gate reports wedged, and returns the steps taken.
func stepUntilWedged(t *testing.T, h *harness) int {
	t.Helper()
	steps := 0
	for h.g.State().State != "wedged" {
		if steps == 40 {
			t.Fatal("not wedged after 40 monitor ticks")
		}
		h.step(t)
		steps++
	}
	return steps
}

// wedge fills the budget with holder:x, starts Admit r1, holds the admission section as pid and
// steps the monitor until the gate is wedged and r1 has failed open. It returns the section's
// unlock, the holder and the steps taken.
func wedge(t *testing.T, h *harness, pid int) (unlock func(), holder *coreslot.Grant, steps int) {
	t.Helper()
	holder = hold(t, h.dir, "holder:x", 4)
	ch := admitAsync(context.Background(), h, alpha("r1"))
	waitTickets(t, h.dir, 3)
	_, unlock = holdSection(t, h.dir, pid)
	steps = stepUntilWedged(t, h)
	r := waitResult(t, ch)
	if r.err != nil || r.ad.Mode != runner.ModeFailOpen {
		t.Fatalf("the waiting Admit = %+v, %v; want fail_open", r.ad, r.err)
	}
	return unlock, holder, steps
}

func lastLine(h *harness, msg string) string {
	lines := h.log.lines(msg)
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

func TestWedgeFailsOpenAndClears(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), on3)
	unlock, _, steps := wedge(t, h, 4242)
	// The first tick sees the section held; the wedge comes WedgeAfter (24 ticks) later.
	if steps != 25 {
		t.Fatalf("wedged after %d ticks; want 25", steps)
	}
	if got, want := h.log.lines("budget wedged"), []string{"pid=4242"}; !slices.Equal(got, want) {
		t.Fatalf("budget wedged lines = %q; want %q", got, want)
	}
	if got := lastLine(h, "runner admitted"); !strings.HasSuffix(got, "mode=fail_open need=3 granted=0 shortfall=0 wait_s=125.0 class=wedged") {
		t.Fatalf("runner admitted = %q; want a fail_open of class wedged", got)
	}

	before := h.cc.snapshots.Load()
	ad, err := h.g.Admit(context.Background(), alpha("r2"))
	if err != nil || ad.Mode != runner.ModeFailOpen {
		t.Fatalf("Admit while wedged = %+v, %v; want fail_open", ad, err)
	}
	if n := h.cc.snapshots.Load() - before; n != 1 {
		t.Fatalf("Admit while wedged took %d snapshots; want 1", n)
	}
	if got := lastLine(h, "runner admitted"); !strings.HasPrefix(got, "runner=r2 ") || !strings.HasSuffix(got, "class=wedged") {
		t.Fatalf("runner admitted = %q; want r2 failing open as wedged", got)
	}

	unlock()
	h.step(t)
	if got := h.g.State().State; got != "on" {
		t.Fatalf("State().State = %q after the section was freed; want on", got)
	}
	if got, want := h.log.lines("budget unwedged"), []string{"pid=4242"}; !slices.Equal(got, want) {
		t.Fatalf("budget unwedged lines = %q; want %q", got, want)
	}
}

func TestWedgeClearsWithNothingWaiting(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), on3)
	unlock, _, _ := wedge(t, h, 4242)

	before := h.cc.snapshots.Load()
	h.step(t)
	h.step(t)
	if n := h.cc.snapshots.Load() - before; n != 2 {
		t.Fatalf("%d snapshots in two ticks while wedged with nothing waiting; want 2", n)
	}
	if got := h.g.State().State; got != "wedged" {
		t.Fatalf("State().State = %q; want still wedged", got)
	}
	unlock()
	h.step(t)
	if got := h.g.State().State; got != "on" {
		t.Fatalf("State().State = %q after the section was freed; want on", got)
	}
}

func TestWedgeClearsOnAnotherHolder(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), on3)
	unlock, _, _ := wedge(t, h, 4242)

	unlock()
	holdSection(t, h.dir, 4343)
	h.step(t)
	if got := h.g.State().State; got != "on" {
		t.Fatalf("State().State = %q with another holder in the section; want on", got)
	}
	if got, want := h.log.lines("budget unwedged"), []string{"pid=4242"}; !slices.Equal(got, want) {
		t.Fatalf("budget unwedged lines = %q; want %q", got, want)
	}

	// An Admit stuck on the new holder keeps the monitor running until it wedges again.
	enqueues := h.cc.enqueues.Load()
	ch := admitAsync(context.Background(), h, alpha("r2"))
	eventually(t, "r2's Enqueue", func() bool { return h.cc.enqueues.Load() > enqueues })
	stepUntilWedged(t, h)
	if got, want := h.log.lines("budget wedged"), []string{"pid=4242", "pid=4343"}; !slices.Equal(got, want) {
		t.Fatalf("budget wedged lines = %q; want %q", got, want)
	}
	if r := waitResult(t, ch); r.err != nil || r.ad.Mode != runner.ModeFailOpen {
		t.Fatalf("r2 = %+v, %v; want fail_open", r.ad, r.err)
	}
}

func TestNewAdmitWhileWedgedProceedsWhenFree(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), on3)
	unlock, holder, _ := wedge(t, h, 4242)

	unlock()
	ch := admitAsync(context.Background(), h, alpha("r2"))
	waitTickets(t, h.dir, 3)
	if got := h.g.State().State; got != "on" {
		t.Fatalf("State().State = %q after a new Admit saw the section free; want on", got)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	r := waitResult(t, ch)
	if r.err != nil || r.ad.Mode != runner.ModeQueued {
		t.Fatalf("r2 = %+v, %v; want queued", r.ad, r.err)
	}
	r.ad.Release()
}

func TestFullBudgetIsNotAWedge(t *testing.T) {
	t.Parallel()
	dir := stateDir(t, cpu4)
	h := newGate(t, dir, on3)
	hold(t, dir, "holder:x", 4)
	ch := admitAsync(context.Background(), h, alpha("r1"))
	waitTickets(t, dir, 3)

	for range 25 { // past WedgeAfter
		h.step(t)
	}
	select {
	case r := <-ch:
		t.Fatalf("Admit returned on a full budget: %+v, %v", r.ad, r.err)
	default:
	}
	if got, want := h.g.State(), (State{State: "on", OldestWaitS: 125}); got != want {
		t.Fatalf("State() = %+v; want %+v", got, want)
	}
	if n := len(h.log.lines("budget full")); n != 0 {
		t.Fatalf("%d budget full lines before LongWait", n)
	}

	want := []string{}
	for _, at := range []int{1925, 3730} {
		h.advance(t, h.g.longWait)
		want = append(want, "oldest_wait_s="+strconv.Itoa(at)+" holds=holder:x cpu=4")
		h.step(t)
		if got := h.log.lines("budget full"); !slices.Equal(got, want) {
			t.Fatalf("budget full lines = %q; want %q", got, want)
		}
	}
	if got := h.g.State().State; got != "on" {
		t.Fatalf("State().State = %q; want on", got)
	}
}

func TestMonitorStopsWhenNothingWaitsAndNotWedged(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), on3)
	ad, err := h.g.Admit(context.Background(), alpha("r1"))
	if err != nil || ad.Mode != runner.ModeQueued {
		t.Fatalf("Admit = %+v, %v; want queued", ad, err)
	}
	ad.Release()

	eventually(t, "the monitor's timer", func() bool { return h.clk.pending() > 0 })
	h.clk.Advance(h.g.snapshotEvery)
	eventually(t, "the monitor to stop", func() bool { return !h.g.monitorRunning() })
	if n := h.clk.pending(); n != 0 {
		t.Fatalf("%d timers pending after the monitor stopped", n)
	}
	before := h.cc.snapshots.Load()
	for range 5 {
		h.clk.Advance(h.g.snapshotEvery)
	}
	if n := h.cc.snapshots.Load() - before; n != 0 {
		t.Fatalf("%d snapshots after the monitor stopped", n)
	}
}

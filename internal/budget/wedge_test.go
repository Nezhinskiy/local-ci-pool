//go:build unix

package budget

import (
	"context"
	"errors"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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

// waitWaiting waits for the runner waiting line of runner name. The gate logs it once Enqueue has
// returned, so the Admit is past the wedge check and its Enqueue has left the admission section.
func waitWaiting(t *testing.T, h *harness, name string) {
	t.Helper()
	eventually(t, name+"'s runner waiting line", func() bool {
		return slices.ContainsFunc(h.log.lines("runner waiting for the budget"), func(l string) bool {
			return strings.HasPrefix(l, "runner="+name+" ")
		})
	})
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
	if got, want := h.log.lines("budget wedged"), []string{"recorded_pid=4242 self=false"}; !slices.Equal(got, want) {
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
	if got, want := h.log.lines("budget unwedged"), []string{"recorded_pid=4242 self=false"}; !slices.Equal(got, want) {
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
	if got, want := h.log.lines("budget unwedged"), []string{"recorded_pid=4242 self=false"}; !slices.Equal(got, want) {
		t.Fatalf("budget unwedged lines = %q; want %q", got, want)
	}

	// An Admit stuck on the new holder keeps the monitor running until it wedges again.
	enqueues := h.cc.enqueues.Load()
	ch := admitAsync(context.Background(), h, alpha("r2"))
	eventually(t, "r2's Enqueue", func() bool { return h.cc.enqueues.Load() > enqueues })
	stepUntilWedged(t, h)
	if got, want := h.log.lines("budget wedged"), []string{"recorded_pid=4242 self=false", "recorded_pid=4343 self=false"}; !slices.Equal(got, want) {
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
	// No snapshot of the test's own runs while r2 decides: a snapshot probes admit.lock and the
	// tickets with a momentary flock, so one taken alongside r2's would show r2 the section held
	// under the wedge's body, or show the test r1's withdrawn ticket live.
	ch := admitAsync(context.Background(), h, alpha("r2"))
	waitWaiting(t, h, "r2")
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
	// r1 waits without polling coreslot. A poll enters the admission section, and on the manual
	// clock the ticks are moments apart in real time: one entry that a stalled goroutine keeps
	// across 25 ticks, or across one tick of LongWait, reads as a wedge.
	h.cc.wrapWaiter = func(w waiter) waiter {
		return waiterFunc(func(ctx context.Context) (*coreslot.Grant, error) {
			<-ctx.Done()
			return w.Wait(ctx)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := admitAsync(ctx, h, alpha("r1"))
	// Before the holder is closed and the state directory removed: r1 withdraws its ticket, rather
	// than being admitted into a directory being removed.
	t.Cleanup(func() {
		cancel()
		if r := waitResult(t, ch); !errors.Is(r.err, context.Canceled) {
			t.Errorf("r1 = %+v, %v; want context.Canceled", r.ad, r.err)
		}
	})
	waitWaiting(t, h, "r1") // its Enqueue has left the section
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

// A *StateError of one Admit marks the client stale; it never takes the client away from the
// monitor. With every slot held by a waiting Admit, nothing else opens coreslot again, so the
// monitor must keep snapshotting through the stale client (or the one it reopens) and still
// wedge the gate when the section is held.
func TestWedgeAfterAStateError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		reopenFails bool
	}{
		{"reopen fails", true},
		{"reopen succeeds", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newGate(t, stateDir(t, cpu4), on3)
			hold(t, h.dir, "holder:x", 4)
			ch := admitAsync(context.Background(), h, alpha("r1"))
			waitTickets(t, h.dir, 3)

			h.cc.mu.Lock()
			h.cc.enqueueErr = &coreslot.StateError{Reason: "injected"}
			if tc.reopenFails {
				h.cc.openFails, h.cc.openErr = math.MaxInt, errors.New("open refused")
			}
			h.cc.mu.Unlock()
			ad, err := h.g.Admit(context.Background(), alpha("r2"))
			if err != nil || ad.Mode != runner.ModeFailOpen {
				t.Fatalf("Admit r2 = %+v, %v; want fail_open", ad, err)
			}
			if got := lastLine(h, "runner admitted"); !strings.HasPrefix(got, "runner=r2 ") || !strings.HasSuffix(got, "class=state") {
				t.Fatalf("runner admitted = %q; want r2 failing open as state", got)
			}

			holdSection(t, h.dir, 4242)
			stepUntilWedged(t, h)
			r := waitResult(t, ch)
			if r.err != nil || r.ad.Mode != runner.ModeFailOpen {
				t.Fatalf("the waiting Admit = %+v, %v; want fail_open", r.ad, r.err)
			}
			if got := lastLine(h, "runner admitted"); !strings.HasPrefix(got, "runner=r1 ") || !strings.HasSuffix(got, "class=wedged") {
				t.Fatalf("runner admitted = %q; want r1 failing open as wedged", got)
			}
			if got := h.g.State().State; got != "wedged" {
				t.Fatalf("State().State = %q; want wedged", got)
			}
		})
	}
}

// A holder that enters the section without rewriting admit.lock's body (a token-only client)
// leaves the last body in place, here the one this process's own client wrote. The wedge is still
// found, and its line names the recorded pid as recorded, marked as this process's own.
func TestWedgeBehindAStaleBody(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), on3)
	hold(t, h.dir, "holder:x", 4)
	ch := admitAsync(context.Background(), h, alpha("r1"))
	waitTickets(t, h.dir, 3)
	holdSectionInChild(t, h.dir)
	stepUntilWedged(t, h)
	if r := waitResult(t, ch); r.err != nil || r.ad.Mode != runner.ModeFailOpen {
		t.Fatalf("the waiting Admit = %+v, %v; want fail_open", r.ad, r.err)
	}
	want := []string{"recorded_pid=" + strconv.Itoa(os.Getpid()) + " self=true"}
	if got := h.log.lines("budget wedged"); !slices.Equal(got, want) {
		t.Fatalf("budget wedged lines = %q; want %q", got, want)
	}
}

// Traffic through the section behind a body nobody rewrites: every snapshot finds the section
// held under the same body, but gen moves, which only a section can do, so no one holder keeps
// it. That is no wedge. Once gen stops moving, the same snapshots wedge the gate.
func TestTrafficBehindAStaleBodyIsNoWedge(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), on3)
	hold(t, h.dir, "holder:x", 4)
	ch := admitAsync(context.Background(), h, alpha("r1"))
	waitTickets(t, h.dir, 3)

	entered := time.Unix(1_700_000_000, 0)
	var gen int64
	moving := true
	h.cc.mu.Lock()
	h.cc.report = func(rep coreslot.Report) coreslot.Report {
		if moving {
			gen++
		}
		rep.Gen = gen
		rep.Section = coreslot.SectionInfo{Held: true, PID: 4242, Entered: entered}
		return rep
	}
	h.cc.mu.Unlock()
	for range 30 { // past WedgeAfter (24 ticks)
		h.step(t)
	}
	if got := h.g.State().State; got != "on" {
		t.Fatalf("State().State = %q with gen moving behind one body; want on", got)
	}
	if n := len(h.log.lines("budget wedged")); n != 0 {
		t.Fatalf("%d budget wedged lines with gen moving", n)
	}
	select {
	case r := <-ch:
		t.Fatalf("Admit returned while gen moved: %+v, %v", r.ad, r.err)
	default:
	}

	h.cc.mu.Lock()
	moving = false
	h.cc.mu.Unlock()
	// The last snapshot with gen moving began the count: WedgeAfter is 24 ticks from it.
	if steps := stepUntilWedged(t, h); steps != 24 {
		t.Fatalf("wedged %d ticks after gen stopped; want 24", steps)
	}
	if got, want := h.log.lines("budget wedged"), []string{"recorded_pid=4242 self=false"}; !slices.Equal(got, want) {
		t.Fatalf("budget wedged lines = %q; want %q", got, want)
	}
	waitResult(t, ch)
}

// unreadableAfterWedge wedges the gate, frees the section and makes the state directory
// unreadable, so that every snapshot fails on coreslot's state.
func unreadableAfterWedge(t *testing.T) *harness {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory of mode 000")
	}
	h := newGate(t, stateDir(t, cpu4), on3)
	unlock, _, _ := wedge(t, h, 4242)
	unlock() // the holder is gone: the section is free
	if err := os.Chmod(h.dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.dir, 0o700) })
	return h
}

func (g *Gate) isStale() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stale
}

// While wedged, a new Admit's snapshot fails on coreslot's state (its directory unreadable): the
// Admit fails open as state, not wedged, the client is marked stale, and /healthz says state.
func TestWedgedSnapshotStateErrorFailsOpenAsState(t *testing.T) {
	t.Parallel()
	h := unreadableAfterWedge(t)
	ad, err := h.g.Admit(context.Background(), alpha("r9"))
	if err != nil || ad.Mode != runner.ModeFailOpen {
		t.Fatalf("Admit = %+v, %v; want fail_open", ad, err)
	}
	if got := lastLine(h, "runner admitted"); !strings.HasPrefix(got, "runner=r9 ") || !strings.HasSuffix(got, "class=state") {
		t.Fatalf("runner admitted = %q; want r9 failing open as state", got)
	}
	if got := h.g.State().Error; got != "state" {
		t.Fatalf("State().Error = %q; want state", got)
	}
	if !h.g.isStale() {
		t.Fatal("the client is not marked stale")
	}
}

// A monitor tick whose snapshot fails on coreslot's state marks the client stale, so the next
// call opens coreslot again; here that Open fails too, and /healthz says open rather than no
// error at all.
func TestMonitorSnapshotStateErrorMarksStale(t *testing.T) {
	t.Parallel()
	h := unreadableAfterWedge(t)
	opens := h.cc.opens.Load()
	h.step(t)
	if !h.g.isStale() {
		t.Fatal("the client is not marked stale after the monitor's snapshot failed")
	}
	h.step(t)
	if n := h.cc.opens.Load() - opens; n != 1 {
		t.Fatalf("%d opens on the tick after the client went stale; want 1", n)
	}
	if got := h.g.State(); got.Error != "open" {
		t.Fatalf("State() = %+v; want error open", got)
	}
}

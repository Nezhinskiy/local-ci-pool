package supervisor

import (
	"strings"
	"testing"
)

// logIndex is the index of the first line of the pool's log containing every
// part, or -1.
func (h *harness) logIndex(parts ...string) int {
	for i, line := range strings.Split(h.logs.String(), "\n") {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			return i
		}
	}
	return -1
}

// The pool acquires the job GitHub offers its scale set (under
// actions/scaleset's Scaler the listener no longer does), and its log records
// the hand-off before the runner starts: the only evidence of what GitHub
// assigned when a job goes missing.
func TestPoolLogsTheAcquisitionBeforeTheRunner(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
	h.eventually("the runner started record", func() bool { return h.logged(`msg="runner started"`) })
	if n := h.actions.Count("POST acquirejobs " + alphaSet + " 1"); n != 1 {
		t.Fatalf("%d acquisitions of the job, want 1", n)
	}
	acquiring := h.logIndex(`msg="Acquiring jobs"`, "count=1")
	acquired := h.logIndex(`msg="Jobs acquired"`, "count=1")
	started := h.logIndex(`msg="runner started"`)
	if acquiring < 0 || acquired < 0 || !(acquiring < acquired && acquired < started) {
		t.Fatalf("want Acquiring jobs, Jobs acquired, then runner started; lines %d %d %d", acquiring, acquired, started)
	}
}

// listenerPollMsg is the text the real v0.5.0 listener writes before each poll:
// the line is dropped, and the listener's other Info line is kept.
func TestRealListenerPollLineIsDropped(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	polls := h.actions.Polls(alphaSet)
	h.eventually("3 more polls", func() bool { return h.actions.Polls(alphaSet) >= polls+3 })
	if h.logged("Getting next message") {
		t.Fatal("the listener's poll line reached the log")
	}
	if !h.logged(`msg="Handling initial session statistics"`) {
		t.Fatal("the listener's initial statistics line is missing")
	}
}

// A failed acquisition is logged and the message handled: the listener keeps
// running, GitHub reissues the request, and the next offer is acquired.
func TestAcquireFailureIsLoggedAndTheListenerKeepsRunning(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.FailNextAcquires(1)
	h.actions.Assign(alphaSet, 1)
	h.eventually("the acquire failure logged", func() bool {
		return h.logIndex("level=WARN", "acquiring jobs failed", "count=1") >= 0
	})
	if n := h.actions.Count("POST acquirejobs " + alphaSet + " 1 failed"); n != 1 {
		t.Fatalf("%d failed acquisitions, want 1", n)
	}
	h.actions.Assign(alphaSet, 1)
	h.eventually("the next offer acquired", func() bool { return h.actions.Count("POST acquirejobs "+alphaSet+" 1") == 1 })
	if h.logged("the listener stopped; restarting it") {
		t.Fatal("an acquire failure restarted the listener")
	}
	if !h.healthy(alphaRepo) {
		t.Fatal("alpha is unhealthy after an acquire failure")
	}
}

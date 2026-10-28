package supervisor

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// refused reports whether the project is listed unhealthy because its
// repository is no longer private.
func (h *harness) refused(repo string) bool {
	p, ok := h.health(repo)
	return ok && !p.Healthy && strings.Contains(p.Reason, "no longer private")
}

// A repository that turns public between two jobs gets no runner for the
// second: no JIT configuration is minted, the project drains with its own
// reason, logged at WARN, and stays listed with that reason after the drain.
func TestVisibilityFlipBetweenTwoJobs(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.cfg.DiscoverEvery = 24 * time.Hour // no discovery pass sees it
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	img := h.imageOf("alpha")

	h.actions.Assign(alphaSet, 1)
	h.eventually("the first job's runner", func() bool { return len(h.docker.runners(img)) == 1 })
	r := h.docker.runners(img)[0]
	h.actions.Started(alphaSet, r)
	h.actions.Completed(alphaSet, r)
	h.actions.Assign(alphaSet, -1)
	h.delivered(alphaSet)
	h.docker.exit(r)

	h.gh.edit(func() { h.gh.repos[alphaRepo].private = false })
	time.Sleep(120 * time.Millisecond) // two minutes of pool time: the cached answer is stale
	h.actions.Assign(alphaSet, 1)
	h.eventually("the project refused", func() bool { return h.refused(alphaRepo) })
	h.eventually("the scale set deleted", func() bool { return h.actions.ScaleSet(alphaSet) == nil })
	if n := h.actions.Count("JIT " + alphaSet); n != 1 {
		t.Fatalf("%d JIT configurations minted, want 1: the second job's runner was minted for a public repository", n)
	}
	if !h.logged(`level=WARN msg="the repository is no longer private`) {
		t.Fatal("the flip is not logged at WARN")
	}
	time.Sleep(50 * time.Millisecond)
	if !h.refused(alphaRepo) {
		p, _ := h.health(alphaRepo)
		t.Fatalf("after the drain alpha is %+v, want it listed with the refusal", p)
	}
}

// The visibility answer is cached for visibleFor: within it, jobs cost no
// GitHub call; past it, one call per period while jobs are assigned.
func TestVisibilityAnswerIsCachedForItsTTL(t *testing.T) {
	t.Run("within the TTL", func(t *testing.T) {
		old := visibleFor
		visibleFor = time.Hour // 3.6 s of wall time
		t.Cleanup(func() { visibleFor = old })
		h := newHarness(t, 2, 1000, "alpha")
		h.cfg.DiscoverEvery = 24 * time.Hour
		h.start()
		h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
		before := h.gh.repoAsks()
		h.actions.Assign(alphaSet, 2)
		img := h.imageOf("alpha")
		h.eventually("two runners", func() bool { return len(h.docker.runners(img)) == 2 })
		time.Sleep(50 * time.Millisecond) // listener calls with jobs assigned
		// The recheck before the session opened answered for the hour.
		if n := h.gh.repoAsks() - before; n != 0 {
			t.Fatalf("%d visibility calls within the TTL, want 0", n)
		}
	})
	t.Run("past the TTL", func(t *testing.T) {
		h := newHarness(t, 2, 1000, "alpha")
		h.cfg.DiscoverEvery = 24 * time.Hour
		h.start()
		h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
		h.actions.Assign(alphaSet, 1)
		img := h.imageOf("alpha")
		h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
		before, began := h.gh.repoAsks(), h.clock.Now()
		time.Sleep(300 * time.Millisecond) // five minutes of pool time
		n, periods := h.gh.repoAsks()-before, int(h.clock.Now().Sub(began)/visibleFor)
		if n < 1 || n > periods+1 {
			t.Fatalf("%d visibility calls over %d periods with a job assigned, want 1 to %d", n, periods, periods+1)
		}
	})
}

// A visibility call that fails refuses the runner and is never read as
// private; the project is not drained, and the next call that succeeds lets
// the runner start.
func TestFailedVisibilityCheckRefusesTheRunner(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.cfg.DiscoverEvery = 24 * time.Hour
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	time.Sleep(120 * time.Millisecond) // the session's answer is stale
	h.gh.edit(func() { h.gh.repos[alphaRepo].recheckErr = errors.New("GitHub answered 502") })
	h.actions.Assign(alphaSet, 1)
	h.delivered(alphaSet)
	time.Sleep(100 * time.Millisecond) // listener calls with the job assigned
	if n := h.actions.Count("JIT " + alphaSet); n != 0 {
		t.Fatalf("%d JIT configurations minted while the visibility call failed, want 0", n)
	}
	if !h.logged("GitHub answered 502") {
		t.Fatal("the failed check is not logged")
	}
	if h.refused(alphaRepo) || h.actions.ScaleSet(alphaSet) == nil {
		t.Fatal("a failed visibility call drained the project")
	}
	h.gh.edit(func() { h.gh.repos[alphaRepo].recheckErr = nil })
	img := h.imageOf("alpha")
	h.eventually("the runner once the call succeeds", func() bool { return len(h.docker.runners(img)) == 1 })
}

// A flip removes the project's running containers at once, a busy one
// included, instead of waiting out the drain bound.
func TestVisibilityFlipRemovesContainersAtOnce(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.cfg.DiscoverEvery = 24 * time.Hour
	h.cfg.Drain = 24 * time.Hour // 86 s of wall time: only a removal at once is seen
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	img := h.imageOf("alpha")
	h.actions.Assign(alphaSet, 1)
	h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
	r := h.docker.runners(img)[0]
	h.actions.Started(alphaSet, r)
	h.delivered(alphaSet)

	h.gh.edit(func() { h.gh.repos[alphaRepo].private = false })
	time.Sleep(120 * time.Millisecond) // the cached answer is stale
	h.actions.Assign(alphaSet, 1)      // a fork's pull request targets the label
	h.eventually("the busy runner's container removed", func() bool { return !h.docker.has(r) })
	h.eventually("the project refused", func() bool { return h.refused(alphaRepo) })
	if n := h.actions.Count("JIT " + alphaSet); n != 1 {
		t.Fatalf("%d JIT configurations minted, want 1", n)
	}
}

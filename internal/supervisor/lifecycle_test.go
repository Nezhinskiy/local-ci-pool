package supervisor

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// A drain tells the service the scale set takes no more jobs: the listener's
// next message request carries a maximum capacity of 0.
func TestDrainSetsTheListenerCapacityToZero(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.eventually("capacity 2 advertised", func() bool { return h.actions.Capacity(alphaSet) == "2" })
	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
	r := h.docker.runners(img)[0]
	h.actions.Started(alphaSet, r)
	h.delivered(alphaSet)

	h.cancel()
	h.eventually("capacity 0 while the drain waits", func() bool { return h.actions.Capacity(alphaSet) == "0" })
	if !h.running() {
		t.Fatal("Run returned while a runner still ran")
	}
	h.actions.Completed(alphaSet, r)
	h.docker.exit(r)
	if err := h.wait(defaultWait); err != nil {
		t.Fatal(err)
	}
}

// Two projects whose clients were built with the same token both see it
// refused. That is one refusal of one token: it is counted once, the token
// is read again once, and both projects serve with the new one.
func TestAStaleTokenRefusalCountsOnce(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha", "beta")
	h.actions.RejectTokens("tok-1")
	h.actions.BarrierRegistrations(2) // both register with tok-1 before either hears 401
	h.start()
	h.eventually("both healthy", func() bool { return h.healthy(alphaRepo) && h.healthy(betaRepo) })
	if n := h.tok.invalidated(); n != 1 {
		t.Fatalf("%d invalidations, want 1", n)
	}
	if !h.running() {
		t.Fatalf("Run returned: %v", *h.result)
	}
}

// A die event lost while Docker's event stream was down is found by the
// reconcile that the resubscription asks for, long before the watchdog's
// next tick (30 s away at wall speed).
func TestEventsReconnectReconciles(t *testing.T) {
	h := newHarness(t, 2, 1, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
	r := h.docker.runners(img)[0]
	h.actions.Started(alphaSet, r)
	h.delivered(alphaSet)
	h.docker.vanish(r)
	h.docker.breakEvents()
	h.eventually("the lost runner's slot released", func() bool { return h.sup.Snapshot().InUse == 0 })
}

// At the drain bound, aborting a start stuck creating its container, removing
// the runners left (whose removal hangs here), closing the session and
// deleting the scale set share one budget: the stop ends within afterBound,
// not one call timeout per runner later.
func TestWorkAfterTheDrainBoundSharesOneBudget(t *testing.T) {
	old := afterBound
	afterBound = 500 * time.Millisecond
	t.Cleanup(func() { afterBound = old })
	h := newHarness(t, 8, 1000, "alpha")
	h.cfg.Drain = time.Minute // 60 ms
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.Assign(alphaSet, 7)
	img := h.imageOf("alpha")
	h.eventually("seven runners", func() bool { return len(h.docker.runners(img)) == 7 })
	for _, r := range h.docker.runners(img) {
		h.actions.Started(alphaSet, r)
	}
	h.delivered(alphaSet)
	release := h.docker.gateCreates(false)
	defer release()
	h.actions.Assign(alphaSet, 1)
	h.eventually("the eighth start stuck creating its container", func() bool { return h.actions.Count("JIT "+alphaSet) == 8 })
	h.docker.set(func(f *fakeDocker) { f.removeHang = true })

	began := time.Now()
	h.cancel()
	if err := h.wait(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("the stop took %v, want about the drain bound plus afterBound", took)
	}
}

// A repository that moved to another owner is drained before a session is
// opened again; a rename that changes only the case is the same repository.
func TestMovedRepoDrainsBeforeReopening(t *testing.T) {
	t.Run("case-only rename", func(t *testing.T) {
		h := newHarness(t, 2, 1000, "alpha")
		h.cfg.DiscoverEvery = 24 * time.Hour
		h.start()
		h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
		h.gh.edit(func() { h.gh.repos[alphaRepo].movedTo = "Example/Alpha" })
		h.actions.FailNextGets(1)
		h.eventually("a second session", func() bool { return h.actions.Count("POST session "+alphaSet) == 2 })
		h.eventually("alpha healthy again", func() bool { return h.healthy(alphaRepo) })
		if h.actions.ScaleSet(alphaSet) == nil {
			t.Fatal("a case-only rename drained the project")
		}
	})
	t.Run("transfer", func(t *testing.T) {
		h := newHarness(t, 2, 1000, "alpha")
		h.cfg.DiscoverEvery = 24 * time.Hour
		h.start()
		h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
		h.gh.edit(func() { h.gh.repos[alphaRepo].movedTo = "someone-else/alpha" })
		h.actions.FailNextGets(1)
		h.eventually("the scale set deleted", func() bool { return h.actions.ScaleSet(alphaSet) == nil })
		if !h.logged("moved") {
			t.Fatal("the reason is not logged")
		}
	})
}

// A scale set found with other labels than its name and the shared label is
// updated to exactly those, never deleted.
func TestExistingScaleSetWithOtherLabelsIsUpdated(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.actions.AddScaleSet(alphaSet, alphaSet, "stale-label")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	got := labelNames(h.actions.ScaleSet(alphaSet))
	slices.Sort(got)
	if want := []string{alphaSet, "alpha-local"}; !slices.Equal(got, want) {
		t.Fatalf("labels %v, want %v", got, want)
	}
	if n := h.actions.Count("PATCH scaleset " + alphaSet); n != 1 {
		t.Fatalf("%d updates, want 1", n)
	}
	if n := h.actions.Count("DELETE scaleset " + alphaSet); n != 0 {
		t.Fatalf("%d deletes of a scale set held by no session of this process", n)
	}
}

// A first discovery pass that fails is retried after 30 s, not after the
// 10-minute period; once a pass succeeds, the period is the normal one.
func TestFirstFailedDiscoveryIsRetriedSooner(t *testing.T) {
	h := newHarness(t, 2, 100, "alpha") // the period is 6 s, the first retry 300 ms
	h.cfg.DiscoverEvery = 10 * time.Minute
	h.gh.edit(func() { h.gh.listErr = errors.New("github: GET /user/repos: status 502") })
	h.start()
	h.eventually("a failed first listing", func() bool { return h.gh.listings() >= 1 })
	h.gh.edit(func() { h.gh.listErr = nil })
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	n := h.gh.listings()
	time.Sleep(time.Second)
	if got := h.gh.listings(); got != n {
		t.Fatalf("%d listings within a second after a successful pass, want none", got-n)
	}
}

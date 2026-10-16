package supervisor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
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
// next tick (30 s away at wall speed). The assigned count drops before the
// container vanishes, so the freed slot is not taken again at once by a
// replacement runner (which made the in-use count flicker past zero).
func TestEventsReconnectReconciles(t *testing.T) {
	h := newHarness(t, 2, 1, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
	r := h.docker.runners(img)[0]
	h.actions.Started(alphaSet, r)
	h.actions.Assign(alphaSet, -1)
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

// waitOnCtx is an Admit that blocks until its context ends.
func waitOnCtx(ctx context.Context, _ runner.Admission) (runner.Admitted, error) {
	<-ctx.Done()
	return runner.Admitted{}, ctx.Err()
}

// A runner waiting for the budget has no container yet: the watchdog's
// reconcile leaves it alone, and it keeps its slot.
func TestWaitingSurvivesReconcile(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha") // a watchdog reconcile every 30 ms
	b := newFakeBudget(true)
	b.admit = waitOnCtx
	h.budget = b
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.Assign(alphaSet, 1)
	h.eventually("a runner waiting", func() bool { return len(b.admitted()) == 1 })
	time.Sleep(100 * time.Millisecond) // about three reconciles
	if h.logged("gone without an exit event") {
		t.Fatal("a reconcile released the waiting runner")
	}
	if got := h.sup.Snapshot().InUse; got != 1 {
		t.Fatalf("in use %d, want 1: the waiting runner holds its slot", got)
	}
}

// A drain gives a waiting runner up at once: the pool drains without
// waiting for the drain bound.
func TestDrainWithWaitersLogsDrained(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha") // the drain bound is 2.4 s
	b := newFakeBudget(true)
	b.admit = waitOnCtx
	h.budget = b
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.Assign(alphaSet, 1)
	h.eventually("a runner waiting", func() bool { return len(b.admitted()) == 1 })
	h.cancel()
	if err := h.wait(defaultWait); err != nil {
		t.Fatal(err)
	}
	if !h.logged(`msg=drained`) {
		t.Fatal("drained is not logged")
	}
	if h.logged("the drain bound passed") {
		t.Fatal("the drain waited for its bound with only a waiting runner left")
	}
}

// A waiter whose Admit ignores its context outlives the drain bound. The
// drain joins it after the abort, so the pool reports drained only once it
// has returned.
func TestDrainJoinsWaitersAfterAbort(t *testing.T) {
	const returned = "the fake Admit returned"
	h := newHarness(t, 2, 1000, "alpha")
	h.cfg.Drain = time.Minute // 60 ms
	b := newFakeBudget(true)
	answer := make(chan struct{})
	b.admit = func(_ context.Context, a runner.Admission) (runner.Admitted, error) {
		<-answer
		_, _ = h.logs.Write([]byte(returned + "\n"))
		return b.grant(a.Runner, runner.ModeQueued, 2), nil
	}
	h.budget = b
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.Assign(alphaSet, 1)
	h.eventually("a runner waiting", func() bool { return len(b.admitted()) == 1 })
	h.cancel()
	h.eventually("the drain bound passed", func() bool { return h.logged("the drain bound passed") })
	time.Sleep(200 * time.Millisecond)
	close(answer)
	if err := h.wait(defaultWait); err != nil {
		t.Fatal(err)
	}
	h.eventually("the fake Admit returned", func() bool { return h.logged(returned) })
	logs := h.logs.String()
	bound, ret, drained := strings.Index(logs, "the drain bound passed"), strings.Index(logs, returned), strings.Index(logs, "msg=drained")
	if bound < 0 || ret < 0 || drained < 0 || bound >= ret || ret >= drained {
		t.Fatalf("want the drain bound (%d), then the Admit's return (%d), then drained (%d)", bound, ret, drained)
	}
	if n := b.releases(b.admitted()[0].Runner); n != 1 {
		t.Fatalf("the late grant was released %d times, want once", n)
	}
}

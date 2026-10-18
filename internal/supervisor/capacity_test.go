package supervisor

import (
	"testing"
	"time"

	"github.com/Nezhinskiy/local-ci-pool/internal/fakeactions"
	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
)

// Each listener advertises the slots its project could use: the free slots
// plus the project's own runners. Two projects share two slots; while alpha's
// runners hold both, beta advertises none, so GitHub assigns beta no job the
// Mac has no slot for, and it gets one back when a slot frees.
func TestCapacityFollowsTheFreeSlots(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha", "beta")
	h.start()
	h.eventually("both advertise 2", func() bool {
		return h.actions.Capacity(alphaSet) == "2" && h.actions.Capacity(betaSet) == "2"
	})
	h.actions.Assign(alphaSet, 2)
	img := h.imageOf("alpha")
	h.eventually("two alpha runners", func() bool { return len(h.docker.runners(img)) == 2 })
	h.eventually("beta advertises 0, alpha 2", func() bool {
		return h.actions.Capacity(betaSet) == "0" && h.actions.Capacity(alphaSet) == "2"
	})
	r := h.docker.runners(img)[0]
	h.actions.Started(alphaSet, r)
	h.actions.Completed(alphaSet, r)
	h.delivered(alphaSet)
	h.docker.exit(r)
	h.eventually("beta advertises 1 once a slot frees", func() bool { return h.actions.Capacity(betaSet) == "1" })
	if got := h.actions.Capacity(alphaSet); got != "2" {
		t.Fatalf("alpha advertises %s with one runner and one free slot, want 2", got)
	}
}

// A drain advertises 0, and a slot that frees while it waits does not raise
// it.
func TestCapacityStaysZeroWhileDraining(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.Assign(alphaSet, 2)
	img := h.imageOf("alpha")
	h.eventually("two runners", func() bool { return len(h.docker.runners(img)) == 2 })
	rs := h.docker.runners(img)
	for _, r := range rs {
		h.actions.Started(alphaSet, r)
	}
	h.delivered(alphaSet)
	h.cancel()
	h.eventually("capacity 0 while the drain waits", func() bool { return h.actions.Capacity(alphaSet) == "0" })
	h.actions.Completed(alphaSet, rs[0])
	h.docker.exit(rs[0])
	time.Sleep(10 * fakeactions.DefaultPoll)
	if got := h.actions.Capacity(alphaSet); got != "0" {
		t.Fatalf("a draining listener advertises %s after a slot freed, want 0", got)
	}
	h.actions.Completed(alphaSet, rs[1])
	h.docker.exit(rs[1])
	if err := h.wait(defaultWait); err != nil {
		t.Fatal(err)
	}
}

// A survivor's job counts among its project's assigned jobs, and its slot is
// the project's: alpha advertises it, beta does not.
func TestCapacityCountsTheProjectsSurvivors(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha", "beta")
	labels := instanceLabels("main", survivorA)
	labels[runner.LabelProject] = "alpha"
	h.docker.add(survivorA, labels)
	h.start()
	h.eventually("alpha advertises 2, beta 1", func() bool {
		return h.actions.Capacity(alphaSet) == "2" && h.actions.Capacity(betaSet) == "1"
	})
	h.docker.exit(survivorA)
	h.eventually("beta advertises 2 once the survivor exits", func() bool { return h.actions.Capacity(betaSet) == "2" })
}

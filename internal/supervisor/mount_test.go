package supervisor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Nezhinskiy/local-ci-pool/internal/runnermount"
)

// A new runner release that the project's image fails: the project's runners
// stay on the old mount, the project is unhealthy with the reason, and Prune
// keeps the old mount. Once the preflight passes, the project moves to the
// new mount and the old one is pruned.
func TestNewRunnerThatFailsThePreflightIsNotAdopted(t *testing.T) {
	h := newHarness(t, 2, 2000, "alpha") // the hourly release check every 1.8 s
	h.cfg.DiscoverEvery = 10 * time.Minute
	h.runnerV = []string{"2.338.0", "2.339.0"}
	h.setRunnerErr("2.339.0", errors.New("Runner.Listener: version GLIBC_2.38 not found"))
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.eventually("alpha held back on the old runner", func() bool {
		a, _ := h.health(alphaRepo)
		return !a.Healthy && strings.Contains(a.Reason, "runner 2.339.0 failed the preflight") &&
			strings.Contains(a.Reason, "stay on runner 2.338.0") && strings.Contains(a.Reason, "GLIBC_2.38")
	})
	h.eventually("both runner mounts kept", func() bool { return h.lastPrune() == "2.338.0,2.339.0" })

	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
	r := h.docker.runners(img)[0]
	if got := h.docker.mountOf(r); got != runnermount.Ref("2.338.0") {
		t.Fatalf("runner mount %q, want the old release the image passed with", got)
	}

	h.setRunnerErr("2.339.0", nil)
	h.eventually("alpha healthy on the new runner", func() bool { return h.healthy(alphaRepo) })
	h.eventually("the old mount pruned", func() bool { return h.lastPrune() == "2.339.0" })
	h.docker.exit(r)
	h.eventually("the next runner on the new mount", func() bool {
		rs := h.docker.runners(img)
		return len(rs) == 1 && h.docker.mountOf(rs[0]) == runnermount.Ref("2.339.0")
	})
}

// Runners that exit before any job, three in a row, mark the project
// unhealthy; a JobStarted that arrives late for one of them clears it.
func TestRunnersExitingWithoutAJobMarkTheProjectUnhealthy(t *testing.T) {
	h := newHarness(t, 4, 1000, "alpha")
	h.cfg.DiscoverEvery = 24 * time.Hour // no refresh clears it
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.Assign(alphaSet, 3)
	img := h.imageOf("alpha")
	h.eventually("three runners", func() bool { return len(h.docker.runners(img)) == 3 })
	rs := h.docker.runners(img)
	for _, r := range rs[:2] {
		h.docker.exit(r)
	}
	h.eventually("two exits released", func() bool { return !h.docker.has(rs[0]) && !h.docker.has(rs[1]) && h.sup.Snapshot().InUse == 3 })
	time.Sleep(50 * time.Millisecond)
	if !h.healthy(alphaRepo) {
		t.Fatal("unhealthy after two exits, want three")
	}
	h.docker.exit(rs[2])
	h.eventually("alpha unhealthy", func() bool {
		a, _ := h.health(alphaRepo)
		return !a.Healthy && strings.Contains(a.Reason, "3 runners in a row went away without starting a job")
	})
	// JobStarted reaches the listener late; a short job's runner may exit first.
	h.actions.Started(alphaSet, rs[0])
	h.eventually("alpha healthy after the late job report", func() bool { return h.healthy(alphaRepo) })
}

// A start that fails after its runner was registered, as with a job image
// deleted under the pool, counts the same way.
func TestFailedStartsMarkTheProjectUnhealthy(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.cfg.DiscoverEvery = 24 * time.Hour
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.docker.set(func(f *fakeDocker) { f.createErr = errors.New("No such image: example.invalid/alpha") })
	h.actions.Assign(alphaSet, 1)
	h.eventually("alpha unhealthy", func() bool {
		a, _ := h.health(alphaRepo)
		return !a.Healthy && strings.Contains(a.Reason, "in a row went away without starting a job")
	})
	if n := h.actions.Count("JIT " + alphaSet); n < 3 {
		t.Fatalf("%d runners minted, want at least 3 before the project is unhealthy", n)
	}
}

// With runners exiting before any job, the next image refresh preflights the
// image again: a failure names the cause, and a pass makes the project healthy.
func TestNoJobExitsClearOnAPassedPreflight(t *testing.T) {
	h := newHarness(t, 4, 1000, "alpha") // a refresh every 50 ms
	img := h.imageOf("alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.setCheckErr(img, errors.New("exec: bash: not found"))
	h.actions.Assign(alphaSet, 3)
	h.eventually("three runners", func() bool { return len(h.docker.runners(img)) == 3 })
	for _, r := range h.docker.runners(img) {
		h.docker.exit(r)
	}
	h.eventually("alpha unhealthy with the preflight's reason", func() bool {
		a, _ := h.health(alphaRepo)
		return !a.Healthy && strings.Contains(a.Reason, "bash: not found")
	})
	h.setCheckErr(img, nil)
	h.eventually("alpha healthy after a passed preflight", func() bool { return h.healthy(alphaRepo) })
}

// A refresh whose changed image fails the preflight keeps the old image for
// the next runners and marks the project unhealthy (so the heartbeat stops)
// until a refresh passes.
func TestFailedRefreshKeepsTheOldImageAndIsUnhealthy(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	old := h.imageOf("alpha")
	next := "example.invalid/alpha@sha256:" + strings.Repeat("f", 64)
	h.setCheckErr(next, errors.New("image has no CA bundle"))
	h.setImage("alpha", next)
	h.eventually("alpha unhealthy", func() bool {
		a, _ := h.health(alphaRepo)
		return !a.Healthy && strings.Contains(a.Reason, "refreshing the image") && strings.Contains(a.Reason, "no CA bundle")
	})
	if a, _ := h.health(alphaRepo); a.Image != old {
		t.Fatalf("image %q, want the old one kept", a.Image)
	}
	h.actions.Assign(alphaSet, 1)
	h.eventually("a runner in the old image", func() bool { return len(h.docker.runners(old)) == 1 })
	h.setCheckErr(next, nil)
	h.eventually("alpha healthy on the new image", func() bool {
		a, _ := h.health(alphaRepo)
		return a.Healthy && a.Image == next
	})
}

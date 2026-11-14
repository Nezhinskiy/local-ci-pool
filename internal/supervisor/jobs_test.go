package supervisor

import (
	"testing"
	"time"
)

// Without a settings file, or with one that sets no cap, the slots are the
// machine's own: every one of them takes a job, and the docker VM runs
// beside them.
func TestNoJobsCapKeepsTheSlots(t *testing.T) {
	for name, settings := range map[string]*fakeSettings{"no file": nil, "no cap": {}} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 3, 1000, "alpha")
			h.withDocker("alpha")
			h.settings = settings
			h.start()
			h.eventually("both classes healthy", func() bool { return h.healthy(alphaRepo) && h.dockerHealthy(alphaRepo) })
			h.actions.Assign(alphaSet, 3)
			h.actions.Assign(alphaDockerSet, 1)
			img := h.imageOf("alpha")
			h.eventually("three runners and a docker VM", func() bool {
				return len(h.docker.runners(img)) == 3 && len(h.lima.running()) == 1
			})
			if snap := h.sup.Snapshot(); snap.Slots != 3 || snap.Jobs != (JobsHealth{}) {
				t.Fatalf("slots %d, jobs %+v; want 3 and no cap", snap.Slots, snap.Jobs)
			}
		})
	}
}

// A cap below the slots is the slots: the pool advertises and starts no more,
// even when the service assigns more jobs.
func TestJobsCapLowersTheSlots(t *testing.T) {
	h := newHarness(t, 4, 1000, "alpha")
	h.settings = &fakeSettings{max: 2}
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.eventually("capacity 2 advertised", func() bool { return h.actions.Capacity(alphaSet) == "2" })
	h.actions.IgnoreCapacity(alphaSet)
	h.actions.Assign(alphaSet, 4)
	img := h.imageOf("alpha")
	h.eventually("two runners", func() bool { return len(h.docker.runners(img)) == 2 })
	time.Sleep(100 * time.Millisecond) // a few listener calls and watchdog ticks
	if got := h.docker.runners(img); len(got) != 2 {
		t.Fatalf("%d runners with a cap of 2, want 2", len(got))
	}
	if snap := h.sup.Snapshot(); snap.Slots != 2 || snap.Jobs != (JobsHealth{Max: 2}) {
		t.Fatalf("slots %d, jobs %+v; want 2 and a cap of 2", snap.Slots, snap.Jobs)
	}
}

// The docker VM counts against the cap with the container jobs: beside a VM,
// a cap of 2 leaves one container job, and the VM's end frees its place for a
// second.
func TestJobsCapCountsTheDockerVM(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.withDocker("alpha")
	h.settings = &fakeSettings{max: 2}
	h.start()
	h.eventually("both classes healthy", func() bool { return h.healthy(alphaRepo) && h.dockerHealthy(alphaRepo) })

	h.actions.Assign(alphaDockerSet, 1)
	h.eventually("a docker VM runs", func() bool { return len(h.lima.running()) == 1 })
	vm := h.lima.running()[0]
	h.actions.Started(alphaDockerSet, vm)
	h.eventually("alpha advertises 1 beside the VM", func() bool { return h.actions.Capacity(alphaSet) == "1" })
	h.actions.IgnoreCapacity(alphaSet)
	h.actions.Assign(alphaSet, 2)
	img := h.imageOf("alpha")
	h.eventually("one container job", func() bool { return len(h.docker.runners(img)) == 1 })
	time.Sleep(100 * time.Millisecond)
	if snap := h.sup.Snapshot(); len(h.docker.runners(img)) != 1 || snap.InUse+snap.VMInUse != 2 {
		t.Fatalf("%d container jobs, %d slots and %d VMs in use with a cap of 2; want 1, 1 and 1",
			len(h.docker.runners(img)), snap.InUse, snap.VMInUse)
	}

	h.actions.Completed(alphaDockerSet, vm)
	h.delivered(alphaDockerSet)
	h.lima.finish(vm)
	h.eventually("a second container job once the VM ended", func() bool { return len(h.docker.runners(img)) == 2 })
	if snap := h.sup.Snapshot(); snap.VMInUse != 0 {
		t.Fatalf("%d VMs in use beside two container jobs with a cap of 2", snap.VMInUse)
	}
}

// The cap is read again on every watchdog tick: raising it starts the jobs
// that waited, lowering it stops none, and removing it gives the slots back,
// all without a restart.
func TestJobsCapIsReadAgainWithoutARestart(t *testing.T) {
	h := newHarness(t, 3, 1000, "alpha") // a watchdog tick every 30 ms
	h.settings = &fakeSettings{max: 1}
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.IgnoreCapacity(alphaSet)
	h.actions.Assign(alphaSet, 3)
	img := h.imageOf("alpha")
	h.eventually("one runner", func() bool { return len(h.docker.runners(img)) == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := h.docker.runners(img); len(got) != 1 {
		t.Fatalf("%d runners with a cap of 1, want 1", len(got))
	}

	h.settings.set(3, false)
	h.eventually("three runners once the cap is 3", func() bool { return len(h.docker.runners(img)) == 3 })

	h.settings.set(1, false)
	h.eventually("the cap lowered to 1", func() bool { return h.sup.Snapshot().Slots == 1 })
	if got := h.docker.runners(img); len(got) != 3 {
		t.Fatalf("%d runners after the cap was lowered, want the 3 running", len(got))
	}

	h.settings.set(0, false)
	h.eventually("no cap", func() bool {
		snap := h.sup.Snapshot()
		return snap.Slots == 3 && snap.Jobs == (JobsHealth{})
	})
}

// A settings file the pool cannot use is reported as the class config, and
// its last valid cap stays in force; at start, with none, there is no cap.
func TestInvalidJobsCapIsReported(t *testing.T) {
	h := newHarness(t, 3, 1000, "alpha")
	h.settings = &fakeSettings{invalid: true}
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	if snap := h.sup.Snapshot(); snap.Slots != 3 || snap.Jobs != (JobsHealth{Error: "config"}) {
		t.Fatalf("slots %d, jobs %+v; want 3, no cap and the class config", snap.Slots, snap.Jobs)
	}

	h.settings.set(2, false)
	h.eventually("the cap of 2", func() bool { return h.sup.Snapshot().Jobs == (JobsHealth{Max: 2}) })
	h.settings.set(2, true) // the file turned invalid: config.File keeps its last valid cap
	h.eventually("the class config", func() bool { return h.sup.Snapshot().Jobs == (JobsHealth{Max: 2, Error: "config"}) })
	if snap := h.sup.Snapshot(); snap.Slots != 2 || !h.healthy(alphaRepo) {
		t.Fatalf("slots %d, alpha healthy %v with an invalid file; want the cap of 2 kept and alpha healthy",
			snap.Slots, h.healthy(alphaRepo))
	}
	h.settings.set(2, false)
	h.eventually("the class cleared", func() bool { return h.sup.Snapshot().Jobs == (JobsHealth{Max: 2}) })
}

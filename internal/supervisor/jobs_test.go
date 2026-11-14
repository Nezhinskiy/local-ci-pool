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

// A changed cap is advertised at once, also on an idle pool: GitHub learns the
// new number without waiting for a slot to change.
func TestARaisedCapIsAdvertisedAtOnce(t *testing.T) {
	h := newHarness(t, 3, 1000, "alpha")
	h.settings = &fakeSettings{max: 1}
	h.start()
	h.eventually("capacity 1", func() bool { return h.actions.Capacity(alphaSet) == "1" })
	h.settings.set(3, false)
	h.eventually("capacity 3 once the cap is 3", func() bool { return h.actions.Capacity(alphaSet) == "3" })
}

// A cap removed without a restart gives back every slot of both kinds: the
// docker VM runs beside a full set of container jobs again.
func TestRemovingTheCapRunsTheDockerVMBesideEverySlot(t *testing.T) {
	h := newHarness(t, 3, 1000, "alpha")
	h.withDocker("alpha")
	h.settings = &fakeSettings{max: 1}
	h.start()
	h.eventually("both classes healthy", func() bool { return h.healthy(alphaRepo) && h.dockerHealthy(alphaRepo) })
	h.settings.set(0, false)
	h.eventually("no cap", func() bool { return h.sup.Snapshot().Jobs == (JobsHealth{}) })
	h.actions.Assign(alphaSet, 3)
	h.actions.Assign(alphaDockerSet, 1)
	img := h.imageOf("alpha")
	h.eventually("three runners and a docker VM", func() bool {
		return len(h.docker.runners(img)) == 3 && len(h.lima.running()) == 1
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

// Every container job is held to its share of the Docker VM's CPUs: the CPUs
// shared by the cap, or by the slots without one, so a job that sizes its
// workers by the CPUs it sees does not start one per CPU of the whole VM.
func TestEveryContainerGetsItsShareOfTheCPUs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings *fakeSettings
		want     int64
	}{
		{"a cap of 2 on 8 CPUs", &fakeSettings{max: 2}, 4e9},
		{"no cap: 4 slots on 8 CPUs", nil, 2e9},
		{"a cap above the slots: the slots share them", &fakeSettings{max: 16}, 2e9},
		{"a cap of 3 on 4 slots", &fakeSettings{max: 3}, 2e9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, 4, 1000, "alpha")
			if tc.settings != nil {
				h.settings = tc.settings
			}
			h.start()
			h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
			h.actions.Assign(alphaSet, 1)
			img := h.imageOf("alpha")
			h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
			h.docker.mu.Lock()
			defer h.docker.mu.Unlock()
			for name, c := range h.docker.containers {
				if c.image == img && c.nanoCPUs != tc.want {
					t.Fatalf("%s: NanoCPUs %d, want %d", name, c.nanoCPUs, tc.want)
				}
			}
		})
	}
}

// A job's memory is the Docker VM's, less Docker's own share, divided among the
// jobs that can run at once, and never more than the Mac can hold beside macOS,
// nor less than the 4 GiB every job had before.
func TestJobMemory(t *testing.T) {
	const gib = int64(1) << 30
	for _, tc := range []struct {
		name         string
		docker, host int64
		jobs         int
		wantGiB      int64
	}{
		{"a 48 GiB Mac with a 47 GiB Docker VM and 4 jobs: the Mac's term", 50425344000, 48 * gib, 4, 10},
		{"the same Mac, one job: the Mac's term", 50425344000, 48 * gib, 1, 40},
		{"the same VM on a Mac of unknown memory: the VM's term", 50425344000, 0, 4, 11},
		{"a small VM on a large Mac: the VM's term", 16 * gib, 64 * gib, 2, 7},
		{"a 16 GiB Mac with a 9.7 GiB Docker VM and 2 jobs", 10416644096, 16 * gib, 2, 4},
		{"never below 4 GiB", 8 * gib, 16 * gib, 2, 4},
		{"7.75 GiB rounds to 8, as Slots rounds it", 8321499136, 64 * gib, 1, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := JobMemory(tc.docker, tc.host, tc.jobs); got != tc.wantGiB*gib {
				t.Fatalf("JobMemory(%d, %d, %d) = %d GiB, want %d", tc.docker, tc.host, tc.jobs, got/gib, tc.wantGiB)
			}
		})
	}
}

// Every container job gets that memory as its limit, divided by the cap (never
// more than the slots) and bounded by the Mac's memory the pool read at start.
func TestEveryContainerGetsItsShareOfTheMemory(t *testing.T) {
	const gib = int64(1) << 30
	for _, tc := range []struct {
		name     string
		hostMem  int64
		settings *fakeSettings
		wantGiB  int64
	}{
		{"a 30 GiB VM, a cap of 2, a 64 GiB Mac", 64 * gib, &fakeSettings{max: 2}, 14},
		{"the same, a 24 GiB Mac", 24 * gib, &fakeSettings{max: 2}, 8},
		{"no cap: 4 slots, the Mac unknown", 0, nil, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, 4, 1000, "alpha")
			h.docker.info.MemTotal = 30 * gib
			h.hostMem = tc.hostMem
			if tc.settings != nil {
				h.settings = tc.settings
			}
			h.start()
			h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
			h.actions.Assign(alphaSet, 1)
			img := h.imageOf("alpha")
			h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
			h.docker.mu.Lock()
			defer h.docker.mu.Unlock()
			for name, c := range h.docker.containers {
				if c.image == img && c.memory != tc.wantGiB*gib {
					t.Fatalf("%s: Memory %d GiB, want %d", name, c.memory/gib, tc.wantGiB)
				}
			}
		})
	}
}

package supervisor

import (
	"context"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
)

const survivorA, survivorB = "local-ci-ssssssssssss", "local-ci-tttttttttttt"

// A pool killed while a job ran (a logout, kill -9, launchd's 60 s exit
// timeout) leaves that job's container running. The next start keeps it, and
// it holds the only slot until it dies: a job queued meanwhile waits, and
// starts once the survivor's die event frees the slot. The clock runs at wall
// speed, so the watchdog's reconcile (first tick at 30 s) cannot stand in for
// the die event. The finished containers of the previous run are swept at
// start, and a replayed die event of the survivor releases nothing more.
func TestSurvivorHoldsItsSlotUntilItDies(t *testing.T) {
	h := newHarness(t, 1, 1, "alpha")
	labels := instanceLabels("main", survivorA)
	h.docker.add(survivorA, labels)
	finished := map[string]container.ContainerState{
		"local-ci-cccccccccccc": container.StateCreated,
		"local-ci-dddddddddddd": container.StateDead,
		"local-ci-eeeeeeeeeeee": container.StateExited,
	}
	for id, st := range finished {
		h.docker.addIn(id, instanceLabels("main", id), st)
	}
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	for id := range finished {
		if h.docker.has(id) || !h.logged(`msg="removed an orphaned runner container" runner=`+id) {
			t.Errorf("the finished container %s was not swept at start", id)
		}
	}
	if !h.docker.has(survivorA) || slices.Contains(h.docker.removedIDs(), survivorA) {
		t.Fatal("the running container of the previous run was removed")
	}
	if got := h.sup.Snapshot().InUse; got != 1 {
		t.Fatalf("in use %d, want 1: the survivor holds a slot", got)
	}

	// The survivor names no project, so its slot is no project's: alpha
	// advertises no capacity, and the service holds the job back.
	h.eventually("alpha advertises 0", func() bool { return h.actions.Capacity(alphaSet) == "0" })
	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	time.Sleep(3 * 20 * time.Millisecond) // a few of the listener's nil-message calls
	if got := h.docker.runners(img); len(got) != 0 {
		t.Fatalf("started %v while the survivor held the only slot", got)
	}

	h.docker.exit(survivorA)
	h.eventually("the waiting job's runner started", func() bool { return len(h.docker.runners(img)) == 1 })
	if got := h.sup.Snapshot().InUse; got != 1 {
		t.Fatalf("in use %d, want 1 (the new runner)", got)
	}
	h.docker.dieAgain(survivorA, labels)
	time.Sleep(100 * time.Millisecond)
	if got := h.sup.Snapshot().InUse; got != 1 {
		t.Fatalf("in use %d after a replayed die event, want 1: the survivor's slot was released twice", got)
	}
}

// A survivor's job still counts in its scale set's assigned jobs. The scaler
// counts the project's survivors against that count, so a free slot gets no
// idle runner for the job the survivor already runs; a second job gets one.
// Once the survivor has died and its job has left the count, the one runner
// is exactly what the scale set needs.
func TestSurvivorsJobStartsNoIdleRunner(t *testing.T) {
	h := newHarness(t, 2, 1, "alpha")
	h.docker.add(survivorA, labelled(survivorA, "alpha"))
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	img := h.imageOf("alpha")

	h.actions.Assign(alphaSet, 1) // the survivor's own job
	h.delivered(alphaSet)
	time.Sleep(3 * 20 * time.Millisecond) // a few of the listener's nil-message calls
	if got := h.docker.runners(img); len(got) != 0 {
		t.Fatalf("started %v for the job the survivor runs, with a slot free", got)
	}

	h.actions.Assign(alphaSet, 1)
	h.delivered(alphaSet)
	h.eventually("the second job's runner started", func() bool { return len(h.docker.runners(img)) == 1 })

	h.docker.exit(survivorA)
	h.actions.Assign(alphaSet, -1)
	h.delivered(alphaSet)
	time.Sleep(3 * 20 * time.Millisecond)
	if got := h.docker.runners(img); len(got) != 1 {
		t.Fatalf("runners %v after the survivor's job left, want the one already started", got)
	}
}

// Only the project's own survivors count against its jobs: a survivor of
// another project, or of an older pool (no project label), leaves alpha's
// job a runner.
func TestOtherSurvivorsLeaveTheJobARunner(t *testing.T) {
	h := newHarness(t, 3, 1, "alpha", "beta")
	h.docker.add(survivorA, labelled(survivorA, "beta"))
	h.docker.add(survivorB, instanceLabels("main", survivorB))
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	img := h.imageOf("alpha")

	h.actions.Assign(alphaSet, 1)
	h.delivered(alphaSet)
	h.eventually("alpha's runner started", func() bool { return len(h.docker.runners(img)) == 1 })
}

// The reconcile leaves a live survivor alone, and releases the slot of one
// whose container vanished without a die event (a Docker restart).
func TestReconcileSparesAndReleasesSurvivors(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha") // a watchdog reconcile every 30 ms
	h.docker.add(survivorA, instanceLabels("main", survivorA))
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	time.Sleep(300 * time.Millisecond) // about ten reconciles
	if !h.docker.has(survivorA) {
		t.Fatal("a reconcile removed the running survivor")
	}
	if got := h.sup.Snapshot().InUse; got != 1 {
		t.Fatalf("in use %d, want 1", got)
	}
	h.docker.vanish(survivorA)
	h.eventually("the vanished survivor's slot released", func() bool { return h.sup.Snapshot().InUse == 0 })
	// The reconcile logs the release after it gave the slot back.
	h.eventually("the release logged", func() bool {
		return h.logged("a runner left from the previous run is gone without an exit event; releasing its slot")
	})
}

// More survivors than slots: each is kept, a slot is held for as many as fit,
// and the rest are logged; the reconcile removes none of them.
func TestMoreSurvivorsThanSlotsAreAllKept(t *testing.T) {
	h := newHarness(t, 1, 1000, "alpha")
	h.docker.add(survivorA, instanceLabels("main", survivorA))
	h.docker.add(survivorB, instanceLabels("main", survivorB))
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	time.Sleep(300 * time.Millisecond) // about ten reconciles
	if !h.docker.has(survivorA) || !h.docker.has(survivorB) {
		t.Fatalf("a survivor was removed: %v", h.docker.removedIDs())
	}
	if got := h.sup.Snapshot().InUse; got != 1 {
		t.Fatalf("in use %d, want the one slot", got)
	}
	if !h.logged("every slot is held") {
		t.Error("the survivor without a slot is not logged")
	}
	h.docker.exit(survivorA)
	h.docker.exit(survivorB)
	h.eventually("both survivors released", func() bool { return h.sup.Snapshot().InUse == 0 })
}

// slotless returns the survivor the pool logged as running without a slot.
func (h *harness) slotless(names ...string) string {
	h.t.Helper()
	for _, line := range strings.Split(h.logs.String(), "\n") {
		if !strings.Contains(line, "every slot is held") {
			continue
		}
		for _, n := range names {
			if strings.Contains(line, "runner="+n) {
				return n
			}
		}
	}
	h.t.Fatalf("no survivor was logged as running without a slot")
	return ""
}

// A survivor kept without a slot gives none back when it exits: the slot
// held for the other survivor stays held until that one exits.
func TestSlotlessSurvivorReleasesNothing(t *testing.T) {
	h := newHarness(t, 1, 1, "alpha")
	h.docker.add(survivorA, instanceLabels("main", survivorA))
	h.docker.add(survivorB, instanceLabels("main", survivorB))
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	without := h.slotless(survivorA, survivorB)
	with := survivorA
	if without == survivorA {
		with = survivorB
	}
	if got := h.sup.Snapshot().InUse; got != 1 {
		t.Fatalf("in use %d, want 1", got)
	}
	h.docker.exit(without)
	h.eventually("the slotless survivor's exit handled", func() bool {
		return h.logged(`msg="a runner left from the previous run exited" runner=` + without)
	})
	if got := h.sup.Snapshot().InUse; got != 1 {
		t.Fatalf("in use %d after the slotless survivor exited, want 1: it gave back a slot it never held", got)
	}
	h.docker.exit(with)
	h.eventually("the slotted survivor's slot released", func() bool { return h.sup.Snapshot().InUse == 0 })
}

// The start line carries the pid, which the installer matches to tell this
// process's "stopping" line from an earlier run's.
func TestStartLogCarriesThePid(t *testing.T) {
	h := newHarness(t, 1, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	want := regexp.MustCompile(`msg="pool starting" pid=` + strconv.Itoa(os.Getpid()) + ` `)
	if !want.MatchString(h.logs.String()) {
		t.Fatalf("no start line with this process's pid:\n%s", h.logs.String())
	}
}

// labelled is a survivor's labels: the instance and runner name, plus the
// project label when not empty.
func labelled(name, project string) map[string]string {
	l := instanceLabels("main", name)
	if project != "" {
		l[runner.LabelProject] = project
	}
	return l
}

// The container labels a runner's start passes carry the project's identity.
func TestProjectStartLabelsTheContainer(t *testing.T) {
	h := newHarness(t, 1, 1, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.sup.mu.Lock()
	pr := h.sup.projects[alphaRepo]
	h.sup.mu.Unlock()
	const name = "local-ci-zzzzzzzzzzzz"
	if err := pr.start(context.Background(), name, "jit"); err != nil {
		t.Fatal(err)
	}
	if got := h.docker.labelsOf(name); got[runner.LabelProject] != "alpha" {
		t.Fatalf("labels %v, want %s=alpha", got, runner.LabelProject)
	}
}

// A survivor whose container vanished without a die event is released by the
// reconcile.
func TestSurvivorReconcileReleases(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha") // a watchdog reconcile every 30 ms
	h.docker.add(survivorA, labelled(survivorA, "alpha"))
	h.start()
	h.eventually("the survivor kept", func() bool { _, ok := h.survivorOf(survivorA); return ok })
	h.docker.vanish(survivorA)
	h.eventually("the vanished survivor's slot released", func() bool { return h.sup.Snapshot().InUse == 0 })
	if _, ok := h.survivorOf(survivorA); ok {
		t.Fatal("the vanished survivor's entry is left")
	}
}

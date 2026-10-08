package supervisor

import (
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

const survivorA, survivorB = "local-ci-ssssssssssss", "local-ci-tttttttttttt"

// A pool killed while a job ran (a logout, kill -9, launchd's 60 s exit
// timeout) leaves that job's container running. The next start keeps it, and
// it holds the only slot until it dies: a job assigned meanwhile waits, and
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

	h.actions.Assign(alphaSet, 1)
	h.delivered(alphaSet)
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
	if !h.logged("gone without an exit event; releasing its slot") {
		t.Error("the release is not logged")
	}
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

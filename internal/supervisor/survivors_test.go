package supervisor

import (
	"context"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
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

// labelled is a survivor's labels: the instance and runner name, plus the
// project and need labels when not empty.
func labelled(name, project, need string) map[string]string {
	l := instanceLabels("main", name)
	if project != "" {
		l[runner.LabelProject] = project
	}
	if need != "" {
		l[runner.LabelNeed] = need
	}
	return l
}

// The container labels a runner's start passes carry the project's identity
// and the need the gate admitted it for.
func TestProjectStartLabelsTheContainer(t *testing.T) {
	h := newHarness(t, 1, 1, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.sup.mu.Lock()
	pr := h.sup.projects[alphaRepo]
	h.sup.mu.Unlock()
	const name = "local-ci-zzzzzzzzzzzz"
	if err := pr.start(context.Background(), name, "jit", 3); err != nil {
		t.Fatal(err)
	}
	got := h.docker.labelsOf(name)
	if got[runner.LabelProject] != "alpha" || got[runner.LabelNeed] != "3" {
		t.Fatalf("labels %v, want %s=alpha and %s=3", got, runner.LabelProject, runner.LabelNeed)
	}
}

// With the budget on, a survivor is adopted under the project and the need
// its labels name, and its die event releases the adoption once.
func TestSurvivorAdoptedUnderItsProject(t *testing.T) {
	h := newHarness(t, 2, 1, "alpha")
	b := newFakeBudget(true)
	h.budget = b
	h.docker.add(survivorA, labelled(survivorA, "alpha", "2"))
	h.start()
	h.eventually("the survivor adopted", func() bool { return len(b.adopted()) == 1 })
	if got, want := b.adopted()[0], (runner.Admission{Runner: survivorA, Project: "alpha", Need: 2}); got != want {
		t.Fatalf("Adopt got %+v, want %+v", got, want)
	}
	h.eventually("the adoption recorded", func() bool {
		e, ok := h.survivorOf(survivorA)
		return ok && e.mode == runner.ModeAdopted
	})
	h.docker.exit(survivorA)
	h.eventually("the adoption released", func() bool { return b.releases(survivorA) == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := b.releases(survivorA); n != 1 {
		t.Fatalf("released %d times, want once", n)
	}
}

// A survivor of an older pool has no labels: it is adopted with no project
// (the gate labels it as the pool's own) and need 0. A need label is passed
// through as the number it names, 0 when it names none: the gate takes a
// need outside 1..256 as the configured one.
func TestUnlabelledSurvivorAdoptedAsPool(t *testing.T) {
	const survivorC, survivorD = "local-ci-uuuuuuuuuuuu", "local-ci-vvvvvvvvvvvv"
	h := newHarness(t, 4, 1, "alpha")
	b := newFakeBudget(true)
	h.budget = b
	h.docker.add(survivorA, labelled(survivorA, "", ""))
	h.docker.add(survivorB, labelled(survivorB, "alpha", "300"))
	h.docker.add(survivorC, labelled(survivorC, "alpha", "-1"))
	h.docker.add(survivorD, labelled(survivorD, "alpha", "x"))
	h.start()
	h.eventually("every survivor adopted", func() bool { return len(b.adopted()) == 4 })
	want := []runner.Admission{
		{Runner: survivorA, Project: "", Need: 0},
		{Runner: survivorB, Project: "alpha", Need: 300}, // the gate asks the configured cpu
		{Runner: survivorC, Project: "alpha", Need: -1},  // likewise
		{Runner: survivorD, Project: "alpha", Need: 0},   // not a number
	}
	if got := b.adopted(); !slices.Equal(got, want) {
		t.Fatalf("Adopt got %+v, want %+v", got, want)
	}
}

// A survivor kept without a slot is adopted all the same.
func TestSurvivorWithoutSlotStillAdopted(t *testing.T) {
	h := newHarness(t, 1, 1, "alpha")
	b := newFakeBudget(true)
	h.budget = b
	h.docker.add(survivorA, labelled(survivorA, "alpha", "2"))
	h.docker.add(survivorB, labelled(survivorB, "alpha", "2"))
	h.start()
	h.eventually("both survivors adopted", func() bool { return len(b.adopted()) == 2 })
	if got := h.sup.Snapshot().InUse; got != 1 {
		t.Fatalf("in use %d, want the one slot", got)
	}
}

// Adoption starts only once the exit watcher has subscribed to die events, so
// a survivor that dies during its adoption is not missed.
func TestAdoptionStartsAfterSubscribe(t *testing.T) {
	h := newHarness(t, 2, 1, "alpha")
	b := newFakeBudget(true)
	var mu sync.Mutex
	var seen []int
	b.adopt = func(_ context.Context, a runner.Admission) runner.Admitted {
		mu.Lock()
		seen = append(seen, h.docker.subscribed())
		mu.Unlock()
		return b.grant(a.Runner, runner.ModeAdopted, a.Need)
	}
	h.budget = b
	h.docker.add(survivorA, labelled(survivorA, "alpha", "2"))
	h.docker.add(survivorB, labelled(survivorB, "alpha", "2"))
	h.start()
	h.eventually("both survivors adopted", func() bool { return len(b.adopted()) == 2 })
	mu.Lock()
	defer mu.Unlock()
	for i, n := range seen {
		if n < 1 {
			t.Errorf("adoption %d ran with %d subscriptions, want at least 1", i, n)
		}
	}
}

// A survivor that dies while its Adopt is in progress is released by its die
// event; the Release the Adopt returns afterwards is called once, and no
// entry is left behind.
func TestSurvivorDiesDuringAdopt(t *testing.T) {
	h := newHarness(t, 2, 1, "alpha")
	b := newFakeBudget(true)
	entered, proceed := make(chan struct{}), make(chan struct{})
	b.adopt = func(_ context.Context, a runner.Admission) runner.Admitted {
		close(entered)
		<-proceed
		return b.grant(a.Runner, runner.ModeAdopted, a.Need)
	}
	h.budget = b
	h.docker.add(survivorA, labelled(survivorA, "alpha", "2"))
	h.start()
	select {
	case <-entered:
	case <-time.After(defaultWait):
		t.Fatal("no Adopt")
	}
	h.docker.exit(survivorA)
	h.eventually("the survivor released by its die event", func() bool {
		_, ok := h.survivorOf(survivorA)
		return !ok
	})
	close(proceed)
	h.eventually("the late adoption released", func() bool { return b.releases(survivorA) == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := b.releases(survivorA); n != 1 {
		t.Fatalf("released %d times, want once", n)
	}
	if _, ok := h.survivorOf(survivorA); ok {
		t.Fatal("an entry was recorded again for the dead survivor")
	}
	if got := h.sup.Snapshot().InUse; got != 0 {
		t.Fatalf("in use %d, want 0", got)
	}
}

// An adoption that fails open leaves the survivor running with its entry and
// its slot; its die event later releases both, once.
func TestSurvivorAdoptFailsOpenRunsOn(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha") // a watchdog reconcile every 30 ms
	b := newFakeBudget(true)
	b.adopt = func(_ context.Context, a runner.Admission) runner.Admitted {
		return b.grant(a.Runner, runner.ModeFailOpen, a.Need)
	}
	h.budget = b
	h.docker.add(survivorA, labelled(survivorA, "alpha", "2"))
	h.start()
	h.eventually("the survivor adopted", func() bool { return len(b.adopted()) == 1 })
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	time.Sleep(300 * time.Millisecond) // about ten reconciles
	if !h.docker.has(survivorA) {
		t.Fatal("the survivor whose adoption failed open was removed")
	}
	e, ok := h.survivorOf(survivorA)
	if !ok || e.mode != runner.ModeFailOpen || !e.held {
		t.Fatalf("survivor entry %+v (recorded %v), want mode fail_open holding its slot", e, ok)
	}
	if got := h.sup.Snapshot().InUse; got != 1 {
		t.Fatalf("in use %d, want 1", got)
	}
	h.docker.exit(survivorA)
	h.eventually("the survivor's slot released", func() bool { return h.sup.Snapshot().InUse == 0 })
	h.eventually("the adoption released", func() bool { return b.releases(survivorA) == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := b.releases(survivorA); n != 1 {
		t.Fatalf("released %d times, want once", n)
	}
}

// An adopted survivor whose container vanished without a die event is
// released by the reconcile, adoption included.
func TestSurvivorReconcileReleases(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha") // a watchdog reconcile every 30 ms
	b := newFakeBudget(true)
	h.budget = b
	h.docker.add(survivorA, labelled(survivorA, "alpha", "2"))
	h.start()
	h.eventually("the adoption recorded", func() bool {
		e, ok := h.survivorOf(survivorA)
		return ok && e.mode == runner.ModeAdopted
	})
	h.docker.vanish(survivorA)
	h.eventually("the vanished survivor's slot released", func() bool { return h.sup.Snapshot().InUse == 0 })
	h.eventually("the adoption released", func() bool { return b.releases(survivorA) == 1 })
}

// With the budget off, or none wired, survivors are kept as before and never
// adopted.
func TestBudgetOffAdoptsNothing(t *testing.T) {
	for name, b := range map[string]*fakeBudget{"disabled": newFakeBudget(false), "none": nil} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 2, 1, "alpha")
			if b != nil {
				h.budget = b
			}
			h.docker.add(survivorA, labelled(survivorA, "alpha", "2"))
			h.start()
			h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
			h.eventually("the exit watcher subscribed", func() bool { return h.docker.subscribed() >= 1 })
			time.Sleep(100 * time.Millisecond)
			if b != nil {
				if n := len(b.adopted()); n != 0 {
					t.Fatalf("%d Adopt calls with the budget off, want none", n)
				}
			}
			if e, ok := h.survivorOf(survivorA); !ok || !e.held || e.mode != "" {
				t.Fatalf("survivor entry %+v (recorded %v), want kept with its slot and no adoption", e, ok)
			}
			if !h.running() {
				t.Fatalf("Run returned: %v", *h.result)
			}
			h.docker.exit(survivorA)
			h.eventually("the survivor's slot released", func() bool { return h.sup.Snapshot().InUse == 0 })
		})
	}
}

// An unlabelled survivor of an older pool counts under no project, not even
// one whose identity is the pool's own name; a survivor labelled with that
// project counts under it.
func TestUnlabelledSurvivorCountsUnderNoProject(t *testing.T) {
	const repo = "example/local-ci-pool"
	h := newHarness(t, 3, 1, "local-ci-pool")
	b := newFakeBudget(true)
	h.budget = b
	h.docker.add(survivorA, labelled(survivorA, "", ""))
	h.docker.add(survivorB, labelled(survivorB, "local-ci-pool", "2"))
	h.start()
	h.eventually("local-ci-pool healthy", func() bool { return h.healthy(repo) })
	h.eventually("both survivors adopted", func() bool {
		a, okA := h.survivorOf(survivorA)
		b, okB := h.survivorOf(survivorB)
		return okA && okB && a.mode == runner.ModeAdopted && b.mode == runner.ModeAdopted
	})
	got, _ := h.health(repo)
	if want := (AdmittedHealth{Adopted: 1}); got.Admitted != want {
		t.Fatalf("local-ci-pool admitted %+v, want %+v: only the labelled survivor counts", got.Admitted, want)
	}
}

// Once the pool's life ends, the survivors not adopted yet are not adopted:
// an Adopt on an ended context would only fail open.
func TestAdoptionStopsAtShutdown(t *testing.T) {
	h := newHarness(t, 2, 1, "alpha")
	b := newFakeBudget(true)
	b.adopt = func(ctx context.Context, a runner.Admission) runner.Admitted {
		<-ctx.Done() // the first Adopt returns only once the pool's life ended
		return b.grant(a.Runner, runner.ModeFailOpen, a.Need)
	}
	h.budget = b
	h.docker.add(survivorA, labelled(survivorA, "alpha", "2"))
	h.docker.add(survivorB, labelled(survivorB, "alpha", "2"))
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.eventually("an Adopt in progress", func() bool { return len(b.adopted()) == 1 })
	h.cancel()
	if err := h.wait(defaultWait); err != nil {
		t.Fatal(err)
	}
	if got := b.adopted(); len(got) != 1 {
		t.Fatalf("Adopt got %+v after the pool's life ended, want only the first", got)
	}
}

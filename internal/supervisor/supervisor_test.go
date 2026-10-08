package supervisor

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"

	"github.com/Nezhinskiy/local-ci-pool/internal/ghauth"
	"github.com/Nezhinskiy/local-ci-pool/internal/github"
	"github.com/Nezhinskiy/local-ci-pool/internal/machine"
	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
	"github.com/Nezhinskiy/local-ci-pool/internal/runnermount"
)

// Two slots, both held by alpha; beta wants one and gets none. When an alpha
// container exits, beta's runner starts on its listener's next nil-message
// call, with no other trigger. No JobCompleted is ever delivered, so the
// release can only come from the exit; the clock runs at wall speed, so the
// watchdog's reconcile (first tick at 30 s) cannot stand in for the exit
// event within the 10 s wait.
func TestBlockedProjectStartsAfterOtherFinishes(t *testing.T) {
	h := newHarness(t, 2, 1, "alpha", "beta") // real time: the watchdog's first tick is 30 s away
	h.start()
	h.eventually("both projects healthy", func() bool { return h.healthy(alphaRepo) && h.healthy(betaRepo) })

	h.actions.Assign(alphaSet, 2)
	alphaImg, betaImg := h.imageOf("alpha"), h.imageOf("beta")
	h.eventually("alpha runs two runners", func() bool { return len(h.docker.runners(alphaImg)) == 2 })
	alpha := h.docker.runners(alphaImg)
	// Both jobs start, so alpha keeps both runners whatever its count says.
	h.actions.Started(alphaSet, alpha[0])
	h.actions.Started(alphaSet, alpha[1])
	h.delivered(alphaSet)

	h.actions.Assign(betaSet, 1)
	h.delivered(betaSet)
	time.Sleep(3 * 20 * time.Millisecond) // a few of beta's nil-message calls
	if got := h.docker.runners(betaImg); len(got) != 0 {
		t.Fatalf("beta started %v with every slot held by alpha", got)
	}

	// alpha's first job is over: its statistics drop, and its container exits.
	// The JobCompleted is dropped.
	h.actions.Assign(alphaSet, -1)
	h.delivered(alphaSet)
	exited := time.Now()
	h.docker.exit(alpha[0])
	deadline := exited.Add(10 * time.Second)
	for len(h.docker.runners(betaImg)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("beta did not start after an alpha container exited")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if s := h.sup.Snapshot(); s.InUse != 2 {
		t.Fatalf("in use %d, want 2", s.InUse)
	}
}

// A drain with one busy runner deletes nothing until its container exits,
// then closes the session, then deletes the scale set. After SetMaxRunners(0)
// the fake delivers nothing, as the service does at zero capacity.
func TestDrainWaitsForContainerExitThenDeletes(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
	r := h.docker.runners(img)[0]
	h.actions.Started(alphaSet, r)
	h.delivered(alphaSet)

	h.cancel()                         // SIGTERM
	time.Sleep(200 * time.Millisecond) // 200 s of pool time: many drain polls
	h.actions.Completed(alphaSet, r)   // the job ends; its container has not exited
	time.Sleep(100 * time.Millisecond)
	if n := h.actions.Count("DELETE session " + alphaSet); n != 0 {
		t.Fatalf("the session was closed while a runner container still ran")
	}
	if n := h.actions.Count("DELETE scaleset " + alphaSet); n != 0 {
		t.Fatalf("the scale set was deleted while a runner container still ran")
	}
	if !h.running() {
		t.Fatal("Run returned while a runner container still ran")
	}

	h.docker.exit(r)
	if err := h.wait(defaultWait); err != nil {
		t.Fatalf("Run after a drain = %v, want nil", err)
	}
	log := h.actions.Log()
	closed := slices.Index(log, "DELETE session "+alphaSet)
	deleted := slices.Index(log, "DELETE scaleset "+alphaSet)
	if closed < 0 || deleted < 0 || closed > deleted {
		t.Fatalf("session close at %d, scale set delete at %d; want the close first, then the delete", closed, deleted)
	}
	if h.actions.ScaleSet(alphaSet) != nil {
		t.Fatal("the scale set is still there")
	}
}

func TestDeleteRetriesJobStillRunning(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.DeleteStillRunning(2)
	h.cancel()
	if err := h.wait(defaultWait); err != nil {
		t.Fatal(err)
	}
	if n := h.actions.Count("DELETE scaleset " + alphaSet); n != 3 {
		t.Fatalf("%d deletes, want 3 (two refused while a job ran)", n)
	}
	if h.actions.ScaleSet(alphaSet) != nil {
		t.Fatal("the scale set is still there")
	}
}

func TestDeleteLeavesTheScaleSetAtTheDrainBound(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.cfg.Drain = time.Minute
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.DeleteStillRunning(-1)
	h.cancel()
	if err := h.wait(defaultWait); err != nil {
		t.Fatal(err)
	}
	if n := h.actions.Count("DELETE scaleset " + alphaSet); n < 2 {
		t.Fatalf("%d deletes, want retries until the bound", n)
	}
	if h.actions.ScaleSet(alphaSet) == nil {
		t.Fatal("the scale set is gone; it should be left for the next start")
	}
	if !h.logged("leaving the scale set for the next start") {
		t.Fatal("leaving the scale set is not logged")
	}
}

// After a kill -9 the old session is stale for a while: the fake answers the
// measured 409 twice, then opens. Only this instance's finished containers are
// swept, and the stale JobStarted/JobCompleted of the previous process (one
// with no runner name) are ignored.
func TestRestartAfterKillReopensSession(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	id := h.actions.AddScaleSet(alphaSet, alphaSet, "alpha-local")
	h.docker.addIn("local-ci-aaaaaaaaaaaa", instanceLabels("main", "local-ci-aaaaaaaaaaaa"), container.StateExited)
	h.docker.addIn("local-ci-bbbbbbbbbbbb", instanceLabels(probeInst, "local-ci-bbbbbbbbbbbb"), container.StateExited)
	h.docker.add("unrelated", map[string]string{"other": "x"})
	h.actions.SessionConflicts(2)
	h.actions.StaleOnOpen()
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	if n := h.actions.Count("POST session " + alphaSet); n != 3 {
		t.Fatalf("%d session creates, want 3 (two 409s, then open)", n)
	}
	if got := h.actions.ScaleSet(alphaSet); got == nil || got.ID != id {
		t.Fatalf("scale set %+v, want the existing one, id %d", got, id)
	}
	if got := h.docker.removedIDs(); !slices.Equal(got, []string{"local-ci-aaaaaaaaaaaa"}) {
		t.Fatalf("swept %v, want only this instance's container", got)
	}
	h.delivered(alphaSet)
	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a runner after the stale message", func() bool { return len(h.docker.runners(img)) == 1 })
	if slices.Contains(h.docker.removedIDs(), "") {
		t.Fatal("a container remove with an empty name")
	}
	if !h.healthy(alphaRepo) {
		t.Fatal("the stale message took the project down")
	}
}

// A 409 that lasts past the measured stale-session window is another holder.
// The pool never deletes a scale set it holds no session on: the service
// deletes it even under another holder's session.
func TestOtherOwnerConflictIsTerminal(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.actions.SessionConflicts(-1)
	h.start()
	err := h.wait(defaultWait)
	if !errors.Is(err, ErrTerminal) || !strings.Contains(err.Error(), "scale set "+alphaSet+" has an active session elsewhere") {
		t.Fatalf("Run = %v, want a terminal active-session error", err)
	}
	if n := h.actions.Count("POST session " + alphaSet); n < 3 {
		t.Fatalf("%d session creates, want retries across the window", n)
	}
	if n := h.actions.Count("DELETE scaleset " + alphaSet); n != 0 {
		t.Fatal("deleted a scale set this process never held a session on")
	}
}

// A drain while the session never opened (another holder within the window)
// leaves the scale set alone.
func TestDrainWithoutSessionNeverDeletes(t *testing.T) {
	h := newHarness(t, 2, 10, "alpha")
	h.actions.SessionConflicts(-1)
	h.start()
	h.eventually("two session attempts", func() bool { return h.actions.Count("POST session "+alphaSet) >= 2 })
	h.cancel()
	if err := h.wait(defaultWait); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if n := h.actions.Count("DELETE scaleset " + alphaSet); n != 0 {
		t.Fatal("deleted a scale set this process never held a session on")
	}
	if n := h.actions.Count("DELETE session " + alphaSet); n != 0 {
		t.Fatal("closed a session this process never opened")
	}
	if h.actions.ScaleSet(alphaSet) == nil {
		t.Fatal("the scale set is gone")
	}
}

func TestGetScaleSetNilCreates(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	ss := h.actions.ScaleSet(alphaSet)
	if ss == nil {
		t.Fatal("no scale set was created")
	}
	if !ss.RunnerSetting.DisableUpdate || ss.RunnerGroupID != 1 {
		t.Fatalf("scale set %+v, want runner updates disabled in the default group", ss)
	}
	if n := h.actions.Count("POST scaleset " + alphaSet); n != 1 {
		t.Fatalf("%d creates, want 1", n)
	}
	if !h.actions.SessionOpen(alphaSet) {
		t.Fatal("no session on the new scale set")
	}
}

func TestExistingScaleSetReused(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.actions.AddScaleSet(alphaSet, alphaSet, "alpha-local")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	if n := h.actions.Count("POST scaleset " + alphaSet); n != 0 {
		t.Fatalf("%d creates of an existing scale set", n)
	}
	if n := h.actions.Count("PATCH scaleset " + alphaSet); n != 0 {
		t.Fatalf("%d updates of a scale set whose labels are right", n)
	}
	if !h.actions.SessionOpen(alphaSet) {
		t.Fatal("no session on the existing scale set")
	}
}

// Every scale set carries the shared label <identity>-local (MODE=shared).
func TestSharedLabelAlwaysPresent(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha", "beta")
	h.start()
	h.eventually("both healthy", func() bool { return h.healthy(alphaRepo) && h.healthy(betaRepo) })
	for set, want := range map[string][]string{alphaSet: {alphaSet, "alpha-local"}, betaSet: {betaSet, "beta-local"}} {
		if got := labelNames(h.actions.ScaleSet(set)); !slices.Equal(got, want) {
			t.Errorf("%s labels %v, want %v", set, got, want)
		}
	}
}

func TestFailedProjectDoesNotStopOthers(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha", "beta")
	h.setCheckErr(h.imageOf("alpha"), errors.New("preflight failed: NO_CA: the image has no CA bundle"))
	h.start()
	h.eventually("beta healthy", func() bool { return h.healthy(betaRepo) })
	h.eventually("alpha reported", func() bool { _, ok := h.health(alphaRepo); return ok && h.logged("cannot be served") })
	a, _ := h.health(alphaRepo)
	if a.Healthy || !strings.Contains(a.Reason, "NO_CA") || !strings.Contains(a.Reason, "with runner 2.338.0") {
		t.Fatalf("alpha %+v, want unhealthy with the preflight's reason", a)
	}
	if h.actions.ScaleSet(alphaSet) != nil {
		t.Fatal("a scale set for a project whose preflight failed")
	}
	if !h.actions.SessionOpen(betaSet) {
		t.Fatal("beta has no session")
	}
	h.sup.mu.Lock()
	failed := h.sup.projects[alphaRepo]
	h.sup.mu.Unlock()
	// The next discovery pass tries alpha again.
	h.setCheckErr(h.imageOf("alpha"), nil)
	h.eventually("alpha healthy after the image is fixed", func() bool { return h.healthy(alphaRepo) })
	if failed.ctx.Err() == nil {
		t.Fatal("the replaced failed project's context was never cancelled")
	}
}

func TestListenerDeathRestartsAndMarksUnhealthy(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	release := h.actions.GateSessions()
	defer release()
	h.actions.FailNextGets(1)
	h.eventually("alpha unhealthy", func() bool { return !h.healthy(alphaRepo) })
	a, _ := h.health(alphaRepo)
	if !strings.Contains(a.Reason, "listener stopped") {
		t.Fatalf("reason %q, want the listener's failure", a.Reason)
	}
	h.eventually("the dead listener's session closed", func() bool { return h.actions.Count("DELETE session "+alphaSet) == 1 })
	release()
	h.eventually("alpha healthy again", func() bool { return h.healthy(alphaRepo) })
	if n := h.actions.Count("POST session " + alphaSet); n != 2 {
		t.Fatalf("%d session creates, want 2", n)
	}
}

func TestListingErrorKeepsCurrentSet(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.gh.edit(func() { h.gh.listErr = errors.New("github: GET /user/repos: status 502") })
	n := h.gh.listings()
	h.eventually("two failed listings", func() bool { return h.gh.listings() >= n+2 })
	if !h.healthy(alphaRepo) || h.actions.Count("DELETE session "+alphaSet) != 0 {
		t.Fatal("a failed listing changed the served set")
	}
}

// A repository whose re-check or marker read failed is kept exactly as it
// is; only a repository positively gone drains.
func TestTransientRepoErrorKeepsProject(t *testing.T) {
	for name, breakAlpha := range map[string]func(r *fakeRepo){
		"marker read fails": func(r *fakeRepo) { r.readErr = errors.New("github: status 502") },
		"re-check fails":    func(r *fakeRepo) { r.recheckErr = errors.New("github: status 502") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 2, 1000, "alpha", "beta")
			h.start()
			h.eventually("both healthy", func() bool { return h.healthy(alphaRepo) && h.healthy(betaRepo) })
			h.gh.edit(func() {
				breakAlpha(h.gh.repos[alphaRepo])
				h.gh.repos[betaRepo].marker = "" // beta removed its marker
			})
			h.eventually("beta drained", func() bool { return h.actions.ScaleSet(betaSet) == nil })
			n := h.gh.listings()
			h.eventually("two more passes", func() bool { return h.gh.listings() >= n+2 })
			if !h.healthy(alphaRepo) || h.actions.Count("DELETE session "+alphaSet) != 0 {
				t.Fatal("a transient error on alpha changed it")
			}
			if _, ok := h.health(betaRepo); ok {
				t.Fatal("beta is still listed after its drain")
			}
		})
	}
}

// Docker's resources changed: the pool exits with a non-terminal error
// within the next watchdog tick, so launchd restarts it with new slots.
func TestDockerResourceChangeExits(t *testing.T) {
	h := newHarness(t, 2, 100, "alpha") // the watchdog ticks every 300 ms
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.docker.setInfo(func(i *system.Info) { i.MemTotal = 12 << 30 }, nil)
	err := h.wait(10 * time.Second)
	if err == nil || errors.Is(err, ErrTerminal) || !strings.Contains(err.Error(), "docker changed") {
		t.Fatalf("Run = %v, want a non-terminal Docker-changed error", err)
	}
	if h.actions.SessionOpen(alphaSet) {
		t.Fatal("the session was left open")
	}
	if h.actions.ScaleSet(alphaSet) == nil {
		t.Fatal("the scale set was deleted; a restart reuses it")
	}
}

func TestUnansweredPingsExit(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.docker.setInfo(nil, errors.New("connection refused"))
	err := h.wait(defaultWait)
	if err == nil || errors.Is(err, ErrTerminal) || !strings.Contains(err.Error(), "3 pings") {
		t.Fatalf("Run = %v, want a non-terminal error after three failed pings", err)
	}
}

func TestSecondInstanceIsTerminal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	h := newHarness(t, 2, 1000, "alpha")
	h.cfg.HealthAddr = ln.Addr().String()
	h.start()
	err = h.wait(defaultWait)
	if !errors.Is(err, ErrTerminal) || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("Run = %v, want terminal already running", err)
	}
	if h.detects != 0 {
		t.Fatal("the machine was probed before the lock was taken")
	}
}

func TestTooLittleDockerIsTerminal(t *testing.T) {
	h := newHarness(t, 0, 1000, "alpha")
	h.start()
	err := h.wait(defaultWait)
	if !errors.Is(err, ErrTerminal) || !strings.Contains(err.Error(), "6 GB") {
		t.Fatalf("Run = %v, want a terminal error naming the memory one slot needs", err)
	}
}

func TestUnauthorizedTwiceIsTerminal(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.actions.RejectTokens()
	h.start()
	err := h.wait(defaultWait)
	if !errors.Is(err, ErrTerminal) || !strings.Contains(err.Error(), "gh login rejected") {
		t.Fatalf("Run = %v, want terminal gh login rejected", err)
	}
	if n := h.tok.invalidated(); n != 1 {
		t.Fatalf("%d invalidations, want 1", n)
	}
	if got := h.actions.Tokens(); !slices.Equal(got, []string{"tok-1", "tok-2"}) {
		t.Fatalf("tokens tried %v, want the cached one, then one read again", got)
	}
}

func TestUnauthorizedOnceRebuildsTheClient(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.actions.RejectTokens("tok-1")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	if n := h.tok.invalidated(); n != 1 {
		t.Fatalf("%d invalidations, want 1", n)
	}
	if got := h.actions.Tokens(); len(got) < 2 || got[len(got)-1] != "tok-2" {
		t.Fatalf("tokens %v, want the client rebuilt with tok-2", got)
	}
}

func TestProbeModeIsolation(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha", "beta")
	h.cfg.Probe, h.cfg.OnlyRepo, h.cfg.MarkerRef = true, alphaRepo, "probe-branch"
	h.docker.addIn("local-ci-aaaaaaaaaaaa", instanceLabels("main", "local-ci-aaaaaaaaaaaa"), container.StateExited)
	h.docker.addIn("local-ci-bbbbbbbbbbbb", instanceLabels(probeInst, "local-ci-bbbbbbbbbbbb"), container.StateExited)
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	if !h.sup.Snapshot().Probe {
		t.Fatal("Snapshot().Probe is false")
	}
	ss := h.actions.ScaleSet("probe-" + alphaSet)
	if ss == nil {
		t.Fatal("no probe- scale set")
	}
	if got, want := labelNames(ss), []string{"probe-" + alphaSet, "probe-alpha-local"}; !slices.Equal(got, want) {
		t.Fatalf("labels %v, want %v", got, want)
	}
	if h.actions.ScaleSet(alphaSet) != nil || h.actions.ScaleSet("probe-"+betaSet) != nil || h.actions.ScaleSet(betaSet) != nil {
		t.Fatal("probe mode touched a scale set other than the probe repository's")
	}
	if got := h.docker.removedIDs(); !slices.Equal(got, []string{"local-ci-bbbbbbbbbbbb"}) {
		t.Fatalf("swept %v, want only the probe instance's container", got)
	}
	h.actions.Assign("probe-"+alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a probe runner", func() bool { return len(h.docker.runners(img)) == 1 })
	if got := h.docker.labelsOf(h.docker.runners(img)[0])[runner.LabelInstance]; got != probeInst {
		t.Fatalf("runner instance label %q, want %q", got, probeInst)
	}
}

func TestProbeModeRefusesAPublicRepository(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.gh.repos[alphaRepo].private = false
	h.cfg.Probe, h.cfg.OnlyRepo = true, alphaRepo
	h.start()
	h.eventually("two passes", func() bool { return h.gh.listings() >= 2 })
	if len(h.sup.Snapshot().Projects) != 0 || h.actions.ScaleSet("probe-"+alphaSet) != nil {
		t.Fatal("a public repository was served in probe mode")
	}
}

// caffeinate keys on slots in use: JobStarted reaches the listener late.
func TestCaffeinateOnlyWhileSlotsInUse(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	time.Sleep(20 * time.Millisecond)
	if starts, _, _ := h.caff.state(); starts != 0 {
		t.Fatal("caffeinate ran with no slot in use")
	}
	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
	h.eventually("caffeinate running", func() bool { _, _, running := h.caff.state(); return running })
	r := h.docker.runners(img)[0]
	h.actions.Started(alphaSet, r)
	h.actions.Completed(alphaSet, r)
	h.delivered(alphaSet)
	h.docker.exit(r)
	h.eventually("caffeinate stopped", func() bool { _, _, running := h.caff.state(); return !running })
	if starts, stops, _ := h.caff.state(); starts != 1 || stops != 1 {
		t.Fatalf("caffeinate started %d and stopped %d times, want once each", starts, stops)
	}
}

// A die event lost across a Docker restart: the reconcile finds the held
// runner's container gone and releases its slot, and removes a labelled
// container no runner holds.
func TestReconcileReleasesLostExit(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
	r := h.docker.runners(img)[0]
	h.actions.Started(alphaSet, r)
	h.actions.Completed(alphaSet, r)
	h.delivered(alphaSet)
	h.docker.add("local-ci-cccccccccccc", instanceLabels("main", "local-ci-cccccccccccc"))
	h.docker.vanish(r)
	h.eventually("the lost runner's slot released", func() bool { return h.sup.Snapshot().InUse == 0 })
	h.eventually("the unheld container removed", func() bool { return !h.docker.has("local-ci-cccccccccccc") })
}

// A discovered image change is preflighted and used by the next runners.
func TestChangedImageUsedByNextStarts(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	next := "example.invalid/alpha@sha256:" + strings.Repeat("e", 64)
	h.setImage("alpha", next)
	h.eventually("the new image adopted", func() bool { a, _ := h.health(alphaRepo); return a.Image == next })
	h.actions.Assign(alphaSet, 1)
	h.eventually("a runner in the new image", func() bool { return len(h.docker.runners(next)) == 1 })
	h.mu.Lock()
	checked := slices.Contains(h.checked, next)
	h.mu.Unlock()
	if !checked {
		t.Fatal("the new image was not preflighted")
	}
}

// A new runner release, checked hourly, becomes the mount of the next runners.
func TestNewRunnerReleaseUsedByNextStarts(t *testing.T) {
	h := newHarness(t, 2, 10000, "alpha") // an hour is 360 ms
	h.cfg.DiscoverEvery = 10 * time.Minute
	h.runnerV = []string{"2.338.0", "2.339.0"}
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.eventually("the hourly release check", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.mounts >= 2 })
	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
	if got := h.docker.mountOf(h.docker.runners(img)[0]); got != runnermount.Ref("2.339.0") {
		t.Fatalf("runner mount %q, want the new release", got)
	}
}

// ---- fix round 1 ----

// The runner mount is the first GitHub call: a 401 there has already been
// retried with a token read again by the GitHub client, so it is terminal at
// once, not after the start-up retry window. The clock runs at wall speed:
// that window would be two real minutes.
func TestStartupWithRejectedTokenIsTerminal(t *testing.T) {
	h := newHarness(t, 2, 1, "alpha")
	h.mountErr = fmt.Errorf("finding the latest runner release: %w", &github.StatusError{Method: "GET", Path: "/repos/actions/runner/releases/latest", Status: 401})
	h.start()
	err := h.wait(10 * time.Second)
	if !errors.Is(err, ErrTerminal) || !strings.Contains(err.Error(), "gh login rejected") {
		t.Fatalf("Run = %v, want terminal gh login rejected", err)
	}
}

// Right after boot gh may not produce a token yet: a failed read is retried.
func TestStartupRetriesAFailedTokenRead(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.tok.firstErr = fmt.Errorf("%w: gh auth token exited with status 1", ghauth.ErrNoToken)
	h.tok.failFirst = 2
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	if n := h.tok.readCount(); n < 3 {
		t.Fatalf("%d token reads, want two failures and a success", n)
	}
}

// A token read failing for longer than the two-minute window is terminal.
func TestStartupWithoutTokenIsTerminal(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.tok.err = fmt.Errorf("%w: gh auth token exited with status 1", ghauth.ErrNoToken)
	h.start()
	began := h.clock.Now()
	err := h.wait(defaultWait)
	if !errors.Is(err, ErrTerminal) || !strings.Contains(err.Error(), "gh logged out") {
		t.Fatalf("Run = %v, want terminal gh logged out", err)
	}
	if waited := h.clock.Now().Sub(began); waited < 2*time.Minute || h.tok.readCount() < 3 {
		t.Fatalf("terminal after %s and %d reads, want retries for two minutes", waited, h.tok.readCount())
	}
	if h.detects != 0 {
		t.Fatal("Docker was probed before the login was checked")
	}
}

func TestDiscoveryUnauthorizedIsTerminal(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.gh.edit(func() {
		h.gh.listErr = fmt.Errorf("listing: %w", &github.StatusError{Method: "GET", Path: "/user/repos", Status: 401})
	})
	err := h.wait(defaultWait)
	if !errors.Is(err, ErrTerminal) || !strings.Contains(err.Error(), "gh login rejected") {
		t.Fatalf("Run = %v, want terminal gh login rejected", err)
	}
}

// A refusal drops the cached token; a token that then cannot be read means gh
// was logged out.
func TestTokenLostAfterRefusalIsTerminal(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.actions.RejectTokens()
	h.tok.err, h.tok.errAfter = fmt.Errorf("%w: gh auth token exited with status 1", ghauth.ErrNoToken), true
	h.start()
	err := h.wait(defaultWait)
	if !errors.Is(err, ErrTerminal) || !strings.Contains(err.Error(), "gh logged out") {
		t.Fatalf("Run = %v, want terminal gh logged out", err)
	}
}

func TestBadHealthAddressIsTerminal(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.cfg.HealthAddr = "127.0.0.1:99999"
	h.start()
	if err := h.wait(defaultWait); !errors.Is(err, ErrTerminal) {
		t.Fatalf("Run = %v, want terminal", err)
	}
}

func TestInvalidHostNameIsTerminal(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.detectErr = fmt.Errorf("deriving the machine name: %w: %q", machine.ErrHostName, "---")
	h.start()
	if err := h.wait(defaultWait); !errors.Is(err, ErrTerminal) {
		t.Fatalf("Run = %v, want terminal", err)
	}
}

// A drain that lands while a dead listener's session is closed (the restart
// waits out its 5 s backoff) still deletes the scale set this process held:
// it opens a session once more to prove the set is its own. The clock runs at
// wall speed so the backoff outlasts the drain.
func TestDrainDuringListenerRestartStillDeletes(t *testing.T) {
	h := newHarness(t, 2, 1, "alpha")
	h.cfg.DiscoverEvery = 50 * time.Millisecond
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.FailNextGets(1)
	h.eventually("the dead listener's session closed", func() bool { return h.actions.Count("DELETE session "+alphaSet) == 1 })
	h.gh.edit(func() { h.gh.repos[alphaRepo].marker = "" })
	h.eventually("the scale set deleted", func() bool { return h.actions.ScaleSet(alphaSet) == nil })
	if n := h.actions.Count("POST session " + alphaSet); n != 2 {
		t.Fatalf("%d session creates, want 2: the first, and the drain's", n)
	}
}

// Once a session create was refused, a drain with no open session leaves the
// scale set, even when a session could be opened again by then: someone else
// held it. The clock runs at wall speed: the restart backs off 5 s, is refused
// once, then waits 3 s, and the drain lands in that wait.
func TestDrainAfterARefusedSessionLeavesTheScaleSet(t *testing.T) {
	h := newHarness(t, 2, 1, "alpha")
	h.cfg.DiscoverEvery = 50 * time.Millisecond
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.SessionConflicts(1)
	h.actions.FailNextGets(1)
	deadline := time.Now().Add(10 * time.Second)
	for h.actions.Count("POST session "+alphaSet) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no refused reopen")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.gh.edit(func() { h.gh.repos[alphaRepo].marker = "" })
	h.eventually("the drain ended", func() bool { return h.logged("no session held; leaving the scale set") })
	if h.actions.ScaleSet(alphaSet) == nil || h.actions.Count("DELETE scaleset "+alphaSet) != 0 {
		t.Fatal("deleted a scale set whose session was refused")
	}
}

// Visibility is re-checked before every session open: a repository that
// turned public is drained instead of served again.
func TestNowPublicRepoDrainsBeforeReopening(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.cfg.DiscoverEvery = 24 * time.Hour // no discovery pass sees it
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.gh.edit(func() { h.gh.repos[alphaRepo].private = false })
	h.actions.FailNextGets(1)
	h.eventually("the scale set deleted", func() bool { return h.actions.ScaleSet(alphaSet) == nil })
	if !h.logged("no longer private") {
		t.Fatal("the reason is not logged")
	}
	h.eventually("the project removed", func() bool { _, ok := h.health(alphaRepo); return !ok })
}

// A scale set deleted under the pool is got-or-created again, and the scaler
// mints for the new one.
func TestDeletedScaleSetIsRecreated(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	old := h.actions.ScaleSet(alphaSet).ID
	h.actions.RemoveScaleSet(alphaSet)
	h.eventually("a new scale set", func() bool { ss := h.actions.ScaleSet(alphaSet); return ss != nil && ss.ID != old })
	h.eventually("alpha healthy again", func() bool { return h.healthy(alphaRepo) && h.actions.SessionOpen(alphaSet) })
	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a runner on the new scale set", func() bool { return len(h.docker.runners(img)) == 1 })
}

// The reconcile spares a runner whose container is still being created: one
// held as Starting before the listing, and one reserved during the listing.
func TestReconcileSparesStartingRunners(t *testing.T) {
	t.Run("starting before the listing", func(t *testing.T) {
		h := newHarness(t, 2, 1000, "alpha")
		h.start()
		h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
		release := h.docker.gateCreates(false)
		defer release()
		h.actions.Assign(alphaSet, 1)
		h.eventually("the JIT minted", func() bool { return h.actions.Count("JIT "+alphaSet) == 1 })
		time.Sleep(200 * time.Millisecond) // about six reconciles
		if n := h.sup.Snapshot().InUse; n != 1 {
			t.Fatalf("in use %d while the runner's container is being created, want 1", n)
		}
		release()
		img := h.imageOf("alpha")
		h.eventually("the runner runs", func() bool { return len(h.docker.runners(img)) == 1 })
		time.Sleep(100 * time.Millisecond)
		if n := h.sup.Snapshot().InUse; n != 1 || len(h.docker.runners(img)) != 1 {
			t.Fatalf("in use %d, running %v; want the started runner kept", n, h.docker.runners(img))
		}
	})
	t.Run("reserved during the listing", func(t *testing.T) {
		h := newHarness(t, 2, 1000, "alpha")
		h.start()
		h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
		img := h.imageOf("alpha")
		release := h.docker.gateCreates(true)
		defer release()
		hooked := make(chan struct{})
		h.docker.onNextList(func() {
			defer close(hooked)
			h.actions.Assign(alphaSet, 1)
			deadline := time.Now().Add(defaultWait)
			for len(h.docker.created(img)) == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
		})
		<-hooked
		time.Sleep(100 * time.Millisecond)
		created := h.docker.created(img)
		if len(created) != 1 {
			t.Fatalf("containers %v; want the one being created kept", created)
		}
		release()
		h.eventually("the runner runs", func() bool { return len(h.docker.runners(img)) == 1 })
	})
}

// At the drain bound the runners still held are removed and, best effort,
// unregistered.
func TestDrainBoundRemovesAndUnregistersLeftRunners(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.cfg.Drain = time.Minute
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
	r := h.docker.runners(img)[0]
	h.actions.Started(alphaSet, r)
	h.delivered(alphaSet)
	h.cancel()
	if err := h.wait(defaultWait); err != nil {
		t.Fatal(err)
	}
	if h.docker.has(r) {
		t.Fatal("the runner left at the bound still has its container")
	}
	if h.actions.Count("DELETE runner "+r) == 0 {
		t.Fatal("the runner left at the bound was not unregistered")
	}
}

// A login lost anywhere outside the supervisor (the heartbeat's variable
// write) ends the run the way the supervisor's own GitHub calls do: terminal.
// Any other error leaves the run alone.
func TestFailOnLoginLossEndsRunTerminal(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })

	if h.sup.FailOnLoginLoss(errors.New("a transient failure")) {
		t.Fatal("a transient error was taken for a lost login")
	}
	if !h.running() {
		t.Fatal("Run returned for a transient error")
	}
	if !h.sup.FailOnLoginLoss(fmt.Errorf("setting a variable: %w", github.ErrUnauthorized)) {
		t.Fatal("a rejected token was not taken for a lost login")
	}
	err := h.wait(defaultWait)
	if !errors.Is(err, ErrTerminal) || !strings.Contains(err.Error(), "gh login rejected") {
		t.Fatalf("Run = %v, want terminal gh login rejected", err)
	}
}

func TestFailOnLoginLossForUnreadableToken(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })

	if !h.sup.FailOnLoginLoss(fmt.Errorf("%w: gh auth token exited with status 1", ghauth.ErrNoToken)) {
		t.Fatal("an unreadable token was not taken for a lost login")
	}
	err := h.wait(defaultWait)
	if !errors.Is(err, ErrTerminal) || !strings.Contains(err.Error(), "gh logged out") {
		t.Fatalf("Run = %v, want terminal gh logged out", err)
	}
}

func TestDefaultsAreWhatCmdAndPlistAssume(t *testing.T) {
	s := New(Config{}, Deps{})
	if s.cfg.Drain != 40*time.Minute || DefaultDrain != 40*time.Minute {
		t.Errorf("drain = %v, want 40m", s.cfg.Drain)
	}
	// The drain and the work after it end within 45 minutes, inside the
	// installer's wait for the stopping pool (install_test.go checks that the
	// wait is at least that long). launchd's ExitTimeOut does not count: macOS
	// clamps it to 60 s.
	if afterBound != DefaultAfterBound {
		t.Errorf("afterBound = %v, want DefaultAfterBound %v", afterBound, DefaultAfterBound)
	}
	if DefaultDrain+afterBound >= 45*time.Minute {
		t.Errorf("drain %v plus the budget after it %v reach 45m", DefaultDrain, afterBound)
	}
	if s.cfg.HealthAddr != "127.0.0.1:8737" || DefaultHealthAddr != "127.0.0.1:8737" {
		t.Errorf("health address = %q, want 127.0.0.1:8737", s.cfg.HealthAddr)
	}
}

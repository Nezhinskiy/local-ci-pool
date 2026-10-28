//go:build unix

package budget

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot"

	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
)

const cpu4 = "[resources]\ncpu = 4\n"

type admitResult struct {
	ad  runner.Admitted
	err error
}

func admitAsync(ctx context.Context, h *harness, a runner.Admission) <-chan admitResult {
	ch := make(chan admitResult, 1)
	go func() {
		ad, err := h.g.Admit(ctx, a)
		ch <- admitResult{ad, err}
	}()
	return ch
}

func waitResult(t *testing.T, ch <-chan admitResult) admitResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("Admit did not return within 10 s")
		return admitResult{}
	}
}

func alpha(name string) runner.Admission { return runner.Admission{Runner: name, Project: "alpha"} }

// checkReleasable fails the test unless ad's Release is callable, as the seam promises on every
// result, an error's included.
func checkReleasable(t *testing.T, ad runner.Admitted) {
	t.Helper()
	if ad.Release == nil {
		t.Fatalf("Admitted %+v has a nil Release", ad)
	}
	ad.Release()
}

// waitTickets waits until dir's live alpha:ci tickets need exactly want.
func waitTickets(t *testing.T, dir string, want ...int) {
	t.Helper()
	c := openClient(t, dir)
	var last []int
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if rep, err := c.Snapshot(); err == nil {
			if last = liveTickets(rep, "alpha:ci"); slices.Equal(last, want) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("live alpha:ci tickets need %v after 5 s; want %v", last, want)
		}
	}
}

func TestAdmitWaitsThenAdmits(t *testing.T) {
	t.Parallel()
	dir := stateDir(t, cpu4)
	h := newGate(t, dir, on3)
	holder := hold(t, dir, "holder:x", 4)

	ch := admitAsync(context.Background(), h, alpha("r1"))
	waitTickets(t, dir, 3)
	select {
	case r := <-ch:
		t.Fatalf("Admit returned while the budget was full: %+v", r)
	default:
	}
	closeAtOnce(t, dir, holder)
	r := waitResult(t, ch)
	if r.err != nil || r.ad.Mode != runner.ModeQueued || r.ad.Need != 3 {
		t.Fatalf("Admit = %+v, %v; want queued, need 3", r.ad, r.err)
	}
	hs := holdsOf(snapshot(t, dir), "alpha:ci")
	if len(hs) != 1 || hs[0].PID != os.Getpid() || hs[0].Granted["cpu"] != 3 || hs[0].Adopted {
		t.Fatalf("holds = %+v; want one alpha:ci of ours, cpu 3, not adopted", hs)
	}

	r.ad.Release()
	r.ad.Release()
	h.g.waitReleases()
	rep := snapshot(t, dir)
	if hs := holdsOf(rep, "alpha:ci"); len(hs) != 0 {
		t.Fatalf("holds after Release = %+v; want none", hs)
	}
	if got := rep.Resources["cpu"].Available; got != 4 {
		t.Fatalf("Available(cpu) = %d after Release; want 4", got)
	}
}

func TestAdmitGiveUpRace(t *testing.T) {
	t.Parallel()
	t.Run("grant after ctx end", func(t *testing.T) {
		t.Parallel()
		dir := stateDir(t, cpu4)
		h := newGate(t, dir, on3)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h.cc.wrapWaiter = func(w waiter) waiter {
			return waiterFunc(func(wctx context.Context) (*coreslot.Grant, error) {
				g, err := w.Wait(wctx)
				cancel()
				return g, err
			})
		}
		ad, err := h.g.Admit(ctx, alpha("r1"))
		if err != nil || ad.Mode != runner.ModeQueued {
			t.Fatalf("Admit = %+v, %v; want queued, nil", ad, err)
		}
		if hs := holdsOf(snapshot(t, dir), "alpha:ci"); len(hs) != 1 {
			t.Fatalf("holds = %+v; want the admitted one", hs)
		}
		ad.Release()
		h.g.waitReleases()
		if hs := holdsOf(snapshot(t, dir), "alpha:ci"); len(hs) != 0 {
			t.Fatalf("holds after Release = %+v; want none", hs)
		}
	})

	t.Run("ctx while waiting", func(t *testing.T) {
		t.Parallel()
		dir := stateDir(t, cpu4)
		h := newGate(t, dir, on3)
		hold(t, dir, "holder:x", 4)
		ctx, cancel := context.WithCancel(context.Background())
		ch := admitAsync(ctx, h, alpha("r1"))
		waitTickets(t, dir, 3)
		cancel()
		r := waitResult(t, ch)
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("Admit = %+v, %v; want context.Canceled", r.ad, r.err)
		}
		checkReleasable(t, r.ad)
		rep := snapshot(t, dir)
		if live := liveTickets(rep, "alpha:ci"); len(live) != 0 {
			t.Fatalf("live alpha:ci tickets after the give-up: %v", live)
		}
		if hs := holdsOf(rep, "alpha:ci"); len(hs) != 0 {
			t.Fatalf("holds after the give-up: %+v", hs)
		}
	})
}

// buildCoreslot builds the coreslot command from the module this package depends on.
func buildCoreslot(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "coreslot")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/core-slot/coreslot/cmd/coreslot")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build the coreslot command: %v\n%s", err, out)
	}
	return bin
}

// withoutContext is env without coreslot's grant context and state directory override.
func withoutContext(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "CORESLOT_") {
			out = append(out, kv)
		}
	}
	return out
}

func TestAdmitCancelledByBudget(t *testing.T) {
	t.Parallel()
	bin := buildCoreslot(t)
	dir := stateDir(t, cpu4)
	h := newGate(t, dir, on3)
	hold(t, dir, "holder:x", 4)
	ch := admitAsync(context.Background(), h, alpha("r1"))
	waitTickets(t, dir, 3)

	cmd := exec.Command(bin, "cancel", "--label", "alpha:ci")
	cmd.Env = append(withoutContext(os.Environ()), "CORESLOT_STATE_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("coreslot cancel: %v\n%s", err, out)
	}
	r := waitResult(t, ch)
	if !errors.Is(r.err, runner.ErrCancelledByBudget) {
		t.Fatalf("Admit = %+v, %v; want ErrCancelledByBudget", r.ad, r.err)
	}
	checkReleasable(t, r.ad)
}

func TestAdmitStateDirRemovedFailsOpen(t *testing.T) {
	t.Parallel()
	dir := stateDir(t, cpu4)
	h := newGate(t, dir, on3)
	holder := hold(t, dir, "holder:x", 4)
	ch := admitAsync(context.Background(), h, alpha("r1"))
	waitTickets(t, dir, 3)

	// Renamed away in one step, the directory is missing at once to every client; removing it
	// in place would race the waiting ticket's own refreshes.
	removed := filepath.Join(filepath.Dir(dir), "removed")
	if err := os.Rename(dir, removed); err != nil {
		t.Fatal(err)
	}
	r := waitResult(t, ch)
	if r.err != nil || r.ad.Mode != runner.ModeFailOpen {
		t.Fatalf("Admit = %+v, %v; want fail_open, nil", r.ad, r.err)
	}
	if got := h.g.State().Error; got != "state" {
		t.Fatalf("State().Error = %q; want state", got)
	}
	holder.Close() // a *StateError: its hold is no longer at its path
	if err := os.RemoveAll(removed); err != nil {
		t.Fatal(err)
	}

	// The next Admit opens coreslot again, which recreates the directory with the defaults.
	ad, err := h.g.Admit(context.Background(), alpha("r2"))
	if err != nil || ad.Mode != runner.ModeQueued {
		t.Fatalf("Admit after the removal = %+v, %v; want queued", ad, err)
	}
	ad.Release()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("the state directory was not recreated: %v", err)
	}
	if got := h.g.State().Error; got != "" {
		t.Fatalf("State().Error = %q after a queued admission; want none", got)
	}
}

func TestStateErrorSetAndCleared(t *testing.T) {
	t.Parallel()
	dir := stateDir(t, cpu4)
	h := newGate(t, dir, on3)
	h.cc.enqueueErr = &coreslot.StateError{Reason: "/Users/someone/state"}

	ad, err := h.g.Admit(context.Background(), alpha("r1"))
	if err != nil || ad.Mode != runner.ModeFailOpen {
		t.Fatalf("Admit = %+v, %v; want fail_open", ad, err)
	}
	if got := h.g.State().Error; got != "state" {
		t.Fatalf("State().Error = %q; want state", got)
	}
	b, err := json.Marshal(h.g.State())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "/Users/") {
		t.Fatalf("State JSON carries the reason: %s", b)
	}

	ad, err = h.g.Admit(context.Background(), alpha("r2"))
	if err != nil || ad.Mode != runner.ModeQueued {
		t.Fatalf("second Admit = %+v, %v; want queued", ad, err)
	}
	defer ad.Release()
	if got := h.g.State().Error; got != "" {
		t.Fatalf("State().Error = %q after a queued admission; want none", got)
	}
}

func TestAdmitOffAtCallTime(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), on3)
	if !h.g.Enabled() {
		t.Fatal("Enabled() = false; want true")
	}
	h.setSettings(t, "[budget]\nenabled = false\n")
	ad, err := h.g.Admit(context.Background(), alpha("r1"))
	if err != nil || ad.Mode != runner.ModeOff || ad.Need != 0 || ad.Release == nil {
		t.Fatalf("Admit = %+v, %v; want off, need 0, a Release", ad, err)
	}
	ad.Release()
	if n := h.cc.opens.Load(); n != 0 {
		t.Fatalf("Open called %d times; want 0", n)
	}
}

func TestStateShowsConfigClass(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), "not toml\n")
	if h.g.Enabled() {
		t.Fatal("Enabled() = true on an invalid file at start")
	}
	if got, want := h.g.State(), (State{State: "off", Error: "config"}); got != want {
		t.Fatalf("State() = %+v; want %+v", got, want)
	}

	h.setSettings(t, on3)
	if !h.g.Enabled() {
		t.Fatal("Enabled() = false on a valid file")
	}
	h.setSettings(t, "not toml either\n")
	if !h.g.Enabled() {
		t.Fatal("Enabled() = false after an invalid write; want the last valid settings")
	}
	if got, want := h.g.State(), (State{State: "on", Error: "config"}); got != want {
		t.Fatalf("State() = %+v; want %+v", got, want)
	}
}

func TestBudgetOffNeverOpens(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), "")
	if h.g.Enabled() {
		t.Fatal("Enabled() = true without a settings file")
	}
	b, err := json.Marshal(h.g.State())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"state":"off","oldest_wait_s":0}`; got != want {
		t.Fatalf("State JSON = %s; want %s", got, want)
	}
	if n := h.cc.opens.Load(); n != 0 {
		t.Fatalf("Open called %d times; want 0", n)
	}
}

func TestOpenFailureBacksOff(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), on3)
	h.cc.openFails, h.cc.openErr = 2, errors.New("open refused")

	for i, step := range []struct {
		advance   time.Duration
		opens     int32
		mode      runner.Mode
		healthErr string
	}{
		{0, 1, runner.ModeFailOpen, "open"},                      // t = 0: fails
		{500 * time.Millisecond, 1, runner.ModeFailOpen, "open"}, // t = 0.5 s: inside the 1 s backoff
		{500 * time.Millisecond, 2, runner.ModeFailOpen, "open"}, // t = 1 s: fails, backoff 2 s
		{time.Second, 2, runner.ModeFailOpen, "open"},            // t = 2 s: inside it
		{time.Second, 3, runner.ModeQueued, ""},                  // t = 3 s: succeeds
	} {
		h.clk.Advance(step.advance)
		ad, err := h.g.Admit(context.Background(), alpha("r"))
		if err != nil || ad.Mode != step.mode {
			t.Fatalf("step %d: Admit = %+v, %v; want %s", i, ad, err, step.mode)
		}
		ad.Release()
		if n := h.cc.opens.Load(); n != step.opens {
			t.Fatalf("step %d: Open called %d times; want %d", i, n, step.opens)
		}
		if got := h.g.State().Error; got != step.healthErr {
			t.Fatalf("step %d: State().Error = %q; want %q", i, got, step.healthErr)
		}
	}
}

func TestHealthCarriesNoReasonString(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), on3)
	h.cc.openFails, h.cc.openErr = 1, errors.New("open /Users/someone/state: permission denied")
	if _, err := h.g.Admit(context.Background(), alpha("r1")); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(h.g.State())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"state":"on","error":"open","oldest_wait_s":0}`; got != want {
		t.Fatalf("State JSON = %s; want %s", got, want)
	}
}

func TestSectionBusyWrappingCanceledIsNotRetried(t *testing.T) {
	t.Parallel()
	dir := stateDir(t, cpu4)
	h := newGate(t, dir, on3)
	holdSection(t, dir, 4242)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	ad, err := h.g.Admit(ctx, alpha("r1"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Admit error = %v; want context.Canceled", err)
	}
	checkReleasable(t, ad)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Admit returned after %v; want within 1 s", d)
	}
	if n := h.cc.enqueues.Load(); n != 1 {
		t.Fatalf("Enqueue called %d times; want 1", n)
	}
}

func TestAdoptShortfall(t *testing.T) {
	t.Parallel()
	dir := stateDir(t, cpu4)
	h := newGate(t, dir, on3)
	hold(t, dir, "holder:x", 3)

	for _, need := range []int{3, 0} {
		ad := h.g.Adopt(context.Background(), runner.Admission{Runner: "s1", Project: "alpha", Need: need})
		if ad.Mode != runner.ModeAdopted || ad.Need != 3 {
			t.Fatalf("need %d: Adopt = %+v; want adopted, need 3", need, ad)
		}
		hs := holdsOf(snapshot(t, dir), "alpha:ci")
		if len(hs) != 1 || !hs[0].Adopted || hs[0].Granted["cpu"] != 1 {
			t.Fatalf("need %d: holds = %+v; want one adopted alpha:ci holding cpu 1", need, hs)
		}
		ad.Release()
		h.g.waitReleases()
	}
	if got, want := h.log.lines("runner admitted"), []string{
		"runner=s1 label=alpha:ci mode=adopted need=3 granted=1 shortfall=2 wait_s=0.0",
		"runner=s1 label=alpha:ci mode=adopted need=3 granted=1 shortfall=2 wait_s=0.0",
	}; !slices.Equal(got, want) {
		t.Fatalf("runner admitted lines = %q; want %q", got, want)
	}
	if got := h.log.lines("survivor oversubscribed"); len(got) != 2 || got[0] != "runner=s1 label=alpha:ci shortfall=2" {
		t.Fatalf("survivor oversubscribed lines = %q", got)
	}

	hold(t, dir, "holder:y", 1)
	ad := h.g.Adopt(context.Background(), runner.Admission{Runner: "s2", Project: "alpha"})
	if ad.Mode != runner.ModeAdopted {
		t.Fatalf("zero free: Adopt = %+v; want adopted", ad)
	}
	hs := holdsOf(snapshot(t, dir), "alpha:ci")
	if len(hs) != 1 || hs[0].Granted["cpu"] != 0 {
		t.Fatalf("zero free: holds = %+v; want one alpha:ci holding cpu 0", hs)
	}
	ad.Release()
	h.g.waitReleases()
	if hs := holdsOf(snapshot(t, dir), "alpha:ci"); len(hs) != 0 {
		t.Fatalf("zero free: holds after Release = %+v; want none", hs)
	}
}

func TestAdoptFailsOpenWhenBusy(t *testing.T) {
	t.Parallel()
	dir := stateDir(t, cpu4)
	h := newGate(t, dir, on3, func(c *Config) { c.AdoptTimeout = 100 * time.Millisecond })
	holdSection(t, dir, 4242)

	done := make(chan runner.Admitted, 1)
	go func() { done <- h.g.Adopt(context.Background(), runner.Admission{Runner: "s1", Project: "alpha"}) }()
	select {
	case ad := <-done:
		if ad.Mode != runner.ModeFailOpen {
			t.Fatalf("Adopt = %+v; want fail_open", ad)
		}
	case <-time.After(time.Second):
		t.Fatal("Adopt did not return within 1 s while the section was held")
	}
}

func TestSettingsChangeWhileWaiting(t *testing.T) {
	t.Parallel()
	dir := stateDir(t, cpu4)
	h := newGate(t, dir, on3)
	hold(t, dir, "holder:x", 4)

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	chA := admitAsync(ctxA, h, alpha("a"))
	waitTickets(t, dir, 3)

	h.setSettings(t, "[budget]\nenabled = true\ncpu = 2\n")
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	chB := admitAsync(ctxB, h, alpha("b"))
	waitTickets(t, dir, 3, 2)
	if ctxA.Err() != nil {
		t.Fatal("A's ctx was cancelled")
	}

	h.setSettings(t, "[budget]\nenabled = false\n")
	if h.g.Enabled() {
		t.Fatal("Enabled() = true after enabled = false")
	}
	waitTickets(t, dir, 3, 2)
	select {
	case r := <-chA:
		t.Fatalf("A returned: %+v, %v", r.ad, r.err)
	case r := <-chB:
		t.Fatalf("B returned: %+v, %v", r.ad, r.err)
	default:
	}
	cancelA()
	cancelB()
	waitResult(t, chA)
	waitResult(t, chB)
}

func TestRunnerAdmittedLines(t *testing.T) {
	t.Parallel()
	dir := stateDir(t, cpu4)
	h := newGate(t, dir, on3)
	holder := hold(t, dir, "holder:x", 4)
	ch := admitAsync(context.Background(), h, alpha("r1"))
	waitTickets(t, dir, 3)
	h.clk.Advance(12400 * time.Millisecond)
	closeAtOnce(t, dir, holder)
	r := waitResult(t, ch)
	if r.err != nil || r.ad.Mode != runner.ModeQueued {
		t.Fatalf("Admit = %+v, %v; want queued", r.ad, r.err)
	}
	r.ad.Release()
	h.g.waitReleases()

	ad := h.g.Adopt(context.Background(), runner.Admission{Runner: "r2", Project: "alpha"})
	ad.Release()

	f := newGate(t, stateDir(t, cpu4), on3)
	f.cc.openFails, f.cc.openErr = 1, errors.New("open refused")
	if _, err := f.g.Admit(context.Background(), alpha("r3")); err != nil {
		t.Fatal(err)
	}

	if got, want := h.log.lines("runner admitted"), []string{
		"runner=r1 label=alpha:ci mode=queued need=3 granted=3 shortfall=0 wait_s=12.4",
		"runner=r2 label=alpha:ci mode=adopted need=3 granted=3 shortfall=0 wait_s=0.0",
	}; !slices.Equal(got, want) {
		t.Fatalf("runner admitted lines = %q; want %q", got, want)
	}
	if got, want := h.log.levels("runner admitted"), []slog.Level{slog.LevelInfo, slog.LevelInfo}; !slices.Equal(got, want) {
		t.Fatalf("runner admitted levels = %v; want %v", got, want)
	}
	if got, want := f.log.lines("runner admitted"), []string{
		"runner=r3 label=alpha:ci mode=fail_open need=3 granted=0 shortfall=0 wait_s=0.0 class=open",
	}; !slices.Equal(got, want) {
		t.Fatalf("fail_open lines = %q; want %q", got, want)
	}
	if got, want := f.log.levels("runner admitted"), []slog.Level{slog.LevelWarn}; !slices.Equal(got, want) {
		t.Fatalf("fail_open levels = %v; want %v", got, want)
	}
}

func TestReleaseNeverBlocks(t *testing.T) {
	t.Parallel()
	dir := stateDir(t, cpu4)
	h := newGate(t, dir, on3)
	ad, err := h.g.Admit(context.Background(), alpha("r1"))
	if err != nil || ad.Mode != runner.ModeQueued {
		t.Fatalf("Admit = %+v, %v; want queued", ad, err)
	}
	_, unlock := holdSection(t, dir, 4242)

	start := time.Now()
	ad.Release()
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("Release took %v while the section was held; want under 100 ms", d)
	}
	time.AfterFunc(300*time.Millisecond, unlock)
	h.g.waitReleases()
	if hs := holdsOf(snapshot(t, dir), "alpha:ci"); len(hs) != 0 {
		t.Fatalf("holds after Release = %+v; want none", hs)
	}
}

// A survivor of an older pool names no project: its request is labelled as
// the pool's own; any other project labels its own.
func TestRequestLabelOfAnUnlabelledSurvivor(t *testing.T) {
	for project, want := range map[string]string{"": "local-ci-pool:ci", "alpha": "alpha:ci"} {
		req, label, _ := request(runner.Admission{Runner: "local-ci-ssssssssssss", Project: project}, Settings{Enabled: true, CPU: 3}, false)
		if label != want || req.Label != want {
			t.Errorf("project %q: label %q, request label %q; want %q", project, label, req.Label, want)
		}
	}
}

// A runner whose ctx ended (a drain, a lower Desired) is given up, never failed open: no Open, no
// admission line, no error class.
func TestAdmitWithEndedContext(t *testing.T) {
	t.Parallel()
	t.Run("at entry", func(t *testing.T) {
		t.Parallel()
		h := newGate(t, stateDir(t, cpu4), on3)
		h.cc.openFails, h.cc.openErr = 1, errors.New("open refused")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ad, err := h.g.Admit(ctx, alpha("r1"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Admit = %+v, %v; want context.Canceled", ad, err)
		}
		checkReleasable(t, ad)
		if n := h.cc.opens.Load(); n != 0 {
			t.Fatalf("Open called %d times; want 0", n)
		}
		if got := h.log.lines("runner admitted"); len(got) != 0 {
			t.Fatalf("runner admitted lines = %q; want none", got)
		}
		if got := h.g.State().Error; got != "" {
			t.Fatalf("State().Error = %q; want none", got)
		}
	})
	t.Run("during Open", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		h := newGate(t, stateDir(t, cpu4), on3, func(c *Config) {
			c.Open = func(coreslot.Options) (client, error) {
				cancel()
				return nil, errors.New("open refused")
			}
		})
		ad, err := h.g.Admit(ctx, alpha("r1"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Admit = %+v, %v; want context.Canceled", ad, err)
		}
		checkReleasable(t, ad)
		if got := h.log.lines("runner admitted"); len(got) != 0 {
			t.Fatalf("runner admitted lines = %q; want none", got)
		}
	})
}

// An adoption whose ctx ended runs on unbudgeted, as a fail open does, but consults nothing and
// logs no admission.
func TestAdoptWithEndedContext(t *testing.T) {
	t.Parallel()
	t.Run("at entry", func(t *testing.T) {
		t.Parallel()
		h := newGate(t, stateDir(t, cpu4), on3)
		h.cc.openFails, h.cc.openErr = 1, errors.New("open refused")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ad := h.g.Adopt(ctx, runner.Admission{Runner: "s1", Project: "alpha"})
		if ad.Mode != runner.ModeFailOpen || ad.Need != 3 {
			t.Fatalf("Adopt = %+v; want fail_open, need 3", ad)
		}
		checkReleasable(t, ad)
		if n := h.cc.opens.Load(); n != 0 {
			t.Fatalf("Open called %d times; want 0", n)
		}
		if got := h.log.lines("runner admitted"); len(got) != 0 {
			t.Fatalf("runner admitted lines = %q; want none", got)
		}
	})
	t.Run("during Open", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		h := newGate(t, stateDir(t, cpu4), on3, func(c *Config) {
			c.Open = func(coreslot.Options) (client, error) {
				cancel()
				return nil, errors.New("open refused")
			}
		})
		ad := h.g.Adopt(ctx, runner.Admission{Runner: "s1", Project: "alpha"})
		if ad.Mode != runner.ModeFailOpen {
			t.Fatalf("Adopt = %+v; want fail_open", ad)
		}
		if got := h.log.lines("runner admitted"); len(got) != 0 {
			t.Fatalf("runner admitted lines = %q; want none", got)
		}
	})
}

// An Enqueue that meets a busy section while the runner's ctx is live is retried, not failed open.
func TestSectionBusyIsRetried(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), on3)
	h.cc.enqueueErr = &coreslot.SectionBusyError{PID: 4242, Err: context.DeadlineExceeded}
	ad, err := h.g.Admit(context.Background(), alpha("r1"))
	if err != nil || ad.Mode != runner.ModeQueued {
		t.Fatalf("Admit = %+v, %v; want queued", ad, err)
	}
	ad.Release()
	if n := h.cc.enqueues.Load(); n != 2 {
		t.Fatalf("Enqueue called %d times; want 2", n)
	}
}

// While both apply, /healthz names open before state.
func TestOpenClassBeforeStateClass(t *testing.T) {
	t.Parallel()
	h := newGate(t, stateDir(t, cpu4), on3)
	h.cc.enqueueErr = &coreslot.StateError{Reason: "injected"}
	if ad, err := h.g.Admit(context.Background(), alpha("r1")); err != nil || ad.Mode != runner.ModeFailOpen {
		t.Fatalf("Admit r1 = %+v, %v; want fail_open", ad, err)
	}
	if got := h.g.State().Error; got != "state" {
		t.Fatalf("State().Error = %q after the state error; want state", got)
	}
	h.cc.mu.Lock()
	h.cc.openFails, h.cc.openErr = 2, errors.New("open refused") // the second Open, the reopen, fails
	h.cc.mu.Unlock()
	if ad, err := h.g.Admit(context.Background(), alpha("r2")); err != nil || ad.Mode != runner.ModeFailOpen {
		t.Fatalf("Admit r2 = %+v, %v; want fail_open", ad, err)
	}
	if n := h.cc.opens.Load(); n != 2 {
		t.Fatalf("Open called %d times; want 2", n)
	}
	if got := h.g.State().Error; got != "open" {
		t.Fatalf("State().Error = %q with both a failed Open and a state error; want open", got)
	}
}

// A need outside 1..256 (a survivor's label of 300, or one that did not parse) asks the
// configured cpu; one inside asks itself. A runner asks it as its most, a survivor exactly.
func TestRequestNeedOutsideRangeAsksConfigured(t *testing.T) {
	t.Parallel()
	for need, want := range map[int]int{-1: 3, 0: 3, 257: 3, 300: 3, 1: 1, 256: 256} {
		a := runner.Admission{Runner: "s1", Project: "alpha", Need: need}
		s := Settings{Enabled: true, CPU: 3}
		if req, _, got := request(a, s, false); got != want || req.Need["cpu"] != (coreslot.Range{Min: 1, Max: want}) {
			t.Errorf("need %d: request asks %d (%+v); want 1..%d", need, got, req.Need["cpu"], want)
		}
		if req, _, got := request(a, s, true); got != want || req.Need["cpu"] != (coreslot.Range{Min: want, Max: want}) {
			t.Errorf("need %d: exact request asks %d (%+v); want %d..%d", need, got, req.Need["cpu"], want, want)
		}
	}
}

// A runner starts once one core is free and takes up to cpu, at priority 5: five minutes of
// seniority, GitHub's window for an assigned job request that no runner acquired.
func TestRequestRangeAndPriority(t *testing.T) {
	t.Parallel()
	s := Settings{Enabled: true, CPU: 3}
	for _, exact := range []bool{false, true} {
		req, _, _ := request(alpha("r1"), s, exact)
		if req.Priority != 5 {
			t.Errorf("exact %v: priority %d; want 5", exact, req.Priority)
		}
		if err := req.Validate(); err != nil {
			t.Errorf("exact %v: %v", exact, err)
		}
	}
}

// With fewer cores free than cpu, a runner is admitted at once with what is free, and its Need is
// the grant, which the pool hands the job as CORESLOT_CPU.
func TestAdmitTakesWhatIsFree(t *testing.T) {
	t.Parallel()
	dir := stateDir(t, cpu4)
	h := newGate(t, dir, on3)
	hold(t, dir, "holder:x", 2)

	r := waitResult(t, admitAsync(context.Background(), h, alpha("r1")))
	if r.err != nil || r.ad.Mode != runner.ModeQueued || r.ad.Need != 2 {
		t.Fatalf("Admit = %+v, %v; want queued, need 2 (the cores free)", r.ad, r.err)
	}
	hs := holdsOf(snapshot(t, dir), "alpha:ci")
	if len(hs) != 1 || hs[0].Granted["cpu"] != 2 {
		t.Fatalf("holds = %+v; want one alpha:ci holding cpu 2", hs)
	}
	if got := h.log.lines("runner admitted"); len(got) != 1 || !strings.HasPrefix(got[0], "runner=r1 label=alpha:ci mode=queued need=3 granted=2 shortfall=0 ") {
		t.Fatalf("runner admitted lines = %q", got)
	}
	r.ad.Release()
	h.g.waitReleases()
}

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
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
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
}

func TestAdmitStateDirRemovedFailsOpen(t *testing.T) {
	t.Parallel()
	dir := stateDir(t, cpu4)
	h := newGate(t, dir, on3)
	hold(t, dir, "holder:x", 4)
	ch := admitAsync(context.Background(), h, alpha("r1"))
	waitTickets(t, dir, 3)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	r := waitResult(t, ch)
	if r.err != nil || r.ad.Mode != runner.ModeFailOpen {
		t.Fatalf("Admit = %+v, %v; want fail_open, nil", r.ad, r.err)
	}
	if got := h.g.State().Error; got != "state" {
		t.Fatalf("State().Error = %q; want state", got)
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
	_, err := h.g.Admit(ctx, alpha("r1"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Admit error = %v; want context.Canceled", err)
	}
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
	holder.Close()
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

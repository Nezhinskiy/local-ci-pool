//go:build unix

package budget

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/core-slot/coreslot"
	"golang.org/x/sys/unix"
)

// stateDir is a state directory as coreslot's own tests build one: a 0700 child of t.TempDir()
// with a space in its name, and config.toml when cfg is not empty.
func stateDir(t *testing.T, cfg string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// openClient opens the test's own coreslot client on dir.
func openClient(t *testing.T, dir string) *coreslot.Client {
	t.Helper()
	c, err := coreslot.Open(coreslot.Options{Independent: true, StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// hold admits a request of n cpu labelled label on dir, closed when the test ends.
func hold(t *testing.T, dir, label string, n int) *coreslot.Grant {
	t.Helper()
	c := openClient(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w, err := c.Enqueue(ctx, coreslot.Request{Label: label, Need: map[string]coreslot.Range{"cpu": {Min: n, Max: n}}})
	if err != nil {
		t.Fatal(err)
	}
	g, err := w.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

// closeAtOnce closes g, a grant on dir, so that a waiting ticket sees all of its cpu free at once.
// Grant.Close unlocks the tokens one by one outside the admission section, and a ticket asking
// 1..n admitted between two of them takes only the cores free so far. So the test holds the
// section until every token of g is unlocked; Close then waits for it to remove the hold.
func closeAtOnce(t *testing.T, dir string, g *coreslot.Grant) {
	t.Helper()
	c := openClient(t, dir)
	free := func() int {
		rep, err := c.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		return rep.Resources["cpu"].Available
	}
	want := free() + g.Granted()["cpu"]
	_, unlock := holdSection(t, dir, os.Getpid())
	closed := make(chan error, 1)
	go func() { closed <- g.Close() }()
	eventually(t, "the grant's cpu unlocked", func() bool { return free() == want })
	unlock()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

// countingClient wraps the real client: it counts calls, signals every Snapshot, and lets a test
// replace the waiter Enqueue returns or make one call return a chosen error.
type countingClient struct {
	opens, enqueues, adopts, snapshots atomic.Int32
	snapped                            chan struct{} // one value per Snapshot taken

	mu         sync.Mutex
	inner      client
	openFails  int   // the first openFails opens fail with openErr
	openErr    error // what a failing open returns
	enqueueErr error // returned once by the next Enqueue, then cleared
	wrapWaiter func(waiter) waiter
	// report, when set, rewrites every successful Snapshot's report.
	report func(coreslot.Report) coreslot.Report
}

func newCountingClient() *countingClient {
	return &countingClient{snapped: make(chan struct{}, 1024)}
}

// open is the gate's Open seam.
func (c *countingClient) open(o coreslot.Options) (client, error) {
	n := int(c.opens.Add(1))
	c.mu.Lock()
	defer c.mu.Unlock()
	if n <= c.openFails {
		return nil, c.openErr
	}
	inner, err := openReal(o)
	if err != nil {
		return nil, err
	}
	c.inner = inner
	return c, nil
}

// innerClient is the client the last successful open made; open replaces it under c.mu, possibly
// from the gate's monitor while another goroutine calls through it.
func (c *countingClient) innerClient() client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner
}

func (c *countingClient) Enqueue(ctx context.Context, r coreslot.Request) (waiter, error) {
	c.enqueues.Add(1)
	c.mu.Lock()
	err, wrap := c.enqueueErr, c.wrapWaiter
	c.enqueueErr = nil
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	w, err := c.innerClient().Enqueue(ctx, r)
	if err != nil || wrap == nil {
		return w, err
	}
	return wrap(w), nil
}

func (c *countingClient) Adopt(ctx context.Context, r coreslot.Request) (*coreslot.Grant, error) {
	c.adopts.Add(1)
	return c.innerClient().Adopt(ctx, r)
}

func (c *countingClient) Snapshot() (coreslot.Report, error) {
	rep, err := c.innerClient().Snapshot()
	c.mu.Lock()
	if c.report != nil && err == nil {
		rep = c.report(rep)
	}
	c.mu.Unlock()
	c.snapshots.Add(1)
	select {
	case c.snapped <- struct{}{}:
	default:
	}
	return rep, err
}

// waiterFunc is a waiter made of a function.
type waiterFunc func(ctx context.Context) (*coreslot.Grant, error)

func (f waiterFunc) Wait(ctx context.Context) (*coreslot.Grant, error) { return f(ctx) }

// manualClock is a Clock that moves only when the test advances it.
type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []manualTimer
}

type manualTimer struct {
	at time.Time
	ch chan time.Time
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.timers = append(c.timers, manualTimer{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock by d and fires every timer then due.
func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, tm := range c.timers {
		if tm.at.After(c.now) {
			kept = append(kept, tm)
			continue
		}
		tm.ch <- c.now
	}
	c.timers = kept
}

// pending is the number of timers waiting.
func (c *manualClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// waitReleases waits for the goroutines closing released grants.
func (g *Gate) waitReleases() { g.closing.Wait() }

// monitorRunning reports whether the monitor runs.
func (g *Gate) monitorRunning() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.monitorOn
}

// harness is a gate on a real state directory, with its counting client, clock and log.
type harness struct {
	g    *Gate
	cc   *countingClient
	clk  *manualClock
	log  *logRecorder
	dir  string // the state directory
	path string // the settings file
}

// newGate builds a gate over a state directory dir whose settings file holds settings ("" leaves
// it absent). opts adjust the Config before New.
func newGate(t *testing.T, dir, settings string, opts ...func(*Config)) *harness {
	t.Helper()
	h := &harness{
		cc:   newCountingClient(),
		clk:  newManualClock(),
		log:  &logRecorder{},
		dir:  dir,
		path: filepath.Join(t.TempDir(), "config.toml"),
	}
	if settings != "" {
		h.setSettings(t, settings)
	}
	cfg := Config{
		SettingsPath: func() (string, error) { return h.path, nil },
		StateDir:     dir,
		Log:          h.log.logger(),
		Open:         h.cc.open,
		Clock:        h.clk,
	}
	for _, o := range opts {
		o(&cfg)
	}
	h.g = New(cfg)
	t.Cleanup(h.g.waitReleases)
	return h
}

func (h *harness) setSettings(t *testing.T, body string) {
	t.Helper()
	writeSettings(t, h.path, body)
}

const on3 = "[budget]\nenabled = true\ncpu = 3\n"

// eventually waits up to 5 s for cond, failing the test otherwise.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// advance waits until the monitor has a timer pending, advances the clock by d, and waits for
// the snapshot that fires and for the monitor to arm its next timer, which it does once that
// tick is handled.
func (h *harness) advance(t *testing.T, d time.Duration) {
	t.Helper()
	eventually(t, "a monitor timer", func() bool { return h.clk.pending() > 0 })
	for len(h.cc.snapped) > 0 {
		<-h.cc.snapped
	}
	h.clk.Advance(d)
	select {
	case <-h.cc.snapped:
	case <-time.After(5 * time.Second):
		t.Fatalf("no snapshot within 5 s of advancing the clock by %v", d)
	}
	eventually(t, "the monitor's next timer", func() bool { return h.clk.pending() > 0 })
}

// step is one monitor tick: advance by the snapshot period.
func (h *harness) step(t *testing.T) { h.advance(t, h.g.snapshotEvery) }

// snapshot reads dir through a client of the test's own.
func snapshot(t *testing.T, dir string) coreslot.Report {
	t.Helper()
	rep, err := openClient(t, dir).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// liveTickets returns the needs ("cpu" Max: the most each asks) of the live tickets labelled label.
func liveTickets(rep coreslot.Report, label string) []int {
	var out []int
	for _, tk := range rep.Tickets {
		if tk.Label == label && !tk.Dead {
			out = append(out, tk.Need["cpu"].Max)
		}
	}
	return out
}

// holdsOf returns the live holds labelled label.
func holdsOf(rep coreslot.Report, label string) []coreslot.HoldInfo {
	var out []coreslot.HoldInfo
	for _, h := range rep.Holds {
		if h.Label == label {
			out = append(out, h)
		}
	}
	return out
}

// holdSection flocks dir's admit.lock from the test process with the body {"pid":pid,
// "entered_ns":now}, as a client stopped inside the admission section would. rewrite replaces the
// body; unlock leaves the section (also at the test's end).
func holdSection(t *testing.T, dir string, pid int) (rewrite func(pid int), unlock func()) {
	t.Helper()
	openClient(t, dir) // creates the layout, admit.lock included
	f, err := os.OpenFile(filepath.Join(dir, "admit.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A waiting client enters the section for a moment on each poll: retry until it is free.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			t.Fatalf("flock admit.lock: %v", err)
		}
	}
	rewrite = func(pid int) {
		body, err := json.Marshal(map[string]int64{"pid": int64(pid), "entered_ns": time.Now().UnixNano()})
		if err != nil {
			t.Error(err)
			return
		}
		if err := f.Truncate(0); err != nil {
			t.Error(err)
		}
		if _, err := f.WriteAt(body, 0); err != nil {
			t.Error(err)
		}
	}
	rewrite(pid)
	var once sync.Once
	unlock = func() { once.Do(func() { f.Close() }) }
	t.Cleanup(unlock)
	return rewrite, unlock
}

// holderEnv names the admit.lock a test binary run as a section holder flocks; see TestMain.
const holderEnv = "LOCAL_CI_POOL_TEST_HOLD_SECTION"

// TestMain runs the test binary as a section holder when holderEnv is set, and the tests
// otherwise.
func TestMain(m *testing.M) {
	if path := os.Getenv(holderEnv); path != "" {
		os.Exit(holdSectionAsChild(path))
	}
	os.Exit(m.Run())
}

// holdSectionAsChild flocks path, as a client that does not rewrite admit.lock's body would,
// prints "locked", and keeps the lock until its stdin closes.
func holdSectionAsChild(path string) int {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 2
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return 3
	}
	if _, err := os.Stdout.WriteString("locked\n"); err != nil {
		return 4
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

// holdSectionInChild starts a child process that holds dir's admission section without rewriting
// admit.lock's body, and returns once it holds it. The child leaves the section when the test
// ends.
func holdSectionInChild(t *testing.T, dir string) {
	t.Helper()
	openClient(t, dir) // creates the layout, admit.lock included
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), holderEnv+"="+filepath.Join(dir, "admit.lock"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("the section holder: %v", err)
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("the section holder said %q, %v; want locked", line, err)
	}
}

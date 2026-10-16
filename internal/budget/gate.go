//go:build unix

package budget

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/core-slot/coreslot"

	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
)

// enqueueTimeout bounds one Enqueue attempt, so that a wedge ends it at once and a held section
// is retried with backoff.
const enqueueTimeout = 10 * time.Second

// Defaults of Config's seams, and the backoffs.
const (
	defaultAdoptTimeout  = 10 * time.Second
	defaultSnapshotEvery = 5 * time.Second
	defaultWedgeAfter    = 2 * time.Minute
	defaultLongWait      = 30 * time.Minute

	busyBackoffFirst = 250 * time.Millisecond // real time
	busyBackoffMax   = 2 * time.Second
	openBackoffFirst = time.Second // on Clock
	openBackoffMax   = time.Minute
)

// unlabelledProject is the project of a survivor whose container names none.
const unlabelledProject = "local-ci-pool"

// errOpenBackoff is a call that did not open coreslot because the last Open failed recently.
var errOpenBackoff = errors.New("coreslot is not opened: the last Open failed, retried after a backoff")

// Config configures a Gate.
type Config struct {
	SettingsPath func() (string, error) // nil: DefaultSettingsPath
	StateDir     string                 // tests only; "" in production
	Log          *slog.Logger
	// Seams; zero values take the defaults.
	Open                                              func(coreslot.Options) (client, error) // nil: coreslot.Open
	Clock                                             Clock                                  // nil: the wall clock
	AdoptTimeout, SnapshotEvery, WedgeAfter, LongWait time.Duration
}

// Gate admits runners into the machine's coreslot budget. It implements runner.Gate. The settings
// file is read on every call; coreslot is opened by the first call that needs it.
type Gate struct {
	settings *settingsFile
	stateDir string
	log      *slog.Logger
	open     func(coreslot.Options) (client, error)
	clock    Clock

	adoptTimeout, snapshotEvery, wedgeAfter, longWait time.Duration

	openMu sync.Mutex // serializes Open

	mu          sync.Mutex
	cl          client // nil until an Open succeeds
	openFailed  bool   // the last Open failed: class open
	openBackoff time.Duration
	nextOpen    time.Time               // no Open before then while openFailed
	stateErr    bool                    // the last admission or adoption failed open on a *StateError
	admits      map[*admitting]struct{} // the Admits in progress
	monitorOn   bool                    // the monitor runs
	wedged      bool                    // the admission section is wedged
	tracked     sectionSeen             // the held section the snapshots show
	lastHolds   []coreslot.HoldInfo     // the holds of the last snapshot
	lastFull    time.Time               // when budget full was last logged, zero when not since

	closing sync.WaitGroup // the goroutines closing released grants
}

var _ runner.Gate = (*Gate)(nil)

// admitting is one Admit in progress.
type admitting struct {
	start  time.Time          // on Clock
	cancel context.CancelFunc // ends the Admit's wait for a wedge
}

// New builds a Gate. It reads nothing and opens nothing.
func New(cfg Config) *Gate {
	g := &Gate{
		stateDir:      cfg.StateDir,
		log:           cfg.Log,
		open:          cfg.Open,
		clock:         cfg.Clock,
		adoptTimeout:  orDefault(cfg.AdoptTimeout, defaultAdoptTimeout),
		snapshotEvery: orDefault(cfg.SnapshotEvery, defaultSnapshotEvery),
		wedgeAfter:    orDefault(cfg.WedgeAfter, defaultWedgeAfter),
		longWait:      orDefault(cfg.LongWait, defaultLongWait),
		admits:        map[*admitting]struct{}{},
	}
	if g.log == nil {
		g.log = slog.New(slog.DiscardHandler)
	}
	if g.open == nil {
		g.open = openReal
	}
	if g.clock == nil {
		g.clock = wallClock{}
	}
	path := cfg.SettingsPath
	if path == nil {
		path = DefaultSettingsPath
	}
	g.settings = newSettingsFile(path, g.log)
	return g
}

// orDefault is d, or def when d is zero.
func orDefault(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

// Enabled reports whether the settings turn the budget on. It reads the settings file once.
func (g *Gate) Enabled() bool {
	s, _ := g.settings.Load()
	return s.Enabled
}

// State is the budget's part of /healthz: off, on or wedged, the error class, and the seconds the
// oldest Admit in progress has waited. It reads the settings file once.
func (g *Gate) State() State {
	s, invalid := g.settings.Load()
	st := State{State: "off"}
	if s.Enabled {
		st.State = "on"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if s.Enabled && g.wedged {
		st.State = "wedged"
	}
	st.OldestWaitS = int(g.oldestWaitLocked(g.clock.Now()) / time.Second)
	switch {
	case invalid:
		st.Error = "config"
	case g.openFailed:
		st.Error = "open"
	case g.stateErr:
		st.Error = "state"
	}
	return st
}

// Admit queues the runner in the budget and blocks until it is admitted. It returns an error only
// when ctx ended or the request was cancelled through coreslot; every other failure fails open.
func (g *Gate) Admit(ctx context.Context, a runner.Admission) (runner.Admitted, error) {
	s, _ := g.settings.Load()
	if !s.Enabled {
		return offAdmitted(), nil
	}
	req, label, need := request(a, s)
	if err := req.Validate(); err != nil {
		return g.failOpen(a.Runner, label, need, 0, "refused", err), nil
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ad := g.begin(cancel)
	defer g.end(ad)

	cl, err := g.client()
	if err != nil {
		return g.failOpen(a.Runner, label, need, g.since(ad), "open", err), nil
	}
	if !g.proceedWhenWedged(cl) {
		return g.failOpen(a.Runner, label, need, g.since(ad), "wedged", errWedged), nil
	}
	var w waiter
	for backoff := busyBackoffFirst; ; backoff = min(2*backoff, busyBackoffMax) {
		actx, acancel := context.WithTimeout(waitCtx, enqueueTimeout)
		w, err = cl.Enqueue(actx, req)
		acancel()
		if err == nil {
			break
		}
		// The runner's ctx first: a *SectionBusyError wraps the ctx's error.
		if ctx.Err() != nil {
			return runner.Admitted{Release: noRelease}, ctx.Err()
		}
		var busy *coreslot.SectionBusyError
		if !errors.As(err, &busy) || waitCtx.Err() != nil {
			return g.failOpen(a.Runner, label, need, g.since(ad), failClass(err, waitCtx.Err() != nil), err), nil
		}
		timer := time.NewTimer(backoff)
		select {
		case <-waitCtx.Done():
		case <-timer.C:
		}
		timer.Stop()
	}

	g.log.Info("runner waiting for the budget",
		slog.String("runner", a.Runner), slog.String("label", label), slog.Int("need", need))
	grant, err := w.Wait(waitCtx)
	switch {
	case err == nil && grant != nil: // whatever ctx is now: the caller releases it
		return g.admitted(a.Runner, label, runner.ModeQueued, need, grant, g.since(ad)), nil
	case errors.Is(err, coreslot.ErrCanceled):
		return runner.Admitted{Release: noRelease}, runner.ErrCancelledByBudget
	case ctx.Err() != nil:
		return runner.Admitted{Release: noRelease}, ctx.Err()
	}
	if err == nil {
		err = errors.New("coreslot returned no grant")
	}
	return g.failOpen(a.Runner, label, need, g.since(ad), failClass(err, waitCtx.Err() != nil), err), nil
}

// Adopt takes what is free for a runner already running, without queueing. It never fails: a
// failure fails open.
func (g *Gate) Adopt(ctx context.Context, a runner.Admission) runner.Admitted {
	s, _ := g.settings.Load()
	if !s.Enabled {
		return offAdmitted()
	}
	req, label, need := request(a, s)
	if err := req.Validate(); err != nil {
		return g.failOpen(a.Runner, label, need, 0, "refused", err)
	}
	cl, err := g.client()
	if err != nil {
		return g.failOpen(a.Runner, label, need, 0, "open", err)
	}
	if !g.proceedWhenWedged(cl) {
		return g.failOpen(a.Runner, label, need, 0, "wedged", errWedged)
	}
	actx, cancel := context.WithTimeout(ctx, g.adoptTimeout)
	defer cancel()
	grant, err := cl.Adopt(actx, req)
	if err != nil {
		class := failClass(err, false)
		if class == "other" && errors.Is(err, context.DeadlineExceeded) {
			class = "busy"
		}
		return g.failOpen(a.Runner, label, need, 0, class, err)
	}
	ad := g.admitted(a.Runner, label, runner.ModeAdopted, need, grant, 0)
	if short := grant.Shortfall()["cpu"]; short > 0 {
		g.log.Warn("survivor oversubscribed",
			slog.String("runner", a.Runner), slog.String("label", label), slog.Int("shortfall", short))
	}
	return ad
}

// waitReleases waits for the goroutines closing released grants.
func (g *Gate) waitReleases() { g.closing.Wait() }

// request is the coreslot request for a: label <project>:ci, priority 0, cpu need..need, where
// need is a.Need when it is in 1..256 and the settings' cpu otherwise.
func request(a runner.Admission, s Settings) (req coreslot.Request, label string, need int) {
	need = a.Need
	if need < 1 || need > maxCPU {
		need = s.CPU
	}
	project := a.Project
	if project == "" {
		project = unlabelledProject
	}
	label = project + ":ci"
	return coreslot.Request{Label: label, Priority: 0,
		Need: map[string]coreslot.Range{"cpu": {Min: need, Max: need}}}, label, need
}

// noRelease is the Release of every result that holds no grant.
func noRelease() {}

func offAdmitted() runner.Admitted {
	return runner.Admitted{Mode: runner.ModeOff, Release: noRelease}
}

// failClass is the class a fail open logs: wedged when the wedge ended the call, else by error.
func failClass(err error, wedged bool) string {
	var (
		state *coreslot.StateError
		busy  *coreslot.SectionBusyError
	)
	switch {
	case wedged:
		return "wedged"
	case errors.As(err, &state):
		return "state"
	case errors.As(err, &busy):
		return "busy"
	case errors.Is(err, coreslot.ErrRefused):
		return "refused"
	}
	return "other"
}

// client returns the coreslot client, opening it on first use. After a failed Open, no call opens
// again before a backoff of 1 s doubling to 1 min.
func (g *Gate) client() (client, error) {
	g.openMu.Lock()
	defer g.openMu.Unlock()
	g.mu.Lock()
	cl, failed, next := g.cl, g.openFailed, g.nextOpen
	g.mu.Unlock()
	if cl != nil {
		return cl, nil
	}
	now := g.clock.Now()
	if failed && now.Before(next) {
		return nil, errOpenBackoff
	}
	cl, err := g.open(coreslot.Options{Independent: true, StateDir: g.stateDir})
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		if g.openFailed {
			g.openBackoff = min(2*g.openBackoff, openBackoffMax)
		} else {
			g.openBackoff = openBackoffFirst
		}
		g.openFailed, g.nextOpen = true, now.Add(g.openBackoff)
		return nil, err
	}
	g.cl, g.openFailed = cl, false
	return cl, nil
}

// begin registers an Admit in progress and starts the monitor unless it runs.
func (g *Gate) begin(cancel context.CancelFunc) *admitting {
	ad := &admitting{start: g.clock.Now(), cancel: cancel}
	g.mu.Lock()
	g.admits[ad] = struct{}{}
	g.startMonitorLocked()
	g.mu.Unlock()
	return ad
}

// end unregisters an Admit.
func (g *Gate) end(ad *admitting) {
	g.mu.Lock()
	delete(g.admits, ad)
	g.mu.Unlock()
}

// since is the time on Clock since ad began.
func (g *Gate) since(ad *admitting) time.Duration { return g.clock.Now().Sub(ad.start) }

// admitted records a grant: it clears the state class and logs the admission.
func (g *Gate) admitted(name, label string, mode runner.Mode, need int, grant *coreslot.Grant,
	wait time.Duration) runner.Admitted {
	g.mu.Lock()
	g.stateErr = false
	g.mu.Unlock()
	g.logAdmitted(slog.LevelInfo, name, label, mode, need, grant.Granted()["cpu"], grant.Shortfall()["cpu"], wait, "")
	return runner.Admitted{Mode: mode, Release: g.release(name, grant), Need: need}
}

// failOpen admits the runner without a grant. The library's reason goes to a debug line only.
func (g *Gate) failOpen(name, label string, need int, wait time.Duration, class string, err error) runner.Admitted {
	if class == "state" {
		// The budget is unusable as opened (its state directory removed, for one): the next call
		// opens coreslot again, which recreates a missing directory.
		g.mu.Lock()
		g.stateErr, g.cl = true, nil
		g.mu.Unlock()
	}
	g.log.Debug("budget call failed", slog.String("runner", name), slog.String("class", class),
		slog.String("error", err.Error()))
	g.logAdmitted(slog.LevelWarn, name, label, runner.ModeFailOpen, need, 0, 0, wait, class)
	return runner.Admitted{Mode: runner.ModeFailOpen, Release: noRelease, Need: need}
}

func (g *Gate) logAdmitted(level slog.Level, name, label string, mode runner.Mode, need, granted, shortfall int,
	wait time.Duration, class string) {
	attrs := []slog.Attr{
		slog.String("runner", name),
		slog.String("label", label),
		slog.String("mode", string(mode)),
		slog.Int("need", need),
		slog.Int("granted", granted),
		slog.Int("shortfall", shortfall),
		slog.String("wait_s", strconv.FormatFloat(wait.Seconds(), 'f', 1, 64)),
	}
	if class != "" {
		attrs = append(attrs, slog.String("class", class))
	}
	g.log.LogAttrs(context.Background(), level, "runner admitted", attrs...)
}

// release returns the grant's Release: idempotent, and it never blocks, since Grant.Close may
// wait up to 2 s for the admission section. The grant closes on a goroutine the gate tracks.
func (g *Gate) release(name string, grant *coreslot.Grant) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.closing.Add(1)
			go func() {
				defer g.closing.Done()
				if err := grant.Close(); err != nil {
					g.log.Warn("closing a budget grant", slog.String("runner", name),
						slog.String("error", err.Error()))
				}
			}()
		})
	}
}

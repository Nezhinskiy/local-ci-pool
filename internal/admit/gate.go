// Package admit asks coreslot, the Mac's memory admission, for room before a
// runner starts. Each runner gets one request: `coreslot run` with the class's
// size, whose command, `pool hold`, reports the admission on one stdout line
// and then waits on its stdin, so the room stays held until the pool closes
// that pipe.
//
// A request can only fail towards starting the runner without the budget,
// never towards a job that is not started: a grant coreslot gives outside the
// budget, a request that ends before it is admitted and a request that does
// not answer each run that one runner unbudgeted. Only what the pool reads
// itself, a coreslot it cannot start or one too old for the flags, latches the
// gate off until the binary changes.
package admit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Timeout is coreslot's --timeout for every request: past it coreslot runs the
// hold outside the budget (--on-timeout run).
const Timeout = 4 * time.Minute

// answerSlack is how long past Timeout the gate waits for coreslot's answer
// before it withdraws the request itself.
const answerSlack = 30 * time.Second

// pollEvery is how often a waiting Bind reads the gate's clock. A variable so
// tests can shorten it.
var pollEvery = time.Second

// exitUsage is coreslot's exit status for a command line it does not accept:
// a coreslot older than the flags the gate passes.
const exitUsage = 64

// Latch reasons.
const (
	ReasonNotInstalled = "not-installed"
	ReasonStart        = "start"
	ReasonTooOld       = "coreslot-too-old"
	// ReasonNoAnswer is the reason of a runner whose request coreslot did not
	// answer within Timeout and the slack.
	ReasonNoAnswer = "no-answer"
	// ReasonHoldLost is Health's reason while a held runner's hold has ended
	// before its Unbind.
	ReasonHoldLost = "hold-lost"
)

// Grant is the line a request's hold printed when coreslot ran it.
type Grant struct {
	// Hold is coreslot's id for the hold (CORESLOT_HOLD).
	Hold string
	// DockerHost is the hold's DOCKER_HOST.
	DockerHost string
	// Unbudgeted is non-empty when coreslot ran the hold outside the budget
	// (CORESLOT_UNBUDGETED); its text is shown, never interpreted.
	Unbudgeted string
}

// Proc is one running request.
type Proc interface {
	// Admitted yields one value on admission; it is closed empty when the
	// child ended first.
	Admitted() <-chan Grant
	// Done yields the child's exit status (-1 when killed by a signal).
	Done() <-chan int
	// Withdraw asks a waiting request to give up (SIGINT; coreslot withdraws
	// and exits 130).
	Withdraw()
	// Close closes the hold's stdin; the hold ends.
	Close()
}

// Config configures one Gate.
type Config struct {
	// Coreslot is the path of the installed coreslot; "" means none was
	// found, and the gate stays latched.
	Coreslot string
	// Label is the request's label, <identity>:<class>.
	Label string
	// Self is the pool executable, run as `<Self> hold`.
	Self string
	// Size returns the size flags, read per request, such as ["--mem", "6G"].
	// The gate appends --timeout and --on-timeout run.
	Size func() []string
	// Env is extra environment for the child.
	Env []string
	// Start starts a request's process; StartProc is the real one.
	Start func(ctx context.Context, path string, args, env []string) (Proc, error)
	// Clock is the gate's time; nil is time.Now.
	Clock func() time.Time
	Log   *slog.Logger
}

// Health is the gate's state for /healthz.
type Health struct {
	// State is "unbudgeted" (latched), "waiting", "held" or "idle", in that
	// precedence.
	State string
	// Reason is the latch's reason while latched, else "hold-lost" while a
	// held runner's hold has ended, else "".
	Reason string
	// Waiting is the number of Binds waiting for an answer.
	Waiting int
	// Held is the number of bound runners.
	Held int
	// Unbudgeted is the number of bound runners running without the budget.
	Unbudgeted int
	// LastUnbudgeted is the reason of the last runner bound without the
	// budget.
	LastUnbudgeted string
	// OldestWaitS is the age of the oldest waiting Bind, in seconds.
	OldestWaitS int
}

// held is one bound runner. proc is nil when the request ended before it was
// admitted; unbudgeted is the reason it runs without the budget, if it does.
type held struct {
	proc       Proc
	grant      Grant
	granted    bool
	unbudgeted string
	lost       bool
}

// Gate makes one coreslot request per runner.
type Gate struct {
	cfg   Config
	clock func() time.Time
	log   *slog.Logger
	// stop is closed by Stop; every waiting Bind wakes on it.
	stop chan struct{}

	mu       sync.Mutex
	stopped  bool
	latched  bool
	reason   string
	binary   os.FileInfo // the binary's stat when the latch was set; nil if it had none
	waiting  map[string]time.Time
	held     map[string]*held
	lastUnbg string
}

// New returns a gate. With no coreslot found it starts latched.
func New(cfg Config) *Gate {
	g := &Gate{
		cfg:     cfg,
		clock:   cfg.Clock,
		log:     cfg.Log,
		stop:    make(chan struct{}),
		waiting: map[string]time.Time{},
		held:    map[string]*held{},
	}
	if g.clock == nil {
		g.clock = time.Now
	}
	if g.log == nil {
		g.log = slog.New(slog.DiscardHandler)
	}
	if cfg.Coreslot == "" {
		g.latched, g.reason = true, ReasonNotInstalled
		g.log.Warn("coreslot is not installed; runners start without the memory budget")
	}
	return g
}

// args is the request's command line after the coreslot path.
func (g *Gate) args() []string {
	a := []string{"run", "--restart=no", "--label", g.cfg.Label}
	if g.cfg.Size != nil {
		a = append(a, g.cfg.Size()...)
	}
	return append(a, "--timeout", durationFlag(Timeout), "--on-timeout", "run", "--", g.cfg.Self, "hold")
}

// durationFlag spells d as coreslot reads it: whole minutes as "4m".
func durationFlag(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

// Bind waits until runner may start. It returns true once coreslot admitted
// the runner, or when the runner is to start without the budget: the gate is
// latched, the request could not start or ended before it was admitted,
// coreslot ran the hold outside the budget, or no answer came within Timeout
// and the slack. It returns false only when ctx ended or Stop was called; the
// request is then withdrawn, and an admission that arrives later is closed at
// once.
func (g *Gate) Bind(ctx context.Context, runner string) bool {
	if !g.Budgeted() {
		g.mu.Lock()
		defer g.mu.Unlock()
		return !g.stopped && ctx.Err() == nil
	}
	g.mu.Lock()
	if g.stopped || ctx.Err() != nil {
		g.mu.Unlock()
		return false
	}
	g.mu.Unlock()

	p, err := g.cfg.Start(ctx, g.cfg.Coreslot, g.args(), g.cfg.Env)
	if err != nil {
		reason := ReasonStart
		if errors.Is(err, os.ErrNotExist) {
			reason = ReasonNotInstalled
		}
		g.latch(reason, err)
		g.mu.Lock()
		defer g.mu.Unlock()
		return !g.stopped
	}

	since := g.clock()
	g.mu.Lock()
	g.waiting[runner] = since
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.waiting, runner)
		g.mu.Unlock()
	}()

	deadline := since.Add(Timeout + answerSlack)
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case grant, ok := <-p.Admitted():
			if !ok {
				return g.endedFirst(runner, <-p.Done())
			}
			return g.admitted(ctx, runner, p, grant)
		case <-ctx.Done():
			g.abandon(p)
			return false
		case <-g.stop:
			g.abandon(p)
			return false
		case <-tick.C:
			if g.clock().Before(deadline) {
				continue
			}
			g.log.Warn("coreslot did not answer; starting the runner without the budget", slog.String("runner", runner))
			g.abandon(p)
			return g.bindUnbudgeted(runner, ReasonNoAnswer)
		}
	}
}

// admitted records an admission, unless ctx ended or Stop came first: then the
// hold is closed at once.
func (g *Gate) admitted(ctx context.Context, runner string, p Proc, grant Grant) bool {
	g.mu.Lock()
	if g.stopped || ctx.Err() != nil {
		g.mu.Unlock()
		p.Close()
		return false
	}
	h := &held{proc: p, grant: grant, granted: true, unbudgeted: grant.Unbudgeted}
	g.held[runner] = h
	if grant.Unbudgeted != "" {
		g.lastUnbg = grant.Unbudgeted
	}
	g.mu.Unlock()
	if grant.Unbudgeted != "" {
		g.log.Warn("coreslot ran the hold without the budget", slog.String("runner", runner), slog.String("unbudgeted", grant.Unbudgeted))
	} else {
		g.log.Info("coreslot admitted the runner", slog.String("runner", runner), slog.String("hold", grant.Hold))
	}
	go g.watch(runner, h)
	return true
}

// endedFirst handles a request whose child ended before it was admitted. A
// usage exit latches the gate; any other status starts this runner without the
// budget. Either way the runner starts, unless Stop came first.
func (g *Gate) endedFirst(runner string, status int) bool {
	if status == exitUsage {
		g.latch(ReasonTooOld, fmt.Errorf("coreslot exited %d", status))
		g.mu.Lock()
		defer g.mu.Unlock()
		return !g.stopped
	}
	reason := fmt.Sprintf("exit %d", status)
	g.log.Warn("the coreslot request ended before it was admitted; starting the runner without the budget",
		slog.String("runner", runner), slog.String("reason", reason))
	return g.bindUnbudgeted(runner, reason)
}

// bindUnbudgeted records runner as bound without the budget and without a
// hold: its request is gone. It reports false when Stop came first.
func (g *Gate) bindUnbudgeted(runner, reason string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return false
	}
	g.held[runner] = &held{unbudgeted: reason}
	g.lastUnbg = reason
	return true
}

// abandon withdraws a request nobody waits for any more, and closes the hold
// if coreslot admits it anyway.
func (g *Gate) abandon(p Proc) {
	p.Withdraw()
	go func() {
		admitted := p.Admitted()
		for {
			select {
			case _, ok := <-admitted:
				if ok {
					p.Close()
				}
				admitted = nil
			case <-p.Done():
				return
			}
		}
	}()
}

// watch marks a held runner's hold lost when its child ends before Unbind.
func (g *Gate) watch(runner string, h *held) {
	<-h.proc.Done()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held[runner] == h {
		h.lost = true
		g.log.Warn("the runner's hold ended before the runner", slog.String("runner", runner))
	}
}

// latch sets the unbudgeted latch with the binary's stat as it is now.
func (g *Gate) latch(reason string, err error) {
	fi, _ := os.Stat(g.cfg.Coreslot)
	g.mu.Lock()
	g.latched, g.reason, g.binary = true, reason, fi
	g.mu.Unlock()
	g.log.Error("running runners without the memory budget until coreslot changes",
		slog.String("reason", reason), slog.String("error", err.Error()))
}

// Budgeted reports whether runners wait for coreslot. While latched it checks
// the binary's stat (device, inode, size, mtime) and clears the latch when it
// changed. With no coreslot found it never clears.
func (g *Gate) Budgeted() bool {
	g.mu.Lock()
	if !g.latched {
		g.mu.Unlock()
		return true
	}
	path, before := g.cfg.Coreslot, g.binary
	g.mu.Unlock()
	if path == "" {
		return false
	}
	now, _ := os.Stat(path)
	if sameBinary(before, now) {
		return false
	}
	g.mu.Lock()
	if g.latched && g.binary == before {
		g.latched, g.reason, g.binary = false, "", nil
		g.log.Info("coreslot changed; runners wait for the memory budget again")
	}
	g.mu.Unlock()
	return true
}

// sameBinary reports whether two stats name the same, unchanged file; two
// missing files are the same.
func sameBinary(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// Unbind closes runner's hold. An unknown or still-waiting name is a no-op.
func (g *Gate) Unbind(runner string) {
	g.mu.Lock()
	h, ok := g.held[runner]
	delete(g.held, runner)
	g.mu.Unlock()
	if ok && h.proc != nil {
		h.proc.Close()
	}
}

// Grant is the grant runner was bound with, if coreslot ran its hold.
func (g *Gate) Grant(runner string) (Grant, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h, ok := g.held[runner]
	if !ok || !h.granted {
		return Grant{}, false
	}
	return h.grant, true
}

// Stop withdraws every waiting Bind, each of which returns false, and makes
// later Binds return false.
func (g *Gate) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.stopped {
		g.stopped = true
		close(g.stop)
	}
}

// Close stops the gate, then unbinds every held runner.
func (g *Gate) Close() {
	g.Stop()
	g.mu.Lock()
	names := make([]string, 0, len(g.held))
	for name := range g.held {
		names = append(names, name)
	}
	g.mu.Unlock()
	for _, name := range names {
		g.Unbind(name)
	}
}

// Health is the gate's state now.
func (g *Gate) Health() Health {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := Health{Waiting: len(g.waiting), Held: len(g.held), LastUnbudgeted: g.lastUnbg}
	now := g.clock()
	for _, since := range g.waiting {
		h.OldestWaitS = max(h.OldestWaitS, int(now.Sub(since)/time.Second))
	}
	lost := false
	for _, r := range g.held {
		if r.unbudgeted != "" {
			h.Unbudgeted++
		}
		lost = lost || r.lost
	}
	switch {
	case g.latched:
		h.State, h.Reason = "unbudgeted", g.reason
	case h.Waiting > 0:
		h.State = "waiting"
	case h.Held > 0:
		h.State = "held"
	default:
		h.State = "idle"
	}
	if !g.latched && lost {
		h.Reason = ReasonHoldLost
	}
	return h
}

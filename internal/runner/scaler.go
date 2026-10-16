package runner

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

// namePrefix starts every runner name and container name the pool creates.
const namePrefix = "local-ci-"

// Every call the scaler makes runs under the listener's context, which the
// listener strips of cancellation while it handles a message, so each is
// bounded here instead. They are variables so tests can shorten them.
var (
	// launchTimeout bounds one runner's start: minting its JIT configuration,
	// then creating, attaching to and starting its container. A start that
	// times out is a failed start, and its slot is released.
	launchTimeout = 2 * time.Minute
	// reapTimeout bounds reaping one idle runner: unregistering it, then
	// removing its container. A reap that times out leaves the runner idle,
	// still holding its slot, for the next call.
	reapTimeout = time.Minute
	// forgetTimeout bounds the best-effort unregistration of a runner that
	// never started.
	forgetTimeout = 30 * time.Second
)

// JITSource mints a runner's JIT configuration. *scaleset.Client satisfies it.
type JITSource interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
}

// Registry is the part of the scale set API a Scaler uses to unregister
// runners. *scaleset.Client satisfies it. GetRunnerByName returns nil and no
// error when no runner has the name.
type Registry interface {
	GetRunnerByName(ctx context.Context, runnerName string) (*scaleset.RunnerReference, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// ScalerConfig configures one scale set's Scaler.
type ScalerConfig struct {
	ScaleSetID int
	// Slots is the machine-wide limit, shared by every Scaler.
	Slots *Slots
	// Start starts a runner container with the given name and JIT
	// configuration, labelled with need when need is positive; the supervisor
	// binds it to StartRunner with the project's image, user and the runner
	// mount.
	Start func(ctx context.Context, name, jit string, need int) error
	JIT   JITSource
	// Registry unregisters runners before they are reaped, and runners whose
	// start failed after their JIT configuration was minted.
	Registry Registry
	// Docker removes reaped runners.
	Docker ContainerRemover
	Log    *slog.Logger
	// Gate admits each new runner into the machine's budget before its JIT
	// configuration is minted. Nil, or a gate that is not Enabled, starts
	// runners at once.
	Gate Gate
	// Project is the project's identity, passed to the gate.
	Project string
}

type runnerState int

const (
	// stateStarting: the slot is taken and the container is being started.
	// The zero value is no state, so a missing name never reads as one.
	stateStarting runnerState = iota + 1
	// stateIdle: the container was started and has not reported a job.
	stateIdle
	// stateBusy: the runner reported a job.
	stateBusy
	// stateReaping: an idle runner being unregistered and removed.
	stateReaping
	// stateDone: the job completed; the container is on its way out.
	stateDone
	// stateWaiting: the slot is taken and the runner's waiter goroutine is
	// in the gate's Admit. Nothing is minted yet.
	stateWaiting
	// stateCancelled: Desired or Drain gave a waiting runner up; its waiter
	// goroutine has not returned yet, and it keeps its slot until then.
	stateCancelled
)

// record is one runner the scaler holds a slot for.
type record struct {
	state runnerState
	// seq orders runners by creation; the newest waiters are given up first.
	seq uint64
	// cancel ends a waiting runner's Admit; nil for a runner started without
	// the gate.
	cancel context.CancelFunc
	// adm is what the gate gave; its Release is nil until then.
	adm Admitted
}

// Scaler keeps one scale set's runners. Every runner it knows holds one slot,
// given back exactly once, when the runner is forgotten: by release (Exited, a
// failed start, a reaped runner, an Admit error), or by admit, for a runner
// given up while it waited whose Admit then returned.
type Scaler struct {
	scaleSetID int
	slots      *Slots
	start      func(ctx context.Context, name, jit string, need int) error
	jit        JITSource
	registry   Registry
	docker     ContainerRemover
	log        *slog.Logger
	gate       Gate
	project    string
	names      func() (string, error)

	// abort ends every launch, reap and Admit in flight; see Abort.
	abort   context.Context
	aborted context.CancelFunc
	// wg counts the waiter goroutines.
	wg sync.WaitGroup

	mu       sync.Mutex
	runners  map[string]*record
	seq      uint64
	draining bool
	// noJob counts the runners in a row that went away without ever
	// reporting a job: a container that exited idle, or a start that failed
	// after its JIT configuration was minted. Any job report clears it.
	noJob int
}

// NewScaler returns a Scaler for one scale set.
func NewScaler(cfg ScalerConfig) *Scaler {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	abort, aborted := context.WithCancel(context.Background())
	return &Scaler{
		abort:      abort,
		aborted:    aborted,
		scaleSetID: cfg.ScaleSetID,
		slots:      cfg.Slots,
		start:      cfg.Start,
		jit:        cfg.JIT,
		registry:   cfg.Registry,
		docker:     cfg.Docker,
		log:        log.With(slog.Int("scaleSetID", cfg.ScaleSetID)),
		gate:       cfg.Gate,
		project:    cfg.Project,
		names:      newName,
		runners:    map[string]*record{},
	}
}

// countLocked counts the runners in the given states. s.mu is held.
func (s *Scaler) countLocked(states ...runnerState) int {
	n := 0
	for _, r := range s.runners {
		for _, want := range states {
			if r.state == want {
				n++
			}
		}
	}
	return n
}

// activeLocked counts the runners that can still take a job. s.mu is held.
func (s *Scaler) activeLocked() int {
	return s.countLocked(stateWaiting, stateStarting, stateIdle, stateBusy)
}

// Desired brings the scale set towards count runners. While draining it starts
// nothing. Otherwise it starts runners until idle plus busy reaches
// min(slots, count) or no slot is free, and removes idle runners that exceed
// count. It never waits for a slot: a count the global limit blocks is retried
// on the listener's next call. A start that fails ends this call's starts and
// is retried on the next. It returns the number of waiting, starting, idle and
// busy runners, and never an error.
//
// When the gate is enabled, a new runner is recorded waiting instead, and its
// own waiter goroutine admits it through the gate and then starts it; Desired
// never waits on the gate. Waiting runners are part of the excess, and are
// given up, newest first, before idle runners are removed.
func (s *Scaler) Desired(ctx context.Context, count int) (int, error) {
	count = max(count, 0)
	// Enabled reads the gate's settings: once per call, never under s.mu.
	gated := s.gate != nil && s.gate.Enabled()
	s.mu.Lock()
	need := min(s.slots.Cap(), count) - s.activeLocked()
	s.mu.Unlock()
	if gated {
		for range max(need, 0) {
			if !s.queue(ctx, count) {
				break
			}
		}
	} else {
		// need is fixed up front, so a runner that dies while it starts is not
		// replaced within the same call.
		for range max(need, 0) {
			name, ok := s.reserve(ctx, count)
			if !ok || !s.launch(ctx, name, 0) {
				break
			}
		}
	}
	s.reap(ctx, count)

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeLocked(), nil
}

// reserve takes a slot for a new runner if the scale set is below its target.
func (s *Scaler) reserve(ctx context.Context, count int) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.reserveLocked(ctx, count)
	if ok {
		s.runners[name] = &record{state: stateStarting, seq: s.nextSeqLocked()}
	}
	return name, ok
}

// queue takes a slot for a new runner if the scale set is below its target,
// records it waiting, and starts its waiter goroutine.
func (s *Scaler) queue(ctx context.Context, count int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.reserveLocked(ctx, count)
	if !ok {
		return false
	}
	waitCtx, cancel := context.WithCancel(s.abort)
	s.runners[name] = &record{state: stateWaiting, seq: s.nextSeqLocked(), cancel: cancel}
	s.wg.Add(1)
	go s.admit(waitCtx, cancel, name)
	return true
}

// reserveLocked names a new runner and takes a slot for it if the scale set
// is below its target. The caller records it. s.mu is held.
func (s *Scaler) reserveLocked(ctx context.Context, count int) (string, bool) {
	if ctx.Err() != nil {
		return "", false
	}
	if s.draining || s.activeLocked() >= min(s.slots.Cap(), count) {
		return "", false
	}
	name, err := s.names()
	if err != nil {
		s.log.Error("naming a runner", slog.String("error", err.Error()))
		return "", false
	}
	if _, taken := s.runners[name]; taken {
		return "", false
	}
	if !s.slots.TryAcquire() {
		return "", false
	}
	return name, true
}

// nextSeqLocked numbers a new runner. s.mu is held.
func (s *Scaler) nextSeqLocked() uint64 {
	s.seq++
	return s.seq
}

// admit is a waiting runner's goroutine. It asks the gate to admit the runner
// and, if the runner is still waiting and the scaler is not draining, starts
// it with the need the gate admitted. A runner given up meanwhile is dropped
// and its grant released; one already released through Exited has no slot
// left to free. An Admit error gives the runner up, which is never a no-job
// exit and never starts it.
func (s *Scaler) admit(ctx context.Context, cancel context.CancelFunc, name string) {
	defer s.wg.Done()
	defer cancel()
	adm, err := s.gate.Admit(ctx, Admission{Runner: name, Project: s.project})
	if err != nil {
		reason := "cancelled"
		if errors.Is(err, ErrCancelledByBudget) {
			reason = "coreslot_cancel"
		}
		s.log.Info("runner given up before admission", slog.String("runner", name), slog.String("reason", reason))
		s.release(name, false)
		return
	}
	s.mu.Lock()
	r, ok := s.runners[name]
	switch {
	case ok && r.state == stateWaiting && !s.draining:
		r.state = stateStarting
		r.adm = adm
		s.mu.Unlock()
		// The launch runs on its own ctx, bounded by launchTimeout and
		// Abort, never on the wait ctx: a drain lets a container creation
		// that already began finish.
		s.launch(context.Background(), name, adm.Need)
		return
	case ok:
		delete(s.runners, name)
		s.slots.Release()
	}
	s.mu.Unlock()
	if adm.Release != nil {
		adm.Release()
	}
}

// launch mints the runner's JIT configuration and starts its container,
// within launchTimeout. On failure it unregisters the runner if its
// configuration was minted, then releases its slot through Exited. A Drain that
// lands while the configuration is minted is honoured the same way: the runner
// is unregistered and released instead of started.
func (s *Scaler) launch(ctx context.Context, name string, need int) bool {
	ctx, cancel := s.bounded(ctx, launchTimeout)
	defer cancel()
	minted, err := s.startRunner(ctx, name, need)
	if err != nil {
		drained := errors.Is(err, errDrained)
		if drained {
			s.log.Info("draining; not starting the runner", slog.String("runner", name))
		} else {
			s.log.Error("starting a runner", slog.String("runner", name), slog.String("error", err.Error()))
		}
		if minted {
			s.forget(ctx, name)
		}
		// A runner that was registered and then could not start counts
		// towards the no-job exits: a missing image fails every start.
		s.release(name, minted && !drained)
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The container may already have exited and been forgotten.
	if r, ok := s.runners[name]; ok && r.state == stateStarting {
		r.state = stateIdle
	}
	s.log.Info("runner started", slog.String("runner", name))
	return true
}

// errDrained stops a start whose scaler began draining while it minted.
var errDrained = errors.New("the scaler is draining")

// startRunner reports whether the JIT configuration was minted, which
// registers the runner on GitHub, and any error.
func (s *Scaler) startRunner(ctx context.Context, name string, need int) (minted bool, err error) {
	s.mu.Lock()
	id := s.scaleSetID
	s.mu.Unlock()
	cfg, err := s.jit.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, id)
	if err != nil {
		return false, fmt.Errorf("minting the JIT configuration: %w", err)
	}
	if cfg == nil || cfg.EncodedJITConfig == "" {
		return true, errors.New("minting the JIT configuration: the response is empty")
	}
	// reserve checked draining before the mint; check again before a
	// container exists.
	s.mu.Lock()
	draining := s.draining
	s.mu.Unlock()
	if draining {
		return true, errDrained
	}
	return true, s.start(ctx, name, cfg.EncodedJITConfig, need)
}

// forget unregisters a runner that never started, so no unused registration
// is left behind. It is best effort: a failure is logged.
func (s *Scaler) forget(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forgetTimeout)
	defer cancel()
	if err := s.unregister(ctx, name); err != nil {
		s.log.Warn("unregistering a runner that did not start", slog.String("runner", name), slog.String("error", err.Error()))
	}
}

// unregister removes the runner's registration on GitHub. A runner GitHub does
// not know counts as unregistered. A runner that is running a job is refused
// with an error matching scaleset.JobStillRunningError.
func (s *Scaler) unregister(ctx context.Context, name string) error {
	if s.registry == nil {
		return errors.New("no runner registry is configured")
	}
	ref, err := s.registry.GetRunnerByName(ctx, name)
	if err != nil {
		return fmt.Errorf("looking up the runner: %w", err)
	}
	if ref == nil {
		return nil
	}
	if err := s.registry.RemoveRunner(ctx, int64(ref.ID)); err != nil && !errors.Is(err, scaleset.RunnerNotFoundError) {
		return fmt.Errorf("unregistering runner %d: %w", ref.ID, err)
	}
	return nil
}

// reap gives up waiting runners and removes idle runners beyond count. Waiting
// runners go first, newest first: each is marked cancelled and its Admit's ctx
// ended, and its waiter goroutine frees its slot. An idle runner may already
// be running a job, because JobStarted reaches the listener late, so each one is
// unregistered on GitHub first: GitHub refuses that for a runner with a job,
// and the runner is then kept and marked busy. Only an unregistered runner's
// container is removed. A removed container is gone, so its slot is released
// at once; its die event then finds nothing to release. Any other failure
// leaves the runner idle for the next call.
func (s *Scaler) reap(ctx context.Context, count int) {
	s.mu.Lock()
	excess := min(s.countLocked(stateWaiting, stateStarting, stateIdle), s.activeLocked()-count)
	var waiting []*record
	for _, r := range s.runners {
		if r.state == stateWaiting {
			waiting = append(waiting, r)
		}
	}
	slices.SortFunc(waiting, func(a, b *record) int { return cmp.Compare(b.seq, a.seq) })
	for _, r := range waiting[:min(max(excess, 0), len(waiting))] {
		r.state = stateCancelled
		r.cancel()
		excess--
	}
	var victims []string
	for name, r := range s.runners {
		if len(victims) >= excess {
			break
		}
		if r.state == stateIdle {
			r.state = stateReaping
			victims = append(victims, name)
		}
	}
	s.mu.Unlock()

	for _, name := range victims {
		s.reapOne(ctx, name)
	}
}

// reapOne unregisters one idle runner and removes its container, within
// reapTimeout.
func (s *Scaler) reapOne(ctx context.Context, name string) {
	ctx, cancel := s.bounded(ctx, reapTimeout)
	defer cancel()
	err := s.unregister(ctx, name)
	switch {
	case errors.Is(err, scaleset.JobStillRunningError):
		s.log.Info("idle runner is running a job; keeping it", slog.String("runner", name))
		s.keepBusy(name)
		return
	case err != nil:
		s.log.Warn("unregistering an idle runner", slog.String("runner", name), slog.String("error", err.Error()))
		s.keepIdle(name)
		return
	}
	if err := RemoveContainer(ctx, s.docker, name); err != nil {
		s.log.Warn("removing an idle runner", slog.String("runner", name), slog.String("error", err.Error()))
		s.keepIdle(name)
		return
	}
	s.log.Info("idle runner removed", slog.String("runner", name))
	s.release(name, false)
}

// bounded limits ctx to d and to Abort.
func (s *Scaler) bounded(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, d)
	stop := context.AfterFunc(s.abort, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// keepBusy marks a runner GitHub reports as running a job busy, unless its
// container exited meanwhile.
func (s *Scaler) keepBusy(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runners[name]; ok && r.state == stateReaping {
		r.state = stateBusy
		s.slots.SetBusy(1)
	}
}

// keepIdle returns a runner whose reaping failed to idle, for the next call,
// unless its container exited meanwhile.
func (s *Scaler) keepIdle(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runners[name]; ok && r.state == stateReaping {
		r.state = stateIdle
	}
}

// Started marks a runner busy. An unknown or empty name is logged and ignored:
// after a restart the first message can name runners of the previous process.
func (s *Scaler) Started(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A job report, even a late one for a runner already gone, proves that
	// runners can take jobs.
	s.noJob = 0
	r, ok := s.runners[name]
	switch {
	case !ok:
		s.log.Info("job started on a runner this scaler does not know", slog.String("runner", name))
	case r.state == stateBusy:
	case r.state == stateDone:
		s.log.Info("job started on a runner that already completed", slog.String("runner", name))
	default:
		r.state = stateBusy
		s.slots.SetBusy(1)
	}
}

// Completed clears a runner's busy mark. The runner keeps its slot until its
// container exits. An unknown or empty name is logged and ignored.
func (s *Scaler) Completed(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noJob = 0
	r, ok := s.runners[name]
	switch {
	case !ok:
		s.log.Info("job completed on a runner this scaler does not know", slog.String("runner", name))
	case r.state == stateDone:
	default:
		if r.state == stateBusy {
			s.slots.SetBusy(-1)
		}
		r.state = stateDone
	}
}

// Exited forgets a runner whose container is gone and releases its slot. It is
// idempotent per name: every Scaler on the machine hears every exit, and a name
// it does not hold is logged and ignored.
// A runner that exits after its start and before any job report counts
// towards NoJobExits.
func (s *Scaler) Exited(name string) { s.release(name, false) }

// release forgets a runner and gives its slot back. The runner counts towards
// NoJobExits if it was idle (started, no job reported) or if failedStart says
// it was registered and could not start. A waiting runner's Admit is ended.
// The runner's grant is detached under s.mu and released after unlocking.
func (s *Scaler) release(name string, failedStart bool) {
	s.mu.Lock()
	r, ok := s.runners[name]
	if !ok {
		s.mu.Unlock()
		s.log.Debug("exit of a runner this scaler does not hold", slog.String("runner", name))
		return
	}
	delete(s.runners, name)
	if r.state == stateBusy {
		s.slots.SetBusy(-1)
	}
	if (r.state == stateIdle || failedStart) && !s.draining {
		s.noJob++
	}
	s.slots.Release()
	if r.state == stateWaiting && r.cancel != nil {
		r.cancel()
	}
	adm := r.adm
	s.mu.Unlock()
	if adm.Release != nil {
		adm.Release()
	}
}

// NoJobExits is the number of runners in a row that went away without
// reporting a job. A job report on this scale set, or ResetNoJobExits after a
// passed preflight, clears it.
func (s *Scaler) NoJobExits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.noJob
}

// ResetNoJobExits clears NoJobExits.
func (s *Scaler) ResetNoJobExits() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noJob = 0
}

// Abort ends every launch, reap and Admit in flight, and makes later ones fail
// at once: a drain past its bound uses it so that nothing waits on a start or an
// unregistration it is about to undo. A start already minted still forgets
// its runner, within forgetTimeout.
//
// Abort never blocks: it ends every waiting runner's Admit and returns. Join
// waits for their goroutines.
func (s *Scaler) Abort() { s.aborted() }

// Join waits up to timeout for every waiter goroutine to return, and reports
// whether all did. A waiter goroutine admits its runner and then launches it,
// so one in a failed start's forget can outlast a short timeout. Call it after
// Drain, once no new runner can start waiting: after Abort alone, Desired can
// still add a waiter goroutine while Join waits.
func (s *Scaler) Join(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// Drain stops new starts: Desired starts nothing from now on, every waiting
// runner is given up (its Admit's ctx ended), and a start whose JIT
// configuration is being minted is unregistered and released instead of
// started. A start already creating its container completes. Desired still
// reaps idle runners, and Exited still releases slots; the supervisor's drain
// waits for Held() to be empty.
func (s *Scaler) Drain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draining = true
	for _, r := range s.runners {
		if r.state == stateWaiting {
			r.state = stateCancelled
			r.cancel()
		}
	}
}

// SetScaleSetID points the scaler at a scale set recreated under a new ID;
// later starts mint their JIT configuration for it. Held runners keep their
// slots.
func (s *Scaler) SetScaleSetID(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scaleSetID = id
}

// Idle is the number of runners started, or starting, that have not reported
// a job.
func (s *Scaler) Idle() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.countLocked(stateStarting, stateIdle)
}

// HeldRunner is one runner a Scaler holds a slot for. Starting is true while
// its container may not exist yet: it is starting, waiting for the gate, or
// given up while waiting.
type HeldRunner struct {
	Name     string
	Starting bool
}

// Held lists every runner holding a slot, in any state, sorted by name. A
// drain waits for it to be empty; the supervisor's reconcile releases a held
// runner that is not Starting and whose container is gone.
func (s *Scaler) Held() []HeldRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]HeldRunner, 0, len(s.runners))
	for name, r := range s.runners {
		starting := r.state == stateStarting || r.state == stateWaiting || r.state == stateCancelled
		out = append(out, HeldRunner{Name: name, Starting: starting})
	}
	slices.SortFunc(out, func(a, b HeldRunner) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// BudgetCounts is the number of runners waiting for the gate, and the number
// of runners the gate admitted, by mode: ModeQueued and ModeFailOpen, both
// always present. Runners admitted with ModeOff are not counted.
func (s *Scaler) BudgetCounts() (waiting int, admitted map[Mode]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	admitted = map[Mode]int{ModeQueued: 0, ModeFailOpen: 0}
	for _, r := range s.runners {
		if r.state == stateWaiting {
			waiting++
		}
		if _, counted := admitted[r.adm.Mode]; counted {
			admitted[r.adm.Mode]++
		}
	}
	return waiting, admitted
}

// BusyCount is the number of this scale set's runners running a job.
func (s *Scaler) BusyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.countLocked(stateBusy)
}

func newName() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return namePrefix + hex.EncodeToString(b), nil
}

var (
	_ JITSource = (*scaleset.Client)(nil)
	_ Registry  = (*scaleset.Client)(nil)
)

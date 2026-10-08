package runner

import (
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
	// configuration; the supervisor binds it to StartRunner with the
	// project's image, user and the runner mount.
	Start func(ctx context.Context, name, jit string) error
	JIT   JITSource
	// Registry unregisters runners before they are reaped, and runners whose
	// start failed after their JIT configuration was minted.
	Registry Registry
	// Docker removes reaped runners.
	Docker ContainerRemover
	Log    *slog.Logger
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
)

// Scaler keeps one scale set's runners. Every runner it knows holds one slot,
// and Exited is the only place a slot is given back.
type Scaler struct {
	scaleSetID int
	slots      *Slots
	start      func(ctx context.Context, name, jit string) error
	jit        JITSource
	registry   Registry
	docker     ContainerRemover
	log        *slog.Logger

	mu       sync.Mutex
	runners  map[string]runnerState
	draining bool
}

// NewScaler returns a Scaler for one scale set.
func NewScaler(cfg ScalerConfig) *Scaler {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Scaler{
		scaleSetID: cfg.ScaleSetID,
		slots:      cfg.Slots,
		start:      cfg.Start,
		jit:        cfg.JIT,
		registry:   cfg.Registry,
		docker:     cfg.Docker,
		log:        log.With(slog.Int("scaleSetID", cfg.ScaleSetID)),
		runners:    map[string]runnerState{},
	}
}

// countLocked counts the runners in the given states. s.mu is held.
func (s *Scaler) countLocked(states ...runnerState) int {
	n := 0
	for _, st := range s.runners {
		for _, want := range states {
			if st == want {
				n++
			}
		}
	}
	return n
}

// activeLocked counts the runners that can still take a job. s.mu is held.
func (s *Scaler) activeLocked() int {
	return s.countLocked(stateStarting, stateIdle, stateBusy)
}

// Desired brings the scale set towards count runners. While draining it starts
// nothing. Otherwise it starts runners until idle plus busy reaches
// min(slots, count) or no slot is free, and removes idle runners that exceed
// count. It never waits for a slot: a count the global limit blocks is retried
// on the listener's next call. A start that fails ends this call's starts and
// is retried on the next. It returns idle plus busy and never an error.
func (s *Scaler) Desired(ctx context.Context, count int) (int, error) {
	count = max(count, 0)
	s.mu.Lock()
	need := min(s.slots.Cap(), count) - s.activeLocked()
	s.mu.Unlock()
	// need is fixed up front, so a runner that dies while it starts is not
	// replaced within the same call.
	for range max(need, 0) {
		name, ok := s.reserve(ctx, count)
		if !ok || !s.launch(ctx, name) {
			break
		}
	}
	s.reap(ctx, count)

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeLocked(), nil
}

// reserve takes a slot for a new runner if the scale set is below its target.
func (s *Scaler) reserve(ctx context.Context, count int) (string, bool) {
	if ctx.Err() != nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining || s.activeLocked() >= min(s.slots.Cap(), count) {
		return "", false
	}
	name, err := newName()
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
	s.runners[name] = stateStarting
	return name, true
}

// launch mints the runner's JIT configuration and starts its container,
// within launchTimeout. On failure it unregisters the runner if its
// configuration was minted, then releases its slot through Exited. A Drain that
// lands while the configuration is minted is honoured the same way: the runner
// is unregistered and released instead of started.
func (s *Scaler) launch(ctx context.Context, name string) bool {
	ctx, cancel := context.WithTimeout(ctx, launchTimeout)
	defer cancel()
	minted, err := s.startRunner(ctx, name)
	if err != nil {
		if errors.Is(err, errDrained) {
			s.log.Info("draining; not starting the runner", slog.String("runner", name))
		} else {
			s.log.Error("starting a runner", slog.String("runner", name), slog.String("error", err.Error()))
		}
		if minted {
			s.forget(ctx, name)
		}
		s.Exited(name)
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The container may already have exited and been forgotten.
	if st, ok := s.runners[name]; ok && st == stateStarting {
		s.runners[name] = stateIdle
	}
	s.log.Info("runner started", slog.String("runner", name))
	return true
}

// errDrained stops a start whose scaler began draining while it minted.
var errDrained = errors.New("the scaler is draining")

// startRunner reports whether the JIT configuration was minted, which
// registers the runner on GitHub, and any error.
func (s *Scaler) startRunner(ctx context.Context, name string) (minted bool, err error) {
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
	return true, s.start(ctx, name, cfg.EncodedJITConfig)
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

// reap removes idle runners beyond count. An idle runner may already be running
// a job, because JobStarted reaches the listener late, so each one is
// unregistered on GitHub first: GitHub refuses that for a runner with a job,
// and the runner is then kept and marked busy. Only an unregistered runner's
// container is removed. A removed container is gone, so its slot is released
// at once; its die event then finds nothing to release. Any other failure
// leaves the runner idle for the next call.
func (s *Scaler) reap(ctx context.Context, count int) {
	s.mu.Lock()
	excess := min(s.countLocked(stateStarting, stateIdle), s.activeLocked()-count)
	var victims []string
	for name, st := range s.runners {
		if len(victims) >= excess {
			break
		}
		if st == stateIdle {
			s.runners[name] = stateReaping
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
	ctx, cancel := context.WithTimeout(ctx, reapTimeout)
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
	s.Exited(name)
}

// keepBusy marks a runner GitHub reports as running a job busy, unless its
// container exited meanwhile.
func (s *Scaler) keepBusy(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.runners[name]; ok && st == stateReaping {
		s.runners[name] = stateBusy
		s.slots.SetBusy(1)
	}
}

// keepIdle returns a runner whose reaping failed to idle, for the next call,
// unless its container exited meanwhile.
func (s *Scaler) keepIdle(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.runners[name]; ok && st == stateReaping {
		s.runners[name] = stateIdle
	}
}

// Started marks a runner busy. An unknown or empty name is logged and ignored:
// after a restart the first message can name runners of the previous process.
func (s *Scaler) Started(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.runners[name]
	switch {
	case !ok:
		s.log.Info("job started on a runner this scaler does not know", slog.String("runner", name))
	case st == stateBusy:
	case st == stateDone:
		s.log.Info("job started on a runner that already completed", slog.String("runner", name))
	default:
		s.runners[name] = stateBusy
		s.slots.SetBusy(1)
	}
}

// Completed clears a runner's busy mark. The runner keeps its slot until its
// container exits. An unknown or empty name is logged and ignored.
func (s *Scaler) Completed(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.runners[name]
	switch {
	case !ok:
		s.log.Info("job completed on a runner this scaler does not know", slog.String("runner", name))
	case st == stateDone:
	default:
		if st == stateBusy {
			s.slots.SetBusy(-1)
		}
		s.runners[name] = stateDone
	}
}

// Exited forgets a runner whose container is gone and releases its slot. It is
// the only release point and is idempotent per name: every Scaler on the
// machine hears every exit, and a name it does not hold is logged and ignored.
func (s *Scaler) Exited(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.runners[name]
	if !ok {
		s.log.Debug("exit of a runner this scaler does not hold", slog.String("runner", name))
		return
	}
	delete(s.runners, name)
	if st == stateBusy {
		s.slots.SetBusy(-1)
	}
	s.slots.Release()
}

// Drain stops new starts: Desired starts nothing from now on, and a start
// whose JIT configuration is being minted is unregistered and released instead
// of started. A start already creating its container completes. Desired still
// reaps idle runners, and Exited still releases slots; the supervisor's drain
// (T8) waits for InUse() to reach 0.
func (s *Scaler) Drain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draining = true
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
// its container may not exist yet.
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
	for name, st := range s.runners {
		out = append(out, HeldRunner{Name: name, Starting: st == stateStarting})
	}
	slices.SortFunc(out, func(a, b HeldRunner) int { return strings.Compare(a.Name, b.Name) })
	return out
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

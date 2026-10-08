package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

// namePrefix starts every runner name and container name the pool creates.
const namePrefix = "local-ci-"

// forgetTimeout bounds the best-effort unregistration of a runner that never
// started.
const forgetTimeout = 30 * time.Second

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

// launch mints the runner's JIT configuration and starts its container. On
// failure it unregisters the runner if its configuration was minted, then
// releases its slot through Exited.
func (s *Scaler) launch(ctx context.Context, name string) bool {
	minted, err := s.startRunner(ctx, name)
	if err != nil {
		s.log.Error("starting a runner", slog.String("runner", name), slog.String("error", err.Error()))
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

// startRunner reports whether the JIT configuration was minted, which
// registers the runner on GitHub, and any error.
func (s *Scaler) startRunner(ctx context.Context, name string) (minted bool, err error) {
	cfg, err := s.jit.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, s.scaleSetID)
	if err != nil {
		return false, fmt.Errorf("minting the JIT configuration: %w", err)
	}
	if cfg == nil || cfg.EncodedJITConfig == "" {
		return true, errors.New("minting the JIT configuration: the response is empty")
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
		err := s.unregister(ctx, name)
		switch {
		case errors.Is(err, scaleset.JobStillRunningError):
			s.log.Info("idle runner is running a job; keeping it", slog.String("runner", name))
			s.mu.Lock()
			if st, ok := s.runners[name]; ok && st == stateReaping {
				s.runners[name] = stateBusy
				s.slots.SetBusy(1)
			}
			s.mu.Unlock()
			continue
		case err != nil:
			s.log.Warn("unregistering an idle runner", slog.String("runner", name), slog.String("error", err.Error()))
			s.keepIdle(name)
			continue
		}
		if err := RemoveRunner(ctx, s.docker, name); err != nil {
			s.log.Warn("removing an idle runner", slog.String("runner", name), slog.String("error", err.Error()))
			s.keepIdle(name)
			continue
		}
		s.log.Info("idle runner removed", slog.String("runner", name))
		s.Exited(name)
	}
}

// keepIdle returns a runner whose reaping failed to idle, for the next call.
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

// Drain stops all further starts.
func (s *Scaler) Drain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draining = true
}

// Idle is the number of runners started, or starting, that have not reported
// a job.
func (s *Scaler) Idle() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.countLocked(stateStarting, stateIdle)
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

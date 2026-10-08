package supervisor

import (
	"context"
	"fmt"

	"github.com/moby/moby/api/types/container"

	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
)

// A runner container of this instance that is still running when the pool
// starts is a survivor: the previous process was killed (a logout or a
// shutdown, kill -9, or launchd's 60 s exit timeout during a drain) while its
// job ran, and the job can still finish and report. The pool leaves it running
// and holds one of the machine's slots for it until its container dies, so the
// machine is not oversubscribed. No scaler knows its name (runner names are
// never reused), so the supervisor keeps the survivors itself; the exit
// watcher and the reconcile release each one exactly once, and only a
// survivor a slot was held for gives one back.
//
// A survivor's job still counts in its scale set's assigned jobs, so the
// listener's desired count includes it and the scaler may start one idle
// runner per survivor, within the slots left. Such a runner takes no job,
// holds its slot like any other, and is reaped when the count drops.

// sweep removes the containers a previous run of this instance left that are
// not running (created, exited or dead), and only those. A running one is kept
// as a survivor. A running container without a runner name is not one this
// pool started, and is removed as before.
func (s *Supervisor) sweep(ctx context.Context) error {
	list, err := s.listContainers(ctx)
	if err != nil {
		return fmt.Errorf("sweeping orphaned runner containers: %w", err)
	}
	for _, c := range list {
		if c.Labels[runner.LabelInstance] != s.instance {
			continue
		}
		name := c.Labels[runner.LabelRunner]
		if name != "" && !sweepable(c.State) {
			s.keepSurvivor(name)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, callTimeout)
		err := runner.RemoveContainer(rctx, s.deps.Docker, c.ID)
		cancel()
		if err != nil {
			s.log.Warn("removing an orphaned runner container", "container", c.ID, "error", err.Error())
			continue
		}
		s.log.Info("removed an orphaned runner container", "runner", name)
	}
	return nil
}

// sweepable reports whether a container in state st has no job to finish.
func sweepable(st container.ContainerState) bool {
	switch st {
	case container.StateCreated, container.StateExited, container.StateDead:
		return true
	}
	return false
}

// keepSurvivor records a running runner of the previous run and holds a slot
// for it if one is free. With more survivors than slots the rest run without
// one, and the machine is oversubscribed until enough of them exit.
func (s *Supervisor) keepSurvivor(name string) {
	s.mu.Lock()
	if _, ok := s.survivors[name]; ok {
		s.mu.Unlock()
		return
	}
	held := s.slots.TryAcquire()
	s.survivors[name] = held
	s.mu.Unlock()
	if held {
		s.log.Info("left a runner of the previous run running to finish its job; it holds a slot until it exits", "runner", name)
		return
	}
	s.log.Warn("left a runner of the previous run running to finish its job, but every slot is held: the Mac runs more runners than slots until enough of them exit", "runner", name)
}

// releaseSurvivor forgets a survivor and gives back the slot held for it, if
// any. It reports whether name was a survivor; a second call for the same name
// does nothing, so a replayed die event and a reconcile never release twice.
func (s *Supervisor) releaseSurvivor(name string) bool {
	s.mu.Lock()
	held, ok := s.survivors[name]
	delete(s.survivors, name)
	slots := s.slots
	s.mu.Unlock()
	if ok && held {
		slots.Release()
	}
	return ok
}

// survivorNames returns the current survivors' names.
func (s *Supervisor) survivorNames() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.survivors))
	for name := range s.survivors {
		out[name] = true
	}
	return out
}

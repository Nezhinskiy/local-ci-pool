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
// listener's desired count includes it. Each project's scaler counts that
// project's survivors against the count (survivorsOf), so it starts no idle
// runner beside one. A survivor of an older pool names no project and counts
// for none: its project's scaler may start one idle runner for its job, within
// the slots left, which takes no job and is reaped when the count drops.

// survivor is one runner of a previous run kept at start.
type survivor struct {
	held    bool   // a slot was taken for it
	project string // its project label; empty for a runner of an older pool
}

// sweep removes the containers a previous run of this instance left that are
// not running (created, exited or dead), and only those. A running one is kept
// as a survivor, with the project its labels name. A running container
// without a runner name is not one this pool started, and is removed as
// before.
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
			// A survivor of an older pool names no project: it counts for
			// none.
			s.keepSurvivor(name, c.Labels[runner.LabelProject])
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
func (s *Supervisor) keepSurvivor(name, project string) {
	s.mu.Lock()
	if _, ok := s.survivors[name]; ok {
		s.mu.Unlock()
		return
	}
	held := s.slots.TryAcquire()
	s.survivors[name] = &survivor{held: held, project: project}
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
	defer s.mu.Unlock()
	e, ok := s.survivors[name]
	if !ok {
		return false
	}
	delete(s.survivors, name)
	if e.held {
		s.slots.Release()
	}
	return true
}

// removeSurvivors removes the containers of the survivors whose labels name
// project, for a repository that turned public, and releases each one
// removed, all within ctx. One whose removal fails is kept, and logged.
func (s *Supervisor) removeSurvivors(ctx context.Context, project string) {
	s.mu.Lock()
	var names []string
	for name, e := range s.survivors {
		if e.project == project {
			names = append(names, name)
		}
	}
	s.mu.Unlock()
	for _, name := range names {
		c, cancel := context.WithTimeout(ctx, callTimeout)
		err := runner.RemoveContainer(c, s.deps.Docker, name)
		cancel()
		if err != nil {
			s.log.Warn("removing a survivor of a repository no longer private", "runner", name, "error", err.Error())
			continue
		}
		s.log.Info("removed a survivor of a repository no longer private", "runner", name)
		s.releaseSurvivor(name)
	}
}

// survivorsOf counts the survivors whose labels name project. A survivor of
// an older pool names none, so it counts for no project.
func (s *Supervisor) survivorsOf(project string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.survivors {
		if e.project == project {
			n++
		}
	}
	return n
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

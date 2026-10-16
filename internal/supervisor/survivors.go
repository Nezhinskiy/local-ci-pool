package supervisor

import (
	"context"
	"fmt"
	"strconv"

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
// With the budget on (Deps.Budget enabled at start), each survivor is also
// adopted into it, under the project and the need its labels name, once the
// exit watcher has subscribed; its adoption is released with its slot. With
// the budget off, survivors are kept as above and never adopted.
//
// A survivor's job still counts in its scale set's assigned jobs, so the
// listener's desired count includes it and the scaler may start one idle
// runner per survivor, within the slots left. Such a runner takes no job,
// holds its slot like any other, and is reaped when the count drops.

// maxNeed is the largest need a need label may name.
const maxNeed = 256

// survivor is one runner of a previous run kept at start.
type survivor struct {
	held    bool        // a slot was taken for it
	project string      // its project label; empty for a runner of an older pool
	need    int         // its need label in 1..256, else 0
	mode    runner.Mode // set by its adoption
	release func()      // set by its adoption; nil before
}

// sweep removes the containers a previous run of this instance left that are
// not running (created, exited or dead), and only those. A running one is kept
// as a survivor, with the project and the need its labels name. A running
// container without a runner name is not one this pool started, and is
// removed as before. With the budget on, sweep returns the survivors' names:
// they are adopted into the budget once the exit watcher has subscribed
// (adoptSurvivors), never here.
func (s *Supervisor) sweep(ctx context.Context) ([]string, error) {
	list, err := s.listContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("sweeping orphaned runner containers: %w", err)
	}
	var kept []string
	for _, c := range list {
		if c.Labels[runner.LabelInstance] != s.instance {
			continue
		}
		name := c.Labels[runner.LabelRunner]
		if name != "" && !sweepable(c.State) {
			// A survivor of an older pool names no project: it is adopted
			// with none (the gate labels it as the pool's own) and counts
			// under no project in the health.
			if s.keepSurvivor(name, c.Labels[runner.LabelProject], survivorNeed(c.Labels)) {
				kept = append(kept, name)
			}
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
	if s.deps.Budget == nil || !s.deps.Budget.Enabled() {
		return nil, nil
	}
	return kept, nil
}

// survivorNeed is the need a survivor's labels name, when it is a number in
// 1..256; otherwise 0, the configured need.
func survivorNeed(labels map[string]string) int {
	n, err := strconv.Atoi(labels[runner.LabelNeed])
	if err != nil || n < 1 || n > maxNeed {
		return 0
	}
	return n
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
// one, and the machine is oversubscribed until enough of them exit. It
// reports whether name was recorded now.
func (s *Supervisor) keepSurvivor(name, project string, need int) bool {
	s.mu.Lock()
	if _, ok := s.survivors[name]; ok {
		s.mu.Unlock()
		return false
	}
	held := s.slots.TryAcquire()
	s.survivors[name] = &survivor{held: held, project: project, need: need}
	s.mu.Unlock()
	if held {
		s.log.Info("left a runner of the previous run running to finish its job; it holds a slot until it exits", "runner", name)
		return true
	}
	s.log.Warn("left a runner of the previous run running to finish its job, but every slot is held: the Mac runs more runners than slots until enough of them exit", "runner", name)
	return true
}

// adoptSurvivors adopts each survivor still recorded into the budget, one at
// a time. Adopt runs outside s.mu (the gate bounds it). A survivor released
// meanwhile (its die event, a reconcile) has its adoption released at once,
// after unlocking; otherwise the entry keeps the adoption's mode and Release.
// A survivor whose adoption failed open runs on, unbudgeted.
func (s *Supervisor) adoptSurvivors(ctx context.Context, names []string) {
	for _, name := range names {
		// After the pool's life ended an Adopt could only fail open: the
		// survivors left keep running unbudgeted, as before adoption.
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		e, ok := s.survivors[name]
		var a runner.Admission
		if ok {
			a = runner.Admission{Runner: name, Project: e.project, Need: e.need}
		}
		s.mu.Unlock()
		if !ok {
			continue
		}
		adm := s.deps.Budget.Adopt(ctx, a)
		s.mu.Lock()
		if s.survivors[name] == e {
			e.mode, e.release = adm.Mode, adm.Release
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()
		if adm.Release != nil {
			adm.Release()
		}
	}
}

// releaseSurvivor forgets a survivor, gives back the slot held for it, if
// any, and releases its adoption, after unlocking. It reports whether name
// was a survivor; a second call for the same name does nothing, so a replayed
// die event and a reconcile never release twice.
func (s *Supervisor) releaseSurvivor(name string) bool {
	s.mu.Lock()
	e, ok := s.survivors[name]
	delete(s.survivors, name)
	var release func()
	if ok {
		release, e.release = e.release, nil
	}
	slots := s.slots
	s.mu.Unlock()
	if ok && e.held {
		slots.Release()
	}
	if release != nil {
		release()
	}
	return ok
}

// survivorCountsLocked counts the survivors admitted into the budget, by the
// project their labels name and by the mode of their adoption: adopted or
// fail_open. A survivor of an older pool names no project: it is counted
// under "", which no served project's identity is (discovery skips an empty
// one), so it counts under none. s.mu is held.
func (s *Supervisor) survivorCountsLocked() map[string]AdmittedHealth {
	out := map[string]AdmittedHealth{}
	for _, e := range s.survivors {
		c := out[e.project]
		switch e.mode {
		case runner.ModeAdopted:
			c.Adopted++
		case runner.ModeFailOpen:
			c.FailOpen++
		default:
			continue
		}
		out[e.project] = c
	}
	return out
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

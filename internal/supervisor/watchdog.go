package supervisor

import (
	"context"
	"fmt"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/Nezhinskiy/local-ci-pool/internal/machine"
	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
)

// watchdog pings Docker every watchdogEvery. A changed fingerprint, or
// maxPingFailures failed pings in a row, is a fatal non-terminal error: the
// pool exits and launchd starts it again, deriving the slots anew. Every
// successful ping, and every resubscription of the exit watcher, reconciles
// the held runners with the live containers.
func (s *Supervisor) watchdog(ctx context.Context) {
	failures := 0
	tick := s.deps.Clock.After(watchdogEvery)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.reconcile:
			s.reconcileOnce(ctx)
			continue
		case <-tick:
			tick = s.deps.Clock.After(watchdogEvery)
		}
		info, err := s.info(ctx)
		if err != nil {
			failures++
			s.setDocker(false)
			s.log.Warn("Docker did not answer", "failures", failures, "error", err.Error())
			if failures >= maxPingFailures {
				s.fail(fmt.Errorf("docker did not answer %d pings in a row: %w", failures, err))
				return
			}
			continue
		}
		failures = 0
		s.setDocker(true)
		if fp := fingerprintOf(info); fp != s.machine.FP {
			s.fail(fmt.Errorf("docker changed under the pool (%s, now %s); restarting to derive the slots again", fpString(s.machine.FP), fpString(fp)))
			return
		}
		s.reconcileOnce(ctx)
	}
}

func (s *Supervisor) info(ctx context.Context) (system.Info, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := s.deps.Docker.Info(ctx, client.InfoOptions{})
	if err != nil {
		return system.Info{}, err
	}
	return res.Info, nil
}

func fingerprintOf(info system.Info) machine.Fingerprint {
	return machine.Fingerprint{ID: info.ID, Mem: info.MemTotal, CPUs: info.NCPU}
}

func fpString(fp machine.Fingerprint) string {
	return fmt.Sprintf("id %s, %d bytes, %d CPUs", fp.ID, fp.Mem, fp.CPUs)
}

func (s *Supervisor) setDocker(ok bool) {
	s.mu.Lock()
	s.dockerOK = ok
	s.mu.Unlock()
}

// reconcileOnce compares the runners the scalers hold with this instance's
// containers. A held runner past its start whose container is gone is
// released: its die event was lost (a Docker restart, a failed cleanup). A
// container no scaler holds, before or after the listing, is removed.
func (s *Supervisor) reconcileOnce(ctx context.Context) {
	before := s.held()
	list, err := s.listContainers(ctx)
	if err != nil {
		s.log.Warn("listing runner containers to reconcile", "error", err.Error())
		return
	}
	live := map[string]bool{}
	for _, c := range list {
		if c.State != container.StateExited && c.State != container.StateDead {
			live[c.Labels[runner.LabelRunner]] = true
		}
	}
	for name, h := range before {
		if !h.starting && !live[name] {
			s.log.Warn("a runner's container is gone without an exit event; releasing its slot", "runner", name)
			h.scaler.Exited(name)
		}
	}
	after := s.held()
	for _, c := range list {
		name := c.Labels[runner.LabelRunner]
		if _, ok := before[name]; ok {
			continue
		}
		if _, ok := after[name]; ok {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, callTimeout)
		err := runner.RemoveContainer(rctx, s.deps.Docker, c.ID)
		cancel()
		if err != nil {
			s.log.Warn("removing a container no runner holds", "container", c.ID, "error", err.Error())
			continue
		}
		s.log.Warn("removed a container no runner holds", "runner", name)
	}
}

type heldRunner struct {
	scaler   *runner.Scaler
	starting bool
}

func (s *Supervisor) held() map[string]heldRunner {
	out := map[string]heldRunner{}
	for _, sc := range s.scalers() {
		for _, r := range sc.Held() {
			out[r.Name] = heldRunner{scaler: sc, starting: r.Starting}
		}
	}
	return out
}

func (s *Supervisor) listContainers(ctx context.Context) ([]container.Summary, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := s.deps.Docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", runner.LabelInstance+"="+s.instance),
	})
	if err != nil {
		return nil, err
	}
	return res.Items, nil
}

// releases checks for a new runner release every releaseEvery. A new version
// becomes the mount of the next started runners.
func (s *Supervisor) releases(ctx context.Context, arch string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.deps.Clock.After(releaseEvery):
		}
		m, err := s.deps.Mount(ctx, arch)
		if t := loginRejected(err); t != nil {
			s.fail(t)
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("checking for a new runner release", "error", err.Error())
			}
			continue
		}
		s.mu.Lock()
		old := s.mount
		s.mount = m
		s.mu.Unlock()
		if m.Version != old.Version {
			s.log.Info("new runner release; the next runners use it", "from", old.Version, "to", m.Version)
		}
	}
}

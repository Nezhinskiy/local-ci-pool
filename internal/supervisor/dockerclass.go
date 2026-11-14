package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/moby/moby/client"

	"github.com/Nezhinskiy/local-ci-pool/internal/discovery"
	"github.com/Nezhinskiy/local-ci-pool/internal/runnermount"
	"github.com/Nezhinskiy/local-ci-pool/internal/vm"
)

// The docker class (docs/docker-class.md): a repository whose marker has a
// docker block, read at its default branch, is served a second time, outside
// probe mode, by a project of its own. Its scale set is
// <identity>-docker-<machine>, labelled <identity>-docker; its "image" is the
// repository's base VM, built from the default branch only; each of its
// runners runs in a throwaway clone of that base (Deps.VM). Like the container
// class's project it runs its own visibility checks and its own drain, so it
// leaves no later than the container class would. It shares nothing with it
// but the identity: one machine-wide docker slot (one VM at a time) instead of the
// container slots, no budget gate (a VM has its own CPUs), no survivors, no
// heartbeat (the routing action does not route docker jobs), and no
// container, so the Docker die events and the reconcile never see its runners:
// a runner's exit is its Launch's done callback.

// errNotInstalled is the docker class's setup on a Mac without limactl: a
// state of the Mac, not a failure (health's not_installed), tried again on
// every discovery pass.
var errNotInstalled = errors.New("docker class: limactl not found")

// dockerKey is the suffix of a docker-class project's key in s.projects.
const dockerKey = " docker"

// servesDocker reports whether p gets a docker-class project: its marker has
// the block, the marker was read at the default branch, and this is no probe.
func (s *Supervisor) servesDocker(p discovery.Project) bool {
	return !s.cfg.Probe && p.Marker.Docker != nil && p.Ref == p.DefaultBranch
}

// baseKey is what the base is built from: the docker block's inputs at the
// default branch's commit and its provision path, Lima's template and version,
// the runner tree's image (the actions/runner release and the entrypoint), and
// the VM's size. A change of any rebuilds it. The Docker Engine version the
// build installs is recorded beside the key (ensureBase): the template takes
// whatever Docker's install script gives, and a new runner release, which
// changes the key, rebuilds regularly enough to pick up a newer one.
// It returns the key and the commit the inputs were read at.
func (pr *project) baseKey(ctx context.Context, p discovery.Project, m runnermount.Mount) (key, commit string, err error) {
	s := pr.s
	b := s.deps.VM
	if b == nil {
		return "", "", errors.New("docker class: no VM backend")
	}
	ver, err := b.Lima.Run(ctx, nil, "--version")
	if errors.Is(err, exec.ErrNotFound) {
		return "", "", errNotInstalled
	}
	if err != nil {
		return "", "", fmt.Errorf("docker class: %w", err)
	}
	tree, err := s.deps.Docker.ImageInspect(ctx, m.Spec.Source)
	if err != nil {
		return "", "", fmt.Errorf("docker base: inspecting the runner tree %s: %w", m.Spec.Source, err)
	}
	// The default branch, never p.Ref: a pull request's or a probe's provision
	// never reaches a base.
	commit, err = s.deps.Mirror.Fetch(ctx, p.Repo, p.DefaultBranch)
	if err != nil {
		return "", "", fmt.Errorf("docker base: fetching %s@%s: %w", p.Repo, p.DefaultBranch, err)
	}
	d := p.Marker.Docker
	digest, err := s.deps.Mirror.InputsDigest(ctx, s.deps.Mirror.Dir(p.Repo), commit, d.Inputs)
	if err != nil {
		return "", "", fmt.Errorf("docker base: digesting the docker inputs of %s: %w", p.Repo, err)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		digest, d.Provision, vm.Template, vm.Recipe, strings.TrimSpace(string(ver)), tree.ID,
		fmt.Sprintf("%d/%d/%d", b.CPUs, b.MemoryGiB, b.DiskGiB),
	}, "\x00")))
	return hex.EncodeToString(sum[:]), commit, nil
}

// ensureBase builds the project's base when its key differs from the one the
// last build stored, and stores the new key. A failed build keeps the old base
// (vm.Backend.BuildBase) and the old key. One base builds at a time on the
// machine. It returns the base as the project's Image.
func (pr *project) ensureBase(ctx context.Context, p discovery.Project, m runnermount.Mount) (Image, error) {
	key, commit, err := pr.baseKey(ctx, p, m)
	if err != nil {
		return Image{}, err
	}
	name, err := pr.s.deps.VM.BaseName(pr.identity)
	if err != nil {
		return Image{}, fmt.Errorf("docker base: %w", err)
	}
	img := Image{Ref: name + "@" + key[:12]}
	path := pr.s.baseKeyPath(pr.identity)
	if old, err := os.ReadFile(path); err == nil && strings.SplitN(string(old), "\n", 2)[0] == key {
		// The key is trusted only while its base is there: a base deleted by hand, or a wiped ~/.lima,
		// is built again rather than served.
		ok, err := pr.s.deps.VM.HasBase(ctx, pr.identity)
		if err != nil { // an unreadable list is no reason to rebuild: try again on the next pass
			return Image{}, fmt.Errorf("docker base: %w", err)
		}
		if ok {
			return img, nil
		}
	}
	// A build is a VM too: it takes the machine's one docker-VM slot, so it
	// waits for a running job and no job starts while it builds. A VM that may
	// still run (leakVM) stops builds too.
	for {
		if err := pr.s.vmLeakErr(); err != nil {
			return Image{}, fmt.Errorf("docker base: %w", err)
		}
		if pr.s.vmSlots.TryAcquire() {
			break
		}
		select {
		case <-ctx.Done():
			return Image{}, ctx.Err()
		case <-time.After(vmSlotPoll):
		}
	}
	defer pr.s.vmSlots.Release()
	pr.log.Info("building the docker base", "commit", commit, "runner", m.Version)
	engine, err := pr.buildBase(ctx, p, commit, m)
	if err != nil {
		return Image{}, fmt.Errorf("docker base: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Image{}, fmt.Errorf("docker base: storing its key: %w", err)
	}
	if err := os.WriteFile(path, []byte(key+"\nengine="+engine+"\n"), 0o600); err != nil {
		return Image{}, fmt.Errorf("docker base: storing its key: %w", err)
	}
	pr.log.Info("the docker base is built", "base", img.Ref, "engine", engine)
	return img, nil
}

func (pr *project) buildBase(ctx context.Context, p discovery.Project, commit string, m runnermount.Mount) (string, error) {
	s := pr.s
	src, err := s.deps.Mirror.Archive(ctx, s.deps.Mirror.Dir(p.Repo), commit, p.Marker.Docker.Inputs)
	if err != nil {
		return "", fmt.Errorf("archiving the docker inputs: %w", err)
	}
	defer func() { _ = src.Close() }()
	tree, err := runnermount.Tree(ctx, s.deps.Docker, m)
	if err != nil {
		return "", fmt.Errorf("exporting the runner tree: %w", err)
	}
	defer func() { _ = tree.Close() }()
	return s.deps.VM.BuildBase(ctx, pr.identity, tree, src, p.Marker.Docker.Provision)
}

// baseKeyPath is where the key of identity's base is kept. Deleting it makes
// the next discovery pass rebuild the base.
func (s *Supervisor) baseKeyPath(identity string) string {
	return filepath.Join(s.deps.StateDir, "docker-base-"+identity+".key")
}

// refreshBase is a docker-class project's refresh: the base is rebuilt when
// its key changed. A failed rebuild keeps the old base, and so the runners,
// and marks the project unhealthy until one passes.
func (pr *project) refreshBase(p discovery.Project) {
	img, err := pr.ensureBase(pr.ctx, p, pr.s.currentMount())
	if pr.ctx.Err() != nil {
		return
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.p = p
	if err != nil {
		pr.log.Warn("refreshing the docker base", "error", err.Error())
		pr.imgErr = err.Error()
		return
	}
	pr.img, pr.imgErr = img, ""
}

// startVM is a docker-class scaler's Start: the runner in a clone of the base.
// The clone's end is the runner's exit. A refusal (too little free disk, say)
// is the project's reason until a launch succeeds; the scaler's failed-start
// path unregisters the runner and frees the docker slot, and the job stays
// queued.
func (pr *project) startVM(ctx context.Context, name, jit string, _ int) error {
	err := pr.s.deps.VM.Launch(ctx, pr.identity, name, jit, func(err error) {
		if err != nil {
			pr.log.Info("a docker VM's runner ended", "runner", name, "error", err.Error())
		}
		if errors.Is(err, vm.ErrDeleteFailed) {
			// The clone may still run: its runner keeps the slot, and no docker runner starts
			// anywhere until a restart's sweep deletes it. health() reports it.
			pr.s.leakVM(err)
			return
		}
		if sc := pr.getScaler(); sc != nil {
			sc.Exited(name)
		}
	})
	if errors.Is(err, vm.ErrDeleteFailed) {
		pr.s.leakVM(err)
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.launchErr, pr.diskShort = "", errors.Is(err, vm.ErrDiskShort)
	if err != nil {
		pr.launchErr = "docker vm: " + err.Error()
	}
	return err
}

// vmRemover is a docker-class scaler's remover: removing a runner deletes its
// clone, which ends the runner. A clone already gone is no error.
type vmRemover struct{ b *vm.Backend }

func (r vmRemover) ContainerRemove(ctx context.Context, name string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	return client.ContainerRemoveResult{}, r.b.Remove(ctx, name)
}

// sweepVMs deletes the job clones a killed pool left, once at start. One it
// could not list or delete may still run, so the class then starts no VM
// until the pool restarts, as after a failed delete (leakVM). A probe
// never does: the main pool's clones may be running.
func (s *Supervisor) sweepVMs(ctx context.Context) {
	if s.cfg.Probe || s.deps.VM == nil {
		return
	}
	deleted, err := s.deps.VM.Sweep(ctx)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		s.log.Debug("no docker VMs to sweep: limactl not found")
	case err != nil:
		s.leakVM(fmt.Errorf("sweeping the docker VMs a previous run left: %w", err))
	}
	for _, name := range deleted {
		s.log.Info("deleted a docker VM left by a previous run", "vm", name)
	}
}

// vmSlotPoll is how often a base build asks for the docker-VM slot while a
// job holds it.
const vmSlotPoll = 200 * time.Millisecond

// vetFor is the scaler's Vet: the repository's visibility, and for the docker
// class the free disk too, before a runner is registered. A short disk thus
// registers nothing (no runner minted and dropped on every listener poll), and
// its reason stays the project's until a launch succeeds.
func (pr *project) vetFor() func(context.Context) error {
	if !pr.docker {
		return pr.vet
	}
	return func(ctx context.Context) error {
		if err := pr.vet(ctx); err != nil {
			return err
		}
		err := pr.s.vmLeakErr()
		if err == nil {
			err = pr.s.deps.VM.DiskOK()
		}
		pr.mu.Lock()
		defer pr.mu.Unlock()
		pr.diskShort = errors.Is(err, vm.ErrDiskShort)
		if err != nil {
			pr.launchErr = "docker vm: " + err.Error()
			return err
		}
		pr.launchErr = "" // the disk recovered: the reason goes with it
		return nil
	}
}

// leakVM records a job clone whose delete failed: it may still run, so no
// docker runner starts on this machine until a restart's sweep deletes it.
func (s *Supervisor) leakVM(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vmLeak == "" {
		s.log.Error("a docker VM may still run; no docker VM starts until the pool restarts", "error", err.Error())
		s.vmLeak = err.Error()
	}
}

func (s *Supervisor) vmLeakErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vmLeak == "" {
		return nil
	}
	return errors.New(s.vmLeak + "; restart the pool once it is gone (limactl list)")
}

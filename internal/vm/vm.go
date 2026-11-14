// Package vm runs docker-class jobs, each in a throwaway Lima VM (docs/docker-class.md).
//
// A repository's base VM is built from its default branch and then only cloned: BuildBase creates it in
// plain mode (no host mounts, no port forwarding, no containerd or Rosetta shares), installs the runner tree,
// runs the repository's provision script as root, stops it, and swaps it in for the old base. Launch makes a
// copy-on-write clone of the stopped base, boots it, starts the runner with its JIT configuration on stdin,
// and deletes the clone once the runner exits or its deadline passes. Nothing a job writes survives it: its
// writes, image builds and caches live in its own clone. Sweep deletes what a killed pool left behind.
//
// Every VM name carries the pool instance, so a probe pool and the main one on the same Mac never touch
// each other's VMs.
//
// Contracts for the caller:
//   - BuildBase and Launch of one identity never run at once: a clone made during the swap fails.
//   - A build is a VM too: it takes the machine's one docker-VM slot, like a job.
//   - The error Launch hands to done can end with text the job wrote (it is root in its VM): treat it like
//     build output, never in a redacted health body.
package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	prefix = "local-ci-docker-"
	// Template is the Lima template every base starts from: Ubuntu, with Docker Engine installed by Docker's
	// install script on first boot. The Engine version is therefore not pinned: the caller reads it back
	// from the built base and records it in the base's key.
	Template = "template:docker-rootful"
	// RunnerUser runs the job inside the VM, in group docker: a job has a Docker daemon, which is root in its
	// own VM and nowhere else.
	RunnerUser = "runner"
	// TreeDir is where the base holds the runner tree that entrypoint.sh copies from, as in a container.
	TreeDir = "/opt/local-ci"
	// SourceDir is where the default branch's archive lies while provision runs; it is removed afterwards.
	SourceDir = "/opt/local-ci-source"
	// MaxNameLen bounds every VM name: Lima keeps sockets under ~/.lima/<name>/, and a Unix socket path is
	// limited to about 104 bytes on macOS.
	MaxNameLen = 50
	// DefaultJobTimeout bounds a job VM when Backend.JobTimeout is zero: GitHub's default job timeout of
	// six hours, plus a margin for the boot and the runner's own start.
	DefaultJobTimeout = 6*time.Hour + 10*time.Minute
	// DefaultDeleteTimeout bounds one delete when Backend.DeleteTimeout is zero.
	DefaultDeleteTimeout = 2 * time.Minute
)

// Lima runs one limactl command, with stdin when it is not nil, and returns its standard output.
type Lima interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
}

// Exec is the real Lima: the limactl binary on PATH, or at Path. limactl shell-quotes each argument it passes
// to the guest, so an argument reaches the guest as one word.
type Exec struct{ Path string }

// Run implements Lima.
func (e Exec) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	bin := e.Path
	if bin == "" {
		bin = "limactl"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = stdin
	// limactl shell's ssh child can hold the output pipes after limactl itself is killed.
	cmd.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("limactl %s: %w: %s", args[0], err, strings.TrimSpace(lastLine(stderr.String())))
	}
	return stdout.Bytes(), nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Backend builds bases and runs job clones for one pool instance.
type Backend struct {
	Lima Lima
	// Instance is the pool instance ("main", or "probe-<owner>"), part of every VM name.
	Instance string
	// CPUs, MemoryGiB and DiskGiB size every VM; a clone inherits them from its base.
	CPUs, MemoryGiB, DiskGiB int
	// MinFreeGiB is the free disk a build or a launch needs before it starts a VM: below it, it refuses.
	MinFreeGiB int
	// FreeGiB reports the free disk where Lima keeps its instances.
	FreeGiB func() (float64, error)
	// JobTimeout and DeleteTimeout bound a job VM and one delete; zero means the defaults above.
	JobTimeout, DeleteTimeout time.Duration
	// Log receives what fails during cleanup; nil discards it.
	Log *slog.Logger
}

// ErrDiskShort is the refusal of a build or a launch when the disk is below MinFreeGiB.
var ErrDiskShort = errors.New("not enough free disk for a docker VM")

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (b *Backend) name(kind, id string) (string, error) {
	n := prefix + kind + "-" + b.Instance + "-" + id
	if !nameRE.MatchString(b.Instance) || !nameRE.MatchString(id) {
		return "", fmt.Errorf("docker VM name %q: the instance and the identity must be [a-z0-9-]", n)
	}
	if len(n) > MaxNameLen {
		return "", fmt.Errorf("docker VM name %q is longer than %d characters", n, MaxNameLen)
	}
	return n, nil
}

// BaseName is the base VM of a repository identity.
func (b *Backend) BaseName(identity string) (string, error) { return b.name("base", identity) }

// JobName is the clone of a runner.
func (b *Backend) JobName(runner string) (string, error) { return b.name("job", runner) }

// provisionRE is a second layer: provision reaches the guest as its own argument, never spliced into a
// script, but a path of these characters needs no quoting anywhere.
var provisionRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)

// provisionScript runs $2 inside $1 as root, only when $2 resolves to itself: no component of the path may
// be a symlink. That is best effort, since provision is the default branch's root code in a throwaway VM.
const provisionScript = `cd "$1" && p="$1/$2" && r="$(realpath -e -- "$p")" && [ "$r" = "$p" ] && [ -f "$r" ] && exec bash "$r"`

// BuildBase builds identity's base from runnerTree (a tar of the runner tree) and source, running provision
// (a path inside source) as root. source must be the default branch's archive of the docker block's inputs,
// never a pull request's or probe mode's ref. It builds under its own name, and swaps the new base in only
// once it is complete and stopped: a failed build, or a failed swap, keeps the old base. It returns the
// Docker Engine version of the new base, for the caller's record of the base's key.
func (b *Backend) BuildBase(ctx context.Context, identity string, runnerTree, source io.Reader, provision string) (string, error) {
	if !provisionRE.MatchString(provision) || strings.Contains(provision, "..") {
		return "", fmt.Errorf("provision path %q is not a plain relative path of letters, digits and ._/-", provision)
	}
	base, err := b.BaseName(identity)
	if err != nil {
		return "", err
	}
	building, err := b.name("building", identity)
	if err != nil {
		return "", err
	}
	if err := b.diskOK(); err != nil {
		return "", err
	}
	_ = b.remove(ctx, building) // a build a killed pool left behind
	steps := []struct {
		stdin io.Reader
		args  []string
	}{
		{nil, []string{"create", "--tty=false", "--plain", "--name=" + building,
			fmt.Sprintf("--cpus=%d", b.CPUs), fmt.Sprintf("--memory=%d", b.MemoryGiB), fmt.Sprintf("--disk=%d", b.DiskGiB), Template}},
		{nil, []string{"start", "--tty=false", building}},
		{nil, []string{"shell", building, "--", "sudo", "useradd", "--uid", "1001", "--create-home", "--groups", "docker", RunnerUser}},
		{runnerTree, []string{"shell", building, "--", "sudo", "sh", "-c", `mkdir -p "$1" && tar -x -C "$1"`, "sh", TreeDir}},
		{source, []string{"shell", building, "--", "sudo", "sh", "-c", `mkdir -p "$1" && tar -x -C "$1"`, "sh", SourceDir}},
		{nil, []string{"shell", building, "--", "sudo", "sh", "-c", provisionScript, "sh", SourceDir, provision}},
		{nil, []string{"shell", building, "--", "sudo", "rm", "-rf", SourceDir}},
		{nil, []string{"shell", building, "--", "sudo", "docker", "version", "--format", "{{.Server.Version}}"}},
		{nil, []string{"stop", building}},
	}
	var engine string
	for i, s := range steps {
		out, err := b.Lima.Run(ctx, s.stdin, s.args...)
		if err != nil {
			_ = b.remove(context.WithoutCancel(ctx), building)
			return "", fmt.Errorf("building the docker base of %s: %w", identity, err)
		}
		if i == len(steps)-2 {
			engine = strings.TrimSpace(string(out))
		}
	}
	if engine == "" {
		_ = b.remove(context.WithoutCancel(ctx), building)
		return "", fmt.Errorf("building the docker base of %s: the base reports no Docker Engine version", identity)
	}
	return engine, b.swap(context.WithoutCancel(ctx), identity, base, building)
}

// swap puts building in place of base. The old base is renamed aside first and deleted only once the new
// one is in place; a failure on the way renames it back. It runs to the end even when the build's context
// is cancelled.
func (b *Backend) swap(ctx context.Context, identity, base, building string) error {
	old, err := b.name("old", identity)
	if err != nil {
		return err
	}
	names, err := b.list(ctx)
	if err != nil {
		return fmt.Errorf("installing the docker base of %s: %w", identity, err)
	}
	fail := func(err error) error { return fmt.Errorf("installing the docker base of %s: %w", identity, err) }
	hadBase := names[base]
	if hadBase {
		_ = b.remove(ctx, old)
		if _, err := b.Lima.Run(ctx, nil, "unprotect", base); err != nil {
			return fail(err)
		}
		if _, err := b.Lima.Run(ctx, nil, "rename", base, old); err != nil {
			_, _ = b.Lima.Run(ctx, nil, "protect", base)
			return fail(err)
		}
	}
	if _, err := b.Lima.Run(ctx, nil, "rename", building, base); err != nil {
		if hadBase {
			if _, rerr := b.Lima.Run(ctx, nil, "rename", old, base); rerr != nil {
				b.logf("the old docker base could not be put back", "identity", identity, "error", rerr.Error())
			} else {
				_, _ = b.Lima.Run(ctx, nil, "protect", base)
			}
		}
		return fail(err)
	}
	// limactl protect refuses a removal of the base, so nothing deletes it by accident.
	if _, err := b.Lima.Run(ctx, nil, "protect", base); err != nil {
		return fmt.Errorf("protecting the docker base of %s: %w", identity, err)
	}
	if hadBase {
		_ = b.remove(ctx, old)
	}
	return nil
}

func (b *Backend) diskOK() error {
	free, err := b.FreeGiB()
	if err != nil {
		return fmt.Errorf("reading free disk: %w", err)
	}
	if free < float64(b.MinFreeGiB) {
		return fmt.Errorf("%w: %.1f GiB free, needs %d", ErrDiskShort, free, b.MinFreeGiB)
	}
	return nil
}

// Launch clones identity's base for runner and boots the clone, then starts the runner with jit on stdin
// and returns, before the runner has registered. done is called once the runner has exited, or its
// deadline passed, and its clone is deleted: with nil on a clean exit, else the runner's error joined with
// any delete error.
func (b *Backend) Launch(ctx context.Context, identity, runner, jit string, done func(error)) error {
	if strings.ContainsAny(jit, "\n\r") || jit == "" {
		return errors.New("the JIT configuration must be one non-empty line")
	}
	base, err := b.BaseName(identity)
	if err != nil {
		return err
	}
	job, err := b.JobName(runner)
	if err != nil {
		return err
	}
	if err := b.diskOK(); err != nil {
		return err
	}
	for _, args := range [][]string{{"clone", "--tty=false", base, job}, {"start", "--tty=false", job}} {
		if _, err := b.Lima.Run(ctx, nil, args...); err != nil {
			_ = b.remove(context.WithoutCancel(ctx), job)
			return fmt.Errorf("starting docker VM %s: %w", job, err)
		}
	}
	// The job outlives the launch's context, but not its own deadline.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), or(b.JobTimeout, DefaultJobTimeout))
	go func() {
		defer cancel()
		_, err := b.Lima.Run(runCtx, strings.NewReader(jit+"\n"),
			"shell", job, "--", "sudo", "-u", RunnerUser, "-H", TreeDir+"/entrypoint.sh")
		done(errors.Join(err, b.remove(context.WithoutCancel(ctx), job)))
	}()
	return nil
}

// Remove deletes runner's clone, running or not, within DeleteTimeout: for a reap, an abort or a drain.
func (b *Backend) Remove(ctx context.Context, runner string) error {
	job, err := b.JobName(runner)
	if err != nil {
		return err
	}
	return b.remove(ctx, job)
}

func (b *Backend) remove(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, or(b.DeleteTimeout, DefaultDeleteTimeout))
	defer cancel()
	if _, err := b.Lima.Run(ctx, nil, "delete", "--force", name); err != nil {
		b.logf("deleting a docker VM failed", "vm", name, "error", err.Error())
		return err
	}
	return nil
}

// Sweep cleans what a killed pool of this instance left behind: it deletes every job clone and unfinished
// build, and resolves an interrupted swap (an old base set aside is put back when no base is in place, and
// deleted otherwise).
func (b *Backend) Sweep(ctx context.Context) ([]string, error) {
	names, err := b.list(ctx)
	if err != nil {
		return nil, err
	}
	scope := func(kind string) string { return prefix + kind + "-" + b.Instance + "-" }
	var deleted []string
	for n := range names {
		switch {
		case strings.HasPrefix(n, scope("job")), strings.HasPrefix(n, scope("building")):
			if b.remove(ctx, n) == nil {
				deleted = append(deleted, n)
			}
		case strings.HasPrefix(n, scope("old")):
			base := scope("base") + strings.TrimPrefix(n, scope("old"))
			if names[base] {
				if b.remove(ctx, n) == nil {
					deleted = append(deleted, n)
				}
			} else if _, err := b.Lima.Run(ctx, nil, "rename", n, base); err == nil {
				_, _ = b.Lima.Run(ctx, nil, "protect", base)
			}
		}
	}
	return deleted, nil
}

func (b *Backend) list(ctx context.Context) (map[string]bool, error) {
	out, err := b.Lima.Run(ctx, nil, "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var inst struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(line, &inst); err != nil {
			return nil, fmt.Errorf("reading limactl list: %w", err)
		}
		names[inst.Name] = true
	}
	return names, nil
}

func (b *Backend) logf(msg string, args ...any) {
	if b.Log != nil {
		b.Log.Warn(msg, args...)
	}
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// FreeGiBAt reports the free disk of the file system holding dir, for Backend.FreeGiB.
func FreeGiBAt(dir string) func() (float64, error) {
	return func() (float64, error) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			return 0, err
		}
		return float64(st.Bavail) * float64(st.Bsize) / (1 << 30), nil
	}
}

// Package vm runs docker-class jobs, each in a throwaway Lima VM (docs/docker-class.md).
//
// A repository's base VM is built once from its default branch and then only cloned: BuildBase creates it in
// plain mode (no host mounts, no port forwarding), installs the runner tree, runs the repository's
// provision script as root, and stops it. Launch makes a copy-on-write clone of the stopped base, boots it,
// starts the runner with its JIT configuration on stdin, and deletes the clone once the runner exits.
// Nothing a job writes survives it: its writes, image builds and caches live in its own clone. Sweep deletes
// the clones a killed pool left behind.
package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
)

const (
	// BasePrefix names a repository's base VM: BasePrefix + identity.
	BasePrefix = "local-ci-docker-base-"
	// JobPrefix names a job's clone: JobPrefix + runner name.
	JobPrefix = "local-ci-docker-job-"
	// Template is the Lima template every base starts from: Ubuntu with rootful Docker Engine.
	Template = "template:docker-rootful"
	// RunnerUser runs the job inside the VM, in group docker: a job has a Docker daemon, which is root in its
	// own VM and nowhere else.
	RunnerUser = "runner"
	// TreeDir is where the base holds the runner tree that entrypoint.sh copies from, as in a container.
	TreeDir = "/opt/local-ci"
	// SourceDir is where the default branch's archive lies while provision runs; it is removed afterwards.
	SourceDir = "/opt/local-ci-source"
)

// Lima runs one limactl command, with stdin when it is not nil, and returns its standard output.
type Lima interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
}

// Exec is the real Lima: the limactl binary on PATH, or at Path.
type Exec struct{ Path string }

// Run implements Lima.
func (e Exec) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	bin := e.Path
	if bin == "" {
		bin = "limactl"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("limactl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(lastLine(stderr.String())))
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

// Backend builds bases and runs job clones.
type Backend struct {
	Lima Lima
	// CPUs, MemoryGiB and DiskGiB size every VM; the clone inherits them from its base.
	CPUs, MemoryGiB, DiskGiB int
	// MinFreeGiB is the free disk Launch needs before it clones: below it, it refuses.
	MinFreeGiB int
	// FreeGiB reports the free disk where Lima keeps its instances.
	FreeGiB func() (float64, error)
}

// ErrDiskShort is Launch's refusal when the disk is below MinFreeGiB.
var ErrDiskShort = errors.New("not enough free disk for a docker VM")

// BaseName is the base VM of a repository identity.
func BaseName(identity string) string { return BasePrefix + identity }

// JobName is the clone of a runner.
func JobName(runner string) string { return JobPrefix + runner }

// provisionRE is what a provision path may be: limactl shell joins its arguments into one remote shell
// command, so the path is held to characters that need no quoting at all.
var provisionRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)

// BuildBase builds identity's base from runnerTree (a tar of the runner tree) and source, running provision
// (a path inside source) as root. source must be the default branch's archive of the docker block's inputs,
// never a pull request's or probe mode's ref: the caller reads the block from the default branch too. A
// provision that is a symlink is refused inside the VM before it runs. It builds under a temporary name and
// replaces the old base only once the new one is complete and stopped, so a failed build keeps the old base.
func (b *Backend) BuildBase(ctx context.Context, identity string, runnerTree, source io.Reader, provision string) error {
	if !provisionRE.MatchString(provision) || strings.Contains(provision, "..") {
		return fmt.Errorf("provision path %q is not a plain relative path of letters, digits and ._/-", provision)
	}
	name, building := BaseName(identity), BaseName(identity)+"-building"
	_, _ = b.Lima.Run(ctx, nil, "delete", "--force", building) // a build a killed pool left behind
	steps := []struct {
		stdin io.Reader
		args  []string
	}{
		{nil, []string{"create", "--tty=false", "--plain", "--name=" + building,
			fmt.Sprintf("--cpus=%d", b.CPUs), fmt.Sprintf("--memory=%d", b.MemoryGiB), fmt.Sprintf("--disk=%d", b.DiskGiB), Template}},
		{nil, []string{"start", "--tty=false", building}},
		{nil, []string{"shell", building, "--", "sudo", "useradd", "--uid", "1001", "--create-home", "--groups", "docker", RunnerUser}},
		{runnerTree, []string{"shell", building, "--", "sudo", "sh", "-c", "mkdir -p " + TreeDir + " && tar -x -C " + TreeDir}},
		{source, []string{"shell", building, "--", "sudo", "sh", "-c", "mkdir -p " + SourceDir + " && tar -x -C " + SourceDir}},
		{nil, []string{"shell", building, "--", "sudo", "sh", "-c", "cd " + SourceDir + " && test -f " + provision + " && test ! -L " + provision + " && bash " + provision}},
		{nil, []string{"shell", building, "--", "sudo", "rm", "-rf", SourceDir}},
		{nil, []string{"stop", building}},
	}
	for _, s := range steps {
		if _, err := b.Lima.Run(ctx, s.stdin, s.args...); err != nil {
			_, _ = b.Lima.Run(context.WithoutCancel(ctx), nil, "delete", "--force", building)
			return fmt.Errorf("building the docker base of %s: %w", identity, err)
		}
	}
	_, _ = b.Lima.Run(ctx, nil, "unprotect", name)
	_, _ = b.Lima.Run(ctx, nil, "delete", "--force", name)
	if _, err := b.Lima.Run(ctx, nil, "rename", building, name); err != nil {
		return fmt.Errorf("installing the docker base of %s: %w", identity, err)
	}
	// Nothing deletes or boots the base by accident; Launch only clones it.
	if _, err := b.Lima.Run(ctx, nil, "protect", name); err != nil {
		return fmt.Errorf("protecting the docker base of %s: %w", identity, err)
	}
	return nil
}

// Launch clones identity's base for runner, boots the clone and starts the runner with jit on stdin. It
// returns once the runner has started; done is called with the runner's error (nil on a clean exit) after
// the runner exits and its clone is deleted.
func (b *Backend) Launch(ctx context.Context, identity, runner, jit string, done func(error)) error {
	if strings.ContainsAny(jit, "\n\r") || jit == "" {
		return errors.New("the JIT configuration must be one non-empty line")
	}
	free, err := b.FreeGiB()
	if err != nil {
		return fmt.Errorf("reading free disk: %w", err)
	}
	if free < float64(b.MinFreeGiB) {
		return fmt.Errorf("%w: %.1f GiB free, needs %d", ErrDiskShort, free, b.MinFreeGiB)
	}
	job := JobName(runner)
	for _, args := range [][]string{{"clone", "--tty=false", BaseName(identity), job}, {"start", "--tty=false", job}} {
		if _, err := b.Lima.Run(ctx, nil, args...); err != nil {
			b.delete(job)
			return fmt.Errorf("starting docker VM %s: %w", job, err)
		}
	}
	go func() {
		_, err := b.Lima.Run(context.WithoutCancel(ctx), strings.NewReader(jit+"\n"),
			"shell", job, "--", "sudo", "-u", RunnerUser, "-H", TreeDir+"/entrypoint.sh")
		b.delete(job)
		done(err)
	}()
	return nil
}

func (b *Backend) delete(job string) {
	_, _ = b.Lima.Run(context.Background(), nil, "delete", "--force", job)
}

// Sweep deletes every job clone, running or not: at start-up none belongs to a live runner of this pool.
func (b *Backend) Sweep(ctx context.Context) ([]string, error) {
	out, err := b.Lima.Run(ctx, nil, "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var inst struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(line, &inst); err != nil {
			return deleted, fmt.Errorf("reading limactl list: %w", err)
		}
		if strings.HasPrefix(inst.Name, JobPrefix) {
			b.delete(inst.Name)
			deleted = append(deleted, inst.Name)
		}
	}
	return deleted, nil
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

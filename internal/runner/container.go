package runner

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/Nezhinskiy/local-ci-pool/internal/runnermount"
)

const (
	// LabelInstance carries the pool instance ("main", or "probe-<owner>") on
	// every runner container; the start-up sweep and the exit watcher key on it.
	LabelInstance = "local-ci-pool"
	// LabelRunner carries the runner name, so an exit maps back to its runner.
	LabelRunner = "local-ci-pool.runner"
	// LabelProject carries the project's identity on every runner container.
	LabelProject = "local-ci-pool.project"
	// LabelNeed carries the cores the budget gate admitted the runner for; a
	// runner the gate did not admit (the budget off) has none.
	LabelNeed = "local-ci-pool.need"
	// EnvCPU carries the same cores into the job's environment; a runner the
	// gate did not admit has none.
	EnvCPU = "CORESLOT_CPU"

	entrypoint = runnermount.Target + "/entrypoint.sh"
	homeDir    = "/tmp/home"
	shmSize    = 1 << 30
	memory     = 4 << 30
	// pidsLimit caps a job's processes and threads, so a fork or thread bomb
	// stops in its own container and not at the VM's threads-max, where
	// every other job and Docker itself would fail with it.
	pidsLimit = 4096

	// iccOption turns off traffic between the containers of a bridge.
	iccOption = "com.docker.network.bridge.enable_icc"

	// cleanupTimeout bounds removing a container that could not be started.
	cleanupTimeout = 30 * time.Second
	// stdinTimeout bounds writing the JIT configuration to the container.
	stdinTimeout = 30 * time.Second
)

// extraHosts point Docker Desktop's host aliases at the container itself, so a
// job cannot reach services on the Mac through those names. The host gateway
// address itself still reaches the Mac's loopback; SECURITY.md states that
// residual.
var extraHosts = []string{"host.docker.internal:127.0.0.1", "gateway.docker.internal:127.0.0.1"}

// Docker is the part of the Docker API this package uses. *client.Client
// satisfies it.
type Docker interface {
	NetworkInspect(ctx context.Context, networkID string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerAttach(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	Events(ctx context.Context, options client.EventsListOptions) client.EventsResult
}

// ContainerRemover is the part of Docker a Scaler needs to reap runners.
type ContainerRemover interface {
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

// ContainerSpec is one runner container. Name is both the runner name and the
// container name; Mount is the runner tree (runnermount.Mount.Spec); User is
// the user the preflight found, never root (image.Preflight). Project is the
// project's identity; Need is the cores the budget gate admitted the runner
// for, 0 when it did not. A positive Need reaches the job as CORESLOT_CPU, so a
// workflow can size its parallelism to its grant. CPUs caps the CPUs the
// container may use (CPUCap); 0 sets no cap.
type ContainerSpec struct {
	Image, User, Name, Instance string
	Project                     string
	Need                        int
	CPUs                        float64
	Mount                       mount.Mount
	JIT                         string
}

// CPUCap is the CPUs one runner may use: its budget grant when the gate
// admitted it for one, otherwise its slot's share of the VM, cpus / slots. A
// grant is cut to the VM's CPUs, more than which Docker refuses. With no CPUs
// or no slots known it is 0, no cap.
func CPUCap(need, cpus, slots int) float64 {
	switch {
	case cpus <= 0 || slots <= 0:
		return 0
	case need > 0:
		return float64(min(need, cpus))
	}
	return float64(cpus) / float64(slots)
}

// NetworkName is the bridge network the runners of an instance join.
func NetworkName(instance string) string { return "local-ci-pool-" + instance }

// networkMu keeps two starts from creating the network at once.
var networkMu sync.Mutex

// EnsureNetwork makes sure the instance's network exists: a bridge with
// outbound traffic, where containers cannot reach each other, so a job cannot
// reach a service that a concurrent job, of its own project or another, has
// open. A network of that name with other settings is refused, not used.
//
// The network outlives the pool: the runners a previous run left running are
// on it, and it is reused on the next start.
func EnsureNetwork(ctx context.Context, d Docker, instance string) error {
	name := NetworkName(instance)
	networkMu.Lock()
	defer networkMu.Unlock()
	got, err := d.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	switch {
	case err == nil:
		n := got.Network
		if n.Driver != "bridge" || n.Internal || n.Options[iccOption] != "false" {
			return fmt.Errorf("network %s exists with other settings (driver %s, internal %v, %s=%q); remove it with docker network rm %s", name, n.Driver, n.Internal, iccOption, n.Options[iccOption], name)
		}
		return nil
	case !cerrdefs.IsNotFound(err):
		return fmt.Errorf("inspecting network %s: %w", name, err)
	}
	if _, err := d.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver:  "bridge",
		Options: map[string]string{iccOption: "false"},
		Labels:  map[string]string{LabelInstance: instance},
	}); err != nil {
		return fmt.Errorf("creating network %s: %w", name, err)
	}
	return nil
}

func (s ContainerSpec) validate() error {
	switch {
	case s.Name == "":
		return errors.New("a runner container needs a name")
	case s.Instance == "":
		return errors.New("a runner container needs an instance")
	case s.Project == "":
		return errors.New("a runner container needs a project")
	case s.Image == "":
		return errors.New("a runner container needs an image")
	case s.User == "":
		return errors.New("a runner container needs a user")
	case s.JIT == "":
		return errors.New("a runner container needs a JIT configuration")
	case strings.ContainsAny(s.JIT, "\r\n"):
		return errors.New("the JIT configuration must be one line")
	}
	m := s.Mount
	if m.Type != mount.TypeImage || !m.ReadOnly || m.Target != runnermount.Target || strings.Contains(m.Source, "docker.sock") {
		return fmt.Errorf("the runner mount must be a read-only image mount at %s, got %s %q at %q (read-only %v)", runnermount.Target, m.Type, m.Source, m.Target, m.ReadOnly)
	}
	return nil
}

// StartRunner creates and starts one runner container and hands it the JIT
// configuration on stdin, the only place the configuration ever goes. The
// container mounts the runner tree and nothing else, joins the instance's
// network (EnsureNetwork), and removes itself when it exits. A container that
// was created but could not be started is removed before the error is
// returned.
func StartRunner(ctx context.Context, d Docker, s ContainerSpec) (id string, err error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	if err := EnsureNetwork(ctx, d, s.Instance); err != nil {
		return "", fmt.Errorf("runner %s: %w", s.Name, err)
	}
	labels := map[string]string{LabelInstance: s.Instance, LabelRunner: s.Name, LabelProject: s.Project}
	env := []string{"HOME=" + homeDir}
	if s.Need > 0 {
		labels[LabelNeed] = strconv.Itoa(s.Need)
		env = append(env, EnvCPU+"="+strconv.Itoa(s.Need))
	}
	init := true
	pids := int64(pidsLimit)
	created, err := d.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: s.Name,
		Config: &container.Config{
			Image:       s.Image,
			User:        s.User,
			Entrypoint:  []string{entrypoint},
			Env:         env,
			OpenStdin:   true,
			StdinOnce:   true,
			AttachStdin: true,
			Labels:      labels,
		},
		HostConfig: &container.HostConfig{
			Init:        &init,
			ShmSize:     shmSize,
			NetworkMode: container.NetworkMode(NetworkName(s.Instance)),
			Resources: container.Resources{
				Memory:    memory,
				PidsLimit: &pids,
				NanoCPUs:  int64(s.CPUs * 1e9),
			},
			AutoRemove: true,
			Mounts:     []mount.Mount{s.Mount},
			// Docker mounts a tmpfs noexec unless told otherwise, and tools
			// unpack and run binaries under HOME.
			Tmpfs:      map[string]string{homeDir: "exec"},
			ExtraHosts: extraHosts,
		},
	})
	if err != nil {
		return "", fmt.Errorf("creating runner %s: %w", s.Name, err)
	}
	cid := created.ID
	defer func() {
		if err == nil {
			return
		}
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if rmErr := RemoveContainer(rmCtx, d, cid); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("removing the unstarted container: %w", rmErr))
		}
	}()

	// Attach before the start, so the entrypoint's read finds the stream.
	attached, err := d.ContainerAttach(ctx, cid, client.ContainerAttachOptions{Stream: true, Stdin: true})
	if err != nil {
		return "", fmt.Errorf("attaching to runner %s: %w", s.Name, err)
	}
	defer attached.Close()
	if _, err := d.ContainerStart(ctx, cid, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("starting runner %s: %w", s.Name, err)
	}
	if err := writeStdin(attached.HijackedResponse, s.JIT); err != nil {
		return "", fmt.Errorf("handing runner %s its configuration: %w", s.Name, err)
	}
	return cid, nil
}

// writeStdin writes one line and closes the container's stdin.
func writeStdin(h client.HijackedResponse, line string) error {
	_ = h.Conn.SetWriteDeadline(time.Now().Add(stdinTimeout))
	if _, err := h.Conn.Write([]byte(line + "\n")); err != nil {
		return err
	}
	return h.CloseWrite()
}

// RemoveContainer force-removes a runner container by name or id. It removes
// the container only; the runner's GitHub registration is the Registry's. A
// container that no longer exists counts as removed, and so does one Docker is
// already removing: an exited AutoRemove container answers a remove with a
// conflict while its own removal runs.
func RemoveContainer(ctx context.Context, d ContainerRemover, nameOrID string) error {
	if nameOrID == "" {
		return errors.New("removing a runner container needs a name")
	}
	_, err := d.ContainerRemove(ctx, nameOrID, client.ContainerRemoveOptions{Force: true})
	switch {
	case err == nil, cerrdefs.IsNotFound(err):
		return nil
	// Measured on Docker Desktop 4.92.0 (Engine 29.8.0, API 1.56): "removal of
	// container <name> is already in progress", a 409. If a later daemon words
	// it differently, the remove fails and the scaler retries on its next call,
	// when the container is gone and the remove reports not found.
	case cerrdefs.IsConflict(err) && strings.Contains(err.Error(), "already in progress"):
		return nil
	}
	return fmt.Errorf("removing runner container %s: %w", nameOrID, err)
}

var _ Docker = (*client.Client)(nil)

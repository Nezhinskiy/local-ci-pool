// Package image makes sure a project's job image is on the machine and that the
// runner can start in it.
package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Nezhinskiy/local-ci-pool/internal/discovery"
	"github.com/Nezhinskiy/local-ci-pool/internal/dockerutil"
	"github.com/Nezhinskiy/local-ci-pool/internal/runnermount"
)

const (
	// DefaultUser runs the job when the image has no non-root user of its own.
	DefaultUser = "1001"

	preflightMemory = 4 << 30
	failureLines    = 20
)

// preflightTimeout bounds one preflight run; a variable so tests can shorten it.
var preflightTimeout = 60 * time.Second

// Docker is the part of the Docker API this package uses. *client.Client
// satisfies it.
type Docker interface {
	ImageInspect(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (dockerimage.InspectResponse, error)
	ImagePull(ctx context.Context, ref string, options dockerimage.PullOptions) (io.ReadCloser, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options build.ImageBuildOptions) (build.ImageBuildResponse, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error)
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
}

// Mirror is the part of mirror.Mirror this package uses.
type Mirror interface {
	Dir(repo string) string
	Fetch(ctx context.Context, repo, ref string) (string, error)
	InputsDigest(ctx context.Context, dir, commit string, inputs []string) (string, error)
	Archive(ctx context.Context, dir, commit string, inputs []string) (io.ReadCloser, error)
}

// Ensure makes the project's job image available and returns the reference to
// run. The image form is pulled by digest and the digest is checked on the pulled
// image. The Dockerfile form is built from a git archive of the marker's inputs
// at the ref's commit, tagged local-ci/<identity>:<digest of the inputs>; an
// existing tag is reused, so a commit that leaves the inputs alone costs nothing.
func Ensure(ctx context.Context, d Docker, m Mirror, p discovery.Project) (string, error) {
	switch {
	case p.Marker.Image != "":
		return ensurePulled(ctx, d, p.Marker.Image)
	case p.Marker.Dockerfile != "":
		return ensureBuilt(ctx, d, m, p)
	}
	return "", fmt.Errorf("project %s has an empty marker", p.Repo)
}

func hasDigest(info dockerimage.InspectResponse, digest string) bool {
	for _, rd := range info.RepoDigests {
		if strings.HasSuffix(rd, "@"+digest) {
			return true
		}
	}
	return false
}

func ensurePulled(ctx context.Context, d Docker, ref string) (string, error) {
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return "", fmt.Errorf("image %q is not pinned by digest", ref)
	}
	digest := ref[at+1:]
	// A digest names its content, so an image that is already here needs no pull.
	if info, err := d.ImageInspect(ctx, ref); err == nil && hasDigest(info, digest) {
		return ref, nil
	}
	rc, err := d.ImagePull(ctx, ref, dockerimage.PullOptions{})
	if err != nil {
		return "", fmt.Errorf("pulling %s: %w", ref, err)
	}
	defer func() { _ = rc.Close() }()
	if err := dockerutil.DrainStream(rc); err != nil {
		return "", fmt.Errorf("pulling %s: %w", ref, err)
	}
	info, err := d.ImageInspect(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("inspecting %s after the pull: %w", ref, err)
	}
	if !hasDigest(info, digest) {
		return "", fmt.Errorf("pulled %s, but its repo digests %v do not include %s", ref, info.RepoDigests, digest)
	}
	return ref, nil
}

func ensureBuilt(ctx context.Context, d Docker, m Mirror, p discovery.Project) (string, error) {
	commit, err := m.Fetch(ctx, p.Repo, p.Ref)
	if err != nil {
		return "", fmt.Errorf("fetching %s@%s: %w", p.Repo, p.Ref, err)
	}
	dir := m.Dir(p.Repo)
	digest, err := m.InputsDigest(ctx, dir, commit, p.Marker.Inputs)
	if err != nil {
		return "", fmt.Errorf("digesting the build inputs of %s: %w", p.Repo, err)
	}
	tag := "local-ci/" + p.Identity + ":" + digest
	if _, err := d.ImageInspect(ctx, tag); err == nil {
		return tag, nil
	}
	archive, err := m.Archive(ctx, dir, commit, p.Marker.Inputs)
	if err != nil {
		return "", fmt.Errorf("archiving the build inputs of %s: %w", p.Repo, err)
	}
	defer func() { _ = archive.Close() }()
	resp, err := d.ImageBuild(ctx, archive, build.ImageBuildOptions{
		Tags:        []string{tag},
		Dockerfile:  p.Marker.Dockerfile,
		Remove:      true,
		ForceRemove: true,
	})
	if err != nil {
		return "", fmt.Errorf("building %s: %w", tag, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := dockerutil.DrainStream(resp.Body); err != nil {
		return "", fmt.Errorf("building %s: %w", tag, err)
	}
	if _, err := d.ImageInspect(ctx, tag); err != nil {
		return "", fmt.Errorf("building %s: the image is missing after the build: %w", tag, err)
	}
	return tag, nil
}

// RunUser is the user a job runs as: the image's own USER unless that is empty
// or root, in which case DefaultUser.
func RunUser(ctx context.Context, d Docker, ref string) (string, error) {
	info, err := d.ImageInspect(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", ref, err)
	}
	user := ""
	if info.Config != nil {
		user = info.Config.User
	}
	name, _, _ := strings.Cut(user, ":")
	switch name {
	case "", "root", "0":
		return DefaultUser, nil
	}
	return user, nil
}

// Preflight runs the runner's own start path once in the image, with the runner
// mount, and fails when it cannot start.
func Preflight(ctx context.Context, d Docker, ref string, m runnermount.Mount) error {
	_, err := PreflightVersion(ctx, d, ref, m)
	return err
}

var versionLine = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+`)

// PreflightVersion is Preflight that also returns the runner version the image
// printed. The entrypoint is run with --preflight and set as Entrypoint, never
// only as Cmd, so an ENTRYPOINT of the image cannot wrap it. The run is
// bounded; a failure carries the last lines of output.
func PreflightVersion(ctx context.Context, d Docker, ref string, m runnermount.Mount) (string, error) {
	user, err := RunUser(ctx, d, ref)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()

	init := true
	created, err := d.ContainerCreate(ctx, &container.Config{
		Image:      ref,
		User:       user,
		Entrypoint: []string{runnermount.Target + "/entrypoint.sh"},
		Cmd:        []string{"--preflight"},
		Env:        []string{"HOME=/tmp/home"},
		Labels:     map[string]string{"local-ci-pool": "preflight"},
	}, &container.HostConfig{
		Init:        &init,
		Mounts:      []mount.Mount{m.Spec},
		Tmpfs:       map[string]string{"/tmp/home": "rw,mode=1777"},
		NetworkMode: "none",
		ExtraHosts:  []string{"host.docker.internal:127.0.0.1", "gateway.docker.internal:127.0.0.1"},
		Resources:   container.Resources{Memory: preflightMemory},
	}, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("preflight of %s: creating the container: %w", ref, err)
	}
	id := created.ID
	// The container is removed even if the run was cancelled or timed out.
	defer func() {
		rmCtx, rmCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer rmCancel()
		_ = d.ContainerRemove(rmCtx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	}()

	if err := d.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("preflight of %s: starting the container: %w", ref, err)
	}

	statusCh, errCh := d.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	var status container.WaitResponse
	timedOut := false
	select {
	case status = <-statusCh:
	case err := <-errCh:
		if ctx.Err() == nil {
			return "", fmt.Errorf("preflight of %s: waiting for the container: %w", ref, err)
		}
		timedOut = true
	case <-ctx.Done():
		timedOut = true
	}

	out := containerOutput(context.WithoutCancel(ctx), d, id)
	switch {
	case timedOut:
		return "", fmt.Errorf("preflight of %s timed out after %s:\n%s", ref, preflightTimeout, lastLines(out, failureLines))
	case status.Error != nil && status.Error.Message != "":
		return "", fmt.Errorf("preflight of %s failed: %s:\n%s", ref, status.Error.Message, lastLines(out, failureLines))
	case status.StatusCode != 0:
		return "", fmt.Errorf("preflight of %s failed (exit %d):\n%s", ref, status.StatusCode, lastLines(out, failureLines))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	version := strings.TrimSpace(lines[len(lines)-1])
	if !versionLine.MatchString(version) {
		return "", fmt.Errorf("preflight of %s exited 0 but printed no runner version:\n%s", ref, lastLines(out, failureLines))
	}
	return version, nil
}

// containerOutput returns the container's combined stdout and stderr, in the
// order they were written. A failure to read it yields what was read.
func containerOutput(ctx context.Context, d Docker, id string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rc, err := d.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "100"})
	if err != nil {
		return fmt.Sprintf("(no output: %v)", err)
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, rc); err != nil && !errors.Is(err, io.EOF) {
		buf.WriteString(fmt.Sprintf("\n(output cut: %v)", err))
	}
	return buf.String()
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

var _ Docker = (*client.Client)(nil)

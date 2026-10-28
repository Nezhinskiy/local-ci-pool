// Package image makes sure a project's job image is on the machine and that the
// runner can start in it.
package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/Nezhinskiy/local-ci-pool/internal/discovery"
	"github.com/Nezhinskiy/local-ci-pool/internal/dockerutil"
	"github.com/Nezhinskiy/local-ci-pool/internal/runnermount"
)

const (
	// DefaultUser runs the job when the image has no non-root user of its own.
	// The group is explicit: without one, a uid with no passwd entry runs
	// with gid 0.
	DefaultUser = "1001:1001"
	// defaultGroup replaces a root group, whoever the user is.
	defaultGroup = "1001"

	preflightMemory = 4 << 30
	failureLines    = 20
)

// preflightTimeout bounds one preflight run; a variable so tests can shorten it.
var preflightTimeout = 60 * time.Second

// Docker is the part of the Docker API this package uses. *client.Client
// satisfies it.
type Docker interface {
	ImageInspect(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options client.ImageBuildOptions) (client.ImageBuildResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerWait(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult
	ContainerLogs(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
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
// at the ref's commit, tagged local-ci/<identity>:<TagDigest>; an
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

func hasDigest(info client.ImageInspectResult, digest string) bool {
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
	rc, err := d.ImagePull(ctx, ref, client.ImagePullOptions{})
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
	tag := "local-ci/" + p.Identity + ":" + TagDigest(digest, p.Marker.Dockerfile)
	if _, err := d.ImageInspect(ctx, tag); err == nil {
		return tag, nil
	}
	archive, err := m.Archive(ctx, dir, commit, p.Marker.Inputs)
	if err != nil {
		return "", fmt.Errorf("archiving the build inputs of %s: %w", p.Repo, err)
	}
	defer func() { _ = archive.Close() }()
	resp, err := d.ImageBuild(ctx, archive, client.ImageBuildOptions{
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

// TagDigest is the tag of a built image: the first 12 hex of sha256 over the
// inputs digest and the Dockerfile path. The inputs digest alone would keep the
// tag when only the marker's dockerfile field changes to another file inside the
// same inputs, and a stale image would be reused.
func TagDigest(inputsDigest, dockerfile string) string {
	sum := sha256.Sum256([]byte(inputsDigest + "\x00" + dockerfile))
	return hex.EncodeToString(sum[:])[:12]
}

// RunUser is the user a job runs as: the image's own USER unless that is empty
// or root, in which case DefaultUser, and with a root group replaced by
// defaultGroup. Root is "root" or any number Docker reads as 0 ("0", "00",
// "+0"). A passwd name with uid 0, or whose primary group is 0, only a run can
// tell; the preflight does (PreflightVersion).
func RunUser(ctx context.Context, d Docker, ref string) (string, error) {
	info, err := d.ImageInspect(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", ref, err)
	}
	user := ""
	if info.Config != nil {
		user = info.Config.User
	}
	name, group, hasGroup := strings.Cut(user, ":")
	switch {
	case isRoot(name):
		return DefaultUser, nil
	case hasGroup && isRoot(group):
		return name + ":" + defaultGroup, nil
	}
	return user, nil
}

// isRoot reports whether a user or group spec names root: empty, "root", or
// a number Docker reads as 0.
func isRoot(spec string) bool {
	n, err := strconv.Atoi(spec)
	return spec == "" || spec == "root" || (err == nil && n == 0)
}

// Preflight runs the runner's own start path once in the image, with the runner
// mount, fails when it cannot start, and returns the user the job runs as.
func Preflight(ctx context.Context, d Docker, ref string, m runnermount.Mount) (string, error) {
	user, _, err := PreflightVersion(ctx, d, ref, m)
	return user, err
}

var versionLine = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+`)

// PreflightVersion is Preflight that also returns the runner version the image
// printed. The run starts as RunUser of the image. When the entrypoint reports
// that it runs as uid 0 (a passwd name such as toor), it runs again as
// DefaultUser; when it reports gid 0 (a passwd entry whose primary group is
// root), it runs again as the same user with defaultGroup. The user of the
// last run is the one returned. A job never starts as root or in group root.
//
// The uid line is printed inside the image, so this guards against an image
// that is root by mistake, not one built to be: such an image could fake the
// line, as it could ship a setuid sudo. Its author already runs the job's code,
// which the threat model in SECURITY.md accepts.
func PreflightVersion(ctx context.Context, d Docker, ref string, m runnermount.Mount) (user, version string, err error) {
	user, err = RunUser(ctx, d, ref)
	if err != nil {
		return "", "", err
	}
	id, version, err := preflightAs(ctx, d, ref, m, user)
	switch {
	case err == nil && id.uid == 0 && user != DefaultUser:
		user = DefaultUser
		id, version, err = preflightAs(ctx, d, ref, m, user)
	case err == nil && id.gid == 0:
		name, _, _ := strings.Cut(user, ":")
		user = name + ":" + defaultGroup
		id, version, err = preflightAs(ctx, d, ref, m, user)
	}
	switch {
	case err != nil:
		return "", "", err
	case id.uid == 0:
		return "", "", fmt.Errorf("preflight of %s: user %s runs as root", ref, user)
	case id.gid == 0:
		return "", "", fmt.Errorf("preflight of %s: user %s runs in group root", ref, user)
	}
	return user, version, nil
}

// ids are the effective uid and gid the entrypoint reported.
type ids struct{ uid, gid int }

var idLine = regexp.MustCompile(`^local-ci: uid ([0-9]+) gid ([0-9]+)$`)

// preflightAs runs the entrypoint with --preflight as user and returns the
// effective uid and gid and the runner version it printed. The entrypoint is set as
// Entrypoint, never only as Cmd, so an ENTRYPOINT of the image cannot wrap it.
// The run is bounded; a failure carries the last lines of output.
func preflightAs(ctx context.Context, d Docker, ref string, m runnermount.Mount, user string) (ids, string, error) {
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()

	init := true
	created, err := d.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      ref,
			User:       user,
			Entrypoint: []string{runnermount.Target + "/entrypoint.sh"},
			Cmd:        []string{"--preflight"},
			// An image's BASH_ENV (or ENV) would run before the entrypoint
			// and could print a uid line of its own; empty, bash reads none.
			Env:    []string{"HOME=/tmp/home", "BASH_ENV=", "ENV="},
			Labels: map[string]string{"local-ci-pool": "preflight"},
		},
		HostConfig: &container.HostConfig{
			Init:        &init,
			Mounts:      []mount.Mount{m.Spec},
			Tmpfs:       map[string]string{"/tmp/home": "rw,mode=1777"},
			NetworkMode: "none",
			ExtraHosts:  []string{"host.docker.internal:127.0.0.1", "gateway.docker.internal:127.0.0.1"},
			Resources:   container.Resources{Memory: preflightMemory},
		},
	})
	if err != nil {
		return ids{}, "", fmt.Errorf("preflight of %s: creating the container: %w", ref, err)
	}
	id := created.ID
	// The container is removed even if the run was cancelled or timed out.
	defer func() {
		rmCtx, rmCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer rmCancel()
		_, _ = d.ContainerRemove(rmCtx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	}()

	if _, err := d.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return ids{}, "", fmt.Errorf("preflight of %s: starting the container: %w", ref, err)
	}

	waiting := d.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	statusCh, errCh := waiting.Result, waiting.Error
	var status container.WaitResponse
	timedOut := false
	select {
	case status = <-statusCh:
	case err := <-errCh:
		if ctx.Err() == nil {
			return ids{}, "", fmt.Errorf("preflight of %s: waiting for the container: %w", ref, err)
		}
		timedOut = true
	case <-ctx.Done():
		timedOut = true
	}

	out := containerOutput(context.WithoutCancel(ctx), d, id)
	switch {
	case timedOut:
		return ids{}, "", fmt.Errorf("preflight of %s timed out after %s:\n%s", ref, preflightTimeout, lastLines(out, failureLines))
	case status.Error != nil && status.Error.Message != "":
		return ids{}, "", fmt.Errorf("preflight of %s failed: %s:\n%s", ref, status.Error.Message, lastLines(out, failureLines))
	case status.StatusCode != 0:
		return ids{}, "", fmt.Errorf("preflight of %s failed (exit %d):\n%s", ref, status.StatusCode, lastLines(out, failureLines))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	version := strings.TrimSpace(lines[len(lines)-1])
	if !versionLine.MatchString(version) {
		return ids{}, "", fmt.Errorf("preflight of %s exited 0 but printed no runner version:\n%s", ref, lastLines(out, failureLines))
	}
	for _, l := range lines {
		if mm := idLine.FindStringSubmatch(strings.TrimSpace(l)); mm != nil {
			uid, uerr := strconv.Atoi(mm[1])
			gid, gerr := strconv.Atoi(mm[2])
			if uerr != nil || gerr != nil {
				break
			}
			return ids{uid, gid}, version, nil
		}
	}
	return ids{}, "", fmt.Errorf("preflight of %s as user %s printed no uid:\n%s", ref, user, lastLines(out, failureLines))
}

// containerOutput returns the container's combined stdout and stderr, in the
// order they were written. A failure to read it yields what was read.
func containerOutput(ctx context.Context, d Docker, id string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rc, err := d.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "100"})
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

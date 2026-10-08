//go:build docker

package runnermount_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/Nezhinskiy/local-ci-pool/internal/ghauth"
	"github.com/Nezhinskiy/local-ci-pool/internal/github"
	"github.com/Nezhinskiy/local-ci-pool/internal/image"
	"github.com/Nezhinskiy/local-ci-pool/internal/machine"
	"github.com/Nezhinskiy/local-ci-pool/internal/runnermount"
)

const (
	// A bare Ubuntu: no CA bundle, so a runner there could never reach GitHub.
	bareUbuntu = "ubuntu@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55"
	// Ships libicu, a CA bundle and the pwuser account.
	playwright = "mcr.microsoft.com/playwright:v1.63.0-noble@sha256:eff16c30e6f3f4af0a03fa4b706120d5e9b0891c344a27d64559aff5900a4a27"
)

type countingSource struct {
	runnermount.ReleaseSource
	downloads atomic.Int32
}

func (c *countingSource) Download(ctx context.Context, url string) (io.ReadCloser, error) {
	c.downloads.Add(1)
	return c.ReleaseSource.Download(ctx, url)
}

func runOnce(t *testing.T, ctx context.Context, cli *client.Client, ref string, m runnermount.Mount, user string, entrypoint, cmd []string) (int64, string) {
	t.Helper()
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: ref, User: user, Entrypoint: entrypoint, Cmd: cmd, Env: []string{"HOME=/tmp/home"},
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{m.Spec},
			Tmpfs:  map[string]string{"/tmp/home": "rw,mode=1777"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	waiting := cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	waitCh, errCh := waiting.Result, waiting.Error
	var code int64
	select {
	case w := <-waitCh:
		code = w.StatusCode
	case err := <-errCh:
		t.Fatal(err)
	}
	logs, err := cli.ContainerLogs(ctx, created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logs.Close() }()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, logs); err != nil {
		t.Fatal(err)
	}
	return code, out.String()
}

func haveImage(ctx context.Context, cli *client.Client, ref string) bool {
	_, err := cli.ImageInspect(ctx, ref)
	return err == nil
}

// TestRealMount builds the runner mount from the real latest actions/runner
// release (a read-only network use of the GitHub API and of the release
// download), then uses it in real containers. It needs Docker and `gh`, so it
// is behind the docker build tag and CI does not run it.
func TestRealMount(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	infoRes, err := cli.Info(ctx, client.InfoOptions{})
	info := infoRes.Info
	if err != nil {
		t.Skipf("Docker is not available: %v", err)
	}
	arch, err := machine.RunnerArch(info.Architecture)
	if err != nil {
		t.Fatal(err)
	}
	gh := &countingSource{ReleaseSource: github.New(ghauth.NewSource(nil), "")}

	// 1. Build the mount from the real release.
	m, err := runnermount.Ensure(ctx, cli, gh, arch)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("runner %s mount %+v", m.Version, m.Spec)
	if m.Spec.Type != mount.TypeImage || !m.Spec.ReadOnly || m.Spec.Target != "/opt/local-ci" || m.Spec.Source != "local-ci/runner:"+m.Version {
		t.Fatalf("unexpected mount %+v", m.Spec)
	}
	if gh.downloads.Load() != 1 {
		t.Logf("the runner image was already current (downloads: %d)", gh.downloads.Load())
	}
	img, err := cli.ImageInspect(ctx, m.Spec.Source)
	if err != nil {
		t.Fatal(err)
	}
	if img.Config == nil || len(img.Config.Labels["local-ci-pool.runner-sha256"]) != 64 || len(img.Config.Labels["local-ci-pool.entrypoint-sha256"]) != 64 {
		t.Fatalf("the image carries no verification labels: %+v", img.Config)
	}

	// 2. A second Ensure reuses the image: nothing is downloaded.
	before := gh.downloads.Load()
	if _, err := runnermount.Ensure(ctx, cli, gh, arch); err != nil {
		t.Fatal(err)
	}
	if gh.downloads.Load() != before {
		t.Fatalf("the second Ensure downloaded the tarball again")
	}

	// 3. The mount is read-only and its entrypoint runs as a non-root user.
	if !haveImage(ctx, cli, bareUbuntu) {
		t.Skipf("%s is not available locally", bareUbuntu)
	}
	code, out := runOnce(t, ctx, cli, bareUbuntu, m, "1001", []string{"/bin/sh", "-c", "touch /opt/local-ci/x 2>&1; ls /opt/local-ci"}, nil)
	t.Logf("read-only check (exit %d):\n%s", code, out)
	if !strings.Contains(out, "Read-only file system") {
		t.Errorf("the mount is writable:\n%s", out)
	}
	for _, want := range []string{"bin", "externals", "run.sh", "entrypoint.sh"} {
		if !strings.Contains(out, want) {
			t.Errorf("the mount lacks %s:\n%s", want, out)
		}
	}
	code, out = runOnce(t, ctx, cli, bareUbuntu, m, "1001", []string{"/opt/local-ci/entrypoint.sh"}, nil)
	if code != 2 || !strings.Contains(out, "no JIT configuration") {
		t.Errorf("entrypoint without a configuration: exit %d, output %q", code, out)
	}

	// 4. Preflight: the bare Ubuntu has no CA bundle and must fail for that
	// reason; the Playwright image must report the runner version.
	err = image.Preflight(ctx, cli, bareUbuntu, m)
	t.Logf("bare Ubuntu preflight: %v", err)
	if err == nil || !strings.Contains(err.Error(), "image has no CA bundle") {
		t.Errorf("preflight of an image without a CA bundle: %v", err)
	}
	if haveImage(ctx, cli, playwright) {
		user, err := image.RunUser(ctx, cli, playwright)
		t.Logf("Playwright run user: %q, %v", user, err)
		version, err := image.PreflightVersion(ctx, cli, playwright, m)
		t.Logf("Playwright preflight: %q, %v", version, err)
		if err != nil || version != m.Version {
			t.Errorf("preflight of the Playwright image = %q, %v; want %s", version, err, m.Version)
		}
	} else {
		t.Logf("skipping the Playwright preflight: %s is not available locally", playwright)
	}

	// 5. Prune removes an older unused runner image and spares the current one
	// and any image a container mounts.
	cleanup := func(tag string) {
		_, _ = cli.ImageRemove(context.WithoutCancel(ctx), tag, client.ImageRemoveOptions{Force: true})
	}
	const stale, busy = "local-ci/runner:0.0.1-smoke-stale", "local-ci/runner:0.0.2-smoke-busy"
	for _, tag := range []string{stale, busy} {
		if _, err := cli.ImageTag(ctx, client.ImageTagOptions{Source: m.Spec.Source, Target: tag}); err != nil {
			t.Fatal(err)
		}
		defer cleanup(tag)
	}
	held, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: bareUbuntu, Cmd: []string{"true"}},
		HostConfig: &container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeImage, Source: busy, Target: "/opt/local-ci", ReadOnly: true}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.WithoutCancel(ctx), held.ID, client.ContainerRemoveOptions{Force: true})
	}()
	if err := runnermount.Prune(ctx, cli, m.Version); err != nil {
		t.Fatal(err)
	}
	left, err := cli.ImageList(ctx, client.ImageListOptions{Filters: make(client.Filters).Add("reference", "local-ci/runner:*")})
	if err != nil {
		t.Fatal(err)
	}
	tags := map[string]bool{}
	for _, s := range left.Items {
		for _, tag := range s.RepoTags {
			tags[tag] = true
		}
	}
	t.Logf("runner image tags after Prune: %v", tags)
	if tags[stale] {
		t.Errorf("Prune left the unused image %s", stale)
	}
	if !tags[busy] {
		t.Errorf("Prune removed %s, which a container mounts", busy)
	}
	if !tags[m.Spec.Source] {
		t.Errorf("Prune removed the current image %s", m.Spec.Source)
	}
}

// termScript prepares a copy of the real runner tree whose Runner.Listener is a
// stub that reports the signal it gets, then execs the pool's real entrypoint.
// run.sh and run-helper.sh are the real ones from the release.
const termScript = `set -e
mkdir -p /tmp/m && cp -a /opt/local-ci/. /tmp/m/
cat > /tmp/m/bin/Runner.Listener <<'X'
#!/bin/bash
trap 'echo LISTENER-GOT-INT; exit 9' INT
trap 'echo LISTENER-GOT-TERM; exit 9' TERM
echo LISTENER-READY
while :; do sleep 0.05; done
X
chmod +x /tmp/m/bin/Runner.Listener
export LOCAL_CI_TEST_HOOKS=1 LOCAL_CI_MOUNT=/tmp/m LOCAL_CI_ROOT=/tmp/root
exec /opt/local-ci/entrypoint.sh <<< JIT-VALUE
`

func containerLogs(ctx context.Context, cli *client.Client, id string) string {
	rc, err := cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, rc)
	return out.String()
}

// TestRealEntrypointForwardsTerm sends SIGTERM to a container whose PID 1 init
// starts the real entrypoint, which runs the release's real run.sh and
// run-helper.sh around a stub listener. The listener must see the signal and its
// status must come back through the helper.
func TestRealEntrypointForwardsTerm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	infoRes, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		t.Skipf("Docker is not available: %v", err)
	}
	arch, err := machine.RunnerArch(infoRes.Info.Architecture)
	if err != nil {
		t.Fatal(err)
	}
	m, err := runnermount.Ensure(ctx, cli, github.New(ghauth.NewSource(nil), ""), arch)
	if err != nil {
		t.Fatal(err)
	}
	if !haveImage(ctx, cli, bareUbuntu) {
		t.Skipf("%s is not available locally", bareUbuntu)
	}
	init := true
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: bareUbuntu, User: "1001", Cmd: []string{"bash", "-c", termScript},
		},
		HostConfig: &container.HostConfig{
			Init:   &init,
			Mounts: []mount.Mount{m.Spec},
			Tmpfs:  map[string]string{"/tmp": "rw,exec,mode=1777"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(containerLogs(ctx, cli, created.ID), "LISTENER-READY") {
		if time.Now().After(deadline) {
			t.Fatalf("the listener never started:\n%s", containerLogs(ctx, cli, created.ID))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := cli.ContainerKill(ctx, created.ID, client.ContainerKillOptions{Signal: "TERM"}); err != nil {
		t.Fatal(err)
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	defer waitCancel()
	waiting := cli.ContainerWait(waitCtx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	var code int64
	select {
	case w := <-waiting.Result:
		code = w.StatusCode
	case err := <-waiting.Error:
		t.Fatalf("the container did not stop within 30 s of TERM: %v\n%s", err, containerLogs(ctx, cli, created.ID))
	}
	logs := containerLogs(ctx, cli, created.ID)
	t.Logf("exit %d, output:\n%s", code, logs)
	if !strings.Contains(logs, "LISTENER-GOT-INT") {
		t.Errorf("the listener did not receive the forwarded signal")
	}
	if !strings.Contains(logs, "unknown error code: 9") {
		t.Errorf("the listener's status did not reach run-helper")
	}
	if code != 143 {
		t.Errorf("exit code %d, want 143 (run.sh reports the interrupted wait)", code)
	}
}

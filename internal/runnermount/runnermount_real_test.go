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

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

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
	created, err := cli.ContainerCreate(ctx, &container.Config{
		Image: ref, User: user, Entrypoint: entrypoint, Cmd: cmd, Env: []string{"HOME=/tmp/home"},
	}, &container.HostConfig{
		Mounts: []mount.Mount{m.Spec},
		Tmpfs:  map[string]string{"/tmp/home": "rw,mode=1777"},
	}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
	}()
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	waitCh, errCh := cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	var code int64
	select {
	case w := <-waitCh:
		code = w.StatusCode
	case err := <-errCh:
		t.Fatal(err)
	}
	logs, err := cli.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
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
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	info, err := cli.Info(ctx)
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
		_, _ = cli.ImageRemove(context.WithoutCancel(ctx), tag, dockerimage.RemoveOptions{Force: true})
	}
	const stale, busy = "local-ci/runner:0.0.1-smoke-stale", "local-ci/runner:0.0.2-smoke-busy"
	for _, tag := range []string{stale, busy} {
		if err := cli.ImageTag(ctx, m.Spec.Source, tag); err != nil {
			t.Fatal(err)
		}
		defer cleanup(tag)
	}
	held, err := cli.ContainerCreate(ctx, &container.Config{Image: bareUbuntu, Cmd: []string{"true"}},
		&container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeImage, Source: busy, Target: "/opt/local-ci", ReadOnly: true}}}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cli.ContainerRemove(context.WithoutCancel(ctx), held.ID, container.RemoveOptions{Force: true})
	}()
	if err := runnermount.Prune(ctx, cli, m.Version); err != nil {
		t.Fatal(err)
	}
	left, err := cli.ImageList(ctx, dockerimage.ListOptions{Filters: filters.NewArgs(filters.Arg("reference", "local-ci/runner:*"))})
	if err != nil {
		t.Fatal(err)
	}
	tags := map[string]bool{}
	for _, s := range left {
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

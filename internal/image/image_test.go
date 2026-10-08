package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Nezhinskiy/local-ci-pool/internal/discovery"
	"github.com/Nezhinskiy/local-ci-pool/internal/runnermount"
)

const digest = "sha256:eff16c30e6f3f4af0a03fa4b706120d5e9b0891c344a27d64559aff5900a4a27"

type fakeDocker struct {
	images   map[string]dockerimage.InspectResponse
	pullAdds *dockerimage.InspectResponse // what a successful pull makes available
	pullBody string
	pullErr  error
	pulls    []string

	builds     int
	buildOpts  build.ImageBuildOptions
	buildInput string
	buildBody  string
	buildErr   error

	// containers
	createdCfg  *container.Config
	createdHost *container.HostConfig
	startErr    error
	exit        int64
	logs        []byte
	neverExits  bool
	removed     []string
	removeOpts  container.RemoveOptions
}

func (f *fakeDocker) ImageInspect(_ context.Context, ref string, _ ...client.ImageInspectOption) (dockerimage.InspectResponse, error) {
	if r, ok := f.images[ref]; ok {
		return r, nil
	}
	return dockerimage.InspectResponse{}, errdefs.NotFound(errors.New("no such image"))
}

func (f *fakeDocker) ImagePull(_ context.Context, ref string, _ dockerimage.PullOptions) (io.ReadCloser, error) {
	f.pulls = append(f.pulls, ref)
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	body := f.pullBody
	if body == "" {
		body = `{"status":"Pulling"}` + "\n"
		if f.pullAdds != nil {
			if f.images == nil {
				f.images = map[string]dockerimage.InspectResponse{}
			}
			f.images[ref] = *f.pullAdds
		}
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func (f *fakeDocker) ImageBuild(_ context.Context, r io.Reader, o build.ImageBuildOptions) (build.ImageBuildResponse, error) {
	f.builds++
	f.buildOpts = o
	b, err := io.ReadAll(r)
	if err != nil {
		return build.ImageBuildResponse{}, err
	}
	f.buildInput = string(b)
	if f.buildErr != nil {
		return build.ImageBuildResponse{}, f.buildErr
	}
	body := f.buildBody
	if body == "" {
		body = `{"stream":"ok\n"}` + "\n"
		if f.images == nil {
			f.images = map[string]dockerimage.InspectResponse{}
		}
		for _, tag := range o.Tags {
			f.images[tag] = dockerimage.InspectResponse{ID: "sha256:built"}
		}
	}
	return build.ImageBuildResponse{Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (f *fakeDocker) ContainerCreate(_ context.Context, c *container.Config, h *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
	f.createdCfg, f.createdHost = c, h
	return container.CreateResponse{ID: "c1"}, nil
}

func (f *fakeDocker) ContainerStart(context.Context, string, container.StartOptions) error {
	return f.startErr
}

func (f *fakeDocker) ContainerWait(ctx context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	st := make(chan container.WaitResponse, 1)
	er := make(chan error, 1)
	if f.neverExits {
		go func() { <-ctx.Done(); er <- ctx.Err() }()
		return st, er
	}
	st <- container.WaitResponse{StatusCode: f.exit}
	return st, er
}

func (f *fakeDocker) ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.logs)), nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, o container.RemoveOptions) error {
	f.removed = append(f.removed, id)
	f.removeOpts = o
	return nil
}

// frames builds a multiplexed log stream from stdout and stderr lines.
func frames(t *testing.T, stdout, stderr string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if stdout != "" {
		if _, err := stdcopy.NewStdWriter(&buf, stdcopy.Stdout).Write([]byte(stdout)); err != nil {
			t.Fatal(err)
		}
	}
	if stderr != "" {
		if _, err := stdcopy.NewStdWriter(&buf, stdcopy.Stderr).Write([]byte(stderr)); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func withUser(user string) dockerimage.InspectResponse {
	return dockerimage.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{User: user}}}
}

func TestRunUser(t *testing.T) {
	for in, want := range map[string]string{
		"":            "1001",
		"root":        "1001",
		"0":           "1001",
		"root:root":   "1001",
		"0:0":         "1001",
		"runner":      "runner",
		"pwuser":      "pwuser",
		"1000":        "1000",
		"runner:ci":   "runner:ci",
		"rootless":    "rootless",
		"10001:10001": "10001:10001",
	} {
		d := &fakeDocker{images: map[string]dockerimage.InspectResponse{"img": withUser(in)}}
		got, err := RunUser(context.Background(), d, "img")
		if err != nil || got != want {
			t.Errorf("RunUser(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// An image with no config at all, and one that is missing.
	d := &fakeDocker{images: map[string]dockerimage.InspectResponse{"bare": {}}}
	if got, err := RunUser(context.Background(), d, "bare"); err != nil || got != "1001" {
		t.Errorf("no config: %q, %v", got, err)
	}
	if _, err := RunUser(context.Background(), d, "missing"); err == nil {
		t.Error("a missing image must be an error")
	}
}

const imageRef = "mcr.example.invalid/playwright:v1.63.0-noble@" + digest

func imageProject() discovery.Project {
	return discovery.Project{Identity: "alpha", Repo: "o/alpha", Ref: "main", Marker: discovery.Marker{Image: imageRef}}
}

func TestImageFormVerifiesRepoDigest(t *testing.T) {
	good := dockerimage.InspectResponse{RepoDigests: []string{"mcr.example.invalid/playwright@" + digest}}
	wrong := dockerimage.InspectResponse{RepoDigests: []string{"mcr.example.invalid/playwright@sha256:" + strings.Repeat("0", 64)}}
	none := dockerimage.InspectResponse{}

	t.Run("pulled and verified", func(t *testing.T) {
		d := &fakeDocker{pullAdds: &good}
		ref, err := Ensure(context.Background(), d, nil, imageProject())
		if err != nil || ref != imageRef {
			t.Fatalf("Ensure = %q, %v", ref, err)
		}
		if len(d.pulls) != 1 || d.pulls[0] != imageRef {
			t.Fatalf("pulls = %v", d.pulls)
		}
	})
	t.Run("already present means no pull", func(t *testing.T) {
		d := &fakeDocker{images: map[string]dockerimage.InspectResponse{imageRef: good}}
		if _, err := Ensure(context.Background(), d, nil, imageProject()); err != nil || len(d.pulls) != 0 {
			t.Fatalf("err = %v, pulls = %v", err, d.pulls)
		}
	})
	t.Run("a local image with another digest is pulled again and checked", func(t *testing.T) {
		d := &fakeDocker{images: map[string]dockerimage.InspectResponse{imageRef: wrong}, pullAdds: &good}
		if _, err := Ensure(context.Background(), d, nil, imageProject()); err != nil || len(d.pulls) != 1 {
			t.Fatalf("err = %v, pulls = %v", err, d.pulls)
		}
	})
	for name, adds := range map[string]*dockerimage.InspectResponse{"wrong digest": &wrong, "no digests": &none} {
		t.Run("refuses "+name, func(t *testing.T) {
			d := &fakeDocker{pullAdds: adds}
			_, err := Ensure(context.Background(), d, nil, imageProject())
			if err == nil || !strings.Contains(err.Error(), "do not include") {
				t.Fatalf("want a digest refusal, got %v", err)
			}
		})
	}
	t.Run("pull stream error", func(t *testing.T) {
		d := &fakeDocker{pullBody: `{"error":"manifest unknown"}` + "\n"}
		if _, err := Ensure(context.Background(), d, nil, imageProject()); err == nil || !strings.Contains(err.Error(), "manifest unknown") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("pull failure", func(t *testing.T) {
		d := &fakeDocker{pullErr: errors.New("offline")}
		if _, err := Ensure(context.Background(), d, nil, imageProject()); err == nil || !strings.Contains(err.Error(), "offline") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a marker without a digest never reaches Docker", func(t *testing.T) {
		d := &fakeDocker{}
		p := imageProject()
		p.Marker.Image = "ubuntu:24.04"
		if _, err := Ensure(context.Background(), d, nil, p); err == nil || len(d.pulls) != 0 {
			t.Fatalf("err = %v, pulls = %v", err, d.pulls)
		}
	})
	t.Run("an empty marker", func(t *testing.T) {
		p := imageProject()
		p.Marker = discovery.Marker{}
		if _, err := Ensure(context.Background(), &fakeDocker{}, nil, p); err == nil {
			t.Fatal("want an error")
		}
	})
}

type fakeMirror struct {
	commit     string
	digest     string
	fetches    []string
	archives   [][]string
	archiveErr error
	body       string
}

func (f *fakeMirror) Dir(repo string) string { return "/mirror/" + repo }

func (f *fakeMirror) Fetch(_ context.Context, repo, ref string) (string, error) {
	f.fetches = append(f.fetches, repo+"@"+ref)
	return f.commit, nil
}

func (f *fakeMirror) InputsDigest(_ context.Context, dir, commit string, inputs []string) (string, error) {
	if dir != "/mirror/o/alpha" || commit != f.commit {
		return "", fmt.Errorf("unexpected dir %q commit %q", dir, commit)
	}
	return f.digest, nil
}

func (f *fakeMirror) Archive(_ context.Context, _, _ string, inputs []string) (io.ReadCloser, error) {
	f.archives = append(f.archives, inputs)
	if f.archiveErr != nil {
		return nil, f.archiveErr
	}
	return io.NopCloser(strings.NewReader(f.body)), nil
}

func dockerfileProject() discovery.Project {
	return discovery.Project{
		Identity: "alpha", Repo: "o/alpha", Ref: "main",
		Marker: discovery.Marker{Dockerfile: "ci/runner/Dockerfile", Inputs: []string{"ci/runner", "uv.lock"}},
	}
}

func TestDockerfileFormSkipsBuildWhenTagPresent(t *testing.T) {
	m := &fakeMirror{commit: strings.Repeat("a", 40), digest: "0123456789ab", body: "TARBYTES"}
	d := &fakeDocker{images: map[string]dockerimage.InspectResponse{"local-ci/alpha:0123456789ab": {}}}
	ref, err := Ensure(context.Background(), d, m, dockerfileProject())
	if err != nil || ref != "local-ci/alpha:0123456789ab" {
		t.Fatalf("Ensure = %q, %v", ref, err)
	}
	if d.builds != 0 || len(m.archives) != 0 {
		t.Fatalf("builds = %d, archives = %v: the tag existed", d.builds, m.archives)
	}
	if len(m.fetches) != 1 || m.fetches[0] != "o/alpha@main" {
		t.Fatalf("fetches = %v", m.fetches)
	}
}

func TestDockerfileFormBuildsFromTheArchive(t *testing.T) {
	m := &fakeMirror{commit: strings.Repeat("a", 40), digest: "0123456789ab", body: "TARBYTES"}
	d := &fakeDocker{}
	ref, err := Ensure(context.Background(), d, m, dockerfileProject())
	if err != nil || ref != "local-ci/alpha:0123456789ab" {
		t.Fatalf("Ensure = %q, %v", ref, err)
	}
	if d.builds != 1 {
		t.Fatalf("builds = %d", d.builds)
	}
	if d.buildInput != "TARBYTES" {
		t.Errorf("the build context is %q, want the archive", d.buildInput)
	}
	if d.buildOpts.Dockerfile != "ci/runner/Dockerfile" || len(d.buildOpts.Tags) != 1 || d.buildOpts.Tags[0] != ref {
		t.Errorf("build options: %+v", d.buildOpts)
	}
	if len(m.archives) != 1 || strings.Join(m.archives[0], ",") != "ci/runner,uv.lock" {
		t.Errorf("archive inputs = %v", m.archives)
	}
}

func TestDockerfileFormReportsFailures(t *testing.T) {
	newMirror := func() *fakeMirror {
		return &fakeMirror{commit: strings.Repeat("a", 40), digest: "0123456789ab", body: "x"}
	}
	d := &fakeDocker{buildBody: `{"stream":"Step 3/4 : RUN false\n"}` + "\n" + `{"error":"returned a non-zero code: 1"}` + "\n"}
	if _, err := Ensure(context.Background(), d, newMirror(), dockerfileProject()); err == nil || !strings.Contains(err.Error(), "non-zero code") {
		t.Errorf("build stream error: %v", err)
	}
	d = &fakeDocker{buildBody: `{"stream":"done\n"}` + "\n"}
	if _, err := Ensure(context.Background(), d, newMirror(), dockerfileProject()); err == nil || !strings.Contains(err.Error(), "missing after the build") {
		t.Errorf("no image after the build: %v", err)
	}
	m := newMirror()
	m.archiveErr = errors.New("git archive broke")
	if _, err := Ensure(context.Background(), &fakeDocker{}, m, dockerfileProject()); err == nil || !strings.Contains(err.Error(), "git archive broke") {
		t.Errorf("archive error: %v", err)
	}
}

var runnerMount = runnermount.Mount{
	Version: "2.338.0",
	Spec:    mount.Mount{Type: mount.TypeImage, Source: "local-ci/runner:2.338.0", Target: "/opt/local-ci", ReadOnly: true},
}

func TestPreflightRunsTheEntrypointInThePreflightMode(t *testing.T) {
	d := &fakeDocker{
		images: map[string]dockerimage.InspectResponse{"img": withUser("")},
		logs:   frames(t, "2.338.0\n", ""),
	}
	version, err := PreflightVersion(context.Background(), d, "img", runnerMount)
	if err != nil || version != "2.338.0" {
		t.Fatalf("version = %q, %v", version, err)
	}
	if err := Preflight(context.Background(), d, "img", runnerMount); err != nil {
		t.Fatal(err)
	}

	c, h := d.createdCfg, d.createdHost
	if strings.Join(c.Entrypoint, " ") != "/opt/local-ci/entrypoint.sh" || strings.Join(c.Cmd, " ") != "--preflight" {
		t.Errorf("entrypoint %v cmd %v: the entrypoint must be set as Entrypoint, not only Cmd", c.Entrypoint, c.Cmd)
	}
	if c.User != "1001" {
		t.Errorf("user = %q, want 1001 for an image without a user", c.User)
	}
	if strings.Join(c.Env, " ") != "HOME=/tmp/home" || c.Image != "img" {
		t.Errorf("env %v image %q", c.Env, c.Image)
	}
	if len(h.Mounts) != 1 || h.Mounts[0] != runnerMount.Spec {
		t.Errorf("mounts = %+v", h.Mounts)
	}
	if h.Init == nil || !*h.Init || h.Memory != 4<<30 || h.NetworkMode != "none" || h.Tmpfs["/tmp/home"] == "" {
		t.Errorf("host config = %+v", h)
	}
	if len(d.removed) != 2 || !d.removeOpts.Force {
		t.Errorf("removed %v, options %+v: the container must be removed, forcibly", d.removed, d.removeOpts)
	}
}

func TestPreflightKeepsAnImageUser(t *testing.T) {
	d := &fakeDocker{images: map[string]dockerimage.InspectResponse{"img": withUser("pwuser")}, logs: frames(t, "2.338.0\n", "")}
	if err := Preflight(context.Background(), d, "img", runnerMount); err != nil {
		t.Fatal(err)
	}
	if d.createdCfg.User != "pwuser" {
		t.Errorf("user = %q", d.createdCfg.User)
	}
}

func TestPreflightReportsFailureTail(t *testing.T) {
	var out strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&out, "output line %02d\n", i)
	}
	d := &fakeDocker{
		images: map[string]dockerimage.InspectResponse{"img": withUser("")},
		exit:   134,
		logs:   frames(t, out.String(), "local-ci: image has no CA bundle\n"),
	}
	err := Preflight(context.Background(), d, "img", runnerMount)
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exit 134") || !strings.Contains(msg, "local-ci: image has no CA bundle") || !strings.Contains(msg, "output line 30") {
		t.Errorf("the error lacks the status or the end of the output:\n%s", msg)
	}
	if strings.Contains(msg, "output line 10") {
		t.Errorf("the error carries more than the last %d lines:\n%s", failureLines, msg)
	}
	if got := strings.Count(msg, "output line") + strings.Count(msg, "no CA bundle"); got != failureLines {
		t.Errorf("error carries %d output lines, want %d:\n%s", got, failureLines, msg)
	}
	if len(d.removed) != 1 {
		t.Errorf("the failed container was not removed: %v", d.removed)
	}
}

func TestPreflightTimesOut(t *testing.T) {
	old := preflightTimeout
	preflightTimeout = 50 * time.Millisecond
	t.Cleanup(func() { preflightTimeout = old })
	d := &fakeDocker{
		images:     map[string]dockerimage.InspectResponse{"img": withUser("")},
		neverExits: true,
		logs:       frames(t, "still starting\n", ""),
	}
	start := time.Now()
	err := Preflight(context.Background(), d, "img", runnerMount)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "still starting") {
		t.Fatalf("want a timeout with the output, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the timeout did not bound the run")
	}
	if len(d.removed) != 1 || !d.removeOpts.Force {
		t.Errorf("a timed-out container must be removed forcibly: %v %+v", d.removed, d.removeOpts)
	}
}

func TestPreflightRequiresAVersion(t *testing.T) {
	for name, logs := range map[string][]byte{
		"no output":     nil,
		"not a version": frames(t, "hello\n", ""),
	} {
		d := &fakeDocker{images: map[string]dockerimage.InspectResponse{"img": withUser("")}, logs: logs}
		if err := Preflight(context.Background(), d, "img", runnerMount); err == nil || !strings.Contains(err.Error(), "no runner version") {
			t.Errorf("%s: want a no-version error, got %v", name, err)
		}
	}
}

func TestPreflightReadsTheVersionFromEitherStream(t *testing.T) {
	d := &fakeDocker{images: map[string]dockerimage.InspectResponse{"img": withUser("")}, logs: frames(t, "", "2.338.0\n")}
	if v, err := PreflightVersion(context.Background(), d, "img", runnerMount); err != nil || v != "2.338.0" {
		t.Fatalf("version = %q, %v", v, err)
	}
}

func TestPreflightStartFailure(t *testing.T) {
	d := &fakeDocker{images: map[string]dockerimage.InspectResponse{"img": withUser("")}, startErr: errors.New("exec format error")}
	if err := Preflight(context.Background(), d, "img", runnerMount); err == nil || !strings.Contains(err.Error(), "exec format error") {
		t.Fatalf("got %v", err)
	}
	if len(d.removed) != 1 {
		t.Errorf("the created container was not removed: %v", d.removed)
	}
}

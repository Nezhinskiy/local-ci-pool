package runnermount

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Nezhinskiy/local-ci-pool/internal/github"
	"github.com/Nezhinskiy/local-ci-pool/runner"
)

type entry struct {
	typeflag byte
	mode     int64
	body     string
	link     string
}

// makeTarball builds a gzipped tar the way actions/runner does, names in the
// "./" form included, and returns it with its sha256.
func makeTarball(t *testing.T, files map[string]entry, order []string) ([]byte, string) {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for _, name := range order {
		e := files[name]
		hdr := &tar.Header{Name: name, Mode: e.mode, Typeflag: e.typeflag, Linkname: e.link}
		if e.typeflag == 0 || e.typeflag == tar.TypeReg {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw.Bytes())
	return raw.Bytes(), hex.EncodeToString(sum[:])
}

func runnerTarball(t *testing.T) ([]byte, string) {
	return makeTarball(t, map[string]entry{
		"./":                        {typeflag: tar.TypeDir, mode: 0o755},
		"./bin/":                    {typeflag: tar.TypeDir, mode: 0o755},
		"./bin/Runner.Listener":     {mode: 0o755, body: "listener-binary"},
		"./run.sh":                  {mode: 0o755, body: "#!/bin/bash\n"},
		"./config.sh":               {mode: 0o755, body: "#!/bin/bash\n"},
		"./externals/node20/":       {typeflag: tar.TypeDir, mode: 0o755},
		"./externals/node":          {typeflag: tar.TypeSymlink, mode: 0o777, link: "node20"},
		"./externals/node20/README": {mode: 0o644, body: "node"},
		"./bin/Runner.Listener.old": {typeflag: tar.TypeLink, mode: 0o755, link: "./bin/Runner.Listener"},
	}, []string{"./", "./bin/", "./bin/Runner.Listener", "./run.sh", "./config.sh", "./externals/node20/", "./externals/node", "./externals/node20/README", "./bin/Runner.Listener.old"})
}

type fakeGH struct {
	rel       github.RunnerRelease
	relErr    error
	tarball   []byte
	trickle   bool
	downloads int
}

func (f *fakeGH) LatestRunnerRelease(_ context.Context, arch string) (github.RunnerRelease, error) {
	if arch != "arm64" {
		return github.RunnerRelease{}, errors.New("unexpected arch " + arch)
	}
	return f.rel, f.relErr
}

func (f *fakeGH) Download(ctx context.Context, _ string) (io.ReadCloser, error) {
	f.downloads++
	if f.trickle {
		return &trickleReader{ctx: ctx}, nil
	}
	return io.NopCloser(bytes.NewReader(f.tarball)), nil
}

// trickleReader delivers a byte now and then and never finishes; it ends only
// when the context does.
type trickleReader struct{ ctx context.Context }

func (t *trickleReader) Read(p []byte) (int, error) {
	select {
	case <-t.ctx.Done():
		return 0, t.ctx.Err()
	case <-time.After(5 * time.Millisecond):
		p[0] = 'x'
		return 1, nil
	}
}

func (t *trickleReader) Close() error { return nil }

type builtContext struct {
	files map[string]*tar.Header
	body  map[string]string
}

type fakeDocker struct {
	images     map[string]client.ImageInspectResult
	builds     int
	opts       client.ImageBuildOptions
	ctx        builtContext
	buildErr   error
	streamBody string // build response body; "" means success

	list        []image.Summary
	containers  []container.Summary
	removed     []string
	removeErrOn string
}

func (f *fakeDocker) ImageInspect(_ context.Context, ref string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	if r, ok := f.images[ref]; ok {
		return r, nil
	}
	return client.ImageInspectResult{}, cerrdefs.ErrNotFound.WithMessage("no such image")
}

func (f *fakeDocker) ImageBuild(_ context.Context, r io.Reader, o client.ImageBuildOptions) (client.ImageBuildResult, error) {
	f.builds++
	f.opts = o
	f.ctx = builtContext{files: map[string]*tar.Header{}, body: map[string]string{}}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return client.ImageBuildResult{}, err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return client.ImageBuildResult{}, err
		}
		f.ctx.files[hdr.Name] = hdr
		f.ctx.body[hdr.Name] = string(b)
	}
	if f.buildErr != nil {
		return client.ImageBuildResult{}, f.buildErr
	}
	body := f.streamBody
	if body == "" {
		body = `{"stream":"Successfully built\n"}` + "\n"
		if f.images == nil {
			f.images = map[string]client.ImageInspectResult{}
		}
		for _, tag := range o.Tags {
			f.images[tag] = client.ImageInspectResult{InspectResponse: image.InspectResponse{ID: "sha256:new", Config: &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{Labels: o.Labels}}}}
		}
	}
	return client.ImageBuildResult{Body: io.NopCloser(strings.NewReader(body))}, nil
}

// ImageList answers the two queries Prune makes: untagged images (filter
// "dangling") and tagged ones (filter "reference").
func (f *fakeDocker) ImageList(_ context.Context, o client.ImageListOptions) (client.ImageListResult, error) {
	_, dangling := o.Filters["dangling"]
	var items []image.Summary
	for _, img := range f.list {
		if (len(img.RepoTags) == 0) == dangling {
			items = append(items, img)
		}
	}
	return client.ImageListResult{Items: items}, nil
}

func (f *fakeDocker) ImageRemove(_ context.Context, ref string, _ client.ImageRemoveOptions) (client.ImageRemoveResult, error) {
	f.removed = append(f.removed, ref)
	if f.removeErrOn == ref {
		return client.ImageRemoveResult{}, errors.New("conflict: unable to remove")
	}
	return client.ImageRemoveResult{}, nil
}

func (f *fakeDocker) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: f.containers}, nil
}

func release(sum string) github.RunnerRelease {
	return github.RunnerRelease{Version: "2.338.0", URL: "https://example.invalid/runner.tgz", DigestSHA256: sum, BodySHA256: sum}
}

func TestEnsureRefusesDigestMismatch(t *testing.T) {
	tarball, sum := runnerTarball(t)
	other := strings.Repeat("a", 64)
	cases := map[string]struct {
		rel       github.RunnerRelease
		mentions  []string
		forbidden []string
	}{
		"API digest differs": {
			rel:       github.RunnerRelease{Version: "2.338.0", URL: "u", DigestSHA256: other, BodySHA256: sum},
			mentions:  []string{"API digest", other},
			forbidden: []string{"body marker"},
		},
		"body marker differs": {
			rel:       github.RunnerRelease{Version: "2.338.0", URL: "u", DigestSHA256: sum, BodySHA256: other},
			mentions:  []string{"body marker", other},
			forbidden: []string{"API digest"},
		},
		"both differ": {
			rel:      github.RunnerRelease{Version: "2.338.0", URL: "u", DigestSHA256: other, BodySHA256: other},
			mentions: []string{"API digest", "body marker"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := &fakeDocker{}
			_, err := Ensure(context.Background(), d, &fakeGH{rel: tc.rel, tarball: tarball}, "arm64")
			if err == nil {
				t.Fatal("Ensure accepted a tarball that does not match its published hash")
			}
			for _, want := range tc.mentions {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			for _, bad := range tc.forbidden {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("error %q blames %q, which matched", err, bad)
				}
			}
			if d.builds != 0 {
				t.Fatalf("an image was built from an unverified tarball (%d builds)", d.builds)
			}
		})
	}
}

func TestEnsureBuildsFromScratchWithEntrypoint(t *testing.T) {
	tarball, sum := runnerTarball(t)
	d := &fakeDocker{}
	gh := &fakeGH{rel: release(sum), tarball: tarball}
	m, err := Ensure(context.Background(), d, gh, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	wantMount := mount.Mount{Type: mount.TypeImage, Source: "local-ci/runner:2.338.0", Target: "/opt/local-ci", ReadOnly: true}
	if m.Version != "2.338.0" || m.Spec != wantMount {
		t.Fatalf("mount = %+v, want version 2.338.0 and %+v", m, wantMount)
	}
	if d.builds != 1 {
		t.Fatalf("builds = %d", d.builds)
	}

	files := d.ctx.files
	listener, ok := files["bin/Runner.Listener"]
	if !ok {
		t.Fatalf("the context has no bin/Runner.Listener: %v", keys(files))
	}
	if listener.Mode&0o777 != 0o755 || d.ctx.body["bin/Runner.Listener"] != "listener-binary" {
		t.Errorf("Runner.Listener mode %o body %q", listener.Mode, d.ctx.body["bin/Runner.Listener"])
	}
	ep, ok := files["entrypoint.sh"]
	if !ok {
		t.Fatalf("the context has no entrypoint.sh: %v", keys(files))
	}
	if ep.Mode&0o777 != 0o755 {
		t.Errorf("entrypoint.sh mode = %o, want 0755", ep.Mode&0o777)
	}
	if d.ctx.body["entrypoint.sh"] != string(runner.Entrypoint) || len(runner.Entrypoint) == 0 {
		t.Error("entrypoint.sh is not the embedded entrypoint")
	}
	if l := files["externals/node"]; l == nil || l.Typeflag != tar.TypeSymlink || l.Linkname != "node20" {
		t.Errorf("the symlink was not preserved: %+v", l)
	}
	if l := files["bin/Runner.Listener.old"]; l == nil || l.Typeflag != tar.TypeLink || l.Linkname != "bin/Runner.Listener" {
		t.Errorf("the hard link was not preserved with a clean target: %+v", l)
	}
	if _, ok := files["bin/"]; !ok {
		t.Errorf("directory entries were dropped: %v", keys(files))
	}
	for name := range files {
		if strings.HasPrefix(name, "./") || name == "." || name == "./" {
			t.Errorf("entry %q was not normalised", name)
		}
	}
	if df := d.ctx.body[dockerfileName]; df != "FROM scratch\nCOPY . /\n" {
		t.Errorf("Dockerfile = %q", df)
	}
	if !strings.Contains(d.ctx.body[".dockerignore"], dockerfileName) {
		t.Errorf(".dockerignore = %q must keep the Dockerfile out of the image", d.ctx.body[".dockerignore"])
	}

	o := d.opts
	if len(o.Tags) != 1 || o.Tags[0] != "local-ci/runner:2.338.0" || o.Dockerfile != dockerfileName {
		t.Errorf("build options: tags %v dockerfile %q", o.Tags, o.Dockerfile)
	}
	if o.NetworkMode != "none" {
		t.Errorf("the build has network access: %q", o.NetworkMode)
	}
	if o.Labels[labelRunnerSHA] != sum || o.Labels[labelEntrypointSHA] != entrypointSHA() {
		t.Errorf("labels = %v", o.Labels)
	}
}

func keys(m map[string]*tar.Header) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestEnsureReusesAnImageBuiltFromTheSameInputs(t *testing.T) {
	tarball, sum := runnerTarball(t)
	labelled := func(runnerSHA, ep string) client.ImageInspectResult {
		return client.ImageInspectResult{InspectResponse: image.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{Labels: map[string]string{labelRunnerSHA: runnerSHA, labelEntrypointSHA: ep}}}}}
	}
	cases := map[string]struct {
		have       map[string]client.ImageInspectResult
		wantBuilds int
	}{
		"current":               {map[string]client.ImageInspectResult{"local-ci/runner:2.338.0": labelled(sum, entrypointSHA())}, 0},
		"older entrypoint":      {map[string]client.ImageInspectResult{"local-ci/runner:2.338.0": labelled(sum, "old")}, 1},
		"different runner hash": {map[string]client.ImageInspectResult{"local-ci/runner:2.338.0": labelled("old", entrypointSHA())}, 1},
		"unlabelled":            {map[string]client.ImageInspectResult{"local-ci/runner:2.338.0": {InspectResponse: image.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{}}}}, 1},
		"absent":                {nil, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := &fakeDocker{images: tc.have}
			gh := &fakeGH{rel: release(sum), tarball: tarball}
			if _, err := Ensure(context.Background(), d, gh, "arm64"); err != nil {
				t.Fatal(err)
			}
			if d.builds != tc.wantBuilds {
				t.Fatalf("builds = %d, want %d", d.builds, tc.wantBuilds)
			}
			if (gh.downloads == 0) != (tc.wantBuilds == 0) {
				t.Fatalf("downloads = %d with %d builds", gh.downloads, d.builds)
			}
		})
	}
}

func TestEnsureReuseNeedsBothHashesToAgree(t *testing.T) {
	// A labelled image must not short-circuit verification when the two published
	// hashes disagree with each other.
	tarball, sum := runnerTarball(t)
	other := strings.Repeat("b", 64)
	d := &fakeDocker{images: map[string]client.ImageInspectResult{
		"local-ci/runner:2.338.0": {InspectResponse: image.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{Labels: map[string]string{labelRunnerSHA: sum, labelEntrypointSHA: entrypointSHA()}}}}},
	}}
	gh := &fakeGH{rel: github.RunnerRelease{Version: "2.338.0", URL: "u", DigestSHA256: sum, BodySHA256: other}, tarball: tarball}
	if _, err := Ensure(context.Background(), d, gh, "arm64"); err == nil || !strings.Contains(err.Error(), "body marker") {
		t.Fatalf("want a body-marker refusal, got %v", err)
	}
}

func TestEnsureReportsBuildFailures(t *testing.T) {
	tarball, sum := runnerTarball(t)
	d := &fakeDocker{streamBody: `{"stream":"Step 2/2 : COPY . /\n"}` + "\n" + `{"error":"COPY failed: no space left"}` + "\n"}
	_, err := Ensure(context.Background(), d, &fakeGH{rel: release(sum), tarball: tarball}, "arm64")
	if err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("want the build error, got %v", err)
	}

	d = &fakeDocker{buildErr: errors.New("daemon unavailable")}
	if _, err = Ensure(context.Background(), d, &fakeGH{rel: release(sum), tarball: tarball}, "arm64"); err == nil || !strings.Contains(err.Error(), "daemon unavailable") {
		t.Fatalf("want the daemon error, got %v", err)
	}

	// A stream that ends clean but leaves no image is still a failure.
	d = &fakeDocker{streamBody: `{"stream":"done\n"}` + "\n"}
	if _, err = Ensure(context.Background(), d, &fakeGH{rel: release(sum), tarball: tarball}, "arm64"); err == nil || !strings.Contains(err.Error(), "missing after the build") {
		t.Fatalf("want a missing-image error, got %v", err)
	}
}

func TestEnsureRefusesEntriesOutsideTheRoot(t *testing.T) {
	for _, name := range []string{"../evil", "/etc/passwd", "bin/../../evil"} {
		t.Run(name, func(t *testing.T) {
			tarball, sum := makeTarball(t, map[string]entry{name: {mode: 0o644, body: "x"}}, []string{name})
			d := &fakeDocker{}
			_, err := Ensure(context.Background(), d, &fakeGH{rel: release(sum), tarball: tarball}, "arm64")
			if err == nil || !strings.Contains(err.Error(), "outside its root") {
				t.Fatalf("want a refusal, got %v", err)
			}
			if _, ok := d.ctx.files[name]; ok {
				t.Error("the entry reached the build context")
			}
		})
	}
}

func TestEnsureReleaseLookupFailure(t *testing.T) {
	d := &fakeDocker{}
	_, err := Ensure(context.Background(), d, &fakeGH{relErr: errors.New("rate limited")}, "arm64")
	if err == nil || !strings.Contains(err.Error(), "rate limited") || d.builds != 0 {
		t.Fatalf("err = %v, builds = %d", err, d.builds)
	}
}

func summary(id string, tags ...string) image.Summary { return image.Summary{ID: id, RepoTags: tags} }

func TestPrune(t *testing.T) {
	d := &fakeDocker{
		list: []image.Summary{
			summary("sha256:cur", "local-ci/runner:2.338.0"),
			summary("sha256:old1", "local-ci/runner:2.337.0"),
			summary("sha256:old2", "local-ci/runner:2.336.0"),
			summary("sha256:busy", "local-ci/runner:2.335.0"),
			summary("sha256:mounted", "local-ci/runner:2.334.0"),
			summary("sha256:other", "local-ci/other:1"),
			summary("sha256:dangling"),
			summary("sha256:multi", "local-ci/runner:2.333.0", "keepme:latest"),
		},
		containers: []container.Summary{
			{ImageID: "sha256:busy", Image: "ubuntu"},
			{ImageID: "sha256:job", Image: "ubuntu", Mounts: []container.MountPoint{{Type: mount.TypeImage, Name: "local-ci/runner:2.334.0", Source: "/var/lib/docker/overlay2/x/merged"}}},
		},
	}
	if err := Prune(context.Background(), d, "2.338.0"); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"local-ci/runner:2.337.0": true, "local-ci/runner:2.336.0": true, "local-ci/runner:2.333.0": true}
	if len(d.removed) != len(want) {
		t.Fatalf("removed %v, want %v", d.removed, want)
	}
	for _, r := range d.removed {
		if !want[r] {
			t.Errorf("removed %s", r)
		}
	}
}

// A project whose image failed the preflight with the newest runner keeps
// the older mount, so Prune keeps every version it is given.
func TestPruneKeepsEveryVersionInUse(t *testing.T) {
	d := &fakeDocker{
		list: []image.Summary{
			summary("sha256:new", "local-ci/runner:2.339.0"),
			summary("sha256:held", "local-ci/runner:2.338.0"),
			summary("sha256:old", "local-ci/runner:2.337.0"),
		},
	}
	if err := Prune(context.Background(), d, "2.339.0", "2.338.0"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(d.removed, ",") != "local-ci/runner:2.337.0" {
		t.Fatalf("removed %v, want only 2.337.0", d.removed)
	}
}

func TestPruneAcceptsAFullReferenceAndJoinsErrors(t *testing.T) {
	d := &fakeDocker{
		list: []image.Summary{
			summary("sha256:cur", "local-ci/runner:2.338.0"),
			summary("sha256:old1", "local-ci/runner:2.337.0"),
			summary("sha256:old2", "local-ci/runner:2.336.0"),
		},
		removeErrOn: "local-ci/runner:2.337.0",
	}
	err := Prune(context.Background(), d, "local-ci/runner:2.338.0")
	if err == nil || !strings.Contains(err.Error(), "2.337.0") {
		t.Fatalf("want the removal error, got %v", err)
	}
	if strings.Join(d.removed, ",") != "local-ci/runner:2.337.0,local-ci/runner:2.336.0" {
		t.Fatalf("removed %v: a failure must not stop the others", d.removed)
	}
}

func TestEnsureBoundsTheWholeDownload(t *testing.T) {
	old := downloadTimeout
	downloadTimeout = 150 * time.Millisecond
	t.Cleanup(func() { downloadTimeout = old })
	d := &fakeDocker{}
	start := time.Now()
	_, err := Ensure(context.Background(), d, &fakeGH{rel: release(strings.Repeat("a", 64)), trickle: true}, "arm64")
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("want a deadline error, got %v", err)
	}
	if time.Since(start) > 10*time.Second || d.builds != 0 {
		t.Fatalf("took %s, builds %d", time.Since(start), d.builds)
	}
}

func labelled(id string) image.Summary {
	return image.Summary{ID: id, Labels: map[string]string{labelRunnerSHA: strings.Repeat("a", 64)}}
}

func TestPruneRemovesUntaggedRebuiltRunnerImages(t *testing.T) {
	d := &fakeDocker{list: []image.Summary{
		summary("sha256:cur", "local-ci/runner:2.338.0"),
		labelled("sha256:rebuilt-old"),
		summary("sha256:foreign-dangling"), // untagged but not ours: no labels
	}}
	if err := Prune(context.Background(), d, "2.338.0"); err != nil {
		t.Fatal(err)
	}
	if len(d.removed) != 1 || d.removed[0] != "sha256:rebuilt-old" {
		t.Fatalf("removed %v, want only the labelled untagged image", d.removed)
	}
}

func TestPruneKeepsUntaggedImagesWhileAContainerMountsARunnerImage(t *testing.T) {
	d := &fakeDocker{
		list: []image.Summary{labelled("sha256:rebuilt-old")},
		containers: []container.Summary{
			{ImageID: "sha256:job", Mounts: []container.MountPoint{{Type: mount.TypeImage, Name: "local-ci/runner:2.338.0"}}},
		},
	}
	if err := Prune(context.Background(), d, "2.338.0"); err != nil {
		t.Fatal(err)
	}
	if len(d.removed) != 0 {
		t.Fatalf("removed %v: a job may still mount the old image under its moved tag", d.removed)
	}
	d.containers = []container.Summary{{ImageID: "sha256:rebuilt-old"}}
	if err := Prune(context.Background(), d, "2.338.0"); err != nil || len(d.removed) != 0 {
		t.Fatalf("an image a container runs from was removed: %v, %v", d.removed, err)
	}
}

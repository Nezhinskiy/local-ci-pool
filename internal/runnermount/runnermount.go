// Package runnermount builds the read-only runner tree every job container
// mounts. The tree is an image, local-ci/runner:<version>, built FROM scratch
// from the actions/runner release tarball plus the pool's entrypoint, and it is
// mounted with an image mount. The tarball is verified against both hashes
// GitHub publishes before anything is built.
package runnermount

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"

	"github.com/Nezhinskiy/local-ci-pool/internal/dockerutil"
	"github.com/Nezhinskiy/local-ci-pool/internal/github"
	"github.com/Nezhinskiy/local-ci-pool/runner"
)

const (
	// Repository is the image repository of the runner trees.
	Repository = "local-ci/runner"
	// Target is where a job container sees the tree.
	Target = "/opt/local-ci"

	labelRunnerSHA     = "local-ci-pool.runner-sha256"
	labelEntrypointSHA = "local-ci-pool.entrypoint-sha256"
	dockerfileName     = "Dockerfile.local-ci"
	dockerfileBody     = "FROM scratch\nCOPY . /\n"
	dockerignoreBody   = dockerfileName + "\n.dockerignore\n"
)

// Docker is the part of the Docker API this package uses. *client.Client
// satisfies it.
type Docker interface {
	ImageInspect(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (image.InspectResponse, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options build.ImageBuildOptions) (build.ImageBuildResponse, error)
	ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error)
	ImageRemove(ctx context.Context, imageID string, options image.RemoveOptions) ([]image.DeleteResponse, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
}

// ReleaseSource is the part of the GitHub client this package uses.
// *github.Client satisfies it.
type ReleaseSource interface {
	LatestRunnerRelease(ctx context.Context, arch string) (github.RunnerRelease, error)
	Download(ctx context.Context, url string) (io.ReadCloser, error)
}

// Mount is a verified runner tree and the mount that exposes it.
type Mount struct {
	Version string
	Spec    mount.Mount
}

// Ref is the image reference of a runner version.
func Ref(version string) string { return Repository + ":" + version }

func entrypointSHA() string {
	sum := sha256.Sum256(runner.Entrypoint)
	return hex.EncodeToString(sum[:])
}

// Ensure makes sure the newest runner release is available as an image and
// returns the mount for it. arch is the runner's architecture token ("arm64"
// or "x64"). The image already present is reused when it was built from the
// same verified tarball and the same entrypoint; otherwise the tarball is
// downloaded, its SHA-256 must equal both the release asset's API digest and
// the marker in the release body, and only then is the image built.
func Ensure(ctx context.Context, d Docker, gh ReleaseSource, arch string) (Mount, error) {
	rel, err := gh.LatestRunnerRelease(ctx, arch)
	if err != nil {
		return Mount{}, fmt.Errorf("finding the latest runner release: %w", err)
	}
	ref := Ref(rel.Version)
	result := Mount{
		Version: rel.Version,
		Spec:    mount.Mount{Type: mount.TypeImage, Source: ref, Target: Target, ReadOnly: true},
	}
	ep := entrypointSHA()

	if rel.DigestSHA256 == rel.BodySHA256 && current(ctx, d, ref, rel.DigestSHA256, ep) {
		return result, nil
	}

	tarball, sum, err := fetch(ctx, gh, rel.URL)
	if err != nil {
		return Mount{}, err
	}
	defer func() { _ = os.Remove(tarball) }()
	if err := verify(rel, sum); err != nil {
		return Mount{}, err
	}

	if err := buildImage(ctx, d, ref, tarball, map[string]string{
		labelRunnerSHA:     sum,
		labelEntrypointSHA: ep,
	}); err != nil {
		return Mount{}, err
	}
	return result, nil
}

// current reports whether ref exists and was built from the verified tarball
// with the current entrypoint.
func current(ctx context.Context, d Docker, ref, runnerSHA, entrypoint string) bool {
	info, err := d.ImageInspect(ctx, ref)
	if err != nil || info.Config == nil {
		return false
	}
	return info.Config.Labels[labelRunnerSHA] == runnerSHA && info.Config.Labels[labelEntrypointSHA] == entrypoint
}

// fetch downloads the tarball to a temporary file and returns its path and
// SHA-256. The caller removes the file.
func fetch(ctx context.Context, gh ReleaseSource, url string) (string, string, error) {
	rc, err := gh.Download(ctx, url)
	if err != nil {
		return "", "", fmt.Errorf("downloading the runner tarball: %w", err)
	}
	defer func() { _ = rc.Close() }()
	f, err := os.CreateTemp("", "local-ci-runner-*.tar.gz")
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", "", fmt.Errorf("downloading the runner tarball: %w", err)
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

// verify refuses a tarball that does not match BOTH published hashes and names
// every one it does not match.
func verify(rel github.RunnerRelease, sum string) error {
	var bad []string
	if sum != rel.DigestSHA256 {
		bad = append(bad, fmt.Sprintf("the release API digest (%s)", rel.DigestSHA256))
	}
	if sum != rel.BodySHA256 {
		bad = append(bad, fmt.Sprintf("the release body marker (%s)", rel.BodySHA256))
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("runner %s tarball sha256 %s does not match %s; refusing to build", rel.Version, sum, strings.Join(bad, " and "))
}

func buildImage(ctx context.Context, d Docker, ref, tarball string, labels map[string]string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeContext(pw, f)) }()
	defer func() { _ = pr.Close() }()

	resp, err := d.ImageBuild(ctx, pr, build.ImageBuildOptions{
		Tags:        []string{ref},
		Dockerfile:  dockerfileName,
		Remove:      true,
		ForceRemove: true,
		NetworkMode: "none", // the build only copies files
		Labels:      labels,
	})
	if err != nil {
		return fmt.Errorf("building %s: %w", ref, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := dockerutil.DrainStream(resp.Body); err != nil {
		return fmt.Errorf("building %s: %w", ref, err)
	}
	if _, err := d.ImageInspect(ctx, ref); err != nil {
		return fmt.Errorf("building %s: the image is missing after the build: %w", ref, err)
	}
	return nil
}

// writeContext streams the build context: the runner tree at the root, the
// entrypoint, and the Dockerfile (which a .dockerignore keeps out of the image).
func writeContext(w io.Writer, tarball io.Reader) error {
	gz, err := gzip.NewReader(tarball)
	if err != nil {
		return fmt.Errorf("reading the runner tarball: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	tw := tar.NewWriter(w)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the runner tarball: %w", err)
		}
		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader:
			continue
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink, tar.TypeLink:
		default:
			return fmt.Errorf("the runner tarball has an unexpected entry %q of type %q", hdr.Name, string(hdr.Typeflag))
		}
		name, err := cleanName(hdr.Name)
		if err != nil {
			return err
		}
		if name == "." {
			continue
		}
		out := *hdr
		out.Name = name
		if hdr.Typeflag == tar.TypeDir {
			out.Name += "/"
		}
		if hdr.Typeflag == tar.TypeLink {
			if out.Linkname, err = cleanName(hdr.Linkname); err != nil {
				return err
			}
		}
		// The format is chosen again for the new header.
		out.Format = tar.FormatUnknown
		if err := tw.WriteHeader(&out); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return fmt.Errorf("reading the runner tarball: %w", err)
			}
		}
	}
	for _, f := range []struct {
		name string
		mode int64
		body []byte
	}{
		{"entrypoint.sh", 0o755, runner.Entrypoint},
		{dockerfileName, 0o644, []byte(dockerfileBody)},
		{".dockerignore", 0o644, []byte(dockerignoreBody)},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(f.body); err != nil {
			return err
		}
	}
	return tw.Close()
}

// cleanName turns a tar entry name into a path inside the context and refuses
// any that would leave it.
func cleanName(name string) (string, error) {
	clean := path.Clean(name)
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("the runner tarball has an entry outside its root: %q", name)
	}
	return clean, nil
}

// Prune removes the local-ci/runner images other than keep that no container
// uses. keep is a version ("2.338.0") or a full reference. Images of other
// repositories, and untagged ones, are never touched. A failure on one image
// does not stop the others; the errors are joined.
func Prune(ctx context.Context, d Docker, keep string) error {
	keepRef := keep
	if !strings.Contains(keep, ":") {
		keepRef = Ref(keep)
	}
	images, err := d.ImageList(ctx, image.ListOptions{Filters: filters.NewArgs(filters.Arg("reference", Repository+":*"))})
	if err != nil {
		return fmt.Errorf("listing runner images: %w", err)
	}
	containers, err := d.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	inUse := map[string]bool{}
	for _, c := range containers {
		inUse[c.ImageID] = true
		inUse[c.Image] = true
		for _, m := range c.Mounts {
			if m.Type == mount.TypeImage {
				inUse[m.Source] = true
				inUse[m.Name] = true
			}
		}
	}
	var errs []error
	for _, img := range images {
		for _, tag := range img.RepoTags {
			if !strings.HasPrefix(tag, Repository+":") || tag == keepRef {
				continue
			}
			if inUse[img.ID] || inUse[tag] {
				continue
			}
			if _, err := d.ImageRemove(ctx, tag, image.RemoveOptions{}); err != nil {
				errs = append(errs, fmt.Errorf("removing %s: %w", tag, err))
			}
		}
	}
	return errors.Join(errs...)
}

var (
	_ Docker        = (*client.Client)(nil)
	_ ReleaseSource = (*github.Client)(nil)
)

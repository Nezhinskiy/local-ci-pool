// Package discovery finds the repositories that opted in to the pool by
// committing .github/local-ci.json, and parses that marker.
package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"strings"

	"github.com/Nezhinskiy/local-ci-pool/internal/github"
)

// MarkerPath is where a repository commits its opt-in marker.
const MarkerPath = ".github/local-ci.json"

// Marker is a parsed .github/local-ci.json. Exactly one form is set: Image, or
// Dockerfile together with Inputs. Docker is optional and independent of the
// form.
type Marker struct {
	Image      string
	Dockerfile string
	Inputs     []string
	// Docker opts the repository into the docker class: jobs labelled
	// <identity>-docker, each in a throwaway Linux VM with its own Docker
	// daemon. Nil when the marker has no "docker" block.
	Docker *DockerVM
}

// DockerVM is the marker's "docker" block. Planned, nothing acts on it yet:
// the pool builds the class's base VM from the default branch only, running
// Provision (a script inside Inputs) as root in it, and rebuilds the base when
// Inputs change on the default branch, or when the pool's pinned Linux image,
// Docker Engine or actions/runner release changes.
type DockerVM struct {
	Provision string
	Inputs    []string
}

type rawMarker struct {
	Image      *string      `json:"image"`
	Dockerfile *string      `json:"dockerfile"`
	Inputs     *[]string    `json:"inputs"`
	Docker     *rawDockerVM `json:"docker"`
}

type rawDockerVM struct {
	Provision *string   `json:"provision"`
	Inputs    *[]string `json:"inputs"`
}

var imageRE = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

// ParseMarker validates a marker. Unknown fields, both forms at once, an image
// without a digest, bad input paths and a Dockerfile outside the inputs are
// errors, and so is a docker block without a provision script inside its own
// inputs.
func ParseMarker(b []byte) (Marker, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var raw rawMarker
	if err := dec.Decode(&raw); err != nil {
		return Marker{}, fmt.Errorf("marker is not valid JSON of the expected shape: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Marker{}, errors.New("marker has trailing data after the JSON object")
	}

	docker, err := parseDockerVM(raw.Docker)
	if err != nil {
		return Marker{}, err
	}
	imageForm := raw.Image != nil
	dockerForm := raw.Dockerfile != nil || raw.Inputs != nil
	switch {
	case imageForm == dockerForm:
		return Marker{}, errors.New("marker must have exactly one form: image, or dockerfile with inputs")
	case imageForm:
		if !imageRE.MatchString(*raw.Image) {
			if *raw.Image == "" {
				return Marker{}, errors.New("marker image is empty")
			}
			return Marker{}, fmt.Errorf("marker image %q must be pinned by digest as name@sha256:<64 hex>", *raw.Image)
		}
		return Marker{Image: *raw.Image, Docker: docker}, nil
	}

	if raw.Dockerfile == nil || raw.Inputs == nil || len(*raw.Inputs) == 0 {
		return Marker{}, errors.New("marker dockerfile form needs a dockerfile and a non-empty inputs list")
	}
	inputs, err := cleanInputs("marker input", *raw.Inputs)
	if err != nil {
		return Marker{}, err
	}
	dockerfile, err := CleanRepoPath(*raw.Dockerfile)
	if err != nil {
		return Marker{}, fmt.Errorf("marker dockerfile %q: %w", *raw.Dockerfile, err)
	}
	if !inside(dockerfile, inputs) {
		return Marker{}, fmt.Errorf("marker dockerfile %q does not lie inside any of its inputs", dockerfile)
	}
	return Marker{Dockerfile: dockerfile, Inputs: inputs, Docker: docker}, nil
}

// parseDockerVM validates the optional docker block: a provision script and a
// non-empty inputs list, every path plain and relative, the script inside the
// inputs (the base is rebuilt when they change, so the script must be one of them).
func parseDockerVM(raw *rawDockerVM) (*DockerVM, error) {
	if raw == nil {
		return nil, nil
	}
	if raw.Provision == nil || raw.Inputs == nil || len(*raw.Inputs) == 0 {
		return nil, errors.New("marker docker block needs a provision script and a non-empty inputs list")
	}
	inputs, err := cleanInputs("marker docker input", *raw.Inputs)
	if err != nil {
		return nil, err
	}
	provision, err := CleanRepoPath(*raw.Provision)
	if err != nil {
		return nil, fmt.Errorf("marker docker provision %q: %w", *raw.Provision, err)
	}
	if !inside(provision, inputs) {
		return nil, fmt.Errorf("marker docker provision %q does not lie inside any of its inputs", provision)
	}
	return &DockerVM{Provision: provision, Inputs: inputs}, nil
}

func cleanInputs(what string, raw []string) ([]string, error) {
	inputs := make([]string, 0, len(raw))
	for _, in := range raw {
		clean, err := CleanRepoPath(in)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", what, in, err)
		}
		inputs = append(inputs, clean)
	}
	return inputs, nil
}

func inside(file string, inputs []string) bool {
	for _, in := range inputs {
		if in == "." || file == in || strings.HasPrefix(file, in+"/") {
			return true
		}
	}
	return false
}

// CleanRepoPath accepts a relative path inside the repository and returns it
// cleaned. Absolute paths, ".." segments, empty paths, pathspec magic (":")
// and option-like names ("-") are refused, and so are the glob characters * ?
// and [, because git would read them as wildcards in a pathspec: the value ends
// up in git arguments. The checks hold for the cleaned value too, which is the
// one returned: "./-x" cleans to "-x" and is refused.
func CleanRepoPath(p string) (string, error) {
	if err := plainRelative(p); err != nil {
		return "", err
	}
	clean := path.Clean(p)
	if err := plainRelative(clean); err != nil {
		return "", err
	}
	return clean, nil
}

func plainRelative(p string) error {
	if p == "" {
		return errors.New("path is empty")
	}
	if strings.ContainsAny(p, "\x00\\*?[") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "-") || strings.HasPrefix(p, ":") {
		return errors.New("must be a plain relative path")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return errors.New("must not contain a .. segment")
		}
	}
	return nil
}

var (
	identityStrip = regexp.MustCompile(`[^a-z0-9-]+`)
	dashRuns      = regexp.MustCompile(`-{2,}`)
)

// Identity is the repository name reduced to [a-z0-9-]: lower case, every other
// character becomes "-", runs of "-" collapse, and the ends are trimmed.
func Identity(repoName string) string {
	s := identityStrip.ReplaceAllString(strings.ToLower(repoName), "-")
	s = dashRuns.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// Project is one repository the pool serves.
type Project struct {
	Identity string
	Repo     string // owner/name
	Ref      string // the ref the marker was read at
	// DefaultBranch is the repository's default branch: the only ref the docker class's base is built from,
	// whatever Ref is (docs/docker-class.md).
	DefaultBranch string
	Marker        Marker
}

// Source is what discovery needs from GitHub. *github.Client satisfies it.
type Source interface {
	ListPrivateOwnedRepos(ctx context.Context) ([]github.Repo, error)
	Repo(ctx context.Context, full string) (github.Repo, error)
	ReadFile(ctx context.Context, full, ref, path string) ([]byte, error)
}

// Options narrow discovery for probe mode.
type Options struct {
	OnlyRepo  string // owner/name; consider this repository only
	MarkerRef string // read the marker at this ref instead of the default branch
}

// Result is one discovery pass. Projects are the repositories to serve.
// Errored are the repositories (owner/name) that were skipped only because a
// call about them failed (the visibility re-check or the marker read); whether
// they still qualify is unknown, so a caller keeps whatever it does with them
// unchanged. A repository skipped for a known reason (gone, public, no marker,
// an invalid marker) is in neither list.
type Result struct {
	Projects []Project
	Errored  []string
}

// Discover lists the private repositories the account owns and returns the
// ones with a valid marker. Every skip is logged with its reason, and a failure
// on one repository (a 403 on its marker, a failed re-check) skips only that
// repository and is reported in Result.Errored. If the listing itself fails,
// Discover returns an empty Result and the error, so the caller keeps its
// current set.
func Discover(ctx context.Context, src Source, opt Options, log *slog.Logger) (Result, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	listed, err := src.ListPrivateOwnedRepos(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("listing private repositories: %w", err)
	}
	var res Result
	seen := map[string]string{} // identity -> repo
	only := false
	for _, r := range listed {
		if opt.OnlyRepo != "" {
			if !strings.EqualFold(r.FullName, opt.OnlyRepo) {
				continue
			}
			only = true
		}
		if !r.Private {
			log.Info("skipping repository: not private", "repo", r.FullName)
			continue
		}
		// The listing can be stale: ask again just before serving it.
		cur, err := src.Repo(ctx, r.FullName)
		if errors.Is(err, github.ErrNotFound) {
			log.Info("skipping repository: gone since the listing", "repo", r.FullName)
			continue
		}
		if err != nil {
			// One repository's failure never costs the others their place.
			log.Warn("skipping repository: the visibility re-check failed", "repo", r.FullName, "reason", err.Error())
			res.Errored = append(res.Errored, r.FullName)
			continue
		}
		if !cur.Private {
			log.Info("skipping repository: no longer private", "repo", r.FullName)
			continue
		}
		// A transferred or renamed repository answers under its new name; it
		// is served under that name once a listing shows it. A rename that
		// changes only the case is the same repository.
		if !strings.EqualFold(cur.FullName, r.FullName) {
			log.Info("skipping repository: moved since the listing", "repo", r.FullName, "now", cur.FullName)
			continue
		}
		ref := opt.MarkerRef
		if ref == "" {
			ref = cur.DefaultBranch
		}
		if ref == "" {
			log.Info("skipping repository: no default branch", "repo", r.FullName)
			continue
		}
		raw, err := src.ReadFile(ctx, r.FullName, ref, MarkerPath)
		if errors.Is(err, github.ErrNotFound) {
			log.Debug("skipping repository: no marker", "repo", r.FullName, "ref", ref)
			continue
		}
		if err != nil {
			log.Warn("skipping repository: the marker could not be read", "repo", r.FullName, "ref", ref, "reason", err.Error())
			res.Errored = append(res.Errored, r.FullName)
			continue
		}
		marker, err := ParseMarker(raw)
		if err != nil {
			log.Warn("skipping repository: invalid marker", "repo", r.FullName, "ref", ref, "reason", err.Error())
			continue
		}
		id := Identity(cur.Name)
		if id == "" {
			log.Warn("skipping repository: empty identity", "repo", r.FullName)
			continue
		}
		if prev, dup := seen[id]; dup {
			log.Warn("skipping repository: identity collision", "repo", r.FullName, "identity", id, "kept", prev)
			continue
		}
		seen[id] = r.FullName
		res.Projects = append(res.Projects, Project{Identity: id, Repo: r.FullName, Ref: ref, DefaultBranch: cur.DefaultBranch, Marker: marker})
	}
	if opt.OnlyRepo != "" && !only {
		log.Warn("skipping repository: not among the private repositories this account owns", "repo", opt.OnlyRepo)
	}
	return res, nil
}

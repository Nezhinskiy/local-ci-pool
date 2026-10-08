package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/Nezhinskiy/local-ci-pool/internal/github"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseMarkerForms(t *testing.T) {
	good := map[string]struct {
		in   string
		want Marker
	}{
		"image form": {
			`{"image":"mcr.example.invalid/tool:v1@` + digest + `"}`,
			Marker{Image: "mcr.example.invalid/tool:v1@" + digest},
		},
		"dockerfile form": {
			`{"dockerfile":"ci/runner/Dockerfile","inputs":["ci/runner"]}`,
			Marker{Dockerfile: "ci/runner/Dockerfile", Inputs: []string{"ci/runner"}},
		},
		"dockerfile among several inputs": {
			`{"dockerfile":"ci/runner/Dockerfile","inputs":["pyproject.toml","ci/runner","uv.lock"]}`,
			Marker{Dockerfile: "ci/runner/Dockerfile", Inputs: []string{"pyproject.toml", "ci/runner", "uv.lock"}},
		},
		"dot-slash prefixes are cleaned": {
			`{"dockerfile":"./infra/ci-runner/Dockerfile","inputs":["./infra/ci-runner"]}`,
			Marker{Dockerfile: "infra/ci-runner/Dockerfile", Inputs: []string{"infra/ci-runner"}},
		},
		"surrounding whitespace": {
			"\n  {\"image\":\"x@" + digest + "\"}\n",
			Marker{Image: "x@" + digest},
		},
	}
	for name, tc := range good {
		t.Run("accept/"+name, func(t *testing.T) {
			got, err := ParseMarker([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if got.Image != tc.want.Image || got.Dockerfile != tc.want.Dockerfile || strings.Join(got.Inputs, ",") != strings.Join(tc.want.Inputs, ",") {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	bad := map[string]struct{ in, why string }{
		"image without digest":           {`{"image":"ubuntu:24.04"}`, "digest"},
		"image with a short digest":      {`{"image":"x@sha256:abc"}`, "digest"},
		"image with an uppercase digest": {`{"image":"x@sha256:` + strings.Repeat("A", 64) + `"}`, "digest"},
		"image with text after digest":   {`{"image":"x@` + digest + `extra"}`, "digest"},
		"image with no name":             {`{"image":"@` + digest + `"}`, "digest"},
		"empty image":                    {`{"image":""}`, "image"},
		"dockerfile outside inputs":      {`{"dockerfile":"other/Dockerfile","inputs":["ci/runner"]}`, "inside"},
		"dockerfile by sibling prefix":   {`{"dockerfile":"ci/runner-evil/Dockerfile","inputs":["ci/runner"]}`, "inside"},
		"both forms":                     {`{"image":"x@` + digest + `","dockerfile":"d/Dockerfile","inputs":["d"]}`, "exactly one"},
		"image with inputs":              {`{"image":"x@` + digest + `","inputs":["d"]}`, "exactly one"},
		"neither form":                   {`{}`, "exactly one"},
		"unknown field":                  {`{"image":"x@` + digest + `","extra":1}`, "extra"},
		"dockerfile without inputs":      {`{"dockerfile":"d/Dockerfile"}`, "inputs"},
		"empty inputs":                   {`{"dockerfile":"d/Dockerfile","inputs":[]}`, "inputs"},
		"inputs without dockerfile":      {`{"inputs":["d"]}`, "dockerfile"},
		"parent in inputs":               {`{"dockerfile":"d/Dockerfile","inputs":["d","../x"]}`, ".."},
		"parent inside an input":         {`{"dockerfile":"d/Dockerfile","inputs":["d/../../x"]}`, ".."},
		"parent in dockerfile":           {`{"dockerfile":"d/../../Dockerfile","inputs":["d"]}`, ".."},
		"absolute input":                 {`{"dockerfile":"d/Dockerfile","inputs":["/d"]}`, "relative"},
		"glob star in input":             {`{"dockerfile":"d/Dockerfile","inputs":["d/*"]}`, "relative"},
		"glob question in input":         {`{"dockerfile":"d/Dockerfile","inputs":["d?"]}`, "relative"},
		"glob bracket in input":          {`{"dockerfile":"d/Dockerfile","inputs":["d[0-9]"]}`, "relative"},
		"glob in dockerfile":             {`{"dockerfile":"d/Docker*","inputs":["d"]}`, "relative"},
		"absolute dockerfile":            {`{"dockerfile":"/d/Dockerfile","inputs":["d"]}`, "relative"},
		"empty input":                    {`{"dockerfile":"d/Dockerfile","inputs":[""]}`, "empty"},
		"pathspec magic input":           {`{"dockerfile":"d/Dockerfile","inputs":[":(glob)d"]}`, "relative"},
		"option-like input":              {`{"dockerfile":"d/Dockerfile","inputs":["-d"]}`, "relative"},
		"option-like after cleaning":     {`{"dockerfile":"./-d/Dockerfile","inputs":["./-d"]}`, "relative"},
		"magic after cleaning":           {`{"dockerfile":"d/Dockerfile","inputs":["d","./:(glob)d"]}`, "relative"},
		"option-like dockerfile cleaned": {`{"dockerfile":"./-Dockerfile","inputs":["."]}`, "relative"},
		"trailing data":                  {`{"image":"x@` + digest + `"} {}`, "trailing"},
		"not an object":                  {`["x"]`, ""},
		"not json":                       {`image: x`, ""},
		"empty file":                     {``, ""},
		"wrong type":                     {`{"image":3}`, ""},
	}
	for name, tc := range bad {
		t.Run("reject/"+name, func(t *testing.T) {
			m, err := ParseMarker([]byte(tc.in))
			if err == nil {
				t.Fatalf("accepted %+v", m)
			}
			if !strings.Contains(err.Error(), tc.why) {
				t.Fatalf("error %q does not mention %q", err, tc.why)
			}
		})
	}
}

func TestIdentity(t *testing.T) {
	cases := map[string]string{
		"Alpha_Site.v2":  "alpha-site-v2",
		"alpha":          "alpha",
		"--Alpha--":      "alpha",
		"a__b..c":        "a-b-c",
		"Alpha  Beta":    "alpha-beta",
		"x-y":            "x-y",
		"UPPER":          "upper",
		"!!!":            "",
		"ünïcode":        "n-code",
		"a/b":            "a-b",
		"end-":           "end",
		"9lives":         "9lives",
		"alpha-site-v2 ": "alpha-site-v2",
		"a_-b":           "a-b",
		"a--b":           "a-b",
		"-a-_-b-":        "a-b",
	}
	for in, want := range cases {
		if got := Identity(in); got != want {
			t.Errorf("Identity(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeSource is a canned GitHub: a listing, per-repo details for the
// re-check, and files by "<repo>@<ref>".
type fakeSource struct {
	listing   []github.Repo
	listErr   error
	repos     map[string]github.Repo
	repoErr   map[string]error
	files     map[string]string
	fileErr   map[string]error
	listCalls int
	repoCalls []string
	readCalls []string
	readPaths []string
}

func (f *fakeSource) ListPrivateOwnedRepos(context.Context) ([]github.Repo, error) {
	f.listCalls++
	return f.listing, f.listErr
}

func (f *fakeSource) Repo(_ context.Context, full string) (github.Repo, error) {
	f.repoCalls = append(f.repoCalls, full)
	if err := f.repoErr[full]; err != nil {
		return github.Repo{}, err
	}
	r, ok := f.repos[full]
	if !ok {
		return github.Repo{}, github.ErrNotFound
	}
	return r, nil
}

func (f *fakeSource) ReadFile(_ context.Context, full, ref, path string) ([]byte, error) {
	key := full + "@" + ref
	f.readCalls = append(f.readCalls, key)
	f.readPaths = append(f.readPaths, path)
	if err := f.fileErr[key]; err != nil {
		return nil, err
	}
	s, ok := f.files[key]
	if !ok {
		return nil, github.ErrNotFound
	}
	return []byte(s), nil
}

func priv(name string) github.Repo {
	return github.Repo{FullName: "o/" + name, Name: name, DefaultBranch: "main", Private: true}
}

func testLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

const goodMarker = `{"image":"x@` + digest + `"}`

func TestDiscoverSkips(t *testing.T) {
	pub := github.Repo{FullName: "o/pubrepo", Name: "pubrepo", DefaultBranch: "main", Private: false}
	flipped := priv("flipped") // listed private, public on re-check
	src := &fakeSource{
		listing: []github.Repo{priv("alpha"), pub, flipped, priv("nomarker"), priv("badmarker")},
		repos: map[string]github.Repo{
			"o/alpha":     priv("alpha"),
			"o/flipped":   {FullName: "o/flipped", Name: "flipped", DefaultBranch: "main", Private: false},
			"o/nomarker":  priv("nomarker"),
			"o/badmarker": priv("badmarker"),
		},
		files: map[string]string{
			"o/alpha@main":     goodMarker,
			"o/flipped@main":   goodMarker,
			"o/badmarker@main": `{"image":"ubuntu:24.04"}`,
		},
	}
	log, buf := testLog()
	res, err := Discover(context.Background(), src, Options{}, log)
	got := res.Projects
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("projects = %+v", got)
	}
	if len(res.Errored) != 0 {
		t.Fatalf("errored = %v; gone, public, unmarked and invalid repositories are known skips, not errors", res.Errored)
	}
	p := got[0]
	if p.Identity != "alpha" || p.Repo != "o/alpha" || p.Ref != "main" || p.Marker.Image != "x@"+digest {
		t.Fatalf("project = %+v", p)
	}
	logs := buf.String()
	for _, want := range []string{
		"o/pubrepo", "not private",
		"o/flipped", "no longer private",
		"o/nomarker", "no marker",
		"o/badmarker", "invalid marker", "digest",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	for _, r := range src.readPaths {
		if r != ".github/local-ci.json" {
			t.Errorf("read %q", r)
		}
	}
	for _, c := range src.readCalls {
		if strings.HasPrefix(c, "o/pubrepo") {
			t.Errorf("read the marker of a public repository: %s", c)
		}
	}
}

// The re-check answers under the repository's current name. One that moved
// to another owner, or was renamed, since the listing is skipped as a known
// reason (not errored); a rename that changes only the case is kept.
func TestDiscoverSkipsARepoMovedSinceTheListing(t *testing.T) {
	src := &fakeSource{
		listing: []github.Repo{priv("moved"), priv("cased")},
		repos: map[string]github.Repo{
			"o/moved": {FullName: "someone-else/moved", Name: "moved", DefaultBranch: "main", Private: true},
			"o/cased": {FullName: "O/Cased", Name: "Cased", DefaultBranch: "main", Private: true},
		},
		files: map[string]string{"o/moved@main": goodMarker, "o/cased@main": goodMarker},
	}
	log, buf := testLog()
	res, err := Discover(context.Background(), src, Options{}, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Projects) != 1 || res.Projects[0].Repo != "o/cased" {
		t.Fatalf("projects = %+v, want only o/cased", res.Projects)
	}
	if len(res.Errored) != 0 {
		t.Fatalf("errored = %v; a moved repository is a known skip", res.Errored)
	}
	if !strings.Contains(buf.String(), "moved since the listing") {
		t.Errorf("the skip is not logged:\n%s", buf.String())
	}
}

func TestDiscoverReadsAtTheRecheckedDefaultBranch(t *testing.T) {
	listed := priv("alpha")
	listed.DefaultBranch = "old"
	now := priv("alpha")
	now.DefaultBranch = "trunk"
	src := &fakeSource{
		listing: []github.Repo{listed},
		repos:   map[string]github.Repo{"o/alpha": now},
		files:   map[string]string{"o/alpha@trunk": goodMarker},
	}
	log, _ := testLog()
	res, err := Discover(context.Background(), src, Options{}, log)
	got := res.Projects
	if err != nil || len(got) != 1 || got[0].Ref != "trunk" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestDiscoverListingErrorReturnsNil(t *testing.T) {
	src := &fakeSource{listErr: errors.New("boom")}
	log, _ := testLog()
	got, err := Discover(context.Background(), src, Options{}, log)
	if err == nil || got.Projects != nil || got.Errored != nil {
		t.Fatalf("got %+v, %v; want an empty result and an error", got, err)
	}
}

func TestDiscoverOneRepositoryFailingNeverAbortsTheOthers(t *testing.T) {
	forbidden := errors.New("403 Forbidden")
	for name, src := range map[string]*fakeSource{
		"marker read returns 403": {
			listing: []github.Repo{priv("broken"), priv("alpha")},
			repos:   map[string]github.Repo{"o/broken": priv("broken"), "o/alpha": priv("alpha")},
			fileErr: map[string]error{"o/broken@main": forbidden},
			files:   map[string]string{"o/alpha@main": goodMarker},
		},
		"re-check returns 403": {
			listing: []github.Repo{priv("broken"), priv("alpha")},
			repos:   map[string]github.Repo{"o/alpha": priv("alpha")},
			repoErr: map[string]error{"o/broken": forbidden},
			files:   map[string]string{"o/alpha@main": goodMarker},
		},
	} {
		t.Run(name, func(t *testing.T) {
			log, buf := testLog()
			res, err := Discover(context.Background(), src, Options{}, log)
			got := res.Projects
			if err != nil {
				t.Fatalf("a per-repository failure aborted discovery: %v", err)
			}
			if len(got) != 1 || got[0].Repo != "o/alpha" {
				t.Fatalf("projects = %+v, want only o/alpha", got)
			}
			if strings.Join(res.Errored, ",") != "o/broken" {
				t.Fatalf("errored = %v, want o/broken: its state is unknown, not absent", res.Errored)
			}
			if !strings.Contains(buf.String(), "o/broken") || !strings.Contains(buf.String(), "403") {
				t.Errorf("the skip and its reason are not logged:\n%s", buf.String())
			}
		})
	}
}

func TestDiscoverSkipsARepoDeletedBetweenListAndRecheck(t *testing.T) {
	src := &fakeSource{
		listing: []github.Repo{priv("gone"), priv("alpha")},
		repos:   map[string]github.Repo{"o/alpha": priv("alpha")},
		files:   map[string]string{"o/alpha@main": goodMarker},
	}
	log, buf := testLog()
	res, err := Discover(context.Background(), src, Options{}, log)
	got := res.Projects
	if err != nil || len(got) != 1 || got[0].Repo != "o/alpha" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if len(res.Errored) != 0 {
		t.Fatalf("errored = %v; a repository gone on re-check is absent, not errored", res.Errored)
	}
	if !strings.Contains(buf.String(), "o/gone") {
		t.Errorf("the skip is not logged:\n%s", buf.String())
	}
}

func TestDiscoverIdentityCollisionKeepsTheFirst(t *testing.T) {
	a := github.Repo{FullName: "o/Alpha_Site", Name: "Alpha_Site", DefaultBranch: "main", Private: true}
	b := github.Repo{FullName: "o/alpha-site", Name: "alpha-site", DefaultBranch: "main", Private: true}
	sym := github.Repo{FullName: "o/...", Name: "...", DefaultBranch: "main", Private: true}
	src := &fakeSource{
		listing: []github.Repo{a, b, sym},
		repos:   map[string]github.Repo{a.FullName: a, b.FullName: b, sym.FullName: sym},
		files:   map[string]string{a.FullName + "@main": goodMarker, b.FullName + "@main": goodMarker, sym.FullName + "@main": goodMarker},
	}
	log, buf := testLog()
	res, err := Discover(context.Background(), src, Options{}, log)
	got := res.Projects
	if err != nil || len(got) != 1 || got[0].Repo != "o/Alpha_Site" {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, want := range []string{"identity collision", "o/alpha-site", "empty identity", "o/..."} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestProbeOptionsReadMarkerAtRef(t *testing.T) {
	probe := priv("probe")
	src := &fakeSource{
		listing: []github.Repo{priv("alpha"), probe},
		repos:   map[string]github.Repo{"o/probe": probe, "o/alpha": priv("alpha")},
		files: map[string]string{
			"o/probe@feature/probe": goodMarker,
			"o/probe@main":          `{"image":"wrong"}`,
			"o/alpha@main":          goodMarker,
		},
	}
	log, _ := testLog()
	res, err := Discover(context.Background(), src, Options{OnlyRepo: "o/probe", MarkerRef: "feature/probe"}, log)
	got := res.Projects
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Repo != "o/probe" || got[0].Ref != "feature/probe" {
		t.Fatalf("projects = %+v", got)
	}
	if strings.Join(src.readCalls, ",") != "o/probe@feature/probe" {
		t.Errorf("ReadFile calls = %v", src.readCalls)
	}
	if strings.Join(src.repoCalls, ",") != "o/probe" {
		t.Errorf("visibility re-check calls = %v, want only o/probe", src.repoCalls)
	}
}

func TestProbeOptionsStillRecheckVisibility(t *testing.T) {
	probe := priv("probe")
	src := &fakeSource{
		listing: []github.Repo{probe},
		repos:   map[string]github.Repo{"o/probe": {FullName: "o/probe", Name: "probe", DefaultBranch: "main", Private: false}},
		files:   map[string]string{"o/probe@feature/probe": goodMarker},
	}
	log, _ := testLog()
	res, err := Discover(context.Background(), src, Options{OnlyRepo: "o/probe", MarkerRef: "feature/probe"}, log)
	got := res.Projects
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v; a repository that turned public must not be served", got, err)
	}
	if len(src.readCalls) != 0 {
		t.Errorf("read a file of a public repository: %v", src.readCalls)
	}
}

func TestProbeOnlyRepoMustBeAListedPrivateOwnedRepo(t *testing.T) {
	src := &fakeSource{
		listing: []github.Repo{priv("alpha")},
		repos:   map[string]github.Repo{"o/elsewhere": priv("elsewhere"), "o/alpha": priv("alpha")},
		files:   map[string]string{"o/elsewhere@main": goodMarker},
	}
	log, buf := testLog()
	res, err := Discover(context.Background(), src, Options{OnlyRepo: "o/elsewhere"}, log)
	got := res.Projects
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if !strings.Contains(buf.String(), "not among") {
		t.Errorf("the refusal is not logged:\n%s", buf.String())
	}
}

func TestDiscoverNilLogger(t *testing.T) {
	src := &fakeSource{
		listing: []github.Repo{priv("alpha")},
		repos:   map[string]github.Repo{"o/alpha": priv("alpha")},
		files:   map[string]string{"o/alpha@main": fmt.Sprint(goodMarker)},
	}
	if res, err := Discover(context.Background(), src, Options{}, nil); err != nil || len(res.Projects) != 1 {
		t.Fatalf("got %+v, %v", res, err)
	}
}

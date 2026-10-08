package mirror

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// source is a throwaway "GitHub": a working repository at <srv>/o/alpha.git
// that a Mirror fetches from through BaseURL.
type source struct {
	t    *testing.T
	srv  string
	work string
}

func gitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.invalid")
}

func newSource(t *testing.T) *source {
	t.Helper()
	gitEnv(t)
	srv := t.TempDir()
	work := filepath.Join(srv, "o", "alpha.git")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &source{t: t, srv: srv, work: work}
	s.git("init", "-q", "-b", "main")
	s.git("config", "uploadpack.allowFilter", "true")
	s.git("config", "uploadpack.allowAnySHA1InWant", "true")
	s.git("config", "commit.gpgsign", "false")
	return s
}

func (s *source) git(args ...string) string {
	s.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = s.work
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (s *source) write(name, body string, mode os.FileMode) {
	s.t.Helper()
	p := filepath.Join(s.work, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		s.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		s.t.Fatal(err)
	}
}

func (s *source) commit(msg string) string {
	s.t.Helper()
	s.git("add", "-A")
	s.git("commit", "-q", "-m", msg)
	return s.git("rev-parse", "HEAD")
}

func (s *source) mirror(t *testing.T) Mirror {
	return Mirror{Root: t.TempDir(), BaseURL: s.srv}
}

func readTar(t *testing.T, r io.Reader) map[string]*tar.Header {
	t.Helper()
	out := map[string]*tar.Header{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = h
	}
}

func TestFetchMirrorsARef(t *testing.T) {
	s := newSource(t)
	s.write("a.txt", "1", 0o644)
	first := s.commit("one")
	m := s.mirror(t)
	ctx := context.Background()

	got, err := m.Fetch(ctx, "o/alpha", "main")
	if err != nil || got != first {
		t.Fatalf("Fetch = %q, %v; want %q", got, err, first)
	}
	if want := filepath.Join(m.Root, "alpha.git"); m.Dir("o/alpha") != want {
		t.Fatalf("Dir = %q, want %q", m.Dir("o/alpha"), want)
	}
	if _, err := os.Stat(filepath.Join(m.Dir("o/alpha"), "HEAD")); err != nil {
		t.Fatalf("no bare repository at %s: %v", m.Dir("o/alpha"), err)
	}

	s.write("a.txt", "2", 0o644)
	second := s.commit("two")
	if got, err = m.Fetch(ctx, "o/alpha", "main"); err != nil || got != second {
		t.Fatalf("second Fetch = %q, %v; want %q", got, err, second)
	}

	s.git("checkout", "-q", "-b", "feature/x")
	s.write("b.txt", "x", 0o644)
	third := s.commit("branch")
	if got, err = m.Fetch(ctx, "o/alpha", "feature/x"); err != nil || got != third {
		t.Fatalf("branch Fetch = %q, %v; want %q", got, err, third)
	}
	// A moved-back branch is followed too (the refspec is forced).
	s.git("checkout", "-q", "main")
	s.git("branch", "-f", "feature/x", first)
	if got, err = m.Fetch(ctx, "o/alpha", "feature/x"); err != nil || got != first {
		t.Fatalf("rewound branch Fetch = %q, %v; want %q", got, err, first)
	}
}

func TestFetchRejectsBadArguments(t *testing.T) {
	m := Mirror{Root: t.TempDir(), BaseURL: t.TempDir()}
	for _, tc := range []struct{ repo, ref string }{
		{"o/alpha", "--upload-pack=touch /tmp/x"},
		{"o/alpha", "-x"},
		{"o/alpha", "a:b"},
		{"o/alpha", "a b"},
		{"o/alpha", "a..b"},
		{"o/alpha", ""},
		{"o/alpha", "/main"},
		{"o/alpha", "main/"},
		{"o/alpha", "a//b"},
		{"o/alpha", "x.lock"},
		{"../alpha", "main"},
		{"alpha", "main"},
		{"o/../x", "main"},
		{"o/a b", "main"},
	} {
		if _, err := m.Fetch(context.Background(), tc.repo, tc.ref); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("Fetch(%q, %q): want an invalid-argument error, got %v", tc.repo, tc.ref, err)
		}
	}
	if entries, _ := os.ReadDir(m.Root); len(entries) != 0 {
		t.Errorf("a rejected Fetch left %d entries in the mirror root", len(entries))
	}
}

func TestFetchUnknownRefFails(t *testing.T) {
	s := newSource(t)
	s.write("a.txt", "1", 0o644)
	s.commit("one")
	m := s.mirror(t)
	if _, err := m.Fetch(context.Background(), "o/alpha", "nope"); err == nil {
		t.Fatal("want an error for a ref that does not exist")
	}
}

// gitSpy puts a `git` wrapper first on PATH that logs its arguments, one
// invocation per line with fields joined by "|", and then runs the real git.
func gitSpy(t *testing.T) (logPath string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '|%s' \"$a\" >> '" + logPath + "'; done\nprintf '\\n' >> '" + logPath + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func TestFetchAndArchiveUseTheGhCredentialHelperAndWriteNoCredential(t *testing.T) {
	s := newSource(t)
	s.write("ci/Dockerfile", "FROM scratch\n", 0o644)
	commit := s.commit("one")
	spy := gitSpy(t)
	m := s.mirror(t)
	ctx := context.Background()

	if _, err := m.Fetch(ctx, "o/alpha", "main"); err != nil {
		t.Fatal(err)
	}
	rc, err := m.Archive(ctx, m.Dir("o/alpha"), commit, []string{"ci"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, rc); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()

	b, err := os.ReadFile(spy)
	if err != nil {
		t.Fatal(err)
	}
	var fetch, archive bool
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Split(line, "|")
		joined := strings.Join(fields, " ")
		isFetch := strings.Contains(joined, " fetch ")
		isArchive := strings.Contains(joined, " archive ")
		if !isFetch && !isArchive {
			continue
		}
		const helper = "-c credential.helper= -c credential.helper=!gh auth git-credential"
		if !strings.HasPrefix(strings.TrimSpace(joined), helper) {
			t.Errorf("git call without the credential helper arguments: %s", joined)
		}
		if isFetch {
			fetch = true
			if !strings.Contains(joined, "fetch --filter=blob:none") || !strings.Contains(joined, "origin +main:main") {
				t.Errorf("unexpected fetch arguments: %s", joined)
			}
		}
		if isArchive {
			archive = true
		}
	}
	if !fetch || !archive {
		t.Fatalf("fetch=%v archive=%v in the git log:\n%s", fetch, archive, b)
	}

	cfg, err := os.ReadFile(filepath.Join(m.Dir("o/alpha"), "config"))
	if err != nil {
		t.Fatal(err)
	}
	var settings []string
	for _, line := range strings.Split(string(cfg), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "url =") { // the path of a temp dir is not a setting
			settings = append(settings, line)
		}
	}
	if strings.Contains(strings.ToLower(strings.Join(settings, "\n")), "credential") || strings.Contains(string(cfg), "@") {
		t.Errorf("the mirror's config holds a credential or a userinfo URL:\n%s", cfg)
	}
}

func TestMirrorArchivePreservesModeAndSymlink(t *testing.T) {
	s := newSource(t)
	s.write("run.sh", "#!/bin/sh\necho hi\n", 0o755)
	s.write("plain.txt", "text", 0o644)
	if err := os.Symlink("run.sh", filepath.Join(s.work, "link")); err != nil {
		t.Fatal(err)
	}
	s.write("other/unrelated.txt", "x", 0o644)
	commit := s.commit("modes")
	m := s.mirror(t)
	ctx := context.Background()

	if got, err := m.Fetch(ctx, "o/alpha", "main"); err != nil || got != commit {
		t.Fatalf("Fetch = %q, %v", got, err)
	}
	// The mirror is a partial clone: the blobs are not there until Archive asks.
	missing, err := exec.Command("git", "-C", m.Dir("o/alpha"), "rev-list", "--objects", "--missing=print", commit).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(missing), "\n?") && !strings.HasPrefix(string(missing), "?") {
		t.Fatalf("the fetch brought blobs although it asked for none:\n%s", missing)
	}
	rc, err := m.Archive(ctx, m.Dir("o/alpha"), commit, []string{"run.sh", "link", "plain.txt"})
	if err != nil {
		t.Fatal(err)
	}
	files := readTar(t, rc)
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if h := files["run.sh"]; h == nil || h.Mode&0o777 != 0o755 {
		t.Errorf("run.sh = %+v, want mode 0755", h)
	}
	if h := files["plain.txt"]; h == nil || h.Mode&0o777 != 0o644 {
		t.Errorf("plain.txt = %+v, want mode 0644", h)
	}
	if h := files["link"]; h == nil || h.Typeflag != tar.TypeSymlink || h.Linkname != "run.sh" {
		t.Errorf("link = %+v, want a symlink to run.sh", h)
	}
	if _, ok := files["other/unrelated.txt"]; ok {
		t.Error("the archive holds a path outside the inputs")
	}
}

func TestArchiveReportsGitFailureAtTheEnd(t *testing.T) {
	s := newSource(t)
	s.write("a.txt", "1", 0o644)
	commit := s.commit("one")
	m := s.mirror(t)
	if _, err := m.Fetch(context.Background(), "o/alpha", "main"); err != nil {
		t.Fatal(err)
	}
	rc, err := m.Archive(context.Background(), m.Dir("o/alpha"), commit, []string{"missing-dir"})
	if err != nil {
		t.Fatal(err) // starting git is fine; the failure arrives with the stream
	}
	defer func() { _ = rc.Close() }()
	_, err = io.Copy(io.Discard, rc)
	if err == nil || !strings.Contains(err.Error(), "missing-dir") {
		t.Fatalf("want git's complaint about the missing path, got %v", err)
	}
}

func TestInputsDigestStableAcrossUnrelatedCommits(t *testing.T) {
	s := newSource(t)
	s.write("ci/runner/Dockerfile", "FROM scratch\n", 0o644)
	s.write("uv.lock", "lock1", 0o644)
	s.write("src/app.py", "print(1)", 0o644)
	c1 := s.commit("one")
	m := s.mirror(t)
	ctx := context.Background()
	inputs := []string{"ci/runner", "uv.lock"}

	fetchDigest := func() (string, string) {
		t.Helper()
		commit, err := m.Fetch(ctx, "o/alpha", "main")
		if err != nil {
			t.Fatal(err)
		}
		d, err := m.InputsDigest(ctx, m.Dir("o/alpha"), commit, inputs)
		if err != nil {
			t.Fatal(err)
		}
		return commit, d
	}
	_, d1 := fetchDigest()
	if len(d1) != 12 || strings.Trim(d1, "0123456789abcdef") != "" {
		t.Fatalf("digest %q is not 12 hex characters", d1)
	}

	s.write("src/app.py", "print(2)", 0o644) // outside the inputs
	c2 := s.commit("unrelated")
	commit2, d2 := fetchDigest()
	if commit2 == c1 || commit2 != c2 {
		t.Fatalf("commits: %s %s %s", c1, c2, commit2)
	}
	if d2 != d1 {
		t.Errorf("an unrelated commit changed the digest: %s -> %s", d1, d2)
	}

	s.write("ci/runner/Dockerfile", "FROM scratch\nRUN true\n", 0o644) // inside
	s.commit("inside")
	if _, d3 := fetchDigest(); d3 == d1 {
		t.Errorf("a commit inside the inputs kept the digest %s", d1)
	}

	s.write("uv.lock", "lock2", 0o644) // a single-file input
	s.commit("lock")
	_, d4 := fetchDigest()
	s.write("uv.lock", "lock1", 0o644)
	s.write("ci/runner/Dockerfile", "FROM scratch\n", 0o644)
	s.commit("back to the original content")
	if _, d5 := fetchDigest(); d5 != d1 || d4 == d1 {
		t.Errorf("digest follows content, not history: d1=%s d4=%s d5=%s", d1, d4, d5)
	}
	// The digest is of the commit asked for, not of whatever the mirror's HEAD is.
	if again, err := m.InputsDigest(ctx, m.Dir("o/alpha"), c1, inputs); err != nil || again != d1 {
		t.Errorf("digest of the first commit = %q, %v; want %s", again, err, d1)
	}
	s.write("uv.lock", "lock3", 0o644)
	s.commit("tip moves on")
	if _, d6 := fetchDigest(); d6 == d1 {
		t.Errorf("a new lock file kept the digest %s", d1)
	}
	if again, err := m.InputsDigest(ctx, m.Dir("o/alpha"), c1, inputs); err != nil || again != d1 {
		t.Errorf("digest of the first commit after the tip moved = %q, %v; want %s", again, err, d1)
	}
}

func TestInputsDigestRefusesEmptyAndUnsafeInputs(t *testing.T) {
	s := newSource(t)
	s.write("a.txt", "1", 0o644)
	commit := s.commit("one")
	m := s.mirror(t)
	if _, err := m.Fetch(context.Background(), "o/alpha", "main"); err != nil {
		t.Fatal(err)
	}
	dir := m.Dir("o/alpha")
	ctx := context.Background()
	if _, err := m.InputsDigest(ctx, dir, commit, []string{"no/such/path"}); err == nil || !strings.Contains(err.Error(), "match nothing") {
		t.Errorf("inputs that match nothing: %v", err)
	}
	for _, in := range [][]string{nil, {"--output=x"}, {":(top)a.txt"}, {"../x"}, {"/abs"}, {""}} {
		if _, err := m.InputsDigest(ctx, dir, commit, in); err == nil {
			t.Errorf("InputsDigest(%q): want an error", in)
		}
		if _, err := m.Archive(ctx, dir, commit, in); err == nil {
			t.Errorf("Archive(%q): want an error", in)
		}
	}
	for _, bad := range []string{"HEAD", "--all", "main; rm", ""} {
		if _, err := m.InputsDigest(ctx, dir, bad, []string{"a.txt"}); err == nil {
			t.Errorf("InputsDigest with commit %q: want an error", bad)
		}
	}
}

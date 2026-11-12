package mirror

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// serveHTTP serves the source over git's smart HTTP protocol and answers 401
// to a request whose password is not token, as GitHub does for a private
// repository. It returns the base URL.
func (s *source) serveHTTP(t *testing.T, token string) string {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(out)), "git-http-backend"),
		Env: []string{
			"GIT_PROJECT_ROOT=" + s.srv, "GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + os.DevNull,
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pass, ok := r.BasicAuth(); !ok || pass != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="t"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// failingGh puts first on PATH a `gh` that exits 1 without printing, as gh
// does when its keychain read times out on a loaded Mac. It returns a path
// that exists once gh has been run.
func failingGh(t *testing.T) (ranPath string) {
	t.Helper()
	dir := t.TempDir()
	ranPath = filepath.Join(dir, "ran")
	script := "#!/bin/sh\ntouch '" + ranPath + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return ranPath
}

// The pool's own token, not a `gh` started per git call, authenticates the
// fetch and the lazy blob fetch of `git archive`: gh fails here as it does
// when its keychain read times out, and both must still succeed.
func TestFetchAndArchiveAuthenticateWithThePoolTokenWithoutGh(t *testing.T) {
	s := newSource(t)
	s.write("ci/Dockerfile", "FROM scratch\n", 0o644)
	commit := s.commit("one")
	const token = "tok-pool-5f3a"
	base := s.serveHTTP(t, token)
	ghRan := failingGh(t)
	spy := gitSpy(t)
	ctx := context.Background()

	wrong := Mirror{Root: t.TempDir(), BaseURL: base, Token: func(context.Context) (string, error) { return "wrong", nil }}
	if _, err := wrong.Fetch(ctx, "o/alpha", "main"); err == nil {
		t.Fatal("a wrong token fetched: the server does not check the password")
	}

	m := Mirror{Root: t.TempDir(), BaseURL: base, Token: func(context.Context) (string, error) { return token, nil }}
	got, err := m.Fetch(ctx, "o/alpha", "main")
	if err != nil || got != commit {
		t.Fatalf("Fetch = %q, %v; want %q", got, err, commit)
	}
	rc, err := m.Archive(ctx, m.Dir("o/alpha"), commit, []string{"ci"})
	if err != nil {
		t.Fatal(err)
	}
	files := readTar(t, rc)
	_ = rc.Close()
	if files["ci/Dockerfile"] == nil {
		t.Fatalf("archive holds %v, want ci/Dockerfile", files)
	}

	if _, err := os.Stat(ghRan); err == nil {
		t.Error("a git call ran gh")
	}
	calls, err := os.ReadFile(spy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), token) {
		t.Errorf("the token is in a git argument:\n%s", calls)
	}
	err = filepath.WalkDir(m.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(b), token) {
			t.Errorf("the token is written in %s", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A token the pool cannot read stops the call before git runs, with that error.
func TestFetchReportsTheTokenError(t *testing.T) {
	s := newSource(t)
	s.write("a.txt", "1", 0o644)
	s.commit("one")
	spy := gitSpy(t)
	errNoToken := errors.New("reading the GitHub token: gh auth token exited with status 1")
	m := Mirror{Root: t.TempDir(), BaseURL: s.srv, Token: func(context.Context) (string, error) { return "", errNoToken }}
	if _, err := m.Fetch(context.Background(), "o/alpha", "main"); !errors.Is(err, errNoToken) {
		t.Fatalf("Fetch error = %v, want it to wrap %v", err, errNoToken)
	}
	calls, _ := os.ReadFile(spy)
	if strings.Contains(string(calls), "|fetch|") {
		t.Errorf("git fetch ran without a token:\n%s", calls)
	}
}

package localcipool

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The deny-list patterns of these tests are neutral stand-ins for the private
// names the real list (an Actions secret) holds.
const testDenyList = "hidden-handle\nprivate-alpha\n"

// denyRepo is a scratch repository with one clean commit by a neutral author.
type denyRepo struct {
	t   *testing.T
	dir string
}

func newDenyRepo(t *testing.T) *denyRepo {
	t.Helper()
	r := &denyRepo{t: t, dir: t.TempDir()}
	r.git(nil, "init", "-q")
	r.commit("README", "clean\n", "first", nil)
	return r
}

func (r *denyRepo) git(env []string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_AUTHOR_NAME=alpha", "GIT_AUTHOR_EMAIL=alpha@example.invalid",
		"GIT_COMMITTER_NAME=alpha", "GIT_COMMITTER_EMAIL=alpha@example.invalid",
	)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *denyRepo) commit(name, body, msg string, env []string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git(nil, "add", name)
	r.git(env, "-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
}

// scan runs the CI deny-list script over the range (from, HEAD] and returns
// its exit code and output.
func (r *denyRepo) scan(from string) (int, string) {
	r.t.Helper()
	script, err := filepath.Abs(filepath.Join("scripts", "deny-list.sh"))
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "DENYLIST="+testDenyList, "RANGE_FROM="+from, "RANGE_TO="+r.git(nil, "rev-parse", "HEAD"))
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatal(err)
	}
	return code, string(out)
}

func TestDenyListScan(t *testing.T) {
	cases := map[string]struct {
		plant func(r *denyRepo)
		want  string // in the output; "" means a clean pass
	}{
		"clean": {func(r *denyRepo) { r.commit("a.txt", "fine\n", "add a", nil) }, ""},
		"commit message": {func(r *denyRepo) {
			r.commit("a.txt", "fine\n", "fix for private-alpha", nil)
		}, "commit messages match"},
		"author email": {func(r *denyRepo) {
			r.commit("a.txt", "fine\n", "add a", []string{"GIT_AUTHOR_EMAIL=Hidden-Handle@example.invalid"})
		}, "author or committer"},
		"committer name": {func(r *denyRepo) {
			r.commit("a.txt", "fine\n", "add a", []string{"GIT_COMMITTER_NAME=hidden-handle"})
		}, "author or committer"},
		"annotated tag message": {func(r *denyRepo) {
			r.git(nil, "-c", "tag.gpgsign=false", "tag", "-a", "v0.0.1", "-m", "release for private-alpha")
		}, "annotated tags"},
		"tagger email": {func(r *denyRepo) {
			r.git([]string{"GIT_COMMITTER_EMAIL=hidden-handle@example.invalid"}, "-c", "tag.gpgsign=false", "tag", "-a", "v0.0.2", "-m", "release")
		}, "annotated tags"},
		"file content": {func(r *denyRepo) { r.commit("b.txt", "see private-alpha\n", "add b", nil) }, "file contents match"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newDenyRepo(t)
			base := r.git(nil, "rev-parse", "HEAD")
			tc.plant(r)
			code, out := r.scan(base)
			if tc.want == "" {
				if code != 0 {
					t.Fatalf("exit %d on a clean push:\n%s", code, out)
				}
				return
			}
			if code != 1 || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d, want 1 naming %q:\n%s", code, tc.want, out)
			}
			for _, secret := range []string{"hidden-handle", "Hidden-Handle", "private-alpha"} {
				if strings.Contains(out, secret) {
					t.Fatalf("the output prints the matched text %q:\n%s", secret, out)
				}
			}
		})
	}
}

// An empty list cannot vouch for anything: the scan fails closed.
func TestDenyListScanFailsClosedWithoutAList(t *testing.T) {
	r := newDenyRepo(t)
	script, _ := filepath.Abs(filepath.Join("scripts", "deny-list.sh"))
	cmd := exec.Command("bash", script)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "DENYLIST=\n  \n", "RANGE_TO="+r.git(nil, "rev-parse", "HEAD"))
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "DENYLIST secret is empty") {
		t.Fatalf("want a failure, got %v:\n%s", err, out)
	}
}

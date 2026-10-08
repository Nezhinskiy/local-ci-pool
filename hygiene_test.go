package localcipool

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// hostedRunsOn is the only form a runs-on value may take: a literal
// GitHub-hosted label. Expressions, lists and self-hosted labels are refused.
var hostedRunsOn = regexp.MustCompile(`^(ubuntu|macos|windows)-[a-z0-9.-]+$`)

var runsOnLine = regexp.MustCompile(`^\s*(?:-\s+)?runs-on:[ \t]*(.*?)[ \t]*$`)

// runsOnViolations returns one message per runs-on line in content whose value
// is not a literal hosted label. A runs-on key with no inline value (a block
// list or mapping on the following lines) is a violation too.
func runsOnViolations(name, content string) []string {
	var out []string
	for i, line := range strings.Split(content, "\n") {
		m := runsOnLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		val := m[1]
		if j := strings.Index(val, " #"); j >= 0 {
			val = strings.TrimRight(val[:j], " \t")
		}
		if !hostedRunsOn.MatchString(val) {
			out = append(out, fmt.Sprintf("%s:%d: runs-on %q is not a literal hosted label", name, i+1, val))
		}
	}
	return out
}

func TestWorkflowsUseHostedRunnersOnly(t *testing.T) {
	var files []string
	for _, pattern := range []string{
		".github/workflows/*.yml",
		".github/workflows/*.yaml",
		"route/*.yml",
		"route/*.yaml",
	} {
		m, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatal("no workflow files found: the guard would pass vacuously")
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range runsOnViolations(f, string(b)) {
			t.Error(v)
		}
	}
}

func TestRunsOnCheckDiscriminates(t *testing.T) {
	cases := []struct {
		name    string
		content string
		bad     int
	}{
		{"literal", "    runs-on: ubuntu-24.04\n", 0},
		{"literal_with_comment", "    runs-on: macos-15 # pinned\n", 0},
		{"arm", "    runs-on: ubuntu-24.04-arm\n", 0},
		{"expression", "    runs-on: ${{ needs.route.outputs.label }}\n", 1},
		{"flow_list", "    runs-on: [self-hosted, linux]\n", 1},
		{"block_list", "    runs-on:\n      - ubuntu-24.04\n", 1},
		{"mapping", "    runs-on:\n      group: pool\n", 1},
		{"self_hosted", "    runs-on: self-hosted\n", 1},
		{"custom_label", "    runs-on: alpha-local\n", 1},
		{"quoted", "    runs-on: \"ubuntu-24.04\"\n", 1},
		{"two_jobs_one_bad", "a:\n  runs-on: ubuntu-24.04\nb:\n  runs-on: ${{ x }}\n", 1},
		{"no_runs_on", "name: x\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runsOnViolations("f.yml", c.content); len(got) != c.bad {
				t.Fatalf("want %d violations, got %d: %v", c.bad, len(got), got)
			}
		})
	}
}

// ownerPath matches any "<owner>/<repo>" reference to the repository owner.
// The one allowed match is this repository itself; every other match is a
// private repository name that must not reach the public tree. The secret
// driven deny-list job in CI is the half of the guard that knows the names.
var ownerPath = regexp.MustCompile(`(?i)nezhinskiy/[a-z0-9._-]+`)

const allowedOwnerPath = "nezhinskiy/local-ci-pool"

// scanOwnerPaths scans the contents of the files git tracks in dir (plus
// untracked, non-ignored files, so the guard works before the first add) and
// returns one finding per disallowed owner path.
func scanOwnerPaths(dir string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	list, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w: %s", dir, err, stderr.String())
	}
	var findings []string
	for _, name := range strings.Split(string(list), "\x00") {
		if name == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if os.IsNotExist(err) { // tracked but deleted in the working tree
				continue
			}
			return nil, err
		}
		for _, m := range ownerPath.FindAll(b, -1) {
			norm := strings.ToLower(strings.TrimRight(string(m), "."))
			if norm != allowedOwnerPath {
				findings = append(findings, fmt.Sprintf("%s: %s", name, m))
			}
		}
	}
	return findings, nil
}

func TestNoOwnerPathsInTree(t *testing.T) {
	findings, err := scanOwnerPaths(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error("owner path in tree: " + f)
	}
}

// gitInit makes a repository in a temp dir with the given files committed.
func gitInit(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "plant")
	return dir
}

// The planted path is assembled at run time so that no tracked file of this
// repository contains an owner path literal (TestNoOwnerPathsInTree would
// otherwise fail on this very file).
func plantedPath(repo string) string { return "Nezhinskiy" + "/" + repo }

func TestHygieneOracleDiscriminates(t *testing.T) {
	dir := gitInit(t, map[string]string{"notes.txt": "see " + plantedPath("alpha-private") + " for details\n"})
	findings, err := scanOwnerPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("want exactly 1 finding, got %d: %v", len(findings), findings)
	}
	if !strings.HasPrefix(findings[0], "notes.txt: ") {
		t.Fatalf("finding does not name the planted file: %q", findings[0])
	}
}

func TestOwnerPathScanBoundaries(t *testing.T) {
	dir := gitInit(t, map[string]string{
		"self.txt":    "uses: " + plantedPath("local-ci-pool") + "/route@abc\nupper " + strings.ToUpper(plantedPath("local-ci-pool")) + ".\n",
		"near.txt":    plantedPath("local-ci-pool-evil") + "\n",
		"mixed.txt":   strings.ToLower(plantedPath("Beta.Private_x")) + "\n",
		"nothing.txt": "no owner here\n",
	})
	findings, err := scanOwnerPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("want 2 findings (near.txt, mixed.txt), got %d: %v", len(findings), findings)
	}
	joined := strings.Join(findings, "\n")
	for _, want := range []string{"near.txt: ", "mixed.txt: "} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing finding for %s in %v", want, findings)
		}
	}
}

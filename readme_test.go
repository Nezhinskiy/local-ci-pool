package localcipool

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The README shows what a run says when the routing falls back. Those lines
// must be the ones route/route.sh prints, so the test runs the script for the
// three cases the README describes and looks for each annotation in the file,
// character for character. The hosted label is the one the README's own
// example passes, so a changed example forces the quoted lines to change too.
func TestReadmeQuotesRouteMessages(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*hosted-label:\s*(\S+)\s*$`).FindSubmatch(readme)
	if m == nil {
		t.Fatal("README.md has no `hosted-label:` line in its routing example")
	}
	hosted := string(m[1])

	const now = "1000000"
	cases := []struct{ name, vars string }{
		{"no heartbeat", `{}`},
		// The pool of 2026-10-21 was down for 42 hours: 151200 s before now.
		{"stale heartbeat", `{"CI_POOL_HB_EXAMPLEMACBO":"848800 4"}`},
		{"heartbeat from the future", `{"CI_POOL_HB_EXAMPLEMACBO":"1000100 4"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := routeAnnotations(t, hosted, now, c.vars)
			if len(lines) != 1 {
				t.Fatalf("route.sh printed %d annotations, want 1: %q", len(lines), lines)
			}
			if !strings.Contains(string(readme), lines[0]) {
				t.Errorf("README.md does not contain the line route.sh prints:\n%s", lines[0])
			}
		})
	}
}

// routeAnnotations runs route/route.sh for one case and returns the ::notice
// and ::warning lines of its standard output.
func routeAnnotations(t *testing.T, hostedLabel, now, varsJSON string) []string {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("route", "route.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// Read the script so that go test's result cache sees it: it is otherwise
	// only executed, and an edit to it would replay a stale pass.
	if _, err := os.ReadFile(script); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	out := filepath.Join(work, "github_output")
	if err := os.WriteFile(out, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = work
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + work,
		"ROUTE_TEST_NOW=" + now,
		"IDENTITY=alpha",
		"HOSTED_LABEL=" + hostedLabel,
		"HOSTED_SHARDS=4",
		"MODE=auto",
		"VARS_JSON=" + varsJSON,
		"GITHUB_OUTPUT=" + out,
	}
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("route.sh: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(string(stdout), "\n") {
		if strings.HasPrefix(l, "::notice ") || strings.HasPrefix(l, "::warning ") {
			lines = append(lines, l)
		}
	}
	return lines
}

package route

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type hbResult struct {
	exit   int
	stdout string
	stderr string
	argv   []string // one entry per gh invocation, arguments joined with "|"
}

// fakeGH is a stand-in for the GitHub CLI. It appends each invocation's argv
// to $FAKE_GH_LOG (arguments joined with "|") and answers `gh api repos/...`
// with $FAKE_GH_PRIVATE (default true). FAKE_GH_API_STATUS makes the api call
// fail with that status; FAKE_GH_SET_STATUS does the same for `variable set`.
const fakeGH = `#!/usr/bin/env bash
{ printf '%s|' "$@"; echo; } >> "$FAKE_GH_LOG"
case "$1" in
  api)
    if [[ -n "${FAKE_GH_API_STATUS-}" ]]; then
      echo "gh: HTTP 404" >&2
      exit "$FAKE_GH_API_STATUS"
    fi
    echo "${FAKE_GH_PRIVATE-true}"
    ;;
  variable)
    exit "${FAKE_GH_SET_STATUS-0}"
    ;;
esac
`

// runHeartbeat runs heartbeat.sh with the fake gh first on PATH. env entries
// override the defaults; "\x00unset" removes a variable.
func runHeartbeat(t *testing.T, env map[string]string, args ...string) hbResult {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("heartbeat.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Read the script so that go test's result cache sees it.
	if _, err := os.ReadFile(script); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	bin := filepath.Join(work, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(work, "gh.log")
	vars := map[string]string{
		"PATH":               bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME":               work,
		"HEARTBEAT_TEST_NOW": now,
		"FAKE_GH_LOG":        logFile,
	}
	for k, v := range env {
		if v == "\x00unset" {
			delete(vars, k)
		} else {
			vars[k] = v
		}
	}
	cmd := exec.Command(bash, append([]string{script}, args...)...)
	cmd.Dir = work
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		exit = ee.ExitCode()
	}
	var argv []string
	if b, err := os.ReadFile(logFile); err == nil {
		argv = strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return hbResult{exit, stdout.String(), stderr.String(), argv}
}

// wrote returns the `variable set` invocations the fake gh saw.
func (r hbResult) wrote() []string {
	var out []string
	for _, a := range r.argv {
		if strings.HasPrefix(a, "variable|set|") {
			out = append(out, a)
		}
	}
	return out
}

func TestHeartbeatScriptWritesTheValueRouteAccepts(t *testing.T) {
	r := runHeartbeat(t, nil, "--repo", "o/r", "--machine", "m1", "--slots", "3")
	if r.exit != 0 {
		t.Fatalf("exit %d\nstderr: %s", r.exit, r.stderr)
	}
	want := "variable|set|CI_POOL_HB_M1|--repo|o/r|--body|1000000 3|"
	if w := r.wrote(); len(w) != 1 || w[0] != want {
		t.Fatalf("gh variable set calls = %q, want exactly [%q]\nall gh calls: %q", w, want, r.argv)
	}
	// What was written is what route.sh accepts as a fresh heartbeat.
	fields := strings.Split(r.wrote()[0], "|")
	vars := fmt.Sprintf(`{%q:%q}`, fields[2], fields[6])
	got := parseOutputs(t, runRoute(t, map[string]string{"VARS_JSON": vars}))
	if got["local"] != "true" || got["shards"] != "3" {
		t.Errorf("route.sh did not take the written heartbeat as fresh: %v (vars %s)", got, vars)
	}
}

func TestHeartbeatScriptRefusesPublicRepo(t *testing.T) {
	r := runHeartbeat(t, map[string]string{"FAKE_GH_PRIVATE": "false"}, "--repo", "o/r", "--machine", "m1", "--slots", "3")
	if r.exit != 2 {
		t.Fatalf("want exit 2, got %d\nstderr: %s", r.exit, r.stderr)
	}
	if want := "heartbeat: o/r is public; self-hosted runners must not serve public repositories"; !strings.Contains(r.stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", r.stderr, want)
	}
	if w := r.wrote(); len(w) != 0 {
		t.Errorf("a variable was written for a public repository: %q", w)
	}
}

func TestHeartbeatScriptServesOrgPrivateRepo(t *testing.T) {
	r := runHeartbeat(t, map[string]string{"FAKE_GH_PRIVATE": "true"}, "--repo", "some-org/app", "--machine", "m1", "--slots", "2")
	if r.exit != 0 {
		t.Fatalf("exit %d\nstderr: %s", r.exit, r.stderr)
	}
	// Visibility is asked of the same repository that is then written.
	want := []string{
		"api|repos/some-org/app|--jq|.private|",
		"variable|set|CI_POOL_HB_M1|--repo|some-org/app|--body|1000000 2|",
	}
	if strings.Join(r.argv, "\n") != strings.Join(want, "\n") {
		t.Errorf("gh calls = %q, want exactly %q", r.argv, want)
	}
}

func TestHeartbeatScriptStopsWhenVisibilityUnknown(t *testing.T) {
	r := runHeartbeat(t, map[string]string{"FAKE_GH_API_STATUS": "1"}, "--repo", "o/r", "--machine", "m1", "--slots", "3")
	if r.exit != 1 {
		t.Errorf("want gh's exit status 1, got %d\nstderr: %s", r.exit, r.stderr)
	}
	if w := r.wrote(); len(w) != 0 {
		t.Errorf("a variable was written although visibility is unknown: %q", w)
	}
	// Any status gh fails with is passed on, and an answer that is not
	// literally "true" is not taken for private.
	r = runHeartbeat(t, map[string]string{"FAKE_GH_API_STATUS": "4"}, "--repo", "o/r", "--machine", "m1", "--slots", "3")
	if r.exit != 4 || len(r.wrote()) != 0 {
		t.Errorf("gh status 4: exit %d, writes %q", r.exit, r.wrote())
	}
	r = runHeartbeat(t, map[string]string{"FAKE_GH_PRIVATE": "null"}, "--repo", "o/r", "--machine", "m1", "--slots", "3")
	if r.exit != 2 || len(r.wrote()) != 0 {
		t.Errorf("answer null: exit %d, writes %q", r.exit, r.wrote())
	}
	if want := "heartbeat: could not tell whether o/r is private; not writing"; !strings.Contains(r.stderr, want) {
		t.Errorf("answer null: stderr = %q, want it to contain %q", r.stderr, want)
	}
	if strings.Contains(r.stderr, "is public") {
		t.Errorf("answer null reported as public: %q", r.stderr)
	}
}

func TestHeartbeatScriptSkipsWhenCheckFails(t *testing.T) {
	r := runHeartbeat(t, nil, "--repo", "o/r", "--machine", "m1", "--slots", "3", "--check", "false")
	if r.exit != 0 {
		t.Errorf("want exit 0, got %d\nstderr: %s", r.exit, r.stderr)
	}
	if w := r.wrote(); len(w) != 0 {
		t.Errorf("a variable was written although the check failed: %q", w)
	}
	if want := "heartbeat: check failed; not writing"; !strings.Contains(r.stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", r.stderr, want)
	}
	// A passing check writes.
	r = runHeartbeat(t, nil, "--repo", "o/r", "--machine", "m1", "--slots", "3", "--check", "true")
	if r.exit != 0 || len(r.wrote()) != 1 {
		t.Errorf("passing check: exit %d, writes %q", r.exit, r.wrote())
	}
}

func TestHeartbeatScriptRejectsBadInput(t *testing.T) {
	good := map[string]string{"repo": "o/r", "machine": "m1", "slots": "3"}
	with := func(k, v string) []string {
		m := map[string]string{}
		for gk, gv := range good {
			m[gk] = gv
		}
		m[k] = v
		return []string{"--repo", m["repo"], "--machine", m["machine"], "--slots", m["slots"]}
	}
	cases := map[string][]string{
		"machine_hyphen":    with("machine", "mac-mini"),
		"machine_empty":     with("machine", ""),
		"machine_13_chars":  with("machine", strings.Repeat("a", 13)),
		"machine_newline":   with("machine", "m1\nx"),
		"slots_0":           with("slots", "0"),
		"slots_9":           with("slots", "9"),
		"slots_text":        with("slots", "x"),
		"slots_empty":       with("slots", ""),
		"slots_newline":     with("slots", "3\n"),
		"repo_no_slash":     with("repo", "o"),
		"repo_two_slashes":  with("repo", "o/r/x"),
		"repo_empty":        with("repo", ""),
		"repo_dotdot_owner": with("repo", "../r"),
		"repo_dotdot_name":  with("repo", "o/.."),
		"repo_dot_owner":    with("repo", "./r"),
		"repo_space":        with("repo", "o/r x"),
		"repo_newline":      with("repo", "o/r\n"),
		"no_args":           nil,
		"unknown_flag":      append(with("repo", "o/r"), "--force"),
		"flag_without_arg":  {"--repo", "o/r", "--machine", "m1", "--slots"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			r := runHeartbeat(t, nil, args...)
			if r.exit != 2 {
				t.Errorf("want exit 2, got %d\nstderr: %s", r.exit, r.stderr)
			}
			if !strings.HasPrefix(r.stderr, "heartbeat: ") {
				t.Errorf("stderr = %q, want a heartbeat: message", r.stderr)
			}
			if len(r.argv) != 0 {
				t.Errorf("gh was called: %q", r.argv)
			}
		})
	}
}

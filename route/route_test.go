package route

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	now           = "1000000"
	identity      = "alpha"
	hostedLabel   = "ubuntu-24.04"
	hostedShards  = "4"
	localLabel    = "alpha-local"
	outputLinePat = `^(label|shards|local)=[a-z0-9.-]+$`
)

var outputLine = regexp.MustCompile(outputLinePat)

type result struct {
	exit    int
	stdout  string
	stderr  string
	output  string // contents of the GITHUB_OUTPUT file
	workdir string
}

// runRoute runs route.sh in a fresh working directory with a controlled
// environment. env entries override the defaults; an entry with the value
// "\x00unset" removes the variable.
func runRoute(t *testing.T, env map[string]string) result {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("route.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Read the script so that go test's result cache sees it: it is otherwise
	// only executed, and an edit to it would replay a stale pass.
	if _, err := os.ReadFile(script); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	outFile := filepath.Join(work, "github_output")
	if err := os.WriteFile(outFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The script runs in a subdirectory so that stray files it creates are
	// distinguishable from the output file.
	cwd := filepath.Join(work, "cwd")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"PATH":          os.Getenv("PATH"),
		"HOME":          work,
		"NOW":           now,
		"IDENTITY":      identity,
		"HOSTED_LABEL":  hostedLabel,
		"HOSTED_SHARDS": hostedShards,
		"MODE":          "auto",
		"VARS_JSON":     "{}",
		"GITHUB_OUTPUT": outFile,
	}
	for k, v := range env {
		if v == "\x00unset" {
			delete(vars, k)
		} else {
			vars[k] = v
		}
	}
	cmd := exec.Command(bash, script)
	cmd.Dir = cwd
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
	out, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	return result{exit, stdout.String(), stderr.String(), string(out), cwd}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// parseOutputs checks the GITHUB_OUTPUT contract (exactly three lines, in
// order, each matching the safe pattern) and returns them as a map.
func parseOutputs(t *testing.T, r result) map[string]string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(r.output, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want exactly 3 output lines, got %d: %q", len(lines), r.output)
	}
	got := map[string]string{}
	for i, key := range []string{"label", "shards", "local"} {
		if !outputLine.MatchString(lines[i]) {
			t.Fatalf("output line %q does not match %s", lines[i], outputLinePat)
		}
		if !strings.HasPrefix(lines[i], key+"=") {
			t.Fatalf("output line %d: want key %s, got %q", i, key, lines[i])
		}
		got[key] = strings.TrimPrefix(lines[i], key+"=")
	}
	return got
}

func TestRouteSelection(t *testing.T) {
	hosted := [3]string{hostedLabel, hostedShards, "false"}
	local := func(shards string) [3]string { return [3]string{localLabel, shards, "true"} }
	cases := []struct {
		name    string
		fixture string
		mode    string // empty means auto
		noNow   bool
		want    [3]string
	}{
		{"real_fixture", "real_fresh", "", false, local("2")},
		{"real_fixture_verbatim_is_stale", "real", "", false, hosted},
		{"real_fixture_default_now", "real", "", true, hosted},
		{"no_heartbeat", "empty", "", false, hosted},
		{"stale", "stale", "", false, hosted},
		{"boundary_300", "boundary_300", "", false, local("2")},
		{"skew_ok", "skew_ok", "", false, local("2")},
		{"skew_too_far", "skew_too_far", "", false, hosted},
		{"two_machines", "two_machines", "", false, local("4")},
		{"tie_fresher", "tie_fresher", "", false, local("4")},
		{"one_fresh_one_stale", "one_fresh_one_stale", "", false, local("3")},
		{"shards_capped_at_4", "many_slots", "", false, local("4")},
		{"forced_hosted", "real_fresh", "hosted", false, hosted},
		{"malformed_value", "malformed_value", "", false, hosted},
		{"malformed_slots_0", "malformed_slots_0", "", false, hosted},
		{"malformed_slots_9", "malformed_slots_9", "", false, hosted},
		{"malformed_name", "malformed_name", "", false, hosted},
		{"malformed_name_nospace", "malformed_name_nospace", "", false, hosted},
		{"newline_in_name", "newline_in_name", "", false, hosted},
		{"lower_name", "lower_name", "", false, hosted},
		{"prefixed_name", "prefixed_name", "", false, hosted},
		{"long_name_13", "long_name_13", "", false, hosted},
		{"long_epoch_13", "long_epoch_13", "", false, hosted},
		{"leading_zero_epoch", "leading_zero_epoch", "", false, local("2")},
		{"trailing_space_value", "trailing_space_value", "", false, hosted},
		{"trailing_newline_value", "trailing_newline_value", "", false, hosted},
		{"trailing_newline_name", "trailing_newline_name", "", false, hosted},
		{"newline_escaped", "newline_escaped", "", false, hosted},
		{"newline_raw", "newline_raw", "", false, hosted},
		{"other_vars_ignored", "other_vars_ignored", "", false, local("2")},
		{"non_heartbeat_only", "non_heartbeat_only", "", false, hosted},
		{"not_string", "not_string", "", false, hosted},
		{"not_string_alongside_valid", "not_string_alongside_valid", "", false, local("2")},
		{"array_json", "array", "", false, hosted},
		{"blank_json", "blank", "", false, hosted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{"VARS_JSON": fixture(t, c.fixture)}
			if c.mode != "" {
				env["MODE"] = c.mode
			}
			if c.noNow {
				env["NOW"] = "\x00unset"
			}
			r := runRoute(t, env)
			if r.exit != 0 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.exit, r.stdout, r.stderr)
			}
			got := parseOutputs(t, r)
			if got["label"] != c.want[0] || got["shards"] != c.want[1] || got["local"] != c.want[2] {
				t.Errorf("got label=%s shards=%s local=%s, want label=%s shards=%s local=%s",
					got["label"], got["shards"], got["local"], c.want[0], c.want[1], c.want[2])
			}
			lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
			if len(lines) != 1 || !strings.HasPrefix(lines[0], "selected: ") {
				t.Errorf("want exactly one stdout line starting with %q, got %q", "selected: ", r.stdout)
			}
			if _, err := os.Stat(filepath.Join(r.workdir, "x")); err == nil {
				t.Error("file x exists: a key was executed as shell")
			}
			entries, err := os.ReadDir(r.workdir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("script left files in its working directory: %v", entries)
			}
		})
	}
}

func TestRouteInvalidJSONIsLoggedOnce(t *testing.T) {
	r := runRoute(t, map[string]string{"VARS_JSON": fixture(t, "newline_raw")})
	if r.exit != 0 {
		t.Fatalf("exit %d: %s", r.exit, r.stderr)
	}
	if n := len(strings.Split(strings.TrimSpace(r.stderr), "\n")); n != 1 || strings.TrimSpace(r.stderr) == "" {
		t.Errorf("want exactly one log line on stderr, got %q", r.stderr)
	}
}

func TestRouteRejectsBadInputs(t *testing.T) {
	cases := map[string]map[string]string{
		"identity_newline":       {"IDENTITY": "alpha\nlabel=evil"},
		"identity_uppercase":     {"IDENTITY": "Alpha"},
		"identity_empty":         {"IDENTITY": ""},
		"hosted_label_newline":   {"HOSTED_LABEL": "ubuntu-24.04\nlocal=true"},
		"hosted_label_empty":     {"HOSTED_LABEL": ""},
		"hosted_shards_text":     {"HOSTED_SHARDS": "four"},
		"hosted_shards_newline":  {"HOSTED_SHARDS": "4\nlocal=true"},
		"hosted_shards_zero":     {"HOSTED_SHARDS": "0"},
		"mode_unknown":           {"MODE": "per-machine"},
		"mode_newline":           {"MODE": "auto\n"},
		"now_not_a_number":       {"NOW": "abc"},
		"github_output_empty":    {"GITHUB_OUTPUT": ""},
		"github_output_unset":    {"GITHUB_OUTPUT": "\x00unset"},
		"identity_unset_in_step": {"IDENTITY": "\x00unset"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			env["VARS_JSON"] = fixture(t, "real_fresh")
			r := runRoute(t, env)
			if r.exit == 0 {
				t.Fatalf("want a non-zero exit\nstdout: %s", r.stdout)
			}
			if r.output != "" {
				t.Errorf("outputs were written despite the failure: %q", r.output)
			}
			if strings.TrimSpace(r.stderr) == "" {
				t.Error("want an explanation on stderr")
			}
		})
	}
}

func TestRouteNeedsJq(t *testing.T) {
	empty := t.TempDir()
	r := runRoute(t, map[string]string{"PATH": empty, "VARS_JSON": fixture(t, "real_fresh")})
	if r.exit != 1 {
		t.Fatalf("want exit 1, got %d\nstderr: %s", r.exit, r.stderr)
	}
	if !strings.Contains(r.stderr, "jq") {
		t.Errorf("message does not name jq: %q", r.stderr)
	}
	if r.output != "" {
		t.Errorf("outputs written without jq: %q", r.output)
	}
}

// The action file is data to the Go toolchain, so these checks read it as
// text: inputs reach the script only through env:, never through the run: body.
func TestActionWiring(t *testing.T) {
	b, err := os.ReadFile("action.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)

	// Every variable the script reads must be fed from an input (or, for the
	// script's own location, from github.action_path).
	wiring := map[string]string{
		"VARS_JSON":     "inputs.vars-json",
		"IDENTITY":      "inputs.identity",
		"HOSTED_LABEL":  "inputs.hosted-label",
		"HOSTED_SHARDS": "inputs.hosted-shards",
		"MODE":          "inputs.mode",
		"ACTION_PATH":   "github.action_path",
	}
	for env, expr := range wiring {
		want := regexp.MustCompile(`(?m)^\s+` + env + `: \$\{\{ ` + regexp.QuoteMeta(expr) + ` \}\}$`)
		if !want.MatchString(text) {
			t.Errorf("env %s is not set from ${{ %s }}", env, expr)
		}
	}

	runLine := regexp.MustCompile(`(?m)^\s+run:\s*(.*)$`)
	runs := runLine.FindAllStringSubmatch(text, -1)
	if len(runs) != 1 {
		t.Fatalf("want exactly one run: step, found %d", len(runs))
	}
	if strings.Contains(runs[0][1], "${{") {
		t.Errorf("run: body splices an expression: %q", runs[0][1])
	}
	if !strings.Contains(runs[0][1], `"$ACTION_PATH/route.sh"`) {
		t.Errorf("run: does not call route.sh through ACTION_PATH: %q", runs[0][1])
	}

	for _, name := range []string{"label", "shards", "local"} {
		want := "value: ${{ steps.route.outputs." + name + " }}"
		if !strings.Contains(text, want) {
			t.Errorf("output %s is not wired to the step output (%s)", name, want)
		}
	}
	if !regexp.MustCompile(`(?s)mode:.*?default: auto`).MatchString(text) {
		t.Error("input mode does not default to auto")
	}
	if strings.Contains(text, "fromJSON") {
		t.Error("the action must not require fromJSON")
	}
}

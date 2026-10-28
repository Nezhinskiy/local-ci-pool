package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Nezhinskiy/local-ci-pool/internal/budget"
	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

// statusFixture is a pool with one healthy project and one that is not, whose
// job has been queued for twelve minutes. The version is made up on purpose:
// a real release number would collide with this project's own tags.
func statusFixture() supervisor.Snapshot {
	return supervisor.Snapshot{
		Version: "v9.9.9",
		Commit:  "abc1234",
		Machine: "examplemacbo",
		Slots:   7,
		InUse:   2,
		Docker:  true,
		Login:   true,
		Budget:  budget.State{State: "off"},
		Projects: []supervisor.ProjectHealth{
			{Repo: "example/DNA", Healthy: true},
			{
				Repo:   "example/rubberduck",
				Reason: "image build failed",
				Stuck: []supervisor.StuckJob{{
					RunURL:  "https://github.com/example/rubberduck/actions/runs/1",
					Job:     "build",
					QueuedS: 720,
				}},
			},
		},
	}
}

// statusServices answers every fetch with the given status and snapshot and
// records the address it was asked for.
func statusServices(status int, snap supervisor.Snapshot, asked *string) (services, []byte) {
	body, err := json.Marshal(snap)
	if err != nil {
		panic(err)
	}
	body = append(body, '\n')
	return services{fetchHealth: func(_ context.Context, addr string) (int, []byte, error) {
		if asked != nil {
			*asked = addr
		}
		return status, body, nil
	}}, body
}

func TestStatusPrintsProjects(t *testing.T) {
	var asked string
	svc, _ := statusServices(http.StatusOK, statusFixture(), &asked)
	code, stdout, stderr := do(t, svc, "status")
	want := `pool v9.9.9 (abc1234) on examplemacbo: docker ok, login ok, 7 slots, 2 in use
  example/DNA         healthy
  example/rubberduck  unhealthy: image build failed
    queued 12m0s: build https://github.com/example/rubberduck/actions/runs/1
`
	// The fixture's second project is unhealthy, but the pool as a whole is
	// up: 200 exits 0.
	if code != exitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if stdout != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", stdout, want)
	}
	if asked != supervisor.DefaultHealthAddr {
		t.Errorf("asked %q, want the default %q", asked, supervisor.DefaultHealthAddr)
	}
}

func TestStatusShowsWaitingForCores(t *testing.T) {
	snap := statusFixture()
	snap.Projects = []supervisor.ProjectHealth{{Repo: "example/DNA", Healthy: true, Waiting: 3}}
	svc, _ := statusServices(http.StatusOK, snap, nil)
	_, stdout, _ := do(t, svc, "status")
	if want := "  example/DNA  healthy, 3 waiting for cores\n"; !strings.Contains(stdout, want) {
		t.Fatalf("stdout lacks %q:\n%s", want, stdout)
	}
}

func TestStatusShowsBudgetWhenOn(t *testing.T) {
	for _, tc := range []struct {
		state, suffix string
	}{
		{"wedged", ", budget wedged"},
		{"on", ", budget on"},
		{"off", ""},
	} {
		snap := statusFixture()
		snap.Budget = budget.State{State: tc.state}
		svc, _ := statusServices(http.StatusOK, snap, nil)
		_, stdout, _ := do(t, svc, "status")
		first, _, _ := strings.Cut(stdout, "\n")
		if !strings.HasSuffix(first, "2 in use"+tc.suffix) {
			t.Errorf("budget %q: first line %q should end with %q", tc.state, first, "2 in use"+tc.suffix)
		}
		if tc.state == "off" && strings.Contains(stdout, "budget") {
			t.Errorf("budget off should print no budget text:\n%s", stdout)
		}
	}
}

func TestStatusUnhealthyExitsOne(t *testing.T) {
	snap := statusFixture()
	snap.Docker = false
	svc, _ := statusServices(http.StatusServiceUnavailable, snap, nil)
	code, stdout, stderr := do(t, svc, "status")
	if code != exitError {
		t.Fatalf("exit %d, want %d; stderr %q", code, exitError, stderr)
	}
	first, _, _ := strings.Cut(stdout, "\n")
	if want := "pool v9.9.9 (abc1234) on examplemacbo: docker down, login ok, 7 slots, 2 in use"; first != want {
		t.Fatalf("first line %q, want %q", first, want)
	}
	snap = statusFixture()
	snap.Login = false
	svc, _ = statusServices(http.StatusServiceUnavailable, snap, nil)
	if _, stdout, _ = do(t, svc, "status"); !strings.Contains(stdout, "docker ok, login lost") {
		t.Errorf("a lost login should read \"login lost\":\n%s", stdout)
	}
}

func TestStatusNotRunning(t *testing.T) {
	svc := services{fetchHealth: func(context.Context, string) (int, []byte, error) {
		return 0, nil, errors.New("dial tcp 127.0.0.1:9999: connect: connection refused")
	}}
	code, stdout, stderr := do(t, svc, "status", "--health-addr", "127.0.0.1:9999")
	if code != exitNotRunning {
		t.Fatalf("exit %d, want %d", code, exitNotRunning)
	}
	if stdout != "" {
		t.Errorf("stdout %q, want nothing", stdout)
	}
	const want = "pool: not running (nothing answers on 127.0.0.1:9999); see launchctl print gui/$(id -u)/com.local-ci-pool.pool\n"
	if stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
}

func TestStatusJSON(t *testing.T) {
	svc, body := statusServices(http.StatusOK, statusFixture(), nil)
	code, stdout, stderr := do(t, svc, "status", "--json")
	if code != exitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if stdout != string(body) {
		t.Fatalf("stdout %q, want the body %q verbatim", stdout, body)
	}
}

func TestStatusRejectsArgs(t *testing.T) {
	svc := services{fetchHealth: func(context.Context, string) (int, []byte, error) {
		t.Error("fetched despite a usage error")
		return 0, nil, errors.New("unused")
	}}
	for _, args := range [][]string{
		{"status", "extra"},
		{"status", "--health-addr", "example.com:80"},
		{"status", "--nope"},
	} {
		if code, _, stderr := do(t, svc, args...); code != exitUsage || !strings.Contains(stderr, "usage:") {
			t.Errorf("%v: exit %d, stderr %q, want a usage error", args, code, stderr)
		}
	}
}

func TestStatusUnreadableBodyIsOneLine(t *testing.T) {
	svc := services{fetchHealth: func(context.Context, string) (int, []byte, error) {
		return http.StatusOK, []byte("<html>"), nil
	}}
	code, stdout, stderr := do(t, svc, "status")
	if code != exitError || stdout != "" || strings.Count(stderr, "\n") != 1 || !strings.HasPrefix(stderr, "pool: ") {
		t.Fatalf("exit %d, stdout %q, stderr %q, want one \"pool: …\" line", code, stdout, stderr)
	}
}

// A job's name is chosen by whoever can write to the repository: ESC and
// newlines in it must not reach the terminal or forge a row.
func TestStatusPrintsNoControlCharacters(t *testing.T) {
	snap := statusFixture()
	snap.Machine = "mac\x1b[2J"
	snap.Projects[1].Reason = "bad\r\x9b0m"
	snap.Projects[1].Stuck[0].Job = "\x1b]0;x\x07\n  fake  healthy"
	svc, _ := statusServices(http.StatusOK, snap, nil)
	_, stdout, _ := do(t, svc, "status")
	for i := 0; i < len(stdout); i++ {
		if c := stdout[i]; (c < 0x20 && c != '\n') || c == 0x7f {
			t.Fatalf("control byte %#x in the output:\n%q", c, stdout)
		}
	}
	if strings.Contains(stdout, "\x9b") || strings.Contains(stdout, "\u009b") {
		t.Errorf("a C1 control reached the output: %q", stdout)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "fake  healthy" {
			t.Errorf("a job name forged the line %q", line)
		}
	}
	if n := strings.Count(stdout, "\n"); n != 4 {
		t.Errorf("%d lines, want 4 (the layout's own):\n%q", n, stdout)
	}
	if want := `\u001b]0;x\u0007\u000a  fake  healthy`; !strings.Contains(stdout, want) {
		t.Errorf("the escaped job name %q is missing:\n%q", want, stdout)
	}
}

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nezhinskiy/local-ci-pool/internal/github"
	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

// fakePool stands in for the supervisor. Run blocks until the context ends
// (a clean drain), a terminal cause arrives through FailOnLoginLoss, or runErr
// is set, in which case it returns that at once.
type fakePool struct {
	mu     sync.Mutex
	snap   supervisor.Snapshot
	runErr error
	failed chan error
	cfg    supervisor.Config
}

func newFakePool() *fakePool { return &fakePool{failed: make(chan error, 1)} }

func (p *fakePool) Run(ctx context.Context) error {
	p.mu.Lock()
	err := p.runErr
	p.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-p.failed:
		return err
	}
}

func (p *fakePool) Snapshot() supervisor.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snap
}

func (p *fakePool) FailOnLoginLoss(err error) bool {
	if !errors.Is(err, github.ErrUnauthorized) {
		return false
	}
	select {
	case p.failed <- fmt.Errorf("gh login rejected: %w", supervisor.ErrTerminal):
	default:
	}
	return true
}

type writeRec struct{ repo, name, value string }

type recVars struct {
	mu     sync.Mutex
	writes []writeRec
	err    error
}

func (v *recVars) SetVariable(_ context.Context, repo, name, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.writes = append(v.writes, writeRec{repo, name, value})
	return v.err
}

func poolServices(p *fakePool, v *recVars) services {
	return services{
		newPool: func(cfg supervisor.Config, _ *slog.Logger) (pool, varWriter, error) {
			p.cfg = cfg
			return p, v, nil
		},
	}
}

func do(t *testing.T, s services, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code = run(ctx, args, &out, &errb, s)
	return code, out.String(), errb.String()
}

func TestTerminalExitsZero(t *testing.T) {
	p := newFakePool()
	p.runErr = fmt.Errorf("%w", supervisor.ErrTerminal) // what supervisor.terminal(...) matches
	code, _, stderr := do(t, poolServices(p, &recVars{}), "run")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 so that launchd does not restart into the same failure", code)
	}
	if !strings.Contains(stderr, "terminal: ") {
		t.Fatalf("stderr = %q, want a terminal: line", stderr)
	}
}

func TestTerminalMessageIsLogged(t *testing.T) {
	p := newFakePool()
	// The real terminal error carries its message alone and matches the sentinel.
	p.runErr = terminalErr("gh logged out: x; run gh auth login")
	_, _, stderr := do(t, poolServices(p, &recVars{}), "run")
	if !strings.Contains(stderr, "terminal: gh logged out: x; run gh auth login") {
		t.Fatalf("stderr = %q, want the reason after terminal:", stderr)
	}
}

type terminalErr string

func (e terminalErr) Error() string        { return string(e) }
func (e terminalErr) Is(target error) bool { return target == supervisor.ErrTerminal }

func TestOtherErrorExitsOne(t *testing.T) {
	p := newFakePool()
	p.runErr = errors.New("docker is gone")
	code, _, stderr := do(t, poolServices(p, &recVars{}), "run")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 so that launchd restarts the pool", code)
	}
	if strings.Contains(stderr, "terminal:") || !strings.Contains(stderr, "docker is gone") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestCleanDrainExitsZero(t *testing.T) {
	p := newFakePool()
	ctx, cancel := context.WithCancel(context.Background())
	var out, errb bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"run"}, &out, &errb, poolServices(p, &recVars{})) }()
	time.Sleep(20 * time.Millisecond)
	cancel() // SIGTERM
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d after a clean drain, want 0; stderr %q", code, errb.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after its context ended")
	}
}

// The heartbeat shares the process's fate: a login lost while writing a
// variable ends the run like the supervisor's own terminal causes (exit 0).
func TestHeartbeatLoginLossExitsZeroAsTerminal(t *testing.T) {
	p := newFakePool()
	p.snap = supervisor.Snapshot{
		Machine: "examplemac", Slots: 2, Docker: true,
		Projects: []supervisor.ProjectHealth{{Repo: "example/alpha", Healthy: true}},
	}
	v := &recVars{err: fmt.Errorf("setting: %w", github.ErrUnauthorized)}
	code, _, stderr := do(t, poolServices(p, v), "run")
	if code != 0 || !strings.Contains(stderr, "terminal: ") {
		t.Fatalf("exit = %d, stderr %q; want 0 and a terminal: line", code, stderr)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.writes) != 1 || v.writes[0].name != "CI_POOL_HB_EXAMPLEMAC" || v.writes[0].value[len(v.writes[0].value)-2:] != " 2" {
		t.Fatalf("writes = %+v", v.writes)
	}
}

func TestVersionAndCommitFlowIntoTheConfig(t *testing.T) {
	oldV, oldC := version, commit
	version, commit = "v9.9.9", "abc1234"
	t.Cleanup(func() { version, commit = oldV, oldC })
	p := newFakePool()
	p.runErr = terminalErr("stop")
	do(t, poolServices(p, &recVars{}), "run", "--probe", "--only-repo", "example/alpha", "--marker-ref", "feature/x")
	c := p.cfg
	if c.Version != "v9.9.9" || c.Commit != "abc1234" {
		t.Errorf("version/commit = %q %q", c.Version, c.Commit)
	}
	if !c.Probe || c.OnlyRepo != "example/alpha" || c.MarkerRef != "feature/x" {
		t.Errorf("probe flags = %+v", c)
	}
	if c.HealthAddr != supervisor.DefaultHealthAddr {
		t.Errorf("health address = %q, want the default", c.HealthAddr)
	}
}

func TestPrintDefaults(t *testing.T) {
	code, stdout, _ := do(t, services{}, "print-defaults")
	want := `{"drain_seconds":2400,"health_addr":"127.0.0.1:8737"}` + "\n"
	if code != 0 || stdout != want {
		t.Fatalf("exit %d, stdout %q, want %q", code, stdout, want)
	}
}

func TestVersionPrintsVersionAndCommit(t *testing.T) {
	oldV, oldC := version, commit
	t.Cleanup(func() { version, commit = oldV, oldC })

	version, commit = "v1.2.3", "abc1234"
	if code, stdout, _ := do(t, services{}, "version"); code != 0 || stdout != "v1.2.3 abc1234\n" {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
	// A build without -ldflags still names itself.
	version, commit = "", ""
	code, stdout, _ := do(t, services{}, "version")
	f := strings.Fields(stdout)
	if code != 0 || len(f) != 2 || f[0] != "dev" || f[1] == "" {
		t.Fatalf("exit %d, stdout %q, want \"dev <commit>\"", code, stdout)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	for name, args := range map[string][]string{
		"no command":              nil,
		"unknown command":         {"frobnicate"},
		"unknown flag":            {"run", "--nope"},
		"probe without a repo":    {"run", "--probe"},
		"repo without probe":      {"run", "--only-repo", "example/alpha"},
		"marker ref without both": {"run", "--marker-ref", "x"},
		"stray argument":          {"run", "extra"},
		"version takes none":      {"version", "x"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newFakePool()
			if code, _, stderr := do(t, poolServices(p, &recVars{}), args...); code != 2 || stderr == "" {
				t.Fatalf("exit = %d, stderr %q; want 2 with a message", code, stderr)
			}
		})
	}
}

// The real constructor wires Docker, GitHub and the health endpoint without
// touching any of them until needed: a second instance fails at the lock,
// before Docker or gh is asked anything, and that is terminal.
func TestRealWiringSecondInstanceExitsZero(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	code, _, stderr := do(t, realServices(), "run", "--health-addr", ln.Addr().String())
	if code != 0 || !strings.Contains(stderr, "terminal: already running") {
		t.Fatalf("exit %d, stderr %q; want 0 and terminal: already running", code, stderr)
	}
}

func TestHealthAddrMustBeLoopback(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:8737", ":8737", "192.168.1.20:8737", "example.com:8737", "[::]:8737",
		"127.0.0.1", "127.0.0.1:0", "127.0.0.1:99999", "127.0.0.1:http", "localhost.evil.example:80",
	} {
		t.Run("refuses "+addr, func(t *testing.T) {
			p := newFakePool()
			p.runErr = terminalErr("stop")
			code, _, stderr := do(t, poolServices(p, &recVars{}), "run", "--health-addr", addr)
			if code != 2 || !strings.Contains(stderr, "--health-addr") {
				t.Fatalf("exit %d, stderr %q; want a usage error naming --health-addr", code, stderr)
			}
			if p.cfg.HealthAddr != "" {
				t.Fatalf("the pool was built with %q", p.cfg.HealthAddr)
			}
		})
	}
	for _, addr := range []string{"127.0.0.1:8738", "localhost:8738", "[::1]:8738", "127.0.0.2:1"} {
		t.Run("accepts "+addr, func(t *testing.T) {
			p := newFakePool()
			p.runErr = terminalErr("stop")
			if code, _, stderr := do(t, poolServices(p, &recVars{}), "run", "--health-addr", addr); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			if p.cfg.HealthAddr != addr {
				t.Fatalf("health address = %q, want %q", p.cfg.HealthAddr, addr)
			}
		})
	}
}

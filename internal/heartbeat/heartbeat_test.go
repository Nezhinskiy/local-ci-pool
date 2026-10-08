package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nezhinskiy/local-ci-pool/internal/ghauth"
	"github.com/Nezhinskiy/local-ci-pool/internal/github"
	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

// The routing action accepts exactly the shapes in route/route.sh; the tests
// take the expressions from that file, so a change there reaches this package.
var routeKey, routeValue = routeRegexps()

// routeRegexps extracts the two jq test() expressions of route.sh (written as
// \\A...\\z inside a jq string) and returns them as anchored Go expressions.
func routeRegexps() (key, value *regexp.Regexp) {
	b, err := os.ReadFile(filepath.Join("..", "..", "route", "route.sh"))
	if err != nil {
		panic(err)
	}
	re := regexp.MustCompile(`test\("\\\\A(.+?)\\\\z"\)`)
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		x := regexp.MustCompile("^" + m[1] + "$")
		switch {
		case strings.HasPrefix(m[1], "CI_POOL_HB_"):
			key = x
		case strings.HasPrefix(m[1], "[0-9]"):
			value = x
		}
	}
	if key == nil || value == nil {
		panic("route/route.sh: the heartbeat key and value expressions were not found")
	}
	return key, value
}

type write struct{ repo, name, value string }

type fakeVars struct {
	mu     sync.Mutex
	writes []write
	err    error
}

func (f *fakeVars) SetVariable(_ context.Context, repo, name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, write{repo, name, value})
	return f.err
}

func (f *fakeVars) all() []write {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]write(nil), f.writes...)
}

// fakeSnap serves a fixed Snapshot and counts how often it was asked, so a
// test that expects silence can wait for several ticks to have passed.
type fakeSnap struct {
	mu    sync.Mutex
	snap  supervisor.Snapshot
	calls int
}

func (f *fakeSnap) get() supervisor.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.snap
}

func (f *fakeSnap) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func snapshot(slots int, projects ...supervisor.ProjectHealth) supervisor.Snapshot {
	return supervisor.Snapshot{Machine: "examplemac", Slots: slots, Docker: true, Projects: projects}
}

func project(repo string, healthy bool) supervisor.ProjectHealth {
	return supervisor.ProjectHealth{Repo: repo, Healthy: healthy}
}

var fixedNow = func() time.Time { return time.Unix(1_800_000_000, 0) }

func start(t *testing.T, cfg Config) (stop func()) {
	t.Helper()
	if cfg.Every == 0 {
		cfg.Every = 5 * time.Millisecond
	}
	if cfg.Now == nil {
		cfg.Now = fixedNow
	}
	cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, cfg)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("Run did not return after its context ended")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHeartbeatWritesOnlyHealthy(t *testing.T) {
	snap := &fakeSnap{snap: snapshot(2, project("example/alpha", true), project("example/beta", false))}
	vars := &fakeVars{}
	stop := start(t, Config{Snapshot: snap.get, Vars: vars})
	waitFor(t, "a heartbeat", func() bool { return len(vars.all()) > 0 })
	stop()

	for _, w := range vars.all() {
		if w.repo != "example/alpha" {
			t.Errorf("wrote %q for %s, which is not healthy", w.name, w.repo)
		}
		if w.name != "CI_POOL_HB_EXAMPLEMAC" {
			t.Errorf("name = %q, want CI_POOL_HB_EXAMPLEMAC", w.name)
		}
		if !regexp.MustCompile(`^\d+ 2$`).MatchString(w.value) {
			t.Errorf("value = %q, want <epoch> 2", w.value)
		}
		if w.value != "1800000000 2" {
			t.Errorf("value = %q, want the clock's epoch and the slots", w.value)
		}
	}
	if got := len(vars.all()); got < 1 {
		t.Fatalf("%d writes", got)
	}
}

func TestHeartbeatWritesOncePerHealthyProjectPerTick(t *testing.T) {
	snap := &fakeSnap{snap: snapshot(4, project("example/alpha", true), project("example/beta", true))}
	vars := &fakeVars{}
	stop := start(t, Config{Every: time.Hour, Snapshot: snap.get, Vars: vars})
	waitFor(t, "the first beat", func() bool { return len(vars.all()) >= 2 })
	stop()
	got := vars.all()
	if len(got) != 2 || got[0].repo != "example/alpha" || got[1].repo != "example/beta" {
		t.Fatalf("writes = %+v, want one per project at the first beat", got)
	}
}

func TestHeartbeatSilentWhenDockerDownOrProbe(t *testing.T) {
	for name, mutate := range map[string]func(*supervisor.Snapshot){
		"docker down": func(s *supervisor.Snapshot) { s.Docker = false },
		"probe mode":  func(s *supervisor.Snapshot) { s.Probe = true },
		"no machine":  func(s *supervisor.Snapshot) { s.Machine = "" },
		"no slots":    func(s *supervisor.Snapshot) { s.Slots = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			s := snapshot(2, project("example/alpha", true))
			mutate(&s)
			snap := &fakeSnap{snap: s}
			vars := &fakeVars{}
			stop := start(t, Config{Snapshot: snap.get, Vars: vars})
			waitFor(t, "several ticks", func() bool { return snap.count() >= 4 })
			stop()
			if w := vars.all(); len(w) != 0 {
				t.Fatalf("wrote %+v, want silence", w)
			}
		})
	}
}

func TestHeartbeatStopsWithContext(t *testing.T) {
	snap := &fakeSnap{snap: snapshot(2, project("example/alpha", true))}
	vars := &fakeVars{}
	stop := start(t, Config{Snapshot: snap.get, Vars: vars})
	waitFor(t, "a heartbeat", func() bool { return len(vars.all()) > 0 })
	stop() // fails the test if Run does not return
	before := len(vars.all())
	time.Sleep(30 * time.Millisecond)
	if after := len(vars.all()); after != before {
		t.Fatalf("%d writes after Run returned", after-before)
	}
}

// Every value the heartbeat writes must be one the routing action accepts, for
// any Snapshot the pool can produce: the slot count is clamped to 1..8.
func TestHeartbeatWritesOnlyWhatRoutingAccepts(t *testing.T) {
	for _, tc := range []struct {
		slots int
		want  string // "" means no write
	}{
		{-1, ""}, {0, ""}, {1, "1"}, {4, "4"}, {8, "8"}, {9, "8"}, {1000, "8"},
	} {
		t.Run(fmt.Sprint(tc.slots), func(t *testing.T) {
			snap := &fakeSnap{snap: snapshot(tc.slots, project("example/alpha", true))}
			vars := &fakeVars{}
			stop := start(t, Config{Snapshot: snap.get, Vars: vars})
			if tc.want == "" {
				waitFor(t, "several ticks", func() bool { return snap.count() >= 4 })
			} else {
				waitFor(t, "a heartbeat", func() bool { return len(vars.all()) > 0 })
			}
			stop()
			ws := vars.all()
			if tc.want == "" {
				if len(ws) != 0 {
					t.Fatalf("wrote %+v for %d slots", ws, tc.slots)
				}
				return
			}
			for _, w := range ws {
				if !routeKey.MatchString(w.name) || !routeValue.MatchString(w.value) {
					t.Errorf("(%q, %q) is rejected by the routing action", w.name, w.value)
				}
				if w.value != "1800000000 "+tc.want {
					t.Errorf("value = %q, want slots %s", w.value, tc.want)
				}
			}
		})
	}
}

func TestVarNameValidatesLikeRouting(t *testing.T) {
	for _, tc := range []struct {
		machine string
		want    string
		ok      bool
	}{
		{"examplemac", "CI_POOL_HB_EXAMPLEMAC", true},
		{"mac2", "CI_POOL_HB_MAC2", true},
		{"examplemacb", "CI_POOL_HB_EXAMPLEMACB", true},
		{"123456789012", "CI_POOL_HB_123456789012", true},
		{"", "", false},
		{"1234567890123", "", false},
		{"example-mac", "", false},
		{"example mac", "", false},
		{"mac\n", "", false},
	} {
		got, err := VarName(tc.machine)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("VarName(%q) = %q, %v; want %q ok=%v", tc.machine, got, err, tc.want, tc.ok)
		}
		if err == nil && !routeKey.MatchString(got) {
			t.Errorf("VarName(%q) = %q, which routing rejects", tc.machine, got)
		}
	}
}

func TestValueOutOfRangeEpochIsRefused(t *testing.T) {
	if _, ok := value(-1, 2); ok {
		t.Error("a negative epoch was accepted")
	}
	if _, ok := value(1_000_000_000_000, 2); ok {
		t.Error("a 13-digit epoch was accepted")
	}
	if v, ok := value(999_999_999_999, 8); !ok || !routeValue.MatchString(v) {
		t.Errorf("value = %q, %v", v, ok)
	}
}

func TestHeartbeatKeepsGoingAfterATransientError(t *testing.T) {
	snap := &fakeSnap{snap: snapshot(2, project("example/alpha", true))}
	vars := &fakeVars{err: errors.New("github: server error")}
	fatalCalls := 0
	var mu sync.Mutex
	stop := start(t, Config{Snapshot: snap.get, Vars: vars, Fatal: func(error) bool {
		mu.Lock()
		defer mu.Unlock()
		fatalCalls++
		return false
	}})
	waitFor(t, "three failed beats", func() bool { return len(vars.all()) >= 3 })
	stop()
	mu.Lock()
	defer mu.Unlock()
	if fatalCalls < 3 {
		t.Fatalf("Fatal asked %d times, want it asked about every failure", fatalCalls)
	}
}

// A rejected or unreadable login ends the heartbeat and is handed to Fatal,
// which ends the process the way the supervisor's own GitHub calls do.
func TestHeartbeatLoginLossIsHandedToFatalAndStopsTheLoop(t *testing.T) {
	for name, cause := range map[string]error{
		"rejected token": fmt.Errorf("setting a variable: %w", github.ErrUnauthorized),
		"no token":       fmt.Errorf("%w: gh auth token exited with status 1", ghauth.ErrNoToken),
	} {
		t.Run(name, func(t *testing.T) {
			snap := &fakeSnap{snap: snapshot(2, project("example/alpha", true))}
			vars := &fakeVars{err: cause}
			var mu sync.Mutex
			var got []error
			cfg := Config{
				Every:    5 * time.Millisecond,
				Snapshot: snap.get,
				Vars:     vars,
				Now:      fixedNow,
				Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
				Fatal: func(err error) bool {
					mu.Lock()
					defer mu.Unlock()
					got = append(got, err)
					return true
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); Run(ctx, cfg) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Run kept going after Fatal took the error")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(got) != 1 || !errors.Is(got[0], cause) {
				t.Fatalf("Fatal saw %v, want exactly the login error", got)
			}
			if n := len(vars.all()); n != 1 {
				t.Fatalf("%d writes, want the loop to stop at the first failure", n)
			}
		})
	}
}

func TestHeartbeatFailureDuringShutdownIsNotFatal(t *testing.T) {
	snap := &fakeSnap{snap: snapshot(2, project("example/alpha", true))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	vars := &cancelingVars{cancel: cancel}
	called := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, Config{
			Every: time.Hour, Snapshot: snap.get, Vars: vars, Now: fixedNow,
			Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			Fatal: func(error) bool { called = true; return true },
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	if !vars.wrote() {
		t.Fatal("the heartbeat never wrote, so the shutdown case was not exercised")
	}
	if called {
		t.Fatal("an error caused by the shutdown itself was reported as a lost login")
	}
}

type cancelingVars struct {
	cancel context.CancelFunc
	n      atomic.Int32
}

func (c *cancelingVars) wrote() bool { return c.n.Load() > 0 }

func (c *cancelingVars) SetVariable(ctx context.Context, _, _, _ string) error {
	c.n.Add(1)
	c.cancel()
	return fmt.Errorf("%w: %w", ctx.Err(), github.ErrUnauthorized)
}

func TestHeartbeatLogsAFailureWithoutTheValue(t *testing.T) {
	var buf strings.Builder
	snap := &fakeSnap{snap: snapshot(2, project("example/alpha", true))}
	vars := &fakeVars{err: errors.New("boom")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, Config{
			Every: time.Hour, Snapshot: snap.get, Vars: vars, Now: fixedNow,
			Log: slog.New(slog.NewTextHandler(&buf, nil)),
		})
	}()
	waitFor(t, "a failed write", func() bool { return len(vars.all()) > 0 })
	cancel()
	<-done
	if !strings.Contains(buf.String(), "example/alpha") || !strings.Contains(buf.String(), "boom") {
		t.Fatalf("log = %q, want the repository and the error", buf.String())
	}
}

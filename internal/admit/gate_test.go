package admit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	pollEvery = time.Millisecond
	os.Exit(m.Run())
}

// fakeProc is one request. The test admits it, ends it with a status, or lets
// it hang.
type fakeProc struct {
	path     string
	args     []string
	env      []string
	admitted chan Grant
	done     chan int
	endOnce  sync.Once

	mu        sync.Mutex
	withdrawn int
	closed    int
}

func (p *fakeProc) Admitted() <-chan Grant { return p.admitted }
func (p *fakeProc) Done() <-chan int       { return p.done }

func (p *fakeProc) Withdraw() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.withdrawn++
}

// Close ends the hold, as the real one does once its stdin closes.
func (p *fakeProc) Close() {
	p.mu.Lock()
	p.closed++
	p.mu.Unlock()
	p.end(0)
}

func (p *fakeProc) admit(g Grant) { p.admitted <- g }

// end makes the child exit with status; before an admission, Admitted closes
// empty.
func (p *fakeProc) end(status int) {
	p.endOnce.Do(func() {
		close(p.admitted)
		p.done <- status
	})
}

func (p *fakeProc) counts() (withdrawn, closed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.withdrawn, p.closed
}

// fakeCoreslot starts fakeProcs. AutoAdmit admits every request at once with
// an empty grant; otherwise each started request is sent on started.
type fakeCoreslot struct {
	AutoAdmit bool
	err       error
	started   chan *fakeProc

	mu    sync.Mutex
	procs []*fakeProc
}

func newFakeCoreslot() *fakeCoreslot { return &fakeCoreslot{started: make(chan *fakeProc, 16)} }

func (f *fakeCoreslot) start(_ context.Context, path string, args, env []string) (Proc, error) {
	if f.err != nil {
		f.mu.Lock()
		f.procs = append(f.procs, nil)
		f.mu.Unlock()
		return nil, f.err
	}
	p := &fakeProc{path: path, args: args, env: env, admitted: make(chan Grant, 1), done: make(chan int, 1)}
	f.mu.Lock()
	f.procs = append(f.procs, p)
	f.mu.Unlock()
	if f.AutoAdmit {
		p.admit(Grant{Hold: "h"})
	} else {
		f.started <- p
	}
	return p, nil
}

func (f *fakeCoreslot) starts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.procs)
}

// next is the next request started, within 1s.
func (f *fakeCoreslot) next(t *testing.T) *fakeProc {
	t.Helper()
	select {
	case p := <-f.started:
		return p
	case <-time.After(time.Second):
		t.Fatal("no request started within 1s")
		return nil
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type rig struct {
	gate     *Gate
	coreslot *fakeCoreslot
	clock    *fakeClock
	path     string
}

// newRig builds a gate whose coreslot is a file in a temporary directory.
func newRig(t *testing.T) *rig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coreslot")
	if err := os.WriteFile(path, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &rig{coreslot: newFakeCoreslot(), clock: &fakeClock{t: time.Unix(1_800_000_000, 0)}, path: path}
	r.gate = New(Config{
		Coreslot: path,
		Label:    "alpha:docker-vm",
		Self:     "/x/pool",
		Size:     func() []string { return []string{"--mem", "6G"} },
		Env:      []string{"DOCKER_HOST=unix:///x/pool/docker.sock"},
		Start:    r.coreslot.start,
		Clock:    r.clock.now,
	})
	return r
}

// bind runs Bind in the background; its answer arrives on the channel.
func (r *rig) bind(ctx context.Context, runner string) <-chan bool {
	out := make(chan bool, 1)
	go func() { out <- r.gate.Bind(ctx, runner) }()
	return out
}

func answer(t *testing.T, c <-chan bool) bool {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(time.Second):
		t.Fatal("Bind did not return within 1s")
		return false
	}
}

func stillWaiting(t *testing.T, c <-chan bool) {
	t.Helper()
	select {
	case v := <-c:
		t.Fatalf("Bind returned %v, want it still waiting", v)
	case <-time.After(50 * time.Millisecond):
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within 2s: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBindRequestArgs(t *testing.T) {
	r := newRig(t)
	r.coreslot.AutoAdmit = true
	if !r.gate.Bind(context.Background(), "r1") {
		t.Fatal("Bind = false")
	}
	p := r.coreslot.procs[0]
	want := []string{"run", "--restart=no", "--label", "alpha:docker-vm", "--mem", "6G",
		"--timeout", "4m", "--on-timeout", "run", "--", "/x/pool", "hold"}
	if p.path != r.path || !slices.Equal(p.args, want) {
		t.Fatalf("started %s %q, want %s %q", p.path, p.args, r.path, want)
	}
	if !slices.Equal(p.env, []string{"DOCKER_HOST=unix:///x/pool/docker.sock"}) {
		t.Fatalf("env %q", p.env)
	}
}

// Mutation: returning true before the admission (right after Start) turns it
// red.
func TestBindWaitsForAdmission(t *testing.T) {
	r := newRig(t)
	got := r.bind(context.Background(), "r1")
	p := r.coreslot.next(t)
	stillWaiting(t, got)
	if h := r.gate.Health(); h.State != "waiting" || h.Waiting != 1 {
		t.Fatalf("health while waiting = %+v", h)
	}
	p.admit(Grant{Hold: "h-1", DockerHost: "unix:///x/d.sock"})
	if !answer(t, got) {
		t.Fatal("Bind = false after the admission")
	}
	if g, ok := r.gate.Grant("r1"); !ok || g.Hold != "h-1" || g.DockerHost != "unix:///x/d.sock" {
		t.Fatalf("Grant = %+v %v", g, ok)
	}
	if h := r.gate.Health(); h.State != "held" || h.Held != 1 || h.Unbudgeted != 0 {
		t.Fatalf("health once held = %+v", h)
	}
}

// Mutation: returning false on ctx's end without Withdraw (the ticket lingers
// in coreslot's queue) turns it red.
func TestBindWithdrawnByContext(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	got := r.bind(ctx, "r1")
	p := r.coreslot.next(t)
	cancel()
	if answer(t, got) {
		t.Fatal("Bind = true after its context ended")
	}
	if w, _ := p.counts(); w != 1 {
		t.Fatalf("withdrawn %d times, want 1", w)
	}
}

// Mutation: keeping a late admission (abandon not closing it) turns it red.
func TestLateAdmissionIsClosed(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	got := r.bind(ctx, "r1")
	p := r.coreslot.next(t)
	cancel()
	if answer(t, got) {
		t.Fatal("Bind = true after its context ended")
	}
	p.admit(Grant{Hold: "h-late"})
	eventually(t, "the late admission is closed", func() bool { _, c := p.counts(); return c == 1 })
	if _, ok := r.gate.Grant("r1"); ok {
		t.Fatal("a late admission is held")
	}
	if h := r.gate.Health(); h.Held != 0 || h.Waiting != 0 {
		t.Fatalf("health = %+v, want nothing held or waiting", h)
	}
}

// Mutation: Stop only refusing later Binds (not waking the waiting ones)
// turns it red.
func TestStopWithdrawsWaiters(t *testing.T) {
	r := newRig(t)
	a := r.bind(context.Background(), "r1")
	pa := r.coreslot.next(t)
	b := r.bind(context.Background(), "r2")
	pb := r.coreslot.next(t)
	r.gate.Stop()
	if answer(t, a) || answer(t, b) {
		t.Fatal("a waiting Bind returned true after Stop")
	}
	for _, p := range []*fakeProc{pa, pb} {
		if w, _ := p.counts(); w != 1 {
			t.Fatalf("withdrawn %d times, want 1", w)
		}
	}
}

// Mutation: starting a child after Stop (dropping the stopped check before
// Start) turns it red.
func TestStopRefusesLaterBinds(t *testing.T) {
	r := newRig(t)
	r.coreslot.AutoAdmit = true
	r.gate.Stop()
	if r.gate.Bind(context.Background(), "r1") {
		t.Fatal("Bind = true after Stop")
	}
	if n := r.coreslot.starts(); n != 0 {
		t.Fatalf("%d requests started after Stop, want 0", n)
	}
}

// Mutation: latching on a non-empty Unbudgeted turns it red.
func TestUnbudgetedGrantIsNoLatch(t *testing.T) {
	r := newRig(t)
	for i, text := range []string{"timeout", "x"} {
		got := r.bind(context.Background(), fmt.Sprintf("r%d", i))
		r.coreslot.next(t).admit(Grant{Hold: "h", Unbudgeted: text})
		if !answer(t, got) {
			t.Fatalf("Bind = false for a grant unbudgeted %q", text)
		}
		if !r.gate.Budgeted() {
			t.Fatalf("Budgeted() = false after a grant unbudgeted %q", text)
		}
	}
	if h := r.gate.Health(); h.Unbudgeted != 2 || h.LastUnbudgeted != "x" || h.State != "held" {
		t.Fatalf("health = %+v, want 2 unbudgeted, the last x, held", h)
	}
	got := r.bind(context.Background(), "r3")
	r.coreslot.next(t).admit(Grant{Hold: "h"})
	if !answer(t, got) {
		t.Fatal("the next Bind = false")
	}
	if n := r.coreslot.starts(); n != 3 {
		t.Fatalf("%d requests, want 3", n)
	}
}

// Mutation: returning false for a request that ended before its admission
// (which strands an assigned job) turns it red.
func TestFailedRequestRunsUnbudgeted(t *testing.T) {
	r := newRig(t)
	got := r.bind(context.Background(), "r1")
	r.coreslot.next(t).end(70)
	if !answer(t, got) {
		t.Fatal("Bind = false after the request exited 70")
	}
	if !r.gate.Budgeted() {
		t.Fatal("exit 70 latched the gate")
	}
	if h := r.gate.Health(); h.Unbudgeted != 1 || h.LastUnbudgeted != "exit 70" {
		t.Fatalf("health = %+v, want 1 unbudgeted, reason exit 70", h)
	}
	if _, ok := r.gate.Grant("r1"); ok {
		t.Fatal("a grant for a request that was never admitted")
	}
	r.gate.Unbind("r1")
	if h := r.gate.Health(); h.Held != 0 || h.Unbudgeted != 0 {
		t.Fatalf("health after Unbind = %+v", h)
	}
}

// Mutation: treating 64 like any other exit (no latch, a child per Bind)
// turns it red.
func TestUsageExitLatchesUnbudgeted(t *testing.T) {
	r := newRig(t)
	got := r.bind(context.Background(), "r1")
	r.coreslot.next(t).end(64)
	if !answer(t, got) {
		t.Fatal("Bind = false after exit 64")
	}
	if r.gate.Budgeted() {
		t.Fatal("Budgeted() = true after exit 64")
	}
	if h := r.gate.Health(); h.State != "unbudgeted" || h.Reason != ReasonTooOld {
		t.Fatalf("health = %+v, want unbudgeted, %s", h, ReasonTooOld)
	}
	if !answer(t, r.bind(context.Background(), "r2")) {
		t.Fatal("a latched Bind = false")
	}
	if n := r.coreslot.starts(); n != 1 {
		t.Fatalf("%d requests, want 1: a latched Bind starts no child", n)
	}
}

// Mutation: returning false on a start error turns it red.
func TestStartFailureIsUnbudgeted(t *testing.T) {
	for _, c := range []struct {
		err    error
		reason string
	}{
		{&fs.PathError{Op: "fork/exec", Path: "/x/coreslot", Err: fs.ErrNotExist}, ReasonNotInstalled},
		{errors.New("too many open files"), ReasonStart},
	} {
		r := newRig(t)
		r.coreslot.err = c.err
		if !r.gate.Bind(context.Background(), "r1") {
			t.Fatalf("Bind = false after the start error %v", c.err)
		}
		if r.gate.Budgeted() {
			t.Fatalf("Budgeted() = true after the start error %v", c.err)
		}
		if h := r.gate.Health(); h.State != "unbudgeted" || h.Reason != c.reason {
			t.Fatalf("health = %+v, want unbudgeted, %s", h, c.reason)
		}
	}
}

func TestLatchClearsOnNewBinary(t *testing.T) {
	r := newRig(t)
	got := r.bind(context.Background(), "r1")
	r.coreslot.next(t).end(64)
	answer(t, got)
	if r.gate.Budgeted() {
		t.Fatal("Budgeted() = true with the same binary")
	}
	if err := os.WriteFile(r.path, []byte("v2, a newer coreslot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !r.gate.Budgeted() {
		t.Fatal("Budgeted() = false after the binary changed")
	}
	got = r.bind(context.Background(), "r2")
	r.coreslot.next(t).admit(Grant{Hold: "h"})
	if !answer(t, got) || r.coreslot.starts() != 2 {
		t.Fatalf("after the change: %d requests, want 2", r.coreslot.starts())
	}

	// With no coreslot found, the latch never clears.
	none := New(Config{Label: "alpha:docker-vm", Self: "/x/pool", Start: r.coreslot.start})
	if none.Budgeted() || none.Health().Reason != ReasonNotInstalled {
		t.Fatalf("no coreslot: budgeted %v, health %+v", none.Budgeted(), none.Health())
	}
	if !none.Bind(context.Background(), "r3") || r.coreslot.starts() != 2 {
		t.Fatalf("no coreslot: Bind started a request (%d)", r.coreslot.starts())
	}
}

// Mutation: returning false when coreslot does not answer (which strands the
// job) turns it red.
func TestNoAnswerRunsUnbudgeted(t *testing.T) {
	r := newRig(t)
	got := r.bind(context.Background(), "r1")
	p := r.coreslot.next(t)
	r.clock.advance(Timeout + answerSlack - time.Second)
	stillWaiting(t, got)
	if h := r.gate.Health(); h.OldestWaitS != int((Timeout + answerSlack - time.Second).Seconds()) {
		t.Fatalf("oldest wait %ds", h.OldestWaitS)
	}
	r.clock.advance(time.Second)
	if !answer(t, got) {
		t.Fatal("Bind = false after no answer")
	}
	if w, _ := p.counts(); w != 1 {
		t.Fatalf("withdrawn %d times, want 1", w)
	}
	if !r.gate.Budgeted() {
		t.Fatal("no answer latched the gate")
	}
	if h := r.gate.Health(); h.Unbudgeted != 1 || h.LastUnbudgeted != ReasonNoAnswer {
		t.Fatalf("health = %+v, want 1 unbudgeted, %s", h, ReasonNoAnswer)
	}
}

func TestUnbindClosesHold(t *testing.T) {
	r := newRig(t)
	got := r.bind(context.Background(), "r1")
	p := r.coreslot.next(t)
	waiting := r.bind(context.Background(), "r2")
	q := r.coreslot.next(t)
	p.admit(Grant{Hold: "h-1"})
	answer(t, got)
	r.gate.Unbind("r2") // still waiting: no-op
	r.gate.Unbind("nope")
	if _, c := q.counts(); c != 0 {
		t.Fatal("Unbind closed a waiting request")
	}
	r.gate.Unbind("r1")
	r.gate.Unbind("r1")
	if _, c := p.counts(); c != 1 {
		t.Fatalf("closed %d times, want 1", c)
	}
	if _, ok := r.gate.Grant("r1"); ok {
		t.Fatal("Grant after Unbind")
	}
	q.admit(Grant{Hold: "h-2"})
	answer(t, waiting)
}

func TestHoldLostAfterBind(t *testing.T) {
	r := newRig(t)
	got := r.bind(context.Background(), "r1")
	p := r.coreslot.next(t)
	p.admit(Grant{Hold: "h-1"})
	answer(t, got)
	p.end(1)
	eventually(t, "hold-lost", func() bool { return r.gate.Health().Reason == ReasonHoldLost })
	if h := r.gate.Health(); h.Held != 1 || h.State != "held" {
		t.Fatalf("health = %+v, want the runner still held", h)
	}
	r.gate.Unbind("r1")
	if h := r.gate.Health(); h.State != "idle" || h.Reason != "" {
		t.Fatalf("health after Unbind = %+v, want idle", h)
	}
}

func TestCloseUnbindsAll(t *testing.T) {
	r := newRig(t)
	var procs []*fakeProc
	for _, name := range []string{"r1", "r2"} {
		got := r.bind(context.Background(), name)
		p := r.coreslot.next(t)
		p.admit(Grant{Hold: name})
		answer(t, got)
		procs = append(procs, p)
	}
	waiting := r.bind(context.Background(), "r3")
	w := r.coreslot.next(t)
	r.gate.Close()
	if answer(t, waiting) {
		t.Fatal("a waiting Bind = true after Close")
	}
	if n, _ := w.counts(); n != 1 {
		t.Fatalf("the waiter was withdrawn %d times, want 1", n)
	}
	for _, p := range procs {
		if _, c := p.counts(); c != 1 {
			t.Fatalf("a held runner closed %d times, want 1", c)
		}
	}
	if h := r.gate.Health(); h.Held != 0 || h.State != "idle" {
		t.Fatalf("health after Close = %+v", h)
	}
}

func TestHealthPrecedence(t *testing.T) {
	r := newRig(t)
	if h := r.gate.Health(); h.State != "idle" {
		t.Fatalf("health = %+v, want idle", h)
	}
	got := r.bind(context.Background(), "r1")
	r.coreslot.next(t).admit(Grant{Hold: "h"})
	answer(t, got)
	if h := r.gate.Health(); h.State != "held" {
		t.Fatalf("health = %+v, want held", h)
	}
	waiting := r.bind(context.Background(), "r2")
	w := r.coreslot.next(t)
	r.clock.advance(5 * time.Second)
	if h := r.gate.Health(); h.State != "waiting" || h.OldestWaitS != 5 || h.Held != 1 || h.Waiting != 1 {
		t.Fatalf("health = %+v, want waiting for 5s beside one held", h)
	}
	latched := r.bind(context.Background(), "r3")
	r.coreslot.next(t).end(64)
	answer(t, latched)
	if h := r.gate.Health(); h.State != "unbudgeted" || h.Reason != ReasonTooOld {
		t.Fatalf("health = %+v, want unbudgeted over waiting", h)
	}
	w.admit(Grant{Hold: "h"})
	answer(t, waiting)
}

func TestStartProcReadsTheLine(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	hold := script("hold", `echo '{"hold":"h-1","docker_host":"unix:///x/d.sock","unbudgeted":""}'; cat >/dev/null`)
	p, err := StartProc(context.Background(), hold, []string{"run", "--", "/x/pool", "hold"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case g, ok := <-p.Admitted():
		if !ok || g != (Grant{Hold: "h-1", DockerHost: "unix:///x/d.sock"}) {
			t.Fatalf("grant %+v %v", g, ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no grant within 5s")
	}
	select {
	case s := <-p.Done():
		t.Fatalf("the hold exited %d before Close", s)
	case <-time.After(50 * time.Millisecond):
	}
	p.Close()
	select {
	case s := <-p.Done():
		if s != 0 {
			t.Fatalf("exit %d after Close, want 0", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hold did not end within 5s of Close")
	}

	old := script("old", "exit 64")
	p, err = StartProc(context.Background(), old, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case g, ok := <-p.Admitted():
		if ok {
			t.Fatalf("a grant %+v from a child that printed nothing", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Admitted not closed within 5s")
	}
	if s := <-p.Done(); s != 64 {
		t.Fatalf("exit %d, want 64", s)
	}
}

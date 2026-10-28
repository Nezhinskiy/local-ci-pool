package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// fakeDocker records the calls the package makes. A container's stdin is read
// from the far end of a net.Pipe until the near end is closed.
type fakeDocker struct {
	mu        sync.Mutex
	calls     []string
	creates   []client.ContainerCreateOptions
	stdin     map[string]*bytes.Buffer
	stdinDone map[string]chan struct{}
	removes   []string
	removeErr func(id string) error
	// removeHangs makes ContainerRemove wait for its context, like a stuck
	// daemon.
	removeHangs bool
	startErr    error
	attachErr   error
	events      func(ctx context.Context, opts client.EventsListOptions) client.EventsResult
	// networks are the networks that exist, by name; networkCreates the
	// create calls.
	networks       map[string]network.Inspect
	networkCreates []client.NetworkCreateOptions
	networkErr     error // every NetworkInspect fails with it
	// networkCreateHook, when set, answers NetworkCreate instead.
	networkCreateHook func(name string) error
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{stdin: map[string]*bytes.Buffer{}, stdinDone: map[string]chan struct{}{}}
}

func (f *fakeDocker) NetworkInspect(_ context.Context, name string, _ client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "network-inspect:"+name)
	if f.networkErr != nil {
		return client.NetworkInspectResult{}, f.networkErr
	}
	n, ok := f.networks[name]
	if !ok {
		return client.NetworkInspectResult{}, cerrdefs.ErrNotFound.WithMessage("network " + name + " not found")
	}
	return client.NetworkInspectResult{Network: n}, nil
}

func (f *fakeDocker) NetworkCreate(_ context.Context, name string, o client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "network-create")
	f.networkCreates = append(f.networkCreates, o)
	if hook := f.networkCreateHook; hook != nil {
		return client.NetworkCreateResult{}, hook(name)
	}
	if f.networks == nil {
		f.networks = map[string]network.Inspect{}
	}
	f.networks[name] = network.Inspect{Network: network.Network{Name: name, Driver: o.Driver, Internal: o.Internal, Options: o.Options}}
	return client.NetworkCreateResult{ID: "net-" + name}, nil
}

func (f *fakeDocker) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeDocker) ContainerCreate(_ context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "create")
	f.creates = append(f.creates, o)
	return client.ContainerCreateResult{ID: fmt.Sprintf("id-%d", len(f.creates))}, nil
}

func (f *fakeDocker) ContainerAttach(_ context.Context, id string, o client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	f.record("attach")
	if f.attachErr != nil {
		return client.ContainerAttachResult{}, f.attachErr
	}
	if !o.Stream || !o.Stdin || o.Stdout || o.Stderr {
		return client.ContainerAttachResult{}, fmt.Errorf("unexpected attach options %+v", o)
	}
	near, far := net.Pipe()
	buf := &bytes.Buffer{}
	done := make(chan struct{})
	f.mu.Lock()
	f.stdin[id] = buf
	f.stdinDone[id] = done
	f.mu.Unlock()
	go func() {
		defer close(done)
		b, _ := io.ReadAll(far)
		f.mu.Lock()
		buf.Write(b)
		f.mu.Unlock()
	}()
	return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(near, "")}, nil
}

func (f *fakeDocker) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.record("start")
	return client.ContainerStartResult{}, f.startErr
}

func (f *fakeDocker) ContainerRemove(ctx context.Context, id string, o client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	if f.removeHangs {
		f.mu.Unlock()
		<-ctx.Done()
		return client.ContainerRemoveResult{}, ctx.Err()
	}
	f.calls = append(f.calls, "remove")
	if o.Force {
		f.removes = append(f.removes, id)
	} else {
		f.removes = append(f.removes, "unforced:"+id)
	}
	hook := f.removeErr
	f.mu.Unlock()
	if hook != nil {
		return client.ContainerRemoveResult{}, hook(id)
	}
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeDocker) Events(ctx context.Context, o client.EventsListOptions) client.EventsResult {
	f.record("events")
	if f.events == nil {
		errs := make(chan error, 1)
		errs <- errors.New("no events configured")
		close(errs)
		return client.EventsResult{Messages: make(chan events.Message), Err: errs}
	}
	return f.events(ctx, o)
}

func (f *fakeDocker) removed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.removes...)
	sort.Strings(out)
	return out
}

func (f *fakeDocker) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// stdinOf waits until the container's stdin is closed and returns what was
// written to it.
func (f *fakeDocker) stdinOf(t *testing.T, id string) string {
	t.Helper()
	f.mu.Lock()
	done := f.stdinDone[id]
	f.mu.Unlock()
	if done == nil {
		t.Fatalf("container %s was never attached", id)
	}
	<-done
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stdin[id].String()
}

// fakeJIT mints a JIT configuration per runner name.
type fakeJIT struct {
	mu     sync.Mutex
	names  []string
	ids    []int
	err    error
	onMint func(name string)
}

func (j *fakeJIT) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	if j.onMint != nil {
		j.onMint(s.Name)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return nil, j.err
	}
	j.names = append(j.names, s.Name)
	j.ids = append(j.ids, scaleSetID)
	return &scaleset.RunnerScaleSetJitRunnerConfig{EncodedJITConfig: "jit-for-" + s.Name}, nil
}

// minted lists the runners a JIT configuration was minted for.
func (j *fakeJIT) minted() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.names...)
}

// fakeRegistry stands in for the scale set's runner registrations. Every
// name is registered unless listed in missing. Lookups and removals are
// recorded in the fake Docker's call log, so their order against container
// removals is visible.
type fakeRegistry struct {
	mu        sync.Mutex
	docker    *fakeDocker
	ids       map[string]int
	missing   map[string]bool
	lookupErr error
	// onLookup runs before a lookup answers, outside the fake's lock; a
	// non-nil result is the lookup's error.
	onLookup  func(ctx context.Context, name string) error
	removeErr func(id int64) error
	lookups   []string
	removed   []int64
}

func (r *fakeRegistry) GetRunnerByName(ctx context.Context, name string) (*scaleset.RunnerReference, error) {
	r.mu.Lock()
	r.lookups = append(r.lookups, name)
	hook := r.onLookup
	r.mu.Unlock()
	r.docker.record("lookup")
	if hook != nil {
		if err := hook(ctx, name); err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lookupErr != nil {
		return nil, r.lookupErr
	}
	if r.missing[name] {
		return nil, nil
	}
	if r.ids == nil {
		r.ids = map[string]int{}
	}
	id, ok := r.ids[name]
	if !ok {
		id = 100 + len(r.ids)
		r.ids[name] = id
	}
	return &scaleset.RunnerReference{ID: id, Name: name}, nil
}

func (r *fakeRegistry) RemoveRunner(_ context.Context, id int64) error {
	r.mu.Lock()
	hook := r.removeErr
	r.removed = append(r.removed, id)
	r.mu.Unlock()
	r.docker.record("unregister")
	if hook != nil {
		return hook(id)
	}
	return nil
}

func (r *fakeRegistry) idOf(name string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return int64(r.ids[name])
}

func (r *fakeRegistry) unregistered() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.removed...)
}

func (r *fakeRegistry) lookedUp() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lookups...)
}

// fakeStarter records the runners a scaler starts.
type fakeStarter struct {
	mu      sync.Mutex
	names   []string
	jits    map[string]string
	needs   map[string]int
	err     error
	onStart func(name string)
	// hangs makes a start wait for its context, like a stuck daemon.
	hangs bool
	// block, when set, makes a start wait until it is closed or its context
	// ends.
	block chan struct{}
	ctxs  map[string]context.Context
}

func (s *fakeStarter) start(ctx context.Context, name, jit string, need int) error {
	s.mu.Lock()
	hook := s.onStart
	if s.jits == nil {
		s.jits = map[string]string{}
		s.needs = map[string]int{}
	}
	s.names = append(s.names, name)
	s.jits[name] = jit
	s.needs[name] = need
	if s.ctxs == nil {
		s.ctxs = map[string]context.Context{}
	}
	s.ctxs[name] = ctx
	err := s.err
	hangs := s.hangs
	block := s.block
	s.mu.Unlock()
	if hook != nil {
		hook(name)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if hangs {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (s *fakeStarter) started() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.names...)
}

// ctxOf is the context a runner's start ran on.
func (s *fakeStarter) ctxOf(name string) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctxs[name]
}

// needOf is the need a runner was started with.
func (s *fakeStarter) needOf(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needs[name]
}

// logBuffer is a concurrency-safe log sink.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	sc := bufio.NewScanner(strings.NewReader(l.buf.String()))
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

func (l *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type harness struct {
	scaler   *Scaler
	docker   *fakeDocker
	registry *fakeRegistry
	jit      *fakeJIT
	starter  *fakeStarter
	logs     *logBuffer
}

func newHarness(slots *Slots) *harness { return newHarnessWith(slots, nil) }

// newHarnessWith lets configure adjust the scaler's configuration first.
func newHarnessWith(slots *Slots, configure func(*ScalerConfig)) *harness {
	h := &harness{docker: newFakeDocker(), jit: &fakeJIT{}, starter: &fakeStarter{}, logs: &logBuffer{}}
	h.registry = &fakeRegistry{docker: h.docker}
	cfg := ScalerConfig{
		ScaleSetID: 7,
		Slots:      slots,
		Start:      h.starter.start,
		JIT:        h.jit,
		Registry:   h.registry,
		Docker:     h.docker,
		Log:        h.logs.logger(),
	}
	if configure != nil {
		configure(&cfg)
	}
	h.scaler = NewScaler(cfg)
	return h
}

// fakeGate is a budget gate the test drives by hand. Each Admit hands the
// test an admitReq and returns exactly what the test answers, even after its
// ctx ended: it never watches the ctx itself.
type fakeGate struct {
	enabled      atomic.Bool
	reqs         chan *admitReq
	enabledCalls atomic.Int32
	// returned counts the Admit calls that returned.
	returned atomic.Int32
	// onEnabled runs inside every Enabled call.
	onEnabled func()
	// onRelease runs after a release is counted.
	onRelease func(name string)

	mu       sync.Mutex
	all      []*admitReq
	released map[string]int
	// closed answers every later Admit at once with context.Canceled.
	closed bool
}

type admitAnswer struct {
	adm Admitted
	err error
}

// admitReq is one Admit call waiting for the test's answer.
type admitReq struct {
	name, project string
	ctx           context.Context
	answer        chan admitAnswer
	once          sync.Once
}

// reply answers the request; only the first answer counts.
func (r *admitReq) reply(adm Admitted, err error) {
	r.once.Do(func() { r.answer <- admitAnswer{adm, err} })
}

// answerOnCancel answers the request with its ctx's error once its ctx ends.
func answerOnCancel(r *admitReq) {
	go func() {
		<-r.ctx.Done()
		r.reply(Admitted{}, r.ctx.Err())
	}()
}

func newFakeGate() *fakeGate {
	g := &fakeGate{reqs: make(chan *admitReq, 64), released: map[string]int{}}
	g.enabled.Store(true)
	return g
}

func (g *fakeGate) Enabled() bool {
	g.enabledCalls.Add(1)
	if g.onEnabled != nil {
		g.onEnabled()
	}
	return g.enabled.Load()
}

func (g *fakeGate) Admit(ctx context.Context, a Admission) (Admitted, error) {
	defer g.returned.Add(1)
	r := &admitReq{name: a.Runner, project: a.Project, ctx: ctx, answer: make(chan admitAnswer, 1)}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return Admitted{}, context.Canceled
	}
	g.all = append(g.all, r)
	g.mu.Unlock()
	g.reqs <- r
	ans := <-r.answer
	return ans.adm, ans.err
}

func (g *fakeGate) Adopt(context.Context, Admission) Admitted {
	return Admitted{Release: func() {}, Mode: ModeAdopted}
}

// admitted builds an Admitted whose Release counts towards released[name],
// then runs onRelease.
func (g *fakeGate) admitted(name string, mode Mode, need int) Admitted {
	return Admitted{Mode: mode, Need: need, Release: func() {
		g.mu.Lock()
		g.released[name]++
		hook := g.onRelease
		g.mu.Unlock()
		if hook != nil {
			hook(name)
		}
	}}
}

// releasedOf is how many times the grant of name was released.
func (g *fakeGate) releasedOf(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.released[name]
}

// admitCalls is the number of Admit calls so far.
func (g *fakeGate) admitCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.all)
}

// next waits for the next Admit call.
func (g *fakeGate) next(t *testing.T) *admitReq {
	t.Helper()
	select {
	case r := <-g.reqs:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("no Admit call within 2s")
		return nil
	}
}

// gateHarness is a harness whose scaler admits runners through a fakeGate,
// on a manual clock.
type gateHarness struct {
	*harness
	gate  *fakeGate
	clock *manualClock
}

// manualClock is a clock that moves only when the test advances it.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// lower calls Desired(count) twice, dipGrace apart, so that the waiting
// runners beyond count are given up, and returns the second call's result.
func (h *gateHarness) lower(t *testing.T, count int) int {
	t.Helper()
	desired(t, h.scaler, count)
	h.clock.advance(dipGrace)
	return desired(t, h.scaler, count)
}

// newGateHarness wires a fakeGate into a harness. By default the gate's
// Enabled and every Release call s.Held() within 2s, so a call made under
// the scaler's lock fails the test. At cleanup the scaler is aborted, every
// unanswered or later Admit is answered with a ctx error, and every waiter goroutine
// must return within 2s.
func newGateHarness(t *testing.T, slots *Slots) *gateHarness {
	t.Helper()
	g := newFakeGate()
	clock := &manualClock{t: time.Unix(1_800_000_000, 0)}
	h := newHarnessWith(slots, func(c *ScalerConfig) {
		c.Gate = g
		c.Project = "alpha"
		c.Now = clock.now
	})
	held := func(what string) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.scaler.Held()
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("%s ran under the scaler's lock: Held() blocked for 2s", what)
		}
	}
	g.onEnabled = func() { held("Enabled") }
	g.onRelease = func(name string) { held("Release of " + name) }
	t.Cleanup(func() {
		h.scaler.Abort()
		g.mu.Lock()
		g.closed = true
		all := append([]*admitReq(nil), g.all...)
		g.mu.Unlock()
		for _, r := range all {
			r.reply(Admitted{}, context.Canceled)
		}
		if !waitGroupWithin(&h.scaler.wg, 2*time.Second) {
			t.Error("waiter goroutines still running 2s after cleanup")
		}
	})
	return &gateHarness{harness: h, gate: g, clock: clock}
}

// waitGroupWithin reports whether wg is done within d.
func waitGroupWithin(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// eventually polls cond for up to 2s.
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

// eventLog is one ordered record of calls made to several fakes, so a test can
// assert their order.
type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = append(l.ev, e)
}

func (l *eventLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.ev)
}

// fakeAcquirer stands in for the session's AcquireJobs. It records the request
// IDs of every call, appends "acquire <ids>" to events when set, and acquires
// every request unless err, or answer when set, says otherwise.
type fakeAcquirer struct {
	mu     sync.Mutex
	ids    [][]int64
	err    error
	answer func(ctx context.Context) error
	events *eventLog
}

func (f *fakeAcquirer) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	f.mu.Lock()
	f.ids = append(f.ids, slices.Clone(requestIDs))
	err, answer, events := f.err, f.answer, f.events
	f.mu.Unlock()
	if events != nil {
		events.add(fmt.Sprintf("acquire %v", requestIDs))
	}
	if answer != nil {
		err = answer(ctx)
	}
	if err != nil {
		return nil, err
	}
	return slices.Clone(requestIDs), nil
}

// calls lists the request IDs of every AcquireJobs call so far.
func (f *fakeAcquirer) calls() [][]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ids)
}

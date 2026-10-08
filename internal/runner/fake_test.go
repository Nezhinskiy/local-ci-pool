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
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/actions/scaleset"
	"github.com/moby/moby/api/types/events"
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
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{stdin: map[string]*bytes.Buffer{}, stdinDone: map[string]chan struct{}{}}
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
	err     error
	onStart func(name string)
	// hangs makes a start wait for its context, like a stuck daemon.
	hangs bool
}

func (s *fakeStarter) start(ctx context.Context, name, jit string) error {
	s.mu.Lock()
	hook := s.onStart
	if s.jits == nil {
		s.jits = map[string]string{}
	}
	s.names = append(s.names, name)
	s.jits[name] = jit
	err := s.err
	hangs := s.hangs
	s.mu.Unlock()
	if hook != nil {
		hook(name)
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

func newHarness(slots *Slots) *harness {
	h := &harness{docker: newFakeDocker(), jit: &fakeJIT{}, starter: &fakeStarter{}, logs: &logBuffer{}}
	h.registry = &fakeRegistry{docker: h.docker}
	h.scaler = NewScaler(ScalerConfig{
		ScaleSetID: 7,
		Slots:      slots,
		Start:      h.starter.start,
		JIT:        h.jit,
		Registry:   h.registry,
		Docker:     h.docker,
		Log:        h.logs.logger(),
	})
	return h
}

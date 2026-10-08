package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/Nezhinskiy/local-ci-pool/internal/discovery"
	"github.com/Nezhinskiy/local-ci-pool/internal/fakeactions"
	"github.com/Nezhinskiy/local-ci-pool/internal/github"
	"github.com/Nezhinskiy/local-ci-pool/internal/machine"
	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
	"github.com/Nezhinskiy/local-ci-pool/internal/runnermount"
)

const (
	owner       = "examplemac-0123abcd"
	probeInst   = "probe-" + owner
	testDigest  = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	alphaSet    = "alpha-examplemac"
	betaSet     = "beta-examplemac"
	alphaRepo   = "example/alpha"
	betaRepo    = "example/beta"
	defaultWait = 5 * time.Second
)

var testFP = machine.Fingerprint{ID: "docker-vm-1", Mem: 8 << 30, CPUs: 8}

// fastClock runs factor times faster than the wall clock, so the pool's
// minute-scale windows pass in milliseconds without any test sleeping for
// them.
type fastClock struct {
	start  time.Time
	factor time.Duration
}

func (c fastClock) Now() time.Time { return c.start.Add(time.Since(c.start) * c.factor) }
func (c fastClock) After(d time.Duration) <-chan time.Time {
	return time.After(d / c.factor)
}

// ---- fake Docker ----

type fakeContainer struct {
	labels map[string]string
	image  string
	mount  string // the runner mount's source
	state  container.ContainerState
}

// fakeDocker keeps containers by name (the ID is the name) and broadcasts die
// events. Methods the supervisor must not call through it (image builds,
// pulls) are left to the nil embedded interface and panic.
type fakeDocker struct {
	Docker

	mu         sync.Mutex
	containers map[string]*fakeContainer
	removed    []string
	subs       []chan events.Message
	subErrs    []chan error
	info       system.Info
	infoErr    error
	// createGate, when set, holds every ContainerCreate until it is closed;
	// with createFirst the container is already listed while it waits.
	createGate  chan struct{}
	createFirst bool
	listHook    func() // runs once, inside the next ContainerList
	createErr   error  // every ContainerCreate fails with it
	// removeHang makes every ContainerRemove wait for its context to end.
	removeHang bool
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		containers: map[string]*fakeContainer{},
		info:       system.Info{ID: testFP.ID, MemTotal: testFP.Mem, NCPU: testFP.CPUs, Architecture: "aarch64"},
	}
}

func (f *fakeDocker) ContainerCreate(ctx context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	c := &fakeContainer{labels: o.Config.Labels, image: o.Config.Image, state: container.StateCreated}
	if len(o.HostConfig.Mounts) == 1 {
		c.mount = o.HostConfig.Mounts[0].Source
	}
	f.mu.Lock()
	gate, first, cerr := f.createGate, f.createFirst, f.createErr
	if gate != nil && first {
		f.containers[o.Name] = c
	}
	f.mu.Unlock()
	if cerr != nil {
		return client.ContainerCreateResult{}, cerr
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return client.ContainerCreateResult{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if gate == nil || !first {
		f.containers[o.Name] = c
	}
	return client.ContainerCreateResult{ID: o.Name}, nil
}

func (f *fakeDocker) ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	near, far := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, far) }()
	return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(near, "")}, nil
}

func (f *fakeDocker) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[id]; c != nil {
		c.state = container.StateRunning
	}
	return client.ContainerStartResult{}, nil
}

func (f *fakeDocker) ContainerRemove(ctx context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	hang := f.removeHang
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return client.ContainerRemoveResult{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	c := f.containers[id]
	if c == nil {
		return client.ContainerRemoveResult{}, fmt.Errorf("no such container %s: %w", id, cerrdefs.ErrNotFound)
	}
	delete(f.containers, id)
	if c.state == container.StateRunning {
		f.dieLocked(id, c)
	}
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeDocker) Events(context.Context, client.EventsListOptions) client.EventsResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan events.Message, 64)
	errs := make(chan error, 1)
	f.subs = append(f.subs, ch)
	f.subErrs = append(f.subErrs, errs)
	return client.EventsResult{Messages: ch, Err: errs}
}

// breakEvents ends every events stream with an error, as a Docker restart
// does; the watcher subscribes again.
func (f *fakeDocker) breakEvents() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, errs := range f.subErrs {
		select {
		case errs <- errors.New("unexpected EOF"):
		default:
		}
	}
	f.subs, f.subErrs = nil, nil
}

func (f *fakeDocker) set(mut func(f *fakeDocker)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mut(f)
}

func (f *fakeDocker) ContainerList(_ context.Context, o client.ContainerListOptions) (client.ContainerListResult, error) {
	f.mu.Lock()
	hook := f.listHook
	f.listHook = nil
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []container.Summary
	for id, c := range f.containers {
		match := true
		for want := range o.Filters["label"] {
			k, v, _ := strings.Cut(want, "=")
			match = match && c.labels[k] == v
		}
		if match {
			out = append(out, container.Summary{ID: id, Names: []string{"/" + id}, Labels: c.labels, State: c.state, Image: c.image})
		}
	}
	return client.ContainerListResult{Items: out}, nil
}

func (f *fakeDocker) Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return client.SystemInfoResult{Info: f.info}, f.infoErr
}

func (f *fakeDocker) dieLocked(id string, c *fakeContainer) {
	attrs := map[string]string{"exitCode": "0"}
	for k, v := range c.labels {
		attrs[k] = v
	}
	m := events.Message{Type: events.ContainerEventType, Action: events.ActionDie, Actor: events.Actor{ID: id, Attributes: attrs}}
	for _, ch := range f.subs {
		select {
		case ch <- m:
		default:
		}
	}
}

// exit ends a container the way an AutoRemove container ends: a die event,
// and it is gone.
func (f *fakeDocker) exit(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[id]; c != nil {
		delete(f.containers, id)
		f.dieLocked(id, c)
	}
}

// vanish removes a container without a die event, as a Docker restart does.
func (f *fakeDocker) vanish(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, id)
}

func (f *fakeDocker) add(id string, labels map[string]string) {
	f.addIn(id, labels, container.StateRunning)
}

// addIn adds a container in the given state.
func (f *fakeDocker) addIn(id string, labels map[string]string, state container.ContainerState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[id] = &fakeContainer{labels: labels, state: state}
}

// dieAgain sends a die event for a container once more, as Docker's replay of
// recent events does after a resubscription.
func (f *fakeDocker) dieAgain(id string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dieLocked(id, &fakeContainer{labels: labels})
}

// runners lists the running containers started in the image, sorted.
func (f *fakeDocker) runners(image string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id, c := range f.containers {
		if c.image == image && c.state == container.StateRunning {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// created lists the containers in the image in any state, sorted.
func (f *fakeDocker) created(image string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id, c := range f.containers {
		if c.image == image {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func (f *fakeDocker) gateCreates(first bool) (release func()) {
	gate := make(chan struct{})
	f.mu.Lock()
	f.createGate, f.createFirst = gate, first
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.createGate = nil
			f.mu.Unlock()
			close(gate)
		})
	}
}

func (f *fakeDocker) onNextList(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listHook = hook
}

func (f *fakeDocker) labelsOf(id string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[id]; c != nil {
		return c.labels
	}
	return nil
}

func (f *fakeDocker) mountOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[id]; c != nil {
		return c.mount
	}
	return ""
}

func (f *fakeDocker) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containers[id] != nil
}

func (f *fakeDocker) removedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

func (f *fakeDocker) setInfo(mut func(*system.Info), err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if mut != nil {
		mut(&f.info)
	}
	f.infoErr = err
}

func (f *fakeDocker) exitAll() {
	f.mu.Lock()
	ids := make([]string, 0, len(f.containers))
	for id := range f.containers {
		ids = append(ids, id)
	}
	f.mu.Unlock()
	for _, id := range ids {
		f.exit(id)
	}
}

// ---- fake GitHub ----

type fakeRepo struct {
	private    bool
	marker     string
	readErr    error
	recheckErr error
	// movedTo is the full name GitHub answers for the repository, as after a
	// transfer or a rename; empty means its own.
	movedTo string
}

type fakeGitHub struct {
	mu      sync.Mutex
	repos   map[string]*fakeRepo
	listErr error
	lists   int
}

func (g *fakeGitHub) ListPrivateOwnedRepos(context.Context) ([]github.Repo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lists++
	if g.listErr != nil {
		return nil, g.listErr
	}
	var out []github.Repo
	for full, r := range g.repos {
		if r.private {
			out = append(out, repoOf(full, r))
		}
	}
	slices.SortFunc(out, func(a, b github.Repo) int { return strings.Compare(a.FullName, b.FullName) })
	return out, nil
}

func (g *fakeGitHub) Repo(_ context.Context, full string) (github.Repo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.repos[full]
	if r == nil {
		return github.Repo{}, github.ErrNotFound
	}
	if r.recheckErr != nil {
		return github.Repo{}, r.recheckErr
	}
	return repoOf(full, r), nil
}

func (g *fakeGitHub) ReadFile(_ context.Context, full, _, _ string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.repos[full]
	switch {
	case r == nil:
		return nil, github.ErrNotFound
	case r.readErr != nil:
		return nil, r.readErr
	case r.marker == "":
		return nil, github.ErrNotFound
	}
	return []byte(r.marker), nil
}

func (g *fakeGitHub) LatestRunnerRelease(context.Context, string) (github.RunnerRelease, error) {
	return github.RunnerRelease{}, errors.New("not in this test")
}

func (g *fakeGitHub) Download(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not in this test")
}

func (g *fakeGitHub) edit(f func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f()
}

func (g *fakeGitHub) listings() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lists
}

func repoOf(full string, r *fakeRepo) github.Repo {
	if r.movedTo != "" {
		full = r.movedTo
	}
	_, name, _ := strings.Cut(full, "/")
	return github.Repo{FullName: full, Name: name, DefaultBranch: "main", Private: r.private}
}

func marker() string { return `{"image":"example.invalid/job@` + testDigest + `"}` }

// ---- fake token ----

type fakeToken struct {
	mu            sync.Mutex
	gen           int
	invalidations int
	err           error // every read fails with it
	// errAfter makes every read after an Invalidate fail with err.
	errAfter bool
	// failFirst makes the first reads fail with firstErr, then succeed.
	failFirst int
	firstErr  error
	reads     int
}

func (f *fakeToken) Token(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.failFirst > 0 {
		f.failFirst--
		return "", f.firstErr
	}
	if f.err != nil && (!f.errAfter || f.gen > 0) {
		return "", f.err
	}
	return fmt.Sprintf("tok-%d", f.gen+1), nil
}

func (f *fakeToken) Invalidate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gen++
	f.invalidations++
}

func (f *fakeToken) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *fakeToken) invalidated() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.invalidations
}

// ---- fake caffeinate ----

type fakeCaffeinate struct {
	mu      sync.Mutex
	starts  int
	stops   int
	running bool
}

func (c *fakeCaffeinate) start() (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts++
	c.running = true
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.stops++
		c.running = false
	}, nil
}

func (c *fakeCaffeinate) state() (starts, stops int, running bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts, c.stops, c.running
}

// ---- log ----

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---- harness ----

type harness struct {
	t       *testing.T
	actions *fakeactions.Actions
	docker  *fakeDocker
	gh      *fakeGitHub
	tok     *fakeToken
	caff    *fakeCaffeinate
	clock   fastClock
	cfg     Config
	slots   int
	logs    *syncBuffer

	mu       sync.Mutex
	images   map[string]string // identity -> image reference
	checkErr map[string]error  // image reference -> preflight failure
	// runnerErr: runner version -> preflight failure of every image with it.
	runnerErr map[string]error
	checked   []string
	pruned    [][]string // the versions each Prune kept
	detects   int
	runnerV   []string // the runner versions Mount returns, in turn
	mounts    int
	mountErr  error
	detectErr error

	sup    *Supervisor
	cancel context.CancelFunc
	done   chan error
	result *error
}

// newHarness serves the named private repositories (owner "example") with a
// valid marker each. The clock runs factor times faster than the wall clock.
func newHarness(t *testing.T, slots int, factor time.Duration, repos ...string) *harness {
	t.Helper()
	h := &harness{
		t:         t,
		actions:   fakeactions.New(t),
		docker:    newFakeDocker(),
		gh:        &fakeGitHub{repos: map[string]*fakeRepo{}},
		tok:       &fakeToken{},
		caff:      &fakeCaffeinate{},
		clock:     fastClock{start: time.Now(), factor: factor},
		slots:     slots,
		logs:      &syncBuffer{},
		images:    map[string]string{},
		checkErr:  map[string]error{},
		runnerErr: map[string]error{},
	}
	for _, r := range repos {
		h.gh.repos["example/"+r] = &fakeRepo{private: true, marker: marker()}
	}
	h.cfg = Config{
		HealthAddr:    "127.0.0.1:0",
		DiscoverEvery: 50 * time.Second,
		Drain:         40 * time.Minute,
		Version:       "v0.0.0-test",
		Commit:        "abc1234",
	}
	t.Cleanup(h.stop)
	return h
}

func (h *harness) imageOf(identity string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ref := h.images[identity]; ref != "" {
		return ref
	}
	return "example.invalid/" + identity + "@" + testDigest
}

func (h *harness) setImage(identity, ref string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.images[identity] = ref
}

func (h *harness) setRunnerErr(version string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		delete(h.runnerErr, version)
		return
	}
	h.runnerErr[version] = err
}

// lastPrune is the versions the last Prune kept, joined by commas.
func (h *harness) lastPrune() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pruned) == 0 {
		return ""
	}
	return strings.Join(h.pruned[len(h.pruned)-1], ",")
}

func (h *harness) setCheckErr(ref string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		delete(h.checkErr, ref)
		return
	}
	h.checkErr[ref] = err
}

func (h *harness) deps() Deps {
	return Deps{
		Docker:  h.docker,
		GitHub:  h.gh,
		Token:   h.tok,
		Clients: NewClientFactory(h.actions.URL, scaleset.WithRetryMax(0)),
		Clock:   h.clock,
		Log:     slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Detect: func(context.Context) (machine.Machine, string, error) {
			h.mu.Lock()
			h.detects++
			err := h.detectErr
			h.mu.Unlock()
			if err != nil {
				return machine.Machine{}, "", err
			}
			return machine.Machine{Name: "examplemac", Owner: owner, Slots: h.slots, FP: testFP}, "arm64", nil
		},
		Mount: func(context.Context, string) (runnermount.Mount, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.mountErr != nil {
				return runnermount.Mount{}, h.mountErr
			}
			v := "2.338.0"
			if len(h.runnerV) > 0 {
				v = h.runnerV[min(h.mounts, len(h.runnerV)-1)]
			}
			h.mounts++
			return runnermount.Mount{Version: v, Spec: mount.Mount{
				Type: mount.TypeImage, Source: runnermount.Ref(v), Target: runnermount.Target, ReadOnly: true,
			}}, nil
		},
		Ensure: func(_ context.Context, p discovery.Project) (string, error) {
			return h.imageOf(p.Identity), nil
		},
		Check: func(_ context.Context, ref string, m runnermount.Mount) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.checked = append(h.checked, ref)
			if err := h.checkErr[ref]; err != nil {
				return "", err
			}
			if err := h.runnerErr[m.Version]; err != nil {
				return "", err
			}
			return "1001", nil
		},
		Prune: func(_ context.Context, keep []string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.pruned = append(h.pruned, slices.Sorted(slices.Values(keep)))
			return nil
		},
		Caffeinate: h.caff.start,
	}
}

func (h *harness) start() {
	h.t.Helper()
	h.sup = New(h.cfg, h.deps())
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- h.sup.Run(ctx) }()
}

// wait returns Run's result, failing the test if it does not return in time.
func (h *harness) wait(d time.Duration) error {
	h.t.Helper()
	select {
	case err := <-h.done:
		h.result = &err
		return err
	case <-time.After(d):
		h.t.Fatalf("Run did not return within %s", d)
		return nil
	}
}

func (h *harness) running() bool {
	if h.result != nil {
		return false
	}
	select {
	case err := <-h.done:
		h.result = &err
		return false
	default:
		return true
	}
}

// stop ends the run: every container exits, every runner is idle on the
// service, and the pool drains.
func (h *harness) stop() {
	if h.sup == nil || h.result != nil {
		h.dumpOnFailure()
		return
	}
	h.docker.exitAll()
	h.actions.ClearBusy()
	h.cancel()
	select {
	case err := <-h.done:
		h.result = &err
	case <-time.After(20 * time.Second):
		h.t.Errorf("Run did not return after cancel")
	}
	h.dumpOnFailure()
}

func (h *harness) dumpOnFailure() {
	if h.t.Failed() {
		h.t.Logf("pool log:\n%s", h.logs.String())
		h.t.Logf("actions log:\n%s", strings.Join(h.actions.Log(), "\n"))
	}
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(defaultWait)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) health(repo string) (ProjectHealth, bool) {
	for _, p := range h.sup.Snapshot().Projects {
		if p.Repo == repo {
			return p, true
		}
	}
	return ProjectHealth{}, false
}

func (h *harness) healthy(repo string) bool {
	p, ok := h.health(repo)
	return ok && p.Healthy
}

// delivered waits until every message queued for the scale set was taken by
// its listener.
func (h *harness) delivered(set string) {
	h.t.Helper()
	h.eventually("the "+set+" listener took its messages", func() bool { return h.actions.Pending(set) == 0 })
}

func (h *harness) logged(substr string) bool { return strings.Contains(h.logs.String(), substr) }

func labelNames(ss *scaleset.RunnerScaleSet) []string {
	var out []string
	for _, l := range ss.Labels {
		out = append(out, l.Name)
	}
	return out
}

func instanceLabels(instance, name string) map[string]string {
	return map[string]string{runner.LabelInstance: instance, runner.LabelRunner: name}
}

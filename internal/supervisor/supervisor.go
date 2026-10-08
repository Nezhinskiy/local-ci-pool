// Package supervisor runs the pool. It serves every discovered project with a
// scale set, a session and a listener, keeps the machine-wide slots, drains a
// project that leaves, and exits when Docker changes under it so launchd can
// start it again with the slots derived anew.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/moby/moby/client"

	"github.com/Nezhinskiy/local-ci-pool/internal/discovery"
	"github.com/Nezhinskiy/local-ci-pool/internal/github"
	"github.com/Nezhinskiy/local-ci-pool/internal/image"
	"github.com/Nezhinskiy/local-ci-pool/internal/machine"
	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
	"github.com/Nezhinskiy/local-ci-pool/internal/runnermount"
)

const (
	// nameLimit is the scale set name limit measured on github.com.
	nameLimit       = 64
	defaultInstance = "main"
	// runnerGroupID is the default runner group.
	runnerGroupID = 1

	defaultDiscoverEvery = 10 * time.Minute
	// A first discovery pass that fails is retried after firstDiscoverRetry,
	// doubling up to the discovery period, until one succeeds.
	firstDiscoverRetry = 30 * time.Second
	// DefaultDrain bounds how long a stopping pool waits for its running jobs.
	// It is not above every served job's timeout: a job still running at the
	// bound is stopped (its container removed) on an upgrade or an uninstall,
	// and needs a re-run. With afterBound it stays below the launchd
	// ExitTimeOut of 45 minutes.
	DefaultDrain = 40 * time.Minute
	// DefaultHealthAddr is where the health endpoint listens, and so the
	// address whose binding is the single-instance lock.
	DefaultHealthAddr = "127.0.0.1:8737"

	// A 409 on session create is retried for conflictWindow: a stale session
	// of this Mac cleared in 21–39 s when measured.
	conflictWindow    = 3 * time.Minute
	conflictFirstWait = 3 * time.Second
	conflictMaxWait   = 30 * time.Second

	restartMin = 5 * time.Second
	restartMax = 5 * time.Minute

	watchdogEvery   = 30 * time.Second
	maxPingFailures = 3
	releaseEvery    = time.Hour
	authWindow      = 10 * time.Minute
	// A token read that fails at start is retried for startRetryWindow.
	startRetryWindow = 2 * time.Minute
	startRetryFirst  = 2 * time.Second
	startRetryMax    = 30 * time.Second
	deleteRetry      = 10 * time.Second
	drainPoll        = time.Second
	inUsePoll        = time.Second
	callTimeout      = 30 * time.Second
	// noJobLimit runners in a row that go away without reporting a job mark
	// the project unhealthy: its image or the runner is broken.
	noJobLimit = 3
)

// afterBound caps, as a whole, the work a drain does once its runners are
// gone or its bound has passed: stopping an in-flight start, removing the
// runners left, closing the session and deleting the scale set. Every project
// drains at once, so the drain bound plus afterBound is the longest a stop
// takes. A variable so tests can shorten it.
var afterBound = 3 * time.Minute

// Config configures a Supervisor. Zero durations take their defaults.
type Config struct {
	// HealthAddr is bound first; binding it is the single-instance lock
	// (DefaultHealthAddr).
	HealthAddr string
	// DiscoverEvery is the discovery period (10 min).
	DiscoverEvery time.Duration
	// Drain bounds a drain, from its start to the last scale set delete
	// (DefaultDrain).
	Drain time.Duration
	// Probe serves OnlyRepo alone, under probe- names and the probe instance.
	Probe     bool
	OnlyRepo  string
	MarkerRef string
	// Instance labels this pool's containers ("main"). Probe mode always
	// uses "probe-<owner>".
	Instance string
	// Version and Commit are reported in the Snapshot.
	Version, Commit string
}

// Docker is the part of the Docker API the pool uses. *client.Client
// satisfies it.
type Docker interface {
	runner.Docker
	image.Docker
	runnermount.Docker
	Info(ctx context.Context, options client.InfoOptions) (client.SystemInfoResult, error)
}

// GitHub is the part of the GitHub client the pool uses. *github.Client
// satisfies it.
type GitHub interface {
	discovery.Source
	runnermount.ReleaseSource
}

// Session is an open message session of one scale set.
// *scaleset.MessageSessionClient satisfies it.
type Session interface {
	GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error)
	DeleteMessage(ctx context.Context, messageID int) error
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
	Session() scaleset.RunnerScaleSetSession
	Close(ctx context.Context) error
}

// ScaleSets is the scale set API of one repository.
type ScaleSets interface {
	runner.JITSource
	runner.Registry
	GetRunnerScaleSet(ctx context.Context, runnerGroupID int, name string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(ctx context.Context, ss *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateRunnerScaleSet(ctx context.Context, id int, ss *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	DeleteRunnerScaleSet(ctx context.Context, id int) error
	OpenSession(ctx context.Context, scaleSetID int, owner string) (Session, error)
}

// ClientFactory builds the scale set client of a repository (owner/name)
// with a token.
type ClientFactory func(repo, token string) (ScaleSets, error)

// NewClientFactory returns a ClientFactory over *scaleset.Client for the
// GitHub at baseURL ("" is https://github.com).
func NewClientFactory(baseURL string, opts ...scaleset.HTTPOption) ClientFactory {
	if baseURL == "" {
		baseURL = "https://github.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return func(repo, token string) (ScaleSets, error) {
		c, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     baseURL + "/" + repo,
			PersonalAccessToken: token,
			SystemInfo:          scaleset.SystemInfo{System: "local-ci-pool"},
		}, opts...)
		if err != nil {
			return nil, err
		}
		return scaleSetClient{c}, nil
	}
}

type scaleSetClient struct{ *scaleset.Client }

func (c scaleSetClient) OpenSession(ctx context.Context, id int, owner string) (Session, error) {
	s, err := c.MessageSessionClient(ctx, id, owner)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Clock is time as the supervisor sees it; tests run it faster.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Image is a project's job image and the user its jobs run as.
type Image struct{ Ref, User string }

// Deps are what the supervisor consumes. The function fields are seams with
// the real behaviour as their default.
type Deps struct {
	Docker  Docker
	GitHub  GitHub
	Token   github.TokenSource
	Clients ClientFactory
	Mirror  image.Mirror
	Clock   Clock
	Log     *slog.Logger

	// Detect derives the machine and the runner architecture
	// (machine.Detect over Docker's Info).
	Detect func(ctx context.Context) (machine.Machine, string, error)
	// Mount ensures the newest runner mount (runnermount.Ensure).
	Mount func(ctx context.Context, arch string) (runnermount.Mount, error)
	// Prune removes the runner mounts other than the versions kept
	// (runnermount.Prune).
	Prune func(ctx context.Context, keep []string) error
	// Ensure makes a project's image available (image.Ensure).
	Ensure func(ctx context.Context, p discovery.Project) (string, error)
	// Check preflights an image with the mount and returns its run user
	// (image.Preflight, image.RunUser).
	Check func(ctx context.Context, ref string, m runnermount.Mount) (string, error)
	// Caffeinate keeps the Mac awake until stop is called
	// (caffeinate -i -w <pid>).
	Caffeinate func() (stop func(), err error)
	// Serve, if set, serves the health endpoint on the bound listener until
	// ctx is cancelled.
	Serve func(ctx context.Context, ln net.Listener)
}

// Snapshot is the pool's state for the health endpoint and the heartbeat.
type Snapshot struct {
	Version  string          `json:"version"`
	Commit   string          `json:"commit"`
	Machine  string          `json:"machine"`
	Slots    int             `json:"slots"`
	InUse    int             `json:"in_use"`
	Busy     int             `json:"busy"`
	Docker   bool            `json:"docker"`
	Probe    bool            `json:"probe"`
	Projects []ProjectHealth `json:"projects"`
}

// ProjectHealth is one project's state. Healthy means its listener runs on an
// open session and its image passed the preflight.
type ProjectHealth struct {
	Repo     string `json:"repo"`
	Identity string `json:"identity"`
	Image    string `json:"image"`
	Reason   string `json:"reason"`
	Healthy  bool   `json:"healthy"`
}

// Supervisor runs the pool; see Run.
type Supervisor struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	fatal     chan error
	reconcile chan struct{}
	// wg counts the project goroutines: runs, drains and image refreshes.
	wg sync.WaitGroup

	mu       sync.Mutex
	life     context.Context // the projects' parent context, set by Run
	machine  machine.Machine
	instance string
	slots    *runner.Slots
	mount    runnermount.Mount
	dockerOK bool
	projects map[string]*project // by owner/name
	tokenGen int
	lastAuth time.Time
}

// New returns a Supervisor; Run starts it.
func New(cfg Config, deps Deps) *Supervisor {
	if cfg.DiscoverEvery <= 0 {
		cfg.DiscoverEvery = defaultDiscoverEvery
	}
	if cfg.Drain <= 0 {
		cfg.Drain = DefaultDrain
	}
	if cfg.HealthAddr == "" {
		cfg.HealthAddr = DefaultHealthAddr
	}
	if cfg.Instance == "" {
		cfg.Instance = defaultInstance
	}
	if deps.Clock == nil {
		deps.Clock = realClock{}
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	s := &Supervisor{
		cfg:       cfg,
		deps:      deps,
		log:       deps.Log,
		fatal:     make(chan error, 1),
		reconcile: make(chan struct{}, 1),
		projects:  map[string]*project{},
	}
	if s.deps.Detect == nil {
		s.deps.Detect = s.detect
	}
	if s.deps.Mount == nil {
		s.deps.Mount = func(ctx context.Context, arch string) (runnermount.Mount, error) {
			return runnermount.Ensure(ctx, s.deps.Docker, s.deps.GitHub, arch)
		}
	}
	if s.deps.Prune == nil {
		s.deps.Prune = func(ctx context.Context, keep []string) error {
			return runnermount.Prune(ctx, s.deps.Docker, keep...)
		}
	}
	if s.deps.Ensure == nil {
		s.deps.Ensure = func(ctx context.Context, p discovery.Project) (string, error) {
			return image.Ensure(ctx, s.deps.Docker, s.deps.Mirror, p)
		}
	}
	if s.deps.Check == nil {
		s.deps.Check = s.checkImage
	}
	if s.deps.Caffeinate == nil {
		s.deps.Caffeinate = caffeinate
	}
	return s
}

// Run serves until ctx is cancelled, then drains every project and returns
// nil. It returns an error matching ErrTerminal for a cause that will not fix
// itself, and any other error when the pool must be restarted.
func (s *Supervisor) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.HealthAddr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return terminal("already running: %s is in use", s.cfg.HealthAddr)
		}
		// A loopback address that cannot be bound for another reason is a
		// configuration error.
		return terminal("binding the health address %s: %v", s.cfg.HealthAddr, err)
	}
	defer func() { _ = ln.Close() }()

	// The token cache is empty at start, so this reads it from gh. Right
	// after boot gh may not produce one yet (the login keychain is still
	// locked), so a failed read is retried for startRetryWindow.
	err = s.retryAtStart(ctx, func() error {
		_, err := s.deps.Token.Token(ctx)
		return err
	})
	switch {
	case ctx.Err() != nil:
		return nil
	case err != nil:
		return terminal("gh logged out: %v; run gh auth login", err)
	}
	m, arch, err := s.deps.Detect(ctx)
	if errors.Is(err, machine.ErrHostName) {
		return terminal("%v: set a LocalHostName with letters or digits", err)
	}
	if err != nil {
		return fmt.Errorf("detecting the machine: %w", err)
	}
	if m.Slots < 1 {
		return terminal("Docker gives the pool %.1f GB of memory and %d CPUs, too little for one runner slot: one slot needs 6 GB of memory and 2 CPUs",
			float64(m.FP.Mem)/(1<<30), m.FP.CPUs)
	}
	if s.cfg.Probe && s.cfg.OnlyRepo == "" {
		return terminal("probe mode needs the repository to serve")
	}
	instance := s.cfg.Instance
	if s.cfg.Probe {
		instance = "probe-" + m.Owner
	}
	var mount runnermount.Mount
	err = s.retryAtStart(ctx, func() error {
		var err error
		mount, err = s.deps.Mount(ctx, arch)
		return err
	})
	if ctx.Err() != nil {
		return nil
	}
	if t := loginRejected(err); t != nil {
		return t
	}
	if err != nil {
		return fmt.Errorf("preparing the runner mount: %w", err)
	}
	s.mu.Lock()
	s.machine, s.instance, s.mount = m, instance, mount
	s.slots = runner.NewSlots(m.Slots)
	s.dockerOK = true
	s.mu.Unlock()
	s.log.Info("pool starting", "machine", m.Name, "slots", m.Slots, "instance", instance, "runner", mount.Version, "probe", s.cfg.Probe)
	s.prune(ctx)
	if err := s.sweep(ctx); err != nil {
		return err
	}

	// The projects live on until they are drained, so a SIGTERM (ctx) does
	// not cancel them; life ends after the drain.
	life, stopLife := context.WithCancel(context.WithoutCancel(ctx))
	defer stopLife()
	s.mu.Lock()
	s.life = life
	s.mu.Unlock()
	var bg sync.WaitGroup
	start := func(f func()) {
		bg.Add(1)
		go func() {
			defer bg.Done()
			f()
		}()
	}
	if s.deps.Serve != nil {
		start(func() { s.deps.Serve(life, ln) })
	}
	exits := subscribeHook{Docker: s.deps.Docker, onSubscribe: s.requestReconcile}
	start(func() { runner.WatchExits(life, exits, instance, s.exited) })
	start(func() { s.watchdog(life) })
	start(func() { s.releases(life, arch) })
	start(func() { s.caffeinate(life) })

	err = s.loop(ctx, life, stopLife)

	stopLife()
	s.wg.Wait()
	s.closeSessions()
	_ = ln.Close()
	bg.Wait()
	return err
}

// retryAtStart calls f until it returns an error that is not a failed token
// read, or until startRetryWindow has passed since the first call, waiting
// startRetryFirst doubling to startRetryMax between calls. A token rejection
// (ErrUnauthorized after the GitHub client's own re-read) is never retried:
// it returns at once. It returns f's last error, or nil once ctx ends.
func (s *Supervisor) retryAtStart(ctx context.Context, f func() error) error {
	clock := s.deps.Clock
	began := clock.Now()
	wait := startRetryFirst
	for {
		err := f()
		if !isTokenRead(err) {
			return err
		}
		left := startRetryWindow - clock.Now().Sub(began)
		if left <= 0 {
			return err
		}
		s.log.Warn("gh has no token yet; retrying", "error", err.Error(), "in", min(wait, left))
		select {
		case <-ctx.Done():
			return nil
		case <-clock.After(min(wait, left)):
		}
		wait = min(wait*2, startRetryMax)
	}
}

// loop runs discovery until ctx ends or a fatal error arrives. Until a pass
// has succeeded (a listing failed at start, say), the next pass comes after
// firstDiscoverRetry, doubling up to the discovery period.
func (s *Supervisor) loop(ctx, life context.Context, stopLife context.CancelFunc) error {
	served := s.discover(ctx, life)
	retry := min(firstDiscoverRetry, s.cfg.DiscoverEvery)
	for {
		next := s.cfg.DiscoverEvery
		if !served {
			next = retry
			retry = min(retry*2, s.cfg.DiscoverEvery)
		}
		select {
		case <-ctx.Done():
			return s.shutdown(life, stopLife)
		case err := <-s.fatal:
			return err
		case <-s.deps.Clock.After(next):
			if s.discover(ctx, life) {
				served = true
			}
		}
	}
}

// shutdown drains every project. A fatal error during the drain aborts it.
func (s *Supervisor) shutdown(life context.Context, stopLife context.CancelFunc) error {
	s.log.Info("stopping: draining every project")
	deadline := s.deps.Clock.Now().Add(s.cfg.Drain)
	s.mu.Lock()
	for _, pr := range s.projects {
		s.drainLocked(life, pr, deadline)
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.log.Info("drained")
		return nil
	case err := <-s.fatal:
		stopLife()
		<-done
		return err
	}
}

// FailOnLoginLoss ends Run with a terminal error if err means the GitHub login
// is gone (gh logged out, or a token GitHub refuses after it was read again),
// the same classification every other GitHub call of the pool gets. It reports
// whether it did. Callers outside the supervisor, such as the heartbeat, use it
// so that a lost login ends the process with exit 0 instead of being logged
// forever.
func (s *Supervisor) FailOnLoginLoss(err error) bool {
	t := loginRejected(err)
	if t == nil {
		return false
	}
	s.fail(t)
	return true
}

// fail reports a fatal error to Run; the first one wins.
func (s *Supervisor) fail(err error) {
	select {
	case s.fatal <- err:
	default:
	}
}

func (s *Supervisor) requestReconcile() {
	select {
	case s.reconcile <- struct{}{}:
	default:
	}
}

// discover runs one discovery pass and applies it: a new project starts, a
// project positively gone (absent, public, without a valid marker) drains, a
// project whose repository errored is left exactly as it is, and a failed
// listing changes nothing, unless it failed because the login is gone. It
// reports whether the listing succeeded.
func (s *Supervisor) discover(ctx, life context.Context) bool {
	res, err := discovery.Discover(ctx, s.deps.GitHub, discovery.Options{OnlyRepo: s.cfg.OnlyRepo, MarkerRef: s.cfg.MarkerRef}, s.log)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		if t := loginRejected(err); t != nil {
			s.fail(t)
			return false
		}
		s.log.Warn("discovery failed; keeping the current projects", "error", err.Error())
		return false
	}
	want := map[string]bool{}
	for _, p := range res.Projects {
		want[p.Repo] = true
	}
	errored := map[string]bool{}
	for _, r := range res.Errored {
		errored[r] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for repo, pr := range s.projects {
		if want[repo] || errored[repo] || pr.isDraining() {
			continue
		}
		pr.log.Info("the project left discovery; draining")
		s.drainLocked(life, pr, s.deps.Clock.Now().Add(s.cfg.Drain))
	}
	for _, p := range res.Projects {
		pr := s.projects[p.Repo]
		switch {
		case pr == nil || pr.isFailed():
			s.startLocked(life, p)
		case pr.isDraining():
			// It starts again on the first pass after its drain ends.
		default:
			s.goLocked(func() { pr.refresh(p) }, pr.claimRefresh())
		}
	}
	return true
}

func (s *Supervisor) startLocked(life context.Context, p discovery.Project) {
	if old := s.projects[p.Repo]; old != nil {
		old.cancel() // a failed project; its goroutine has returned
	}
	pr := newProject(s, life, p)
	s.projects[p.Repo] = pr
	pr.log.Info("serving the project")
	s.goLocked(pr.run, true)
}

func (s *Supervisor) drainLocked(life context.Context, pr *project, deadline time.Time) {
	if !pr.markDraining() {
		return
	}
	s.goLocked(func() {
		if pr.drain(life, deadline) {
			s.mu.Lock()
			if s.projects[pr.repo] == pr {
				delete(s.projects, pr.repo)
			}
			s.mu.Unlock()
		}
	}, true)
}

// requestDrain drains a project from its own goroutine's request, such as a
// repository found public before a session opens.
func (s *Supervisor) requestDrain(pr *project) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.projects[pr.repo] == pr && s.life != nil {
		s.drainLocked(s.life, pr, s.deps.Clock.Now().Add(s.cfg.Drain))
	}
}

// goLocked runs f as a project goroutine if ok.
func (s *Supervisor) goLocked(f func(), ok bool) {
	if !ok {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

// closeSessions closes every session still open, after the projects stopped
// without a drain. Scale sets are left for the next start.
func (s *Supervisor) closeSessions() {
	s.mu.Lock()
	prs := s.projectList()
	s.mu.Unlock()
	for _, pr := range prs {
		pr.closeSession(context.Background())
	}
}

func (s *Supervisor) projectList() []*project {
	prs := make([]*project, 0, len(s.projects))
	for _, pr := range s.projects {
		prs = append(prs, pr)
	}
	slices.SortFunc(prs, func(a, b *project) int { return strings.Compare(a.repo, b.repo) })
	return prs
}

// exited releases the slot of a runner whose container died. Runner names are
// unique, so only the scaler holding it releases.
func (s *Supervisor) exited(name string) {
	for _, sc := range s.scalers() {
		sc.Exited(name)
	}
}

func (s *Supervisor) scalers() []*runner.Scaler {
	s.mu.Lock()
	prs := s.projectList()
	s.mu.Unlock()
	var out []*runner.Scaler
	for _, pr := range prs {
		if sc := pr.getScaler(); sc != nil {
			out = append(out, sc)
		}
	}
	return out
}

// authFailure counts an authentication failure of a client built with token
// generation gen. The first drops the cached token so the next client reads
// it again; a second within authWindow is terminal. A failure of a token that
// was already replaced does not count.
func (s *Supervisor) authFailure(gen int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen < s.tokenGen {
		return nil
	}
	now := s.deps.Clock.Now()
	if !s.lastAuth.IsZero() && now.Sub(s.lastAuth) < authWindow {
		return terminal("gh login rejected: GitHub refused the token twice within %s; run gh auth login", authWindow)
	}
	s.lastAuth = now
	s.tokenGen++
	s.deps.Token.Invalidate()
	s.log.Warn("GitHub refused the token; reading it again")
	return nil
}

// newClient builds a repository's scale set client with the current token
// and returns the token generation it was built with.
func (s *Supervisor) newClient(ctx context.Context, repo string) (ScaleSets, int, error) {
	s.mu.Lock()
	gen := s.tokenGen
	s.mu.Unlock()
	tok, err := s.deps.Token.Token(ctx)
	if err != nil {
		// The token is cached, so a read happens only after a refusal
		// dropped it: gh no longer has a login.
		return nil, gen, terminal("gh logged out: %v; run gh auth login", err)
	}
	c, err := s.deps.Clients(repo, tok)
	if err != nil {
		return nil, gen, fmt.Errorf("building the scale set client: %w", err)
	}
	return c, gen, nil
}

func (s *Supervisor) currentMount() runnermount.Mount {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mount
}

// Snapshot returns the pool's current state.
func (s *Supervisor) Snapshot() Snapshot {
	s.mu.Lock()
	snap := Snapshot{
		Version: s.cfg.Version,
		Commit:  s.cfg.Commit,
		Machine: s.machine.Name,
		Docker:  s.dockerOK,
		Probe:   s.cfg.Probe,
	}
	slots := s.slots
	prs := s.projectList()
	s.mu.Unlock()
	if slots != nil {
		snap.Slots, snap.InUse, snap.Busy = slots.Cap(), slots.InUse(), slots.Busy()
	}
	for _, pr := range prs {
		snap.Projects = append(snap.Projects, pr.health())
	}
	return snap
}

// sweep removes the containers a previous run of this instance left, and
// only those.
func (s *Supervisor) sweep(ctx context.Context) error {
	list, err := s.listContainers(ctx)
	if err != nil {
		return fmt.Errorf("sweeping orphaned runner containers: %w", err)
	}
	for _, c := range list {
		if c.Labels[runner.LabelInstance] != s.instance {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, callTimeout)
		err := runner.RemoveContainer(rctx, s.deps.Docker, c.ID)
		cancel()
		if err != nil {
			s.log.Warn("removing an orphaned runner container", "container", c.ID, "error", err.Error())
			continue
		}
		s.log.Info("removed an orphaned runner container", "runner", c.Labels[runner.LabelRunner])
	}
	return nil
}

// caffeinate keeps the Mac awake while any slot is in use. JobStarted reaches
// the listener late, so slots in use, not busy runners, are the signal.
func (s *Supervisor) caffeinate(ctx context.Context) {
	var stop func()
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	for {
		inUse := s.slots.InUse()
		switch {
		case inUse > 0 && stop == nil:
			st, err := s.deps.Caffeinate()
			if err != nil {
				s.log.Warn("starting caffeinate", "error", err.Error())
			} else {
				stop = st
			}
		case inUse == 0 && stop != nil:
			stop()
			stop = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-s.deps.Clock.After(inUsePoll):
		}
	}
}

func caffeinate() (func(), error) {
	cmd := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}, nil
}

// detect is the default Deps.Detect.
func (s *Supervisor) detect(ctx context.Context) (machine.Machine, string, error) {
	var arch string
	m, err := machine.Detect(ctx, func(ctx context.Context) (machine.Fingerprint, error) {
		info, err := s.info(ctx)
		if err != nil {
			return machine.Fingerprint{}, err
		}
		arch = info.Architecture
		return fingerprintOf(info), nil
	})
	if err != nil {
		return machine.Machine{}, "", err
	}
	runnerArch, err := machine.RunnerArch(arch)
	if err != nil {
		return machine.Machine{}, "", terminal("%v", err)
	}
	return m, runnerArch, nil
}

// prune removes the runner mounts no project uses: it keeps the newest and
// every older one a project still runs on.
func (s *Supervisor) prune(ctx context.Context) {
	s.mu.Lock()
	keep := []string{s.mount.Version}
	prs := s.projectList()
	s.mu.Unlock()
	for _, pr := range prs {
		if v := pr.mountVersion(); v != "" && !slices.Contains(keep, v) {
			keep = append(keep, v)
		}
	}
	if err := s.deps.Prune(ctx, keep); err != nil {
		s.log.Warn("pruning old runner mounts", "error", err.Error())
	}
}

// checkImage is the default Deps.Check.
func (s *Supervisor) checkImage(ctx context.Context, ref string, m runnermount.Mount) (string, error) {
	if err := image.Preflight(ctx, s.deps.Docker, ref, m); err != nil {
		return "", fmt.Errorf("preflight failed: %w", err)
	}
	return image.RunUser(ctx, s.deps.Docker, ref)
}

// subscribeHook asks for a reconcile whenever WatchExits subscribes: die
// events lost while the stream was down (a Docker restart) are then found by
// comparing the held runners with the live containers.
type subscribeHook struct {
	runner.Docker
	onSubscribe func()
}

func (h subscribeHook) Events(ctx context.Context, o client.EventsListOptions) client.EventsResult {
	res := h.Docker.Events(ctx, o)
	h.onSubscribe()
	return res
}

var _ Docker = (*client.Client)(nil)
var _ GitHub = (*github.Client)(nil)
var _ Session = (*scaleset.MessageSessionClient)(nil)
var _ ScaleSets = scaleSetClient{}

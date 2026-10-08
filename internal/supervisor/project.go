package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/Nezhinskiy/local-ci-pool/internal/discovery"
	"github.com/Nezhinskiy/local-ci-pool/internal/github"
	"github.com/Nezhinskiy/local-ci-pool/internal/machine"
	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
)

// project serves one repository: its image, its scale set, one session at a
// time and the listener on it, restarted in place when it dies.
type project struct {
	s        *Supervisor
	repo     string
	identity string
	name     string   // the scale set
	labels   []string // the scale set's labels
	log      *slog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{} // closed when run returns

	mu         sync.Mutex
	p          discovery.Project
	img        Image
	imgErr     string // the last image refresh failed
	up         bool   // a listener runs on an open session
	reason     string // why the project is not up
	failed     bool   // setup failed; the next discovery pass starts it again
	draining   bool
	refreshing bool
	client     ScaleSets
	gen        int // the token generation client was built with
	scaleSetID int
	scaler     *runner.Scaler
	listener   *listener.Listener
	// session is open, and this process opened it. Only a scale set this
	// process holds a session on is ever deleted: the service deletes a
	// scale set even while another process holds its session.
	session Session
	// opened: this process opened a session on the scale set at least once;
	// refused: a session create was answered 409 at least once. A drain that
	// finds no open session reopens one to delete the scale set only when
	// opened and never refused.
	opened, refused bool
}

// errLeft stops a project's run when it asked for its own drain.
var errLeft = errors.New("the repository is no longer served")

func newProject(s *Supervisor, life context.Context, p discovery.Project) *project {
	prefix := ""
	if s.cfg.Probe {
		prefix = "probe-"
	}
	name := machine.ScaleSetName(prefix+p.Identity, s.machine.Name, nameLimit)
	ctx, cancel := context.WithCancel(life)
	return &project{
		s:        s,
		repo:     p.Repo,
		identity: p.Identity,
		name:     name,
		labels:   []string{name, prefix + p.Identity + "-local"},
		log:      s.log.With("repo", p.Repo, "scaleSet", name),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		p:        p,
		reason:   "starting",
	}
}

// run sets the project up, then keeps a listener on an open session until
// the project is cancelled. A listener that returns marks the project
// unhealthy at once and is restarted with backoff from restartMin to
// restartMax.
func (pr *project) run() {
	defer close(pr.done)
	if err := pr.setup(); err != nil {
		if pr.ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrTerminal) {
			pr.s.fail(err)
			return
		}
		pr.log.Error("the project cannot be served", "error", err.Error())
		pr.mu.Lock()
		pr.failed, pr.reason = true, err.Error()
		pr.mu.Unlock()
		return
	}
	wait := restartMin
	for pr.ctx.Err() == nil {
		began := pr.s.deps.Clock.Now()
		err := pr.serveOnce()
		if errors.Is(err, errLeft) {
			<-pr.ctx.Done() // the drain cancels it
			return
		}
		if pr.ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrTerminal) {
			pr.s.fail(err)
			return
		}
		pr.setDown(err.Error())
		pr.log.Warn("the listener stopped; restarting it", "error", err.Error(), "in", wait)
		pr.closeSession()
		if pr.s.deps.Clock.Now().Sub(began) >= restartMax {
			wait = restartMin
		}
		if !pr.sleep(wait) {
			return
		}
		wait = min(wait*2, restartMax)
	}
}

// setup prepares the image, the client and the scale set, and creates the
// scaler.
func (pr *project) setup() error {
	s := pr.s
	pr.mu.Lock()
	p := pr.p
	pr.mu.Unlock()
	ref, err := s.deps.Ensure(pr.ctx, p)
	if err != nil {
		return fmt.Errorf("preparing the image: %w", err)
	}
	user, err := s.deps.Check(pr.ctx, ref, s.currentMount())
	if err != nil {
		return fmt.Errorf("image %s: %w", ref, err)
	}
	pr.mu.Lock()
	pr.img = Image{Ref: ref, User: user}
	pr.mu.Unlock()
	if err := pr.connect(); err != nil {
		return err
	}
	id, err := pr.ensureScaleSet()
	if err != nil {
		return err
	}
	sc := runner.NewScaler(runner.ScalerConfig{
		ScaleSetID: id,
		Slots:      s.slots,
		Start:      pr.start,
		JIT:        clientRef{pr},
		Registry:   clientRef{pr},
		Docker:     s.deps.Docker,
		Log:        pr.log,
	})
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.scaleSetID, pr.scaler = id, sc
	if pr.draining {
		sc.Drain()
	}
	return nil
}

// connect (re)builds the client with the current token.
func (pr *project) connect() error {
	c, gen, err := pr.s.newClient(pr.ctx, pr.repo)
	if err != nil {
		return err
	}
	pr.mu.Lock()
	pr.client, pr.gen = c, gen
	pr.mu.Unlock()
	return nil
}

func (pr *project) currentClient() (ScaleSets, int) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.client, pr.gen
}

// reauth handles an authentication failure: it is counted, and the client is
// rebuilt with a token read again.
func (pr *project) reauth(gen int) error {
	if err := pr.s.authFailure(gen); err != nil {
		return err
	}
	return pr.connect()
}

// ensureScaleSet gets the scale set by name or, when there is none, creates
// it with the shared label and runner updates disabled.
func (pr *project) ensureScaleSet() (int, error) {
	for {
		c, gen := pr.currentClient()
		ss, err := c.GetRunnerScaleSet(pr.ctx, runnerGroupID, pr.name)
		if err == nil && ss == nil {
			ss, err = c.CreateRunnerScaleSet(pr.ctx, &scaleset.RunnerScaleSet{
				Name:          pr.name,
				RunnerGroupID: runnerGroupID,
				Labels:        labelsOf(pr.labels),
				RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
			})
			if err == nil {
				pr.log.Info("scale set created")
			}
		}
		switch {
		case err == nil && ss != nil:
			return ss.ID, nil
		case err == nil:
			return 0, fmt.Errorf("scale set %s: the service returned none", pr.name)
		case isUnauthorized(err) && pr.ctx.Err() == nil:
			if err := pr.reauth(gen); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("getting or creating scale set %s: %w", pr.name, err)
		}
	}
}

func labelsOf(names []string) []scaleset.Label {
	out := make([]scaleset.Label, 0, len(names))
	for _, n := range names {
		out = append(out, scaleset.Label{Type: "System", Name: n})
	}
	return out
}

// openSession opens the scale set's session. A 409 is retried with backoff
// for conflictWindow, after which the scale set is held elsewhere and the
// error is terminal.
func (pr *project) openSession() (Session, error) {
	clock := pr.s.deps.Clock
	began := clock.Now()
	wait := conflictFirstWait
	recreated := false
	for {
		c, gen := pr.currentClient()
		sess, err := c.OpenSession(pr.ctx, pr.currentID(), pr.s.machine.Owner)
		if err == nil {
			pr.mu.Lock()
			pr.session, pr.opened = sess, true
			pr.mu.Unlock()
			return sess, nil
		}
		if pr.ctx.Err() != nil {
			return nil, pr.ctx.Err()
		}
		switch {
		case isUnauthorized(err):
			if err := pr.reauth(gen); err != nil {
				return nil, err
			}
		case isNotFound(err) && !recreated:
			// The scale set was deleted under the pool: get or create it
			// again, and point the scaler at it.
			recreated = true
			pr.log.Warn("the scale set is gone; getting or creating it again")
			id, err := pr.ensureScaleSet()
			if err != nil {
				return nil, err
			}
			pr.mu.Lock()
			pr.scaleSetID, pr.opened, pr.refused = id, false, false
			sc := pr.scaler
			pr.mu.Unlock()
			sc.SetScaleSetID(id)
		case isSessionConflict(err):
			pr.mu.Lock()
			pr.refused = true
			pr.mu.Unlock()
			waited := clock.Now().Sub(began)
			if waited >= conflictWindow {
				return nil, terminal("scale set %s has an active session elsewhere: another pool, or a Mac with the same name", pr.name)
			}
			pr.log.Info("the scale set has an active session; retrying", "waited", waited.Round(time.Second))
			if !pr.sleep(min(wait, conflictWindow-waited)) {
				return nil, pr.ctx.Err()
			}
			wait = min(wait*2, conflictMaxWait)
		default:
			return nil, err
		}
	}
}

// serveOnce opens a session and runs a listener on it until it returns.
func (pr *project) serveOnce() error {
	if err := pr.recheck(); err != nil {
		return err
	}
	sess, err := pr.openSession()
	if err != nil {
		return fmt.Errorf("opening the session: %w", err)
	}
	pr.mu.Lock()
	maxRunners := pr.s.slots.Cap()
	if pr.draining {
		maxRunners = 0
	}
	l, err := listener.New(sess, listener.Config{ScaleSetID: pr.scaleSetID, MaxRunners: maxRunners, Logger: pr.log.With("component", "listener")})
	if err == nil {
		pr.listener, pr.up, pr.reason = l, true, ""
	}
	sc := pr.scaler
	pr.mu.Unlock()
	if err != nil {
		return fmt.Errorf("creating the listener: %w", err)
	}
	pr.log.Info("listening")
	err = l.Run(pr.ctx, runner.NewAdapter(sc))
	pr.mu.Lock()
	pr.listener = nil
	pr.mu.Unlock()
	return fmt.Errorf("the listener stopped: %w", err)
}

// recheck asks GitHub again, before a session opens, whether the repository
// is still there and private. One that is not is drained; the run returns
// errLeft.
func (pr *project) recheck() error {
	r, err := pr.s.deps.GitHub.Repo(pr.ctx, pr.repo)
	switch {
	case errors.Is(err, github.ErrNotFound), err == nil && !r.Private:
		pr.log.Warn("the repository is gone or no longer private; draining")
		pr.setDown("the repository is gone or no longer private")
		pr.s.requestDrain(pr)
		return errLeft
	case err != nil:
		if t := loginRejected(err); t != nil {
			return t
		}
		return fmt.Errorf("re-checking the repository's visibility: %w", err)
	}
	return nil
}

func (pr *project) currentID() int {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.scaleSetID
}

// start is the scaler's Start: one runner container in the current image with
// the current runner mount.
func (pr *project) start(ctx context.Context, name, jit string) error {
	pr.mu.Lock()
	img := pr.img
	pr.mu.Unlock()
	_, err := runner.StartRunner(ctx, pr.s.deps.Docker, runner.ContainerSpec{
		Image:    img.Ref,
		User:     img.User,
		Name:     name,
		Instance: pr.s.instance,
		Mount:    pr.s.currentMount().Spec,
		JIT:      jit,
	})
	return err
}

// refresh ensures the discovered project's image again; a changed reference
// is preflighted and used by the next starts. A failure marks the project
// unhealthy and keeps the current image.
func (pr *project) refresh(p discovery.Project) {
	defer func() {
		pr.mu.Lock()
		pr.refreshing = false
		pr.mu.Unlock()
	}()
	ref, err := pr.s.deps.Ensure(pr.ctx, p)
	if err == nil {
		pr.mu.Lock()
		same := ref == pr.img.Ref
		pr.mu.Unlock()
		if !same {
			var user string
			if user, err = pr.s.deps.Check(pr.ctx, ref, pr.s.currentMount()); err == nil {
				pr.log.Info("the image changed; the next runners use it", "image", ref)
				pr.mu.Lock()
				pr.img = Image{Ref: ref, User: user}
				pr.mu.Unlock()
			} else {
				err = fmt.Errorf("image %s: %w", ref, err)
			}
		}
	}
	if pr.ctx.Err() != nil {
		return
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.p = p
	pr.imgErr = ""
	if err != nil {
		pr.log.Warn("refreshing the image", "error", err.Error())
		pr.imgErr = "refreshing the image: " + err.Error()
	}
}

// claimRefresh reports whether a refresh may start, and marks one running. A
// project still in its setup (no scaler yet) is preparing its image already.
func (pr *project) claimRefresh() bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.refreshing || pr.draining || pr.failed || pr.scaler == nil {
		return false
	}
	pr.refreshing = true
	return true
}

// drain stops the project for good: no new runners, then it waits until the
// scaler holds no runner or the deadline passes, stops the listener, closes
// the session and deletes the scale set, retrying a delete refused while a
// job still runs until the deadline. It reports false if life ended first,
// leaving the session to Run.
func (pr *project) drain(life context.Context, deadline time.Time) bool {
	clock := pr.s.deps.Clock
	pr.mu.Lock()
	l, sc := pr.listener, pr.scaler
	pr.mu.Unlock()
	if l != nil {
		l.SetMaxRunners(0)
	}
	if sc != nil {
		sc.Drain()
	}
	pr.log.Info("draining")
	for {
		sc := pr.getScaler()
		if sc == nil || len(sc.Held()) == 0 {
			break
		}
		if !clock.Now().Before(deadline) {
			pr.log.Warn("the drain bound passed with runners left; removing them", "runners", len(sc.Held()))
			break
		}
		select {
		case <-life.Done():
			return false
		case <-clock.After(drainPoll):
		}
	}
	pr.cancel()
	<-pr.done
	pr.removeLeft()
	held := pr.closeSession()
	if !held && pr.mayReopen() {
		held = pr.reopenToDelete()
	}
	if !held {
		pr.log.Info("no session held; leaving the scale set")
		return true
	}
	pr.deleteScaleSet(life, deadline)
	return true
}

// mayReopen reports whether this process opened a session on the scale set
// and was never refused one: then a session closed by a listener restart can
// be opened again to prove the scale set is still this process's to delete.
func (pr *project) mayReopen() bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.opened && !pr.refused && pr.client != nil
}

// reopenToDelete opens and closes one session, once, and reports whether it
// opened: only then may the scale set be deleted.
func (pr *project) reopenToDelete() bool {
	c, _ := pr.currentClient()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	sess, err := c.OpenSession(ctx, pr.currentID(), pr.s.machine.Owner)
	if err != nil {
		pr.log.Info("could not open a session to delete the scale set; leaving it", "error", err.Error())
		return false
	}
	if err := sess.Close(ctx); err != nil {
		pr.log.Warn("closing the session", "error", err.Error())
	}
	return true
}

// removeLeft removes the containers of runners still held after the drain
// bound and releases their slots.
func (pr *project) removeLeft() {
	sc := pr.getScaler()
	if sc == nil {
		return
	}
	for _, r := range sc.Held() {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		if err := runner.RemoveContainer(ctx, pr.s.deps.Docker, r.Name); err != nil {
			pr.log.Warn("removing a runner left after the drain", "runner", r.Name, "error", err.Error())
		}
		// Best effort: GitHub refuses it while it still counts the job as
		// running, and the scale set delete then leaves the set for the
		// next start.
		if err := pr.unregister(ctx, r.Name); err != nil {
			pr.log.Info("unregistering a runner left after the drain", "runner", r.Name, "error", err.Error())
		}
		cancel()
		sc.Exited(r.Name)
	}
}

func (pr *project) unregister(ctx context.Context, name string) error {
	if c, _ := pr.currentClient(); c == nil {
		return nil
	}
	ref, err := clientRef{pr}.GetRunnerByName(ctx, name)
	if err != nil || ref == nil {
		return err
	}
	return clientRef{pr}.RemoveRunner(ctx, int64(ref.ID))
}

func (pr *project) deleteScaleSet(life context.Context, deadline time.Time) {
	clock := pr.s.deps.Clock
	for {
		c, _ := pr.currentClient()
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		err := c.DeleteRunnerScaleSet(ctx, pr.currentID())
		cancel()
		if err == nil {
			pr.log.Info("scale set deleted")
			return
		}
		left := deadline.Sub(clock.Now())
		if !errors.Is(err, scaleset.JobStillRunningError) || left <= 0 {
			pr.log.Warn("leaving the scale set for the next start", "error", err.Error())
			return
		}
		select {
		case <-life.Done():
			return
		case <-clock.After(min(deleteRetry, left)):
		}
	}
}

// closeSession closes the open session, if any, and reports whether there
// was one.
func (pr *project) closeSession() bool {
	pr.mu.Lock()
	sess := pr.session
	pr.session = nil
	pr.mu.Unlock()
	if sess == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if err := sess.Close(ctx); err != nil {
		pr.log.Warn("closing the session", "error", err.Error())
	}
	return true
}

func (pr *project) sleep(d time.Duration) bool {
	select {
	case <-pr.ctx.Done():
		return false
	case <-pr.s.deps.Clock.After(d):
		return true
	}
}

func (pr *project) setDown(reason string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.up, pr.reason = false, reason
}

func (pr *project) getScaler() *runner.Scaler {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.scaler
}

// markDraining reports whether the project was not draining yet, and marks it.
func (pr *project) markDraining() bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.draining {
		return false
	}
	pr.draining = true
	return true
}

func (pr *project) isDraining() bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.draining
}

func (pr *project) isFailed() bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.failed
}

func (pr *project) health() ProjectHealth {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	h := ProjectHealth{Repo: pr.repo, Identity: pr.identity, Image: pr.img.Ref}
	switch {
	case pr.draining:
		h.Reason = "draining"
	case pr.failed || !pr.up:
		h.Reason = pr.reason
	case pr.imgErr != "":
		h.Reason = pr.imgErr
	default:
		h.Healthy = true
	}
	return h
}

// clientRef is the scaler's JIT source and registry: the project's current
// client, which a re-authentication replaces.
type clientRef struct{ pr *project }

func (r clientRef) GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, id int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	c, _ := r.pr.currentClient()
	return c.GenerateJitRunnerConfig(ctx, setting, id)
}

func (r clientRef) GetRunnerByName(ctx context.Context, name string) (*scaleset.RunnerReference, error) {
	c, _ := r.pr.currentClient()
	return c.GetRunnerByName(ctx, name)
}

func (r clientRef) RemoveRunner(ctx context.Context, id int64) error {
	c, _ := r.pr.currentClient()
	return c.RemoveRunner(ctx, id)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"sort"

	"github.com/Nezhinskiy/local-ci-pool/internal/discovery"
	"github.com/Nezhinskiy/local-ci-pool/internal/heartbeat"
	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

// Exit codes. launchd restarts the pool on a non-zero status only
// (KeepAlive { SuccessfulExit = false }), so a cause that restarting cannot fix
// exits 0.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const usage = `usage: pool <command>

commands:
  run [--probe --only-repo OWNER/NAME [--marker-ref REF]] [--health-addr ADDR]
                 serve the private repositories that opted in
  forget         delete this Mac's heartbeat variable from every repository
  version        print the version and the commit
  print-defaults print the built-in defaults as JSON
`

// pool is the supervisor as run needs it. *supervisor.Supervisor satisfies it.
type pool interface {
	Run(ctx context.Context) error
	Snapshot() supervisor.Snapshot
	FailOnLoginLoss(err error) bool
}

// varWriter writes the heartbeat variables. *github.Client satisfies it.
type varWriter = heartbeat.VarWriter

// varDeleter removes a repository variable; a missing one counts as success.
// *github.Client satisfies it.
type varDeleter interface {
	DeleteVariable(ctx context.Context, repo, name string) error
}

// forgetter is what forget consumes: the repositories to look at, the way to
// delete a variable, and this Mac's machine name.
type forgetter struct {
	source  discovery.Source
	deleter varDeleter
	machine func(ctx context.Context) (string, error)
}

// services are the seams between the command line and the outside world; main
// passes the real ones and the tests pass fakes.
type services struct {
	newPool      func(cfg supervisor.Config, log *slog.Logger) (pool, varWriter, error)
	newForgetter func(log *slog.Logger) (forgetter, error)
}

// run executes one command line and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, svc services) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run":
		return cmdRun(ctx, rest, stderr, svc)
	case "forget":
		return cmdForget(ctx, rest, stdout, stderr, svc)
	case "version":
		if len(rest) != 0 {
			return usageError(stderr, "version takes no arguments")
		}
		ver, rev := buildIdentity()
		_, _ = fmt.Fprintf(stdout, "%s %s\n", ver, rev)
		return exitOK
	case "print-defaults":
		if len(rest) != 0 {
			return usageError(stderr, "print-defaults takes no arguments")
		}
		return cmdPrintDefaults(stdout, stderr)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return exitOK
	}
	return usageError(stderr, fmt.Sprintf("unknown command %q", cmd))
}

func usageError(stderr io.Writer, msg string) int {
	_, _ = fmt.Fprintf(stderr, "pool: %s\n\n%s", msg, usage)
	return exitUsage
}

// defaults are the built-in settings the installer's plist depends on: the
// launchd ExitTimeOut must exceed the drain.
type defaults struct {
	DrainSeconds int    `json:"drain_seconds"`
	HealthAddr   string `json:"health_addr"`
}

func cmdPrintDefaults(stdout, stderr io.Writer) int {
	b, err := json.Marshal(defaults{
		DrainSeconds: int(supervisor.DefaultDrain.Seconds()),
		HealthAddr:   supervisor.DefaultHealthAddr,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pool: %v\n", err)
		return exitError
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", b)
	return exitOK
}

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func cmdRun(ctx context.Context, args []string, stderr io.Writer, svc services) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	probe := fs.Bool("probe", false, "serve one repository under probe names")
	onlyRepo := fs.String("only-repo", "", "with --probe: the repository (OWNER/NAME) to serve")
	markerRef := fs.String("marker-ref", "", "with --probe: read the marker at this ref instead of the default branch")
	healthAddr := fs.String("health-addr", supervisor.DefaultHealthAddr, "health endpoint address; binding it is the single-instance lock")
	if err := fs.Parse(args); err != nil {
		return usageError(stderr, err.Error())
	}
	switch {
	case fs.NArg() != 0:
		return usageError(stderr, fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	case *probe && *onlyRepo == "":
		return usageError(stderr, "--probe needs --only-repo")
	case !*probe && (*onlyRepo != "" || *markerRef != ""):
		return usageError(stderr, "--only-repo and --marker-ref belong to --probe")
	}

	log := newLogger(stderr)
	ver, rev := buildIdentity()
	p, vars, err := svc.newPool(supervisor.Config{
		HealthAddr: *healthAddr,
		Probe:      *probe,
		OnlyRepo:   *onlyRepo,
		MarkerRef:  *markerRef,
		Version:    ver,
		Commit:     rev,
	}, log)
	if err != nil {
		log.Error("setting up the pool", "error", err.Error())
		return exitError
	}

	// The heartbeat lives and dies with the pool's signal context, not with
	// the drain: once the pool is told to stop it claims no new jobs, so it
	// stops advertising itself at once.
	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		heartbeat.Run(hbCtx, heartbeat.Config{
			Every:    heartbeat.Every,
			Snapshot: p.Snapshot,
			Vars:     vars,
			Fatal:    p.FailOnLoginLoss,
			Log:      log,
		})
	}()
	runErr := p.Run(ctx)
	stopHeartbeat()
	<-hbDone

	switch {
	case runErr == nil:
		log.Info("stopped")
		return exitOK
	case errors.Is(runErr, supervisor.ErrTerminal):
		log.Error("terminal: " + runErr.Error())
		return exitOK
	}
	log.Error("exiting so that launchd starts the pool again", "error", runErr.Error())
	return exitError
}

func cmdForget(ctx context.Context, args []string, stdout, stderr io.Writer, svc services) int {
	if len(args) != 0 {
		return usageError(stderr, "forget takes no arguments")
	}
	log := newLogger(stderr)
	f, err := svc.newForgetter(log)
	if err != nil {
		log.Error("setting up", "error", err.Error())
		return exitError
	}
	machine, err := f.machine(ctx)
	if err != nil {
		log.Error("reading the machine name", "error", err.Error())
		return exitError
	}
	name, err := heartbeat.VarName(machine)
	if err != nil {
		log.Error("no heartbeat variable for this machine", "error", err.Error())
		return exitError
	}
	res, err := discovery.Discover(ctx, f.source, discovery.Options{}, log)
	if err != nil {
		log.Error("discovering the repositories", "error", err.Error())
		return exitError
	}
	// A repository discovery could not judge just now may still hold the
	// variable; deleting what is already gone is not an error.
	seen := map[string]bool{}
	var repos []string
	for _, p := range res.Projects {
		if !seen[p.Repo] {
			seen[p.Repo] = true
			repos = append(repos, p.Repo)
		}
	}
	for _, r := range res.Errored {
		if !seen[r] {
			seen[r] = true
			repos = append(repos, r)
		}
	}
	sort.Strings(repos)
	failed := 0
	for _, repo := range repos {
		if err := f.deleter.DeleteVariable(ctx, repo, name); err != nil {
			log.Error("deleting the heartbeat variable", "repo", repo, "variable", name, "error", err.Error())
			failed++
			continue
		}
		_, _ = fmt.Fprintf(stdout, "forgot %s in %s\n", name, repo)
	}
	if failed > 0 {
		return exitError
	}
	_, _ = fmt.Fprintf(stdout, "forgot %s in %d repositories\n", name, len(repos))
	return exitOK
}

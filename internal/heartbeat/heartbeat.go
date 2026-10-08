// Package heartbeat tells GitHub that this Mac can take jobs. For every
// project that is healthy it keeps one repository variable,
// CI_POOL_HB_<MACHINE>, at "<unix epoch> <slots>"; the routing action in a
// workflow treats a fresh value as "a Mac is available". Nothing is written
// for a project that is not healthy, so its value ages out and the workflow
// falls back to hosted runners.
//
// The heartbeat runs inside the pool process, under the context of the
// process, so when the pool dies the heartbeats stop with it.
package heartbeat

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

// Prefix starts the name of every heartbeat variable.
const Prefix = "CI_POOL_HB_"

// Every is the heartbeat period.
const Every = 120 * time.Second

const (
	minSlots = 1 // the pool refuses to start below one slot
	maxSlots = 8 // the routing action accepts 1..8
	maxEpoch = 999_999_999_999
)

// nameRE and valueRE are the shapes the routing action (route/route.sh)
// accepts. The heartbeat refuses to write anything else, so a value the
// routing would skip never reaches GitHub.
var (
	nameRE  = regexp.MustCompile(`^` + Prefix + `[A-Z0-9]{1,12}$`)
	valueRE = regexp.MustCompile(`^[0-9]{1,12} [1-8]$`)
)

// VarName is the heartbeat variable of a machine: CI_POOL_HB_ and the machine
// name in upper case. It fails for a name the routing action would not accept.
func VarName(machine string) (string, error) {
	name := Prefix + strings.ToUpper(machine)
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("%q is not a machine name the routing accepts (1 to 12 of A-Z and 0-9)", machine)
	}
	return name, nil
}

// value is "<epoch> <slots>", with the slots clamped to 1..8. It reports
// false when the result is not one the routing accepts.
func value(epoch int64, slots int) (string, bool) {
	if epoch < 0 || epoch > maxEpoch || slots < minSlots {
		return "", false
	}
	v := fmt.Sprintf("%d %d", epoch, min(slots, maxSlots))
	return v, valueRE.MatchString(v)
}

// VarWriter sets a repository variable. *github.Client satisfies it.
type VarWriter interface {
	SetVariable(ctx context.Context, repo, name, value string) error
}

// Config configures Run.
type Config struct {
	// Every is the period (the package constant Every in production).
	Every time.Duration
	// Snapshot reports the pool's state (supervisor.Supervisor.Snapshot).
	Snapshot func() supervisor.Snapshot
	Vars     VarWriter
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Fatal, if set, is asked about every failed write while the context is
	// live. It returns true when err means the GitHub login is lost
	// (supervisor.Supervisor.FailOnLoginLoss), which ends the process; Run
	// then returns, because no later write can succeed.
	Fatal func(err error) bool
	// Log receives write failures; nil discards them.
	Log *slog.Logger
}

// Run beats once at once and then every cfg.Every, until ctx ends or Fatal
// reports a lost login. A beat writes the variable of this machine to every
// healthy project, and only while Docker answers and the pool is not in probe
// mode (a probe serves a repository under its own names and must not attract
// the real workflows).
func Run(ctx context.Context, cfg Config) {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	if beat(ctx, cfg, now, log) {
		return
	}
	t := time.NewTicker(cfg.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if beat(ctx, cfg, now, log) {
				return
			}
		}
	}
}

// beat writes one round of heartbeats. It reports whether Run must stop.
func beat(ctx context.Context, cfg Config, now func() time.Time, log *slog.Logger) bool {
	snap := cfg.Snapshot()
	if !snap.Docker || snap.Probe {
		return false
	}
	name, err := VarName(snap.Machine)
	if err != nil {
		log.Debug("no heartbeat", "reason", err.Error())
		return false
	}
	v, ok := value(now().Unix(), snap.Slots)
	if !ok {
		log.Debug("no heartbeat: nothing the routing accepts", "slots", snap.Slots)
		return false
	}
	for _, p := range snap.Projects {
		if !p.Healthy {
			continue
		}
		if ctx.Err() != nil {
			return false
		}
		err := cfg.Vars.SetVariable(ctx, p.Repo, name, v)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return false // the process is stopping; this is not a lost login
		}
		if cfg.Fatal != nil && cfg.Fatal(err) {
			log.Error("heartbeat stopped: the GitHub login is lost", "repo", p.Repo, "error", err.Error())
			return true
		}
		log.Warn("heartbeat write failed", "repo", p.Repo, "variable", name, "error", err.Error())
	}
	return false
}

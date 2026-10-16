//go:build unix

package budget

import (
	"cmp"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/core-slot/coreslot"
)

// errWedged is a call that failed open because the admission section is wedged.
var errWedged = errors.New("the admission section is wedged")

// maxFullHolds is how many holds a budget full line names.
const maxFullHolds = 3

// sectionSeen is the held admission section the monitor follows: its holder, as admit.lock's
// body records it, and when a snapshot first showed it (on Clock).
type sectionSeen struct {
	held    bool
	pid     int
	entered time.Time
	since   time.Time
}

// startMonitorLocked starts the monitor unless it runs. g.mu is held.
func (g *Gate) startMonitorLocked() {
	if !g.monitorOn {
		g.monitorOn = true
		go g.monitor()
	}
}

// monitor takes a snapshot every SnapshotEvery while an Admit is in progress or the gate is
// wedged, and stops at the first tick where neither holds. It takes its client from client, as
// Admit and Adopt do: a stale client is opened again when the backoff allows, and snapshots as it
// is otherwise. A tick is skipped only while no Open has ever succeeded.
func (g *Gate) monitor() {
	for {
		<-g.clock.After(g.snapshotEvery)
		g.mu.Lock()
		if len(g.admits) == 0 && !g.wedged {
			g.monitorOn = false
			g.mu.Unlock()
			return
		}
		g.mu.Unlock()
		cl, err := g.client()
		if err != nil {
			g.log.Debug("budget open failed", slog.String("error", err.Error()))
		}
		if cl == nil {
			continue
		}
		rep, err := cl.Snapshot()
		if err != nil {
			g.log.Debug("budget snapshot failed", slog.String("error", err.Error()))
			continue
		}
		g.mu.Lock()
		g.observeLocked(rep)
		g.mu.Unlock()
	}
}

// monitorRunning reports whether the monitor runs.
func (g *Gate) monitorRunning() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.monitorOn
}

// observeLocked applies a snapshot: it wedges the gate when the section has been held by the same
// (PID, Entered) for WedgeAfter, clears the wedge when the section is free or held by another,
// and reports a budget full for too long. g.mu is held.
func (g *Gate) observeLocked(rep coreslot.Report) {
	now := g.clock.Now()
	g.lastHolds = rep.Holds
	sec := rep.Section
	same := sec.Held && g.tracked.held && sec.PID == g.tracked.pid && sec.Entered.Equal(g.tracked.entered)
	if g.wedged && !same {
		g.wedged = false
		g.log.Info("budget unwedged", slog.Int("pid", g.tracked.pid))
	}
	switch {
	case !sec.Held:
		g.tracked = sectionSeen{}
	case !same:
		g.tracked = sectionSeen{held: true, pid: sec.PID, entered: sec.Entered, since: now}
	}
	if !g.wedged && g.tracked.held && now.Sub(g.tracked.since) >= g.wedgeAfter {
		g.wedged = true
		g.log.Warn("budget wedged", slog.Int("pid", g.tracked.pid))
		for ad := range g.admits {
			ad.cancel()
		}
	}
	g.reportFullLocked(now)
}

// reportFullLocked warns budget full once per LongWait while the oldest Admit in progress has
// waited LongWait or more and the gate is not wedged. g.mu is held.
func (g *Gate) reportFullLocked(now time.Time) {
	oldest := g.oldestWaitLocked(now)
	if oldest < g.longWait {
		g.lastFull = time.Time{}
		return
	}
	if g.wedged || (!g.lastFull.IsZero() && now.Sub(g.lastFull) < g.longWait) {
		return
	}
	g.lastFull = now
	g.log.Warn("budget full", slog.Int("oldest_wait_s", int(oldest/time.Second)),
		slog.String("holds", largestHolds(g.lastHolds)))
}

// largestHolds names up to three holds, largest cpu grant first: "<label> cpu=<n>, …".
func largestHolds(holds []coreslot.HoldInfo) string {
	hs := slices.Clone(holds)
	slices.SortStableFunc(hs, func(a, b coreslot.HoldInfo) int {
		return cmp.Compare(b.Granted["cpu"], a.Granted["cpu"])
	})
	parts := make([]string, 0, maxFullHolds)
	for _, h := range hs[:min(len(hs), maxFullHolds)] {
		parts = append(parts, h.Label+" cpu="+strconv.Itoa(h.Granted["cpu"]))
	}
	return strings.Join(parts, ", ")
}

// oldestWaitLocked is the time on Clock since the oldest Admit in progress began, 0 when none.
// g.mu is held.
func (g *Gate) oldestWaitLocked(now time.Time) time.Duration {
	var oldest time.Duration
	for ad := range g.admits {
		oldest = max(oldest, now.Sub(ad.start))
	}
	return oldest
}

// proceedWhenWedged decides whether a new Admit or Adopt goes on: at once when the gate is not
// wedged; while it is, only when one snapshot shows the wedge cleared.
func (g *Gate) proceedWhenWedged(cl client) bool {
	g.mu.Lock()
	wedged := g.wedged
	g.mu.Unlock()
	if !wedged {
		return true
	}
	rep, err := cl.Snapshot()
	if err != nil {
		g.log.Debug("budget snapshot failed", slog.String("error", err.Error()))
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.observeLocked(rep)
	return !g.wedged
}

//go:build unix

package budget

import (
	"cmp"
	"errors"
	"log/slog"
	"os"
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

// sectionSeen is the held admission section the monitor follows, and when a snapshot first showed
// it (on Clock). One holder is told by the tuple (gen, pid, entered). gen is written only inside
// the section, so it cannot change while one holder keeps it: a changed gen is traffic through the
// section. pid and entered come from admit.lock's body, which is informational only: a holder that
// does not rewrite it (a token-only client) leaves an older holder's, often the pool's own. They
// tell holders apart when the body is kept, and are logged as recorded, never as the holder.
type sectionSeen struct {
	held    bool
	gen     int64
	pid     int
	entered time.Time
	since   time.Time
}

// pidAttrs are the attributes a wedge line names the recorded holder with: the pid admit.lock's
// body records (0 when the body is unreadable), and whether it is this process's own.
func (t sectionSeen) pidAttrs() []any {
	return []any{slog.Int("recorded_pid", t.pid), slog.Bool("self", t.pid == os.Getpid())}
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
			g.snapshotFailed(err)
			continue
		}
		g.mu.Lock()
		g.observeLocked(rep)
		g.mu.Unlock()
	}
}

// snapshotFailed logs a failed snapshot. A *StateError marks the client stale, as a call that
// fails open on one does: the next call of client opens coreslot again. It reports whether err is
// a *StateError.
func (g *Gate) snapshotFailed(err error) bool {
	g.log.Debug("budget snapshot failed", slog.String("error", err.Error()))
	var state *coreslot.StateError
	if !errors.As(err, &state) {
		return false
	}
	g.mu.Lock()
	g.stale = true
	g.mu.Unlock()
	return true
}

// observeLocked applies a snapshot: it wedges the gate when every snapshot for WedgeAfter showed
// the section held with the same (Gen, PID, Entered), clears the wedge at the first that shows it
// free or that tuple changed, and reports a budget full for too long. g.mu is held.
func (g *Gate) observeLocked(rep coreslot.Report) {
	now := g.clock.Now()
	g.lastHolds = rep.Holds
	sec := rep.Section
	same := sec.Held && g.tracked.held && rep.Gen == g.tracked.gen && sec.PID == g.tracked.pid &&
		sec.Entered.Equal(g.tracked.entered)
	if g.wedged && !same {
		g.wedged = false
		g.log.Info("budget unwedged", g.tracked.pidAttrs()...)
	}
	switch {
	case !sec.Held:
		g.tracked = sectionSeen{}
	case !same:
		g.tracked = sectionSeen{held: true, gen: rep.Gen, pid: sec.PID, entered: sec.Entered, since: now}
	}
	if !g.wedged && g.tracked.held && now.Sub(g.tracked.since) >= g.wedgeAfter {
		g.wedged = true
		g.log.Warn("budget wedged", g.tracked.pidAttrs()...)
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
// wedged; while it is, only when one snapshot shows the wedge cleared. Otherwise it returns the
// error the call fails open with: the snapshot's *StateError, or errWedged.
func (g *Gate) proceedWhenWedged(cl client) error {
	g.mu.Lock()
	wedged := g.wedged
	g.mu.Unlock()
	if !wedged {
		return nil
	}
	rep, err := cl.Snapshot()
	if err != nil {
		if g.snapshotFailed(err) {
			return err
		}
		return errWedged
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.observeLocked(rep)
	if g.wedged {
		return errWedged
	}
	return nil
}

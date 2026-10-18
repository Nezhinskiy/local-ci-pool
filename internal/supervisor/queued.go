package supervisor

import (
	"context"
	"log/slog"
	"slices"
	"time"
)

// The queued-job watch reports, and only reports, a job GitHub keeps queued
// while the pool could take it: the job has the project's label, and for
// queuedAfter both it was queued and the pool had a free slot while the
// project had no runner waiting, starting or idle. GitHub then does not offer
// the job to the pool (seen after a revoked request was reissued and never
// counted again). Nothing is cancelled or re-run.
const (
	// queuedAfter is twice GitHub's 5-minute window for an unacquired
	// request.
	queuedAfter = 10 * time.Minute
	// queuedTick samples each project's conditions, which costs no request.
	queuedTick = time.Minute
	// queuedPoll spaces one project's checks on GitHub; a check makes at most
	// 2 + 10 requests (github.Client.QueuedJobs), and only while the
	// conditions have held for queuedAfter.
	queuedPoll = 5 * time.Minute
)

// StuckJob is a job reported by the queued-job watch.
type StuckJob struct {
	RunURL  string `json:"run_url"`
	Job     string `json:"job"`
	QueuedS int    `json:"queued_s"`
}

// watchQueued runs the queued-job watch until ctx ends.
func (s *Supervisor) watchQueued(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.deps.Clock.After(queuedTick):
		}
		s.mu.Lock()
		prs := s.projectList()
		s.mu.Unlock()
		for _, pr := range prs {
			s.checkQueued(ctx, pr)
		}
	}
}

// checkQueued samples pr's conditions and, when they have held for
// queuedAfter and its last check is queuedPoll old, asks GitHub for its queued
// jobs. A failed check is logged at debug and changes nothing.
func (s *Supervisor) checkQueued(ctx context.Context, pr *project) {
	clock := s.deps.Clock
	now := clock.Now()
	free := s.slots.InUse() < s.slots.Cap()
	pr.mu.Lock()
	sc, ready := pr.scaler, pr.up && !pr.draining && !pr.failed
	pr.mu.Unlock()
	if ready && sc != nil {
		waiting, _ := sc.BudgetCounts()
		ready = waiting == 0 && sc.Idle() == 0
	}
	pr.mu.Lock()
	if !free || !ready {
		pr.freeSince, pr.stuck, pr.warned = time.Time{}, nil, nil
		pr.mu.Unlock()
		return
	}
	if pr.freeSince.IsZero() {
		pr.freeSince = now
	}
	since := pr.freeSince
	due := now.Sub(since) >= queuedAfter && now.Sub(pr.lastQueuedCheck) >= queuedPoll
	if due {
		pr.lastQueuedCheck = now
	}
	pr.mu.Unlock()
	if !due {
		return
	}

	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	jobs, err := s.deps.GitHub.QueuedJobs(cctx, pr.repo)
	if err != nil {
		pr.log.Debug("checking the queued jobs", "error", err.Error())
		return
	}
	var stuck []StuckJob
	warned := map[int64]bool{}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.freeSince != since {
		return // the conditions broke during the check
	}
	for _, j := range jobs {
		if !slices.ContainsFunc(j.Labels, func(l string) bool { return slices.Contains(pr.labels, l) }) {
			continue
		}
		if now.Sub(later(j.Created, since)) < queuedAfter {
			continue
		}
		queued := int(now.Sub(j.Created) / time.Second)
		stuck = append(stuck, StuckJob{RunURL: j.RunURL, Job: j.Name, QueuedS: queued})
		warned[j.ID] = true
		if !pr.warned[j.ID] {
			pr.log.LogAttrs(ctx, slog.LevelWarn, "a job is queued while the pool has a free slot and no runner waits for it: GitHub is not offering it to the pool; re-run it if it stays",
				slog.String("run", j.RunURL), slog.String("job", j.Name), slog.Int("queued_s", queued))
		}
	}
	pr.stuck, pr.warned = stuck, warned
}

// later is the later of a and b.
func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

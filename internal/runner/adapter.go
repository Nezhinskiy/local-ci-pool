package runner

// The handler shape below (the listener.Scaler method Scale, acquiring first,
// and the fields it logs) is adapted from github.com/actions/scaleset v0.5.0,
// examples/dockerscaleset/scaler.go, Copyright GitHub, Inc., MIT License; see
// third_party/NOTICE. The statistics cache, the acquire policy and the runner
// bookkeeping are the pool's own (Scaler).

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// Adapter is the listener.Scaler of actions/scaleset v0.5.0 for a Scaler. It
// is the only type that implements that interface, so a change of the upstream
// interface touches this file alone.
type Adapter struct {
	s    *Scaler
	jobs JobAcquirer
	// count is the last assigned-jobs count a message carried, and seen
	// whether one did: an idle poll, or a message without statistics,
	// converges on it.
	count int
	seen  bool
	// desired is s.Desired; tests wrap it to see the count it receives.
	desired func(ctx context.Context, count int) (int, error)
}

// JobAcquirer acquires the job requests GitHub offers the scale set. The
// session (supervisor.Session, scaleset.MessageSessionClient) satisfies it.
type JobAcquirer interface {
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
}

// NewAdapter returns the listener.Scaler for s, which acquires the jobs it is
// offered through jobs. The supervisor builds one per listener run
// (project.serveOnce).
func NewAdapter(s *Scaler, jobs JobAcquirer) *Adapter {
	return &Adapter{s: s, jobs: jobs, desired: s.Desired}
}

// Scale handles one poll: a nil msg is an idle poll. It acquires every job
// offered, first, then marks the runners the message reports started and
// completed, and then brings the scale set to the assigned jobs: the
// message's statistics, or the last ones seen when it carries none.
//
// It returns an error only when ctx is done during the acquisition, before
// anything else in the message ran, so the listener stops without acking it.
// Any other acquire failure, a slow one whose error matches
// context.DeadlineExceeded under a live ctx included, is logged and the
// message handled: GitHub
// completes an assigned request that no runner acquired after 5 minutes and
// reissues it. Job events never fail it.
//
// A real message is handled without cancellation, so a shutdown cannot cut a
// start or a reap short (each is bounded in the Scaler); the initial message
// and idle polls run under ctx.
func (a *Adapter) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		if a.seen {
			_, _ = a.desired(ctx, a.count)
		}
		return nil
	}
	if err := a.acquire(ctx, msg.JobAvailableMessages); err != nil {
		return err
	}
	handle := ctx
	if msg.MessageID != listener.InitialMessageID {
		handle = context.WithoutCancel(ctx)
	}
	for _, j := range msg.JobStartedMessages {
		a.jobStarted(j)
	}
	for _, j := range msg.JobCompletedMessages {
		a.jobCompleted(j)
	}
	if msg.Statistics != nil {
		a.count, a.seen = msg.Statistics.TotalAssignedJobs, true
	}
	if a.seen {
		_, _ = a.desired(handle, a.count)
	}
	return nil
}

// acquire acquires the offered jobs. It returns an error only when ctx is
// done; any other failure is logged.
func (a *Adapter) acquire(ctx context.Context, offered []*scaleset.JobAvailable) error {
	ids := make([]int64, 0, len(offered))
	for _, j := range offered {
		if j != nil {
			ids = append(ids, j.RunnerRequestID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	a.s.log.Info("Acquiring jobs", slog.Int("count", len(ids)), slog.Any("runnerRequestIds", ids))
	acquired, err := a.jobs.AcquireJobs(ctx, ids)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("acquiring jobs: %w", err)
		}
		a.s.log.Warn("acquiring jobs failed; GitHub reissues an unacquired request after 5 minutes",
			slog.Int("count", len(ids)), slog.Any("runnerRequestIds", ids), slog.String("error", err.Error()))
		return nil
	}
	a.s.log.Info("Jobs acquired", slog.Int("count", len(acquired)), slog.Int("requested", len(ids)))
	return nil
}

// jobStarted marks the runner busy. A nil or unknown payload is logged.
func (a *Adapter) jobStarted(jobInfo *scaleset.JobStarted) {
	if jobInfo == nil {
		a.s.log.Info("job started message without a payload")
		return
	}
	a.s.log.Info("job started",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID),
		slog.String("jobId", jobInfo.JobID),
		slog.String("runner", jobInfo.RunnerName),
		slog.String("job", jobInfo.JobDisplayName),
	)
	a.s.Started(jobInfo.RunnerName)
}

// jobCompleted clears the runner's busy mark; its slot is released when its
// container exits. A nil or unknown payload is logged.
func (a *Adapter) jobCompleted(jobInfo *scaleset.JobCompleted) {
	if jobInfo == nil {
		a.s.log.Info("job completed message without a payload")
		return
	}
	a.s.log.Info("job completed",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID),
		slog.String("jobId", jobInfo.JobID),
		slog.String("runner", jobInfo.RunnerName),
		slog.String("result", jobInfo.Result),
	)
	a.s.Completed(jobInfo.RunnerName)
}

var _ listener.Scaler = (*Adapter)(nil)

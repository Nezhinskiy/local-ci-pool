package runner

// The handler shape below (the three listener.Scaler methods and the fields
// they log) is adapted from github.com/actions/scaleset v0.4.0,
// examples/dockerscaleset/scaler.go, Copyright GitHub, Inc., MIT License; see
// third_party/NOTICE. The runner bookkeeping is the pool's own (Scaler).

import (
	"context"
	"log/slog"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// Adapter is the listener.Scaler of actions/scaleset v0.4.0 for a Scaler. It
// is the only type that implements that interface, so a change of the upstream
// interface touches this file alone.
type Adapter struct {
	s *Scaler
}

// NewAdapter returns the listener.Scaler for s.
func NewAdapter(s *Scaler) *Adapter { return &Adapter{s: s} }

// HandleDesiredRunnerCount implements listener.Scaler.
func (a *Adapter) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	return a.s.Desired(ctx, count)
}

// HandleJobStarted implements listener.Scaler. It never fails the listener.
func (a *Adapter) HandleJobStarted(_ context.Context, jobInfo *scaleset.JobStarted) error {
	if jobInfo == nil {
		a.s.log.Info("job started message without a payload")
		return nil
	}
	a.s.log.Info("job started",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID),
		slog.String("jobId", jobInfo.JobID),
		slog.String("runner", jobInfo.RunnerName),
	)
	a.s.Started(jobInfo.RunnerName)
	return nil
}

// HandleJobCompleted implements listener.Scaler. It never fails the listener;
// the runner's slot is released when its container exits.
func (a *Adapter) HandleJobCompleted(_ context.Context, jobInfo *scaleset.JobCompleted) error {
	if jobInfo == nil {
		a.s.log.Info("job completed message without a payload")
		return nil
	}
	a.s.log.Info("job completed",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID),
		slog.String("jobId", jobInfo.JobID),
		slog.String("runner", jobInfo.RunnerName),
		slog.String("result", jobInfo.Result),
	)
	a.s.Completed(jobInfo.RunnerName)
	return nil
}

var _ listener.Scaler = (*Adapter)(nil)

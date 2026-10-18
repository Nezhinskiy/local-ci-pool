package supervisor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Nezhinskiy/local-ci-pool/internal/github"
	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
)

const stuckRun = "https://github.example/example/alpha/actions/runs/11"

// A job with the project's label queued past queuedAfter, while the pool has a
// free slot and the project nothing waiting for it, is warned about once and
// shown in the project's health with its run. A job with another label, or one
// queued for less, is not.
func TestQueuedJobWithFreeSlotsIsReported(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	long := h.clock.Now().Add(-time.Hour)
	h.gh.setQueued(alphaRepo,
		github.QueuedJob{RunURL: stuckRun, ID: 1, Name: "frontend", Labels: []string{"alpha-local"}, Created: long},
		github.QueuedJob{RunURL: stuckRun, ID: 2, Name: "lint", Labels: []string{"ubuntu-latest"}, Created: long},
		github.QueuedJob{RunURL: stuckRun, ID: 3, Name: "fresh", Labels: []string{"alpha-local"}, Created: h.clock.Now().Add(time.Hour)},
	)
	h.start()
	h.eventually("the stuck job reported", func() bool {
		p, _ := h.health(alphaRepo)
		return len(p.Stuck) > 0
	})
	p, _ := h.health(alphaRepo)
	if len(p.Stuck) != 1 || p.Stuck[0].RunURL != stuckRun || p.Stuck[0].Job != "frontend" || p.Stuck[0].QueuedS < 3600 {
		t.Fatalf("Stuck = %+v; want the frontend job of %s, queued an hour", p.Stuck, stuckRun)
	}
	if !p.Healthy {
		t.Fatalf("the project is unhealthy (%s); a report does not change its health", p.Reason)
	}
	calls := h.gh.calls()
	h.eventually("another check", func() bool { return h.gh.calls() > calls })
	if n := strings.Count(h.logs.String(), "level=WARN msg=\"a job is queued while the pool has a free slot"); n != 1 {
		t.Fatalf("%d warnings, want one per job", n)
	}
	if !h.logged("run=" + stuckRun) {
		t.Fatal("the warning names no run")
	}
}

// Nothing is reported, and GitHub is not asked, while a runner of the project
// waits for the job; the report clears once one does.
func TestQueuedJobWithARunnerWaitingIsNotReported(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.budget = newFakeBudget(true)
	hold := make(chan struct{})
	h.budget.(*fakeBudget).admit = func(ctx context.Context, a runner.Admission) (runner.Admitted, error) {
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return runner.Admitted{Release: runner.NoRelease}, ctx.Err()
	}
	t.Cleanup(func() { close(hold) })
	h.gh.setQueued(alphaRepo, github.QueuedJob{RunURL: stuckRun, ID: 1, Name: "frontend",
		Labels: []string{"alpha-local"}, Created: h.clock.Now().Add(-time.Hour)})
	h.start()
	h.eventually("the stuck job reported", func() bool {
		p, _ := h.health(alphaRepo)
		return len(p.Stuck) == 1
	})
	h.actions.Assign(alphaSet, 1)
	h.eventually("the report cleared while a runner waits", func() bool {
		p, _ := h.health(alphaRepo)
		return p.Waiting == 1 && len(p.Stuck) == 0
	})
	calls := h.gh.calls()
	time.Sleep(2 * queuedPoll / 1000)
	if got := h.gh.calls(); got != calls {
		t.Fatalf("GitHub asked %d more times while a runner waits, want none", got-calls)
	}
}

// A failed check changes nothing and warns nothing.
func TestQueuedCheckFailsQuietly(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.gh.queuedErr = errors.New("API rate limit exceeded")
	h.start()
	h.eventually("checked", func() bool { return h.gh.calls() >= 2 })
	p, _ := h.health(alphaRepo)
	if len(p.Stuck) != 0 || !p.Healthy {
		t.Fatalf("health %+v after failed checks; want healthy, nothing stuck", p)
	}
	for _, line := range strings.Split(h.logs.String(), "\n") {
		if strings.Contains(line, "queued") && !strings.Contains(line, "level=DEBUG") && strings.Contains(line, "rate limit") {
			t.Fatalf("a failed check logged above debug: %s", line)
		}
	}
	if !slices.ContainsFunc(strings.Split(h.logs.String(), "\n"), func(l string) bool {
		return strings.Contains(l, "level=DEBUG") && strings.Contains(l, "checking the queued jobs")
	}) {
		t.Fatal("no debug line for the failed check")
	}
}

// With every slot taken, a queued job waits for a slot, and nothing is
// reported.
func TestQueuedJobWithNoFreeSlotIsNotReported(t *testing.T) {
	h := newHarness(t, 1, 1000, "alpha")
	h.start()
	h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
	h.actions.Assign(alphaSet, 1)
	img := h.imageOf("alpha")
	h.eventually("a runner", func() bool { return len(h.docker.runners(img)) == 1 })
	h.actions.Started(alphaSet, h.docker.runners(img)[0])
	h.delivered(alphaSet)
	h.gh.setQueued(alphaRepo, github.QueuedJob{RunURL: stuckRun, ID: 1, Name: "frontend",
		Labels: []string{"alpha-local"}, Created: h.clock.Now().Add(-time.Hour)})
	time.Sleep(3 * (queuedAfter + queuedPoll) / 1000)
	if p, _ := h.health(alphaRepo); len(p.Stuck) != 0 {
		t.Fatalf("Stuck = %+v with every slot busy; want none", p.Stuck)
	}
	if got := h.gh.calls(); got != 0 {
		t.Fatalf("GitHub asked %d times with every slot busy, want none", got)
	}
}

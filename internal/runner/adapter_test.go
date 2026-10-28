package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

func TestAdapterDrivesTheScaler(t *testing.T) {
	slots := NewSlots(2)
	h := newHarness(slots)
	var a listener.Scaler = NewAdapter(h.scaler, &fakeAcquirer{})
	ctx := context.Background()

	if err := a.Scale(ctx, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(2)}); err != nil {
		t.Fatal(err)
	}
	names := h.starter.started()
	if len(names) != 2 {
		t.Fatalf("started %v on 2 assigned jobs, want 2 runners", names)
	}
	name := names[0]
	if err := a.Scale(ctx, &scaleset.RunnerScaleSetMessage{MessageID: 2, Statistics: stats(2),
		JobStartedMessages: []*scaleset.JobStarted{{RunnerName: name}}}); err != nil {
		t.Fatal(err)
	}
	if slots.Busy() != 1 {
		t.Fatalf("Busy %d after JobStarted, want 1", slots.Busy())
	}
	if err := a.Scale(ctx, &scaleset.RunnerScaleSetMessage{MessageID: 3, Statistics: stats(1),
		JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: name}}}); err != nil {
		t.Fatal(err)
	}
	if slots.Busy() != 0 || slots.InUse() != 2 {
		t.Fatalf("busy %d in use %d after JobCompleted, want 0 and 2 (the slot waits for the exit)", slots.Busy(), slots.InUse())
	}

	// Messages for runners of an earlier process, with an empty name, or
	// with no payload at all, are logged and change nothing.
	if err := a.Scale(ctx, &scaleset.RunnerScaleSetMessage{
		MessageID:            4,
		Statistics:           stats(1),
		JobStartedMessages:   []*scaleset.JobStarted{{}, {RunnerName: "alpha-old"}, nil},
		JobCompletedMessages: []*scaleset.JobCompleted{{}, {RunnerName: "alpha-old"}, nil},
	}); err != nil {
		t.Fatalf("a stale message failed the listener: %v", err)
	}
	if slots.Busy() != 0 || slots.InUse() != 2 || h.scaler.Idle() != 1 {
		t.Fatalf("busy %d in use %d idle %d after stale messages, want 0 2 1", slots.Busy(), slots.InUse(), h.scaler.Idle())
	}
}

func TestAdapterJobStartedNamesTheJob(t *testing.T) {
	h := newHarness(NewSlots(1))
	a := NewAdapter(h.scaler, &fakeAcquirer{})
	if err := a.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 1, JobStartedMessages: []*scaleset.JobStarted{{
		RunnerName:     "alpha-1",
		JobMessageBase: scaleset.JobMessageBase{JobDisplayName: "unit (2)"},
	}}}); err != nil {
		t.Fatal(err)
	}
	for _, line := range h.logs.lines() {
		if strings.Contains(line, `msg="job started"`) {
			if !strings.Contains(line, `job="unit (2)"`) {
				t.Fatalf("the job started record does not name the job: %s", line)
			}
			return
		}
	}
	t.Fatalf("no job started record in %q", h.logs.lines())
}

var errBoom = errors.New("boom")

// stats is a message's statistics with assigned jobs.
func stats(assigned int) *scaleset.RunnerScaleSetStatistic {
	return &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned}
}

// available is one JobAvailable per request ID.
func available(ids ...int64) []*scaleset.JobAvailable {
	var out []*scaleset.JobAvailable
	for _, id := range ids {
		out = append(out, &scaleset.JobAvailable{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: id}})
	}
	return out
}

// scale calls a.Scale and fails the test on an error.
func scale(t *testing.T, a *Adapter, ctx context.Context, msg *scaleset.RunnerScaleSetMessage) {
	t.Helper()
	if err := a.Scale(ctx, msg); err != nil {
		t.Fatalf("Scale(%+v): %v", msg, err)
	}
}

// indexOf is the index of the first line containing every part, or -1.
func indexOf(lines []string, parts ...string) int {
	for i, line := range lines {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			return i
		}
	}
	return -1
}

func TestScaleAcquiresBeforeAnythingElse(t *testing.T) {
	h := newHarness(NewSlots(4))
	events := &eventLog{}
	h.starter.onStart = func(name string) { events.add("start " + name) }
	jobs := &fakeAcquirer{events: events}
	a := NewAdapter(h.scaler, jobs)

	scale(t, a, context.Background(), &scaleset.RunnerScaleSetMessage{
		MessageID:            1,
		Statistics:           stats(1),
		JobAvailableMessages: available(11, 12, 13),
	})
	if got := jobs.calls(); len(got) != 1 || !slices.Equal(got[0], []int64{11, 12, 13}) {
		t.Fatalf("AcquireJobs calls %v, want one with [11 12 13]", got)
	}
	ev := events.list()
	if len(ev) != 2 || ev[0] != "acquire [11 12 13]" || !strings.HasPrefix(ev[1], "start ") {
		t.Fatalf("events %q, want the acquire, then the one start", ev)
	}
}

func TestScaleAcquireFailureIsLoggedNotFatal(t *testing.T) {
	slots := NewSlots(4)
	h := newHarness(slots)
	h.scaler.names = scripted("a", "b", "c", "d")
	jobs := &fakeAcquirer{}
	a := NewAdapter(h.scaler, jobs)
	ctx := context.Background()
	scale(t, a, ctx, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(2)})

	jobs.err = errBoom
	err := a.Scale(ctx, &scaleset.RunnerScaleSetMessage{
		MessageID:            2,
		Statistics:           stats(2),
		JobAvailableMessages: available(21),
		JobStartedMessages:   []*scaleset.JobStarted{{RunnerName: "a"}},
		JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "b"}},
	})
	if err != nil {
		t.Fatalf("Scale failed on an acquire error: %v", err)
	}
	if i := indexOf(h.logs.lines(), "level=WARN", `msg="acquiring jobs failed; GitHub reissues an unacquired request after 5 minutes"`, "count=1", "runnerRequestIds=[21]", "error=boom"); i < 0 {
		t.Fatalf("no acquire failure warning in %q", h.logs.lines())
	}
	// The rest of the message is handled: a is busy, b is done, and Desired(2)
	// starts c beside the busy a.
	if slots.Busy() != 1 || stateOf(h.scaler, "a") != stateBusy || stateOf(h.scaler, "b") != stateDone {
		t.Fatalf("busy %d, a %v, b %v; want 1, busy, done", slots.Busy(), stateOf(h.scaler, "a"), stateOf(h.scaler, "b"))
	}
	if got := h.starter.started(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("started %v, want [a b c]", got)
	}
}

func TestScaleReturnsTheContextError(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		slots := NewSlots(4)
		h := newHarness(slots)
		h.scaler.names = scripted("a", "b")
		jobs := &fakeAcquirer{answer: func(ctx context.Context) error { return ctx.Err() }}
		a := NewAdapter(h.scaler, jobs)
		scale(t, a, context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(1)})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := a.Scale(ctx, &scaleset.RunnerScaleSetMessage{
			MessageID:            2,
			Statistics:           stats(2),
			JobAvailableMessages: available(31),
			JobStartedMessages:   []*scaleset.JobStarted{{RunnerName: "a"}},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Scale under a cancelled ctx = %v, want context.Canceled", err)
		}
		// Nothing after the acquire ran: the message is left for redelivery.
		if slots.Busy() != 0 || len(h.starter.started()) != 1 {
			t.Fatalf("busy %d, started %v after the failed acquire; want 0 and [a]", slots.Busy(), h.starter.started())
		}
	})
	// A slow acquire under a live ctx, such as the HTTP client's own timeout
	// (it matches context.DeadlineExceeded), is an acquire failure like any
	// other: logged, and the message handled.
	t.Run("deadline with a live ctx", func(t *testing.T) {
		h := newHarness(NewSlots(4))
		a := NewAdapter(h.scaler, &fakeAcquirer{err: fmt.Errorf("posting: %w", context.DeadlineExceeded)})
		err := a.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(1), JobAvailableMessages: available(32)})
		if err != nil {
			t.Fatalf("Scale on a slow acquire under a live ctx = %v, want nil", err)
		}
		if indexOf(h.logs.lines(), "level=WARN", "acquiring jobs failed", "runnerRequestIds=[32]") < 0 {
			t.Fatalf("no acquire failure warning in %q", h.logs.lines())
		}
		if got := h.starter.started(); len(got) != 1 {
			t.Fatalf("started %v, want the message's Desired to start one runner", got)
		}
	})
}

func TestScaleNeverFailsOnJobEvents(t *testing.T) {
	slots := NewSlots(4)
	h := newHarness(slots)
	a := NewAdapter(h.scaler, &fakeAcquirer{})
	ctx := context.Background()
	scale(t, a, ctx, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(1)})

	err := a.Scale(ctx, &scaleset.RunnerScaleSetMessage{
		MessageID:            2,
		JobStartedMessages:   []*scaleset.JobStarted{nil, {}, {RunnerName: "alpha-old"}},
		JobCompletedMessages: []*scaleset.JobCompleted{nil, {}, {RunnerName: "alpha-old"}},
	})
	if err != nil {
		t.Fatalf("a message with stale job events failed: %v", err)
	}
	if slots.Busy() != 0 || slots.InUse() != 1 || h.scaler.Idle() != 1 {
		t.Fatalf("busy %d in use %d idle %d, want 0 1 1", slots.Busy(), slots.InUse(), h.scaler.Idle())
	}
}

func TestScaleLogsTheAcquisition(t *testing.T) {
	h := newHarness(NewSlots(4))
	a := NewAdapter(h.scaler, &fakeAcquirer{})
	scale(t, a, context.Background(), &scaleset.RunnerScaleSetMessage{
		MessageID:            1,
		Statistics:           stats(1),
		JobAvailableMessages: available(41, 42, 43),
	})
	lines := h.logs.lines()
	acquiring := indexOf(lines, "level=INFO", `msg="Acquiring jobs"`, "count=3", "runnerRequestIds=\"[41 42 43]\"")
	acquired := indexOf(lines, "level=INFO", `msg="Jobs acquired"`, "count=3", "requested=3")
	started := indexOf(lines, `msg="runner started"`)
	if acquiring < 0 || acquired < 0 || started < 0 || !(acquiring < acquired && acquired < started) {
		t.Fatalf("want Acquiring jobs, Jobs acquired, then runner started; got %q", lines)
	}

	h = newHarness(NewSlots(4))
	a = NewAdapter(h.scaler, &fakeAcquirer{})
	scale(t, a, context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(1)})
	if lines := h.logs.lines(); indexOf(lines, "Acquiring jobs") >= 0 || indexOf(lines, "Jobs acquired") >= 0 {
		t.Fatalf("an acquisition was logged for a message without JobAvailable: %q", lines)
	}
}

// The 1af781f guard at the Adapter: an idle poll still calls Desired with the
// last count, so a lasting dip gives the waiters up after the grace.
func TestScaleNilMessageReusesTheLastStatistics(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	h.scaler.names = scripted("a", "b", "c")
	a := NewAdapter(h.scaler, &fakeAcquirer{})
	ctx := context.Background()
	scale(t, a, ctx, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(3)})
	reqs := map[string]*admitReq{}
	for range 3 {
		r := h.gate.next(t)
		reqs[r.name] = r
	}
	scale(t, a, ctx, &scaleset.RunnerScaleSetMessage{MessageID: 2, Statistics: stats(1)})
	for name, r := range reqs {
		if ended(r.ctx) {
			t.Fatalf("%s given up within the grace", name)
		}
	}
	h.clock.advance(dipGrace)
	scale(t, a, ctx, nil)
	if ended(reqs["a"].ctx) || !ended(reqs["b"].ctx) || !ended(reqs["c"].ctx) {
		t.Fatalf("ctx ended: a %v, b %v, c %v; want the newest two given up on the idle poll", ended(reqs["a"].ctx), ended(reqs["b"].ctx), ended(reqs["c"].ctx))
	}
	if got := h.gate.admitCalls(); got != 3 {
		t.Fatalf("%d Admit calls, want 3", got)
	}
}

// A new Adapter (one per listener run) over a Scaler that holds an idle
// runner: an idle poll before any statistics leaves it alone.
func TestScaleNilMessageBeforeAnyStatisticsDoesNothing(t *testing.T) {
	h := newHarness(NewSlots(4))
	scale(t, NewAdapter(h.scaler, &fakeAcquirer{}), context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(1)})
	if h.scaler.Idle() != 1 {
		t.Fatalf("idle %d, want the seeded idle runner", h.scaler.Idle())
	}
	scale(t, NewAdapter(h.scaler, &fakeAcquirer{}), context.Background(), nil)
	if got := h.docker.removed(); len(got) != 0 {
		t.Fatalf("removed %v on an idle poll before any statistics, want nothing", got)
	}
	if got := h.starter.started(); len(got) != 1 || h.scaler.Idle() != 1 {
		t.Fatalf("started %v, idle %d; want the one seeded runner", got, h.scaler.Idle())
	}
}

func TestScaleMessageWithoutStatisticsKeepsTheLastCount(t *testing.T) {
	h := newHarness(NewSlots(4))
	h.scaler.names = scripted("a", "b", "c")
	a := NewAdapter(h.scaler, &fakeAcquirer{})
	ctx := context.Background()
	scale(t, a, ctx, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(2)})
	h.scaler.Exited("a")
	scale(t, a, ctx, &scaleset.RunnerScaleSetMessage{
		MessageID:            2,
		JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "a"}},
	})
	if got := h.starter.started(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("started %v, want c to replace a on the last count of 2", got)
	}
	if got := h.docker.removed(); len(got) != 0 {
		t.Fatalf("removed %v, want nothing", got)
	}
}

// A characterization guard for the Scaler's idempotence under redelivery: the
// listener redelivers a message Scale failed on, and GitHub may deliver one
// again. The job events change nothing the second time. The statistics it
// carries replace the cached count, even when they are older (an accepted
// risk: a message redelivered across sessions would carry stale statistics).
func TestScaleRedeliveredMessageRepeatsOnlyTheAcquire(t *testing.T) {
	slots := NewSlots(4)
	h := newHarness(slots)
	h.scaler.names = scripted("a", "b", "c")
	jobs := &fakeAcquirer{}
	a := NewAdapter(h.scaler, jobs)
	ctx := context.Background()
	scale(t, a, ctx, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(2)})

	msg := func(assigned int) *scaleset.RunnerScaleSetMessage {
		return &scaleset.RunnerScaleSetMessage{
			MessageID:            2,
			Statistics:           stats(assigned),
			JobAvailableMessages: available(51, 52),
			JobStartedMessages:   []*scaleset.JobStarted{{RunnerName: "a"}},
		}
	}
	scale(t, a, ctx, msg(2))
	if got := h.starter.started(); !slices.Equal(got, []string{"a", "b"}) || slots.Busy() != 1 {
		t.Fatalf("started %v, busy %d after the first delivery; want [a b] and 1", got, slots.Busy())
	}
	scale(t, a, ctx, msg(1))
	if got := h.starter.started(); !slices.Equal(got, []string{"a", "b"}) || slots.Busy() != 1 {
		t.Fatalf("started %v, busy %d after the redelivery; want [a b] and 1", got, slots.Busy())
	}
	if got := jobs.calls(); len(got) != 2 || !slices.Equal(got[0], []int64{51, 52}) || !slices.Equal(got[1], []int64{51, 52}) {
		t.Fatalf("AcquireJobs calls %v, want [51 52] twice", got)
	}
	// The redelivered count of 1 is applied: the idle b is reaped, the busy
	// a kept, and an idle poll keeps that count.
	if got := h.docker.removed(); len(got) != 1 || stateOf(h.scaler, "b") != 0 || stateOf(h.scaler, "a") != stateBusy {
		t.Fatalf("removed %v, a %v, b %v; want b reaped on the redelivered count and a busy", got, stateOf(h.scaler, "a"), stateOf(h.scaler, "b"))
	}
	scale(t, a, ctx, nil)
	if got := h.starter.started(); len(got) != 2 || len(h.docker.removed()) != 1 {
		t.Fatalf("started %v, removed %v after an idle poll; want no change", got, h.docker.removed())
	}
}

// Real messages are handled without cancellation, as v0.4.0's listener did;
// the initial message and idle polls run under the listener's ctx. Under a
// cancelled ctx, Desired starts nothing.
func TestScaleContextPerMessageKind(t *testing.T) {
	h := newHarness(NewSlots(4))
	a := NewAdapter(h.scaler, &fakeAcquirer{})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	scale(t, a, cancelled, &scaleset.RunnerScaleSetMessage{MessageID: listener.InitialMessageID, Statistics: stats(1)})
	if got := h.starter.started(); len(got) != 0 {
		t.Fatalf("the initial message under a cancelled ctx started %v", got)
	}
	scale(t, a, cancelled, nil)
	if got := h.starter.started(); len(got) != 0 {
		t.Fatalf("an idle poll under a cancelled ctx started %v", got)
	}
	// The control: the cache holds the initial count, so a live idle poll
	// starts the runner.
	scale(t, a, context.Background(), nil)
	if got := h.starter.started(); len(got) != 1 {
		t.Fatalf("a live idle poll started %v, want one runner on the cached count", got)
	}
	scale(t, a, cancelled, &scaleset.RunnerScaleSetMessage{MessageID: 5, Statistics: stats(2)})
	if got := h.starter.started(); len(got) != 2 {
		t.Fatalf("message 5 under a cancelled ctx: started %v, want one more runner", got)
	}
}

// Acquiring while draining is deliberate, as in v0.4.0: the job is assigned to
// this scale set and nothing starts for it, so GitHub reissues it after about
// 5 minutes, or the drain deletes the scale set first.
func TestScaleAcquiresWhileDraining(t *testing.T) {
	h := newHarness(NewSlots(4))
	jobs := &fakeAcquirer{}
	a := NewAdapter(h.scaler, jobs)
	h.scaler.Drain()
	scale(t, a, context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(1), JobAvailableMessages: available(61)})
	if got := jobs.calls(); len(got) != 1 || !slices.Equal(got[0], []int64{61}) {
		t.Fatalf("AcquireJobs calls %v while draining, want [61]", got)
	}
	if got := h.starter.started(); len(got) != 0 {
		t.Fatalf("started %v while draining", got)
	}
}

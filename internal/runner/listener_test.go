package runner

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

// scriptedClient is a listener.Client that hands the listener its messages in
// order, then idle polls, and records every ack and acquire in events.
type scriptedClient struct {
	events     *eventLog
	acquireErr error

	mu      sync.Mutex
	msgs    []*scaleset.RunnerScaleSetMessage
	deleted []int
}

func (c *scriptedClient) GetMessage(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	c.mu.Lock()
	if len(c.msgs) > 0 {
		m := c.msgs[0]
		c.msgs = c.msgs[1:]
		c.mu.Unlock()
		return m, nil
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Millisecond):
		return nil, nil
	}
}

func (c *scriptedClient) DeleteMessage(_ context.Context, messageID int) error {
	c.mu.Lock()
	c.deleted = append(c.deleted, messageID)
	c.mu.Unlock()
	c.events.add(fmt.Sprintf("delete %d", messageID))
	return nil
}

func (c *scriptedClient) AcquireJobs(_ context.Context, requestIDs []int64) ([]int64, error) {
	c.events.add(fmt.Sprintf("acquire %v", requestIDs))
	if c.acquireErr != nil {
		return nil, c.acquireErr
	}
	return slices.Clone(requestIDs), nil
}

func (c *scriptedClient) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: uuid.New(), Statistics: &scaleset.RunnerScaleSetStatistic{}}
}

func (c *scriptedClient) acked(id int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.deleted, id)
}

// run is one listener run; done is closed when Run returns, with its error.
type run struct {
	done chan struct{}
	err  error
}

// runListener runs the real listener over c, driving h's scaler through an
// Adapter that acquires through c, until the test ends.
func runListener(t *testing.T, h *harness, c *scriptedClient) *run {
	t.Helper()
	l, err := listener.New(c, listener.Config{ScaleSetID: 7, MaxRunners: h.scaler.slots.Cap()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.err = l.Run(ctx, NewAdapter(h.scaler, c))
	}()
	t.Cleanup(func() {
		cancel()
		<-r.done
	})
	return r
}

// keepsRunning fails the test if Run returned, or returns within 50ms.
func keepsRunning(t *testing.T, r *run) {
	t.Helper()
	select {
	case <-r.done:
		t.Fatalf("the listener stopped: %v", r.err)
	case <-time.After(50 * time.Millisecond):
	}
}

// An upstream contract test (V5 listener.go acks after Scale returns): no
// line of the Adapter turns it red unless the Adapter went asynchronous, but
// it fails on a bump that acks first again, as v0.4.0 did.
func TestListenerAcksOnlyAfterScale(t *testing.T) {
	h := newHarness(NewSlots(4))
	events := &eventLog{}
	h.starter.onStart = func(name string) { events.add("start") }
	c := &scriptedClient{events: events, msgs: []*scaleset.RunnerScaleSetMessage{{MessageID: 3, Statistics: stats(1)}}}
	done := runListener(t, h, c)
	eventually(t, "message 3 acked", func() bool { return c.acked(3) })
	keepsRunning(t, done)
	ev := events.list()
	if s, d := slices.Index(ev, "start"), slices.Index(ev, "delete 3"); s < 0 || d < s {
		t.Fatalf("events %q, want the runner started before message 3 is acked", ev)
	}
}

func TestListenerAcksAMessageWhoseAcquireFailed(t *testing.T) {
	for name, acquireErr := range map[string]error{
		"error": errBoom,
		// The HTTP client's own timeout matches context.DeadlineExceeded
		// while the listener's ctx is live.
		"deadline with a live ctx": fmt.Errorf("posting: %w", context.DeadlineExceeded),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(NewSlots(4))
			c := &scriptedClient{events: &eventLog{}, acquireErr: acquireErr, msgs: []*scaleset.RunnerScaleSetMessage{
				{MessageID: 4, Statistics: stats(1), JobAvailableMessages: available(71)},
			}}
			done := runListener(t, h, c)
			eventually(t, "message 4 acked", func() bool { return c.acked(4) })
			keepsRunning(t, done)
			if got := c.events.list(); slices.Index(got, "acquire [71]") < 0 {
				t.Fatalf("events %q, want the acquire of 71", got)
			}
		})
	}
}

func TestListenerAcksMessagesWithBadJobEvents(t *testing.T) {
	h := newHarness(NewSlots(4))
	c := &scriptedClient{events: &eventLog{}, msgs: []*scaleset.RunnerScaleSetMessage{{
		MessageID:            5,
		JobStartedMessages:   []*scaleset.JobStarted{nil, {}, {RunnerName: "alpha-old"}},
		JobCompletedMessages: []*scaleset.JobCompleted{nil, {}, {RunnerName: "alpha-old"}},
	}}}
	done := runListener(t, h, c)
	eventually(t, "message 5 acked", func() bool { return c.acked(5) })
	keepsRunning(t, done)
}

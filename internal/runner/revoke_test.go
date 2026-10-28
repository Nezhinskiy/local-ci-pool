package runner

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/Nezhinskiy/local-ci-pool/internal/fakeactions"
)

// countingAdapter is the Adapter, recording every count the listener passes
// to Desired.
type countingAdapter struct {
	*Adapter
	mu     sync.Mutex
	counts []int
}

func (c *countingAdapter) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	c.mu.Lock()
	c.counts = append(c.counts, count)
	c.mu.Unlock()
	return c.Adapter.HandleDesiredRunnerCount(ctx, count)
}

func (c *countingAdapter) seen() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.counts)
}

// listenOn runs the real listener of actions/scaleset on a session of the
// fake service's scale set, driving sc, until the test ends.
func listenOn(t *testing.T, a *fakeactions.Actions, set string, sc *Scaler) *countingAdapter {
	t.Helper()
	c, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     a.ConfigURL("example/alpha"),
		PersonalAccessToken: "token",
		SystemInfo:          scaleset.SystemInfo{System: "local-ci-pool-test"},
	}, scaleset.WithRetryMax(0))
	if err != nil {
		t.Fatal(err)
	}
	id := a.ScaleSet(set).ID
	ctx, cancel := context.WithCancel(context.Background())
	sess, err := c.MessageSessionClient(ctx, id, "examplemac")
	if err != nil {
		t.Fatal(err)
	}
	l, err := listener.New(sess, listener.Config{ScaleSetID: id, MaxRunners: sc.slots.Cap()})
	if err != nil {
		t.Fatal(err)
	}
	ad := &countingAdapter{Adapter: NewAdapter(sc, sess)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = l.Run(ctx, ad)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = sess.Close(context.Background())
	})
	return ad
}

// handled waits until the listener has handled every message queued so far:
// it queues a statistics-only sentinel and waits until it is taken, which the
// listener does only after the messages before it.
func handled(t *testing.T, a *fakeactions.Actions, set string) {
	t.Helper()
	eventually(t, "the messages taken", func() bool { return a.Pending(set) == 0 })
	a.Assign(set, 0)
	eventually(t, "the sentinel taken", func() bool { return a.Pending(set) == 0 })
}

// TestRevokedRequestKeepsItsWaiters replays the sequence github.com sent on
// 2026-10-18: four jobs assigned while every runner waited for the budget;
// five minutes later two of the requests were completed as canceled with no
// runner, which dipped TotalAssignedJobs from 4 to 2, and 0.3 s later reissued,
// which restored it to 4. The waiters keep their place in the budget's queue:
// none is given up and no fresh waiter queues behind the others.
func TestRevokedRequestKeepsItsWaiters(t *testing.T) {
	const set = "alpha-examplemac"
	a := fakeactions.New(t)
	a.AddScaleSet(set, set, "alpha-local")
	h := newGateHarness(t, NewSlots(4))
	ad := listenOn(t, a, set, h.scaler)

	a.Assign(set, 4)
	var waiters []*admitReq
	for range 4 {
		waiters = append(waiters, h.gate.next(t))
	}
	a.Revoke(set, 2)
	handled(t, a, set)
	time.Sleep(300 * time.Millisecond)
	a.Assign(set, 2)
	handled(t, a, set)

	counts := ad.seen()
	if i := slices.Index(counts, 2); i < 1 || counts[i-1] != 4 || !slices.Contains(counts[i:], 4) {
		t.Fatalf("Desired saw %v, want 4, then the dip to 2, then 4 again", counts)
	}
	for _, w := range waiters {
		if ended(w.ctx) {
			t.Errorf("waiter %s was given up on the transient dip", w.name)
		}
	}
	if got := h.gate.admitCalls(); got != 4 {
		t.Fatalf("%d Admit calls, want the 4 first waiters only", got)
	}
}

// A drop of the assigned jobs below the waiting runners gives them up only
// once it has lasted dipGrace, on the first Desired call after that; until
// then they keep their tickets.
func TestDipGivesUpWaitersAfterTheGrace(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	h.scaler.names = scripted("a", "b", "c")
	reqs := map[string]*admitReq{}
	desired(t, h.scaler, 3)
	for range 3 {
		r := h.gate.next(t)
		reqs[r.name] = r
	}
	desired(t, h.scaler, 1)
	h.clock.advance(dipGrace - time.Nanosecond)
	if got := desired(t, h.scaler, 1); got != 3 {
		t.Fatalf("Desired(1) within the grace = %d, want the 3 waiters kept", got)
	}
	for name, r := range reqs {
		if ended(r.ctx) {
			t.Fatalf("%s given up within the grace", name)
		}
	}
	h.clock.advance(time.Nanosecond)
	if got := desired(t, h.scaler, 1); got != 1 {
		t.Fatalf("Desired(1) at the grace = %d, want 1", got)
	}
	if ended(reqs["a"].ctx) || !ended(reqs["b"].ctx) || !ended(reqs["c"].ctx) {
		t.Fatalf("ctx ended: a %v, b %v, c %v; want the newest two given up", ended(reqs["a"].ctx), ended(reqs["b"].ctx), ended(reqs["c"].ctx))
	}
	if got := h.gate.admitCalls(); got != 3 {
		t.Fatalf("%d Admit calls, want 3", got)
	}
}

// A count that comes back ends the dip: the next drop waits a whole grace
// again.
func TestDipEndsWhenTheCountComesBack(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	desired(t, h.scaler, 2)
	w1, w2 := h.gate.next(t), h.gate.next(t)
	desired(t, h.scaler, 0)
	h.clock.advance(dipGrace / 2)
	desired(t, h.scaler, 2)
	h.clock.advance(dipGrace / 2)
	desired(t, h.scaler, 0)
	if ended(w1.ctx) || ended(w2.ctx) {
		t.Fatal("a waiter was given up on a new dip; want a whole grace from the new dip")
	}
	h.clock.advance(dipGrace)
	desired(t, h.scaler, 0)
	if !ended(w1.ctx) || !ended(w2.ctx) {
		t.Fatal("the waiters are live a whole grace into the dip")
	}
	if got := h.gate.admitCalls(); got != 2 {
		t.Fatalf("%d Admit calls, want 2", got)
	}
}

// The grace keeps waiting runners only: an idle runner beyond the waiters is
// removed at once, as before.
func TestDipReapsIdleRunnersBeyondTheWaiters(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	idle := admitIdle(t, h)
	desired(t, h.scaler, 2)
	w := h.gate.next(t)
	desired(t, h.scaler, 0)
	if got := h.docker.removed(); !slices.Equal(got, []string{idle}) {
		t.Fatalf("removed %v, want the idle runner %s", got, idle)
	}
	if ended(w.ctx) {
		t.Fatal("the waiter was given up within the grace")
	}
}

// A drain gives every waiter up at once, whatever the grace.
func TestDrainIgnoresTheGrace(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	desired(t, h.scaler, 2)
	w1, w2 := h.gate.next(t), h.gate.next(t)
	desired(t, h.scaler, 0)
	h.scaler.Drain()
	if !ended(w1.ctx) || !ended(w2.ctx) {
		t.Fatal("a waiter is live after Drain")
	}
}

// A dip confirmed and acted on ends there: a new dip on the very next call,
// such as a revocation, keeps the remaining waiter for a whole grace again.
func TestDipAfterAGiveUpWaitsAWholeGrace(t *testing.T) {
	h := newGateHarness(t, NewSlots(4))
	h.scaler.names = scripted("a", "b")
	reqs := map[string]*admitReq{}
	desired(t, h.scaler, 2)
	for range 2 {
		r := h.gate.next(t)
		reqs[r.name] = r
	}
	desired(t, h.scaler, 1)
	h.clock.advance(dipGrace)
	desired(t, h.scaler, 1)
	if ended(reqs["a"].ctx) || !ended(reqs["b"].ctx) {
		t.Fatalf("ctx ended: a %v, b %v; want b given up at the grace, a kept", ended(reqs["a"].ctx), ended(reqs["b"].ctx))
	}
	h.clock.advance(time.Second)
	desired(t, h.scaler, 0)
	if ended(reqs["a"].ctx) {
		t.Fatal("a was given up at once on a new dip; want a whole grace from the new dip")
	}
	h.clock.advance(dipGrace)
	desired(t, h.scaler, 0)
	if !ended(reqs["a"].ctx) {
		t.Fatal("a is live a whole grace into the new dip")
	}
}

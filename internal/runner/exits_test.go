package runner

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

func die(instance, runner string) events.Message {
	attrs := map[string]string{"exitCode": "0"}
	if instance != "" {
		attrs[LabelInstance] = instance
	}
	if runner != "" {
		attrs[LabelRunner] = runner
	}
	return events.Message{Type: events.ContainerEventType, Action: events.ActionDie, Actor: events.Actor{ID: "c-" + runner, Attributes: attrs}}
}

func TestWatchExitsFiltersAndMapsRunnerNames(t *testing.T) {
	d := newFakeDocker()
	var opts []client.EventsListOptions
	msgs := make(chan events.Message, 8)
	msgs <- die("main", "alpha-1")
	msgs <- die("probe-examplemac", "alpha-2")                                                                               // another instance
	msgs <- die("main", "")                                                                                                  // no runner label
	msgs <- events.Message{Type: events.ContainerEventType, Action: events.ActionStart, Actor: die("main", "alpha-3").Actor} // not a die
	msgs <- die("main", "alpha-4")
	d.events = func(_ context.Context, o client.EventsListOptions) client.EventsResult {
		opts = append(opts, o)
		return client.EventsResult{Messages: msgs, Err: make(chan error)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var got []string
	WatchExits(ctx, d, "main", func(name string) {
		got = append(got, name)
		if name == "alpha-4" {
			cancel()
		}
	})
	if !slices.Equal(got, []string{"alpha-1", "alpha-4"}) {
		t.Fatalf("exits %v, want alpha-1 and alpha-4", got)
	}
	if len(opts) != 1 {
		t.Fatalf("%d subscriptions, want 1", len(opts))
	}
	f := opts[0].Filters
	if !f["type"]["container"] || len(f["type"]) != 1 {
		t.Fatalf("type filter %v", f["type"])
	}
	if !f["event"]["die"] || len(f["event"]) != 1 {
		t.Fatalf("event filter %v", f["event"])
	}
	if !f["label"]["local-ci-pool=main"] || len(f["label"]) != 1 {
		t.Fatalf("label filter %v", f["label"])
	}
}

func TestWatchExitsResubscribesAndReplaysTheGap(t *testing.T) {
	old := exitsRetry
	exitsRetry = time.Millisecond
	defer func() { exitsRetry = old }()

	d := newFakeDocker()
	var mu sync.Mutex
	var opts []client.EventsListOptions
	d.events = func(_ context.Context, o client.EventsListOptions) client.EventsResult {
		mu.Lock()
		defer mu.Unlock()
		opts = append(opts, o)
		msgs := make(chan events.Message, 1)
		errs := make(chan error, 1)
		switch len(opts) {
		case 1:
			// One exit, then the stream breaks.
			unbuffered := make(chan events.Message)
			go func() {
				unbuffered <- die("main", "alpha-1")
				errs <- errors.New("unexpected EOF")
			}()
			return client.EventsResult{Messages: unbuffered, Err: errs}
		case 2:
			errs <- errors.New("daemon restarting")
		default:
			msgs <- die("main", "alpha-2")
		}
		return client.EventsResult{Messages: msgs, Err: errs}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var got []string
	WatchExits(ctx, d, "main", func(name string) {
		got = append(got, name)
		if name == "alpha-2" {
			cancel()
		}
	})
	if !slices.Equal(got, []string{"alpha-1", "alpha-2"}) {
		t.Fatalf("exits %v, want alpha-1 then alpha-2", got)
	}
	if len(opts) != 3 {
		t.Fatalf("%d subscriptions, want 3", len(opts))
	}
	for i, o := range opts {
		since, err := time.Parse(time.RFC3339Nano, o.Since)
		if err != nil {
			t.Fatalf("subscription %d: Since %q: %v", i, o.Since, err)
		}
		if age := time.Since(since); age < replayWindow || age > replayWindow+time.Minute {
			t.Fatalf("subscription %d replays from %s ago, want about %s", i, age, replayWindow)
		}
	}
}

func TestWatchExitsReturnsWhenCancelled(t *testing.T) {
	d := newFakeDocker()
	d.events = func(ctx context.Context, _ client.EventsListOptions) client.EventsResult {
		return client.EventsResult{Messages: make(chan events.Message), Err: make(chan error)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		WatchExits(ctx, d, "main", func(string) {})
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchExits did not return after cancellation")
	}
}

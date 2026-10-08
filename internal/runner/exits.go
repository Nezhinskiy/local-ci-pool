package runner

import (
	"context"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

// replayWindow is how far back every subscription asks Docker to replay. A
// die event lost while the stream was down, or before the first subscription
// was live, is delivered again; the window also covers drift between the Mac's
// clock and the Docker VM's. Replays are harmless because Exited is idempotent
// and runner names are never reused.
const replayWindow = 5 * time.Minute

// exitsRetry and exitsRetryMax bound the wait before resubscribing; variables
// so tests can shorten them.
var (
	exitsRetry    = time.Second
	exitsRetryMax = 30 * time.Second
)

// WatchExits calls onExit with the runner name of every container of this
// instance that dies, until ctx is cancelled. It reads Docker's die events for
// the instance label and maps each to the runner label the event carries. A
// broken stream is resubscribed with backoff.
func WatchExits(ctx context.Context, d Docker, instance string, onExit func(runnerName string)) {
	filters := make(client.Filters).
		Add("type", string(events.ContainerEventType)).
		Add("event", string(events.ActionDie)).
		Add("label", LabelInstance+"="+instance)
	wait := exitsRetry
	for {
		since := time.Now().Add(-replayWindow).UTC().Format(time.RFC3339Nano)
		sub := d.Events(ctx, client.EventsListOptions{Since: since, Filters: filters})
		if delivered := consume(ctx, sub, instance, onExit); delivered {
			wait = exitsRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, exitsRetryMax)
	}
}

// consume reads one subscription until it ends and reports whether it
// delivered any message.
func consume(ctx context.Context, sub client.EventsResult, instance string, onExit func(string)) bool {
	delivered := false
	for {
		select {
		case <-ctx.Done():
			return delivered
		case <-sub.Err:
			return delivered
		case m := <-sub.Messages:
			delivered = true
			if m.Type != events.ContainerEventType || m.Action != events.ActionDie {
				continue
			}
			if m.Actor.Attributes[LabelInstance] != instance {
				continue
			}
			if name := m.Actor.Attributes[LabelRunner]; name != "" {
				onExit(name)
			}
		}
	}
}

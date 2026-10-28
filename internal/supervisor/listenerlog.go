package supervisor

import (
	"context"
	"log/slog"
)

// listenerPollMsg is the Info line actions/scaleset v0.4.0's listener writes
// before every poll for the next message: one line per poll per scale set, and
// nearly all of the daemon's log. The listener's other Info
// lines (Acquiring jobs, Jobs acquired) are the only record that GitHub handed
// the scale set a job before a runner started, so only this one is dropped. If
// upstream renames it the line comes back: the failure mode is noise, never
// lost data.
const listenerPollMsg = "Getting next message"

// dropPoll wraps h so that an Info record whose message is listenerPollMsg is
// discarded and every other record reaches h unchanged.
func dropPoll(h slog.Handler) slog.Handler { return pollDropper{h} }

type pollDropper struct{ slog.Handler }

func (d pollDropper) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelInfo && r.Message == listenerPollMsg {
		return nil
	}
	return d.Handler.Handle(ctx, r)
}

func (d pollDropper) WithAttrs(attrs []slog.Attr) slog.Handler {
	return pollDropper{d.Handler.WithAttrs(attrs)}
}

func (d pollDropper) WithGroup(name string) slog.Handler {
	return pollDropper{d.Handler.WithGroup(name)}
}

package supervisor

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func newListenerLogBuffer() (*bytes.Buffer, *slog.Logger) {
	var buf bytes.Buffer
	return &buf, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func TestListenerLogDropsThePollLine(t *testing.T) {
	buf, base := newListenerLogBuffer()
	l := slog.New(dropPoll(base.With("component", "listener").Handler()))

	l.Info(listenerPollMsg, "lastMessageID", 7)
	if buf.Len() != 0 {
		t.Fatalf("the poll line was written: %q", buf.String())
	}

	// Everything else the listener logs at Info is the record that GitHub
	// handed the scale set a job, and stays.
	for _, msg := range []string{"Handling initial session statistics", "Acquiring jobs", "Jobs acquired"} {
		buf.Reset()
		l.Info(msg, "count", 1)
		out := buf.String()
		if strings.Count(out, "\n") != 1 || !strings.Contains(out, "msg=\""+msg+"\"") || !strings.Contains(out, "component=listener") {
			t.Errorf("%q: want one line with component=listener, got %q", msg, out)
		}
	}

	// Only the Info line is dropped: the same text at another level is news.
	buf.Reset()
	l.Warn(listenerPollMsg)
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a Warn with the poll text was dropped: %q", buf.String())
	}
}

func TestListenerLogKeepsAttrsAndGroups(t *testing.T) {
	buf, base := newListenerLogBuffer()
	l := slog.New(dropPoll(base.Handler())).With("a", 1).WithGroup("g")

	l.Info("Jobs acquired", "b", 2)
	out := buf.String()
	if !strings.Contains(out, "a=1") || !strings.Contains(out, "g.b=2") {
		t.Errorf("attrs or group lost: %q", out)
	}

	buf.Reset()
	l.Info(listenerPollMsg)
	if buf.Len() != 0 {
		t.Errorf("the poll line survived With/WithGroup: %q", buf.String())
	}
}

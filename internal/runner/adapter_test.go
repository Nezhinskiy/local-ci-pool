package runner

import (
	"context"
	"testing"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

func TestAdapterDrivesTheScaler(t *testing.T) {
	slots := NewSlots(2)
	h := newHarness(slots)
	var a listener.Scaler = NewAdapter(h.scaler)
	ctx := context.Background()

	n, err := a.HandleDesiredRunnerCount(ctx, 2)
	if err != nil || n != 2 {
		t.Fatalf("HandleDesiredRunnerCount(2) = %d, %v", n, err)
	}
	name := h.starter.started()[0]
	if err := a.HandleJobStarted(ctx, &scaleset.JobStarted{RunnerName: name}); err != nil {
		t.Fatal(err)
	}
	if slots.Busy() != 1 {
		t.Fatalf("Busy %d after JobStarted, want 1", slots.Busy())
	}
	if err := a.HandleJobCompleted(ctx, &scaleset.JobCompleted{RunnerName: name}); err != nil {
		t.Fatal(err)
	}
	if slots.Busy() != 0 || slots.InUse() != 2 {
		t.Fatalf("busy %d in use %d after JobCompleted, want 0 and 2 (the slot waits for the exit)", slots.Busy(), slots.InUse())
	}

	// Messages for runners of an earlier process, with an empty name, or
	// with no payload at all, are logged and change nothing.
	for _, err := range []error{
		a.HandleJobStarted(ctx, &scaleset.JobStarted{}),
		a.HandleJobCompleted(ctx, &scaleset.JobCompleted{}),
		a.HandleJobStarted(ctx, &scaleset.JobStarted{RunnerName: "alpha-old"}),
		a.HandleJobCompleted(ctx, &scaleset.JobCompleted{RunnerName: "alpha-old"}),
		a.HandleJobStarted(ctx, nil),
		a.HandleJobCompleted(ctx, nil),
	} {
		if err != nil {
			t.Fatalf("a stale message failed the listener: %v", err)
		}
	}
	if slots.Busy() != 0 || slots.InUse() != 2 || h.scaler.Idle() != 1 {
		t.Fatalf("busy %d in use %d idle %d after stale messages, want 0 2 1", slots.Busy(), slots.InUse(), h.scaler.Idle())
	}
}

package runner

import "testing"

func TestModeStrings(t *testing.T) {
	for mode, want := range map[Mode]string{
		ModeOff:      "off",
		ModeQueued:   "queued",
		ModeAdopted:  "adopted",
		ModeFailOpen: "fail_open",
	} {
		if string(mode) != want {
			t.Errorf("mode %q, want %q", mode, want)
		}
	}
	if got, want := ErrCancelledByBudget.Error(), "cancelled through coreslot"; got != want {
		t.Errorf("ErrCancelledByBudget %q, want %q", got, want)
	}
}

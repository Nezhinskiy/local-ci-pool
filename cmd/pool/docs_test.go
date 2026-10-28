package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nezhinskiy/local-ci-pool/internal/budget"
	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

// The README's quickstart and the runbook's "CI went hosted" quote what
// `pool status` prints. A reader compares their own output with those blocks,
// so each must be what formatStatus prints for the state it describes,
// character for character. The quickstart's pool runs without the budget,
// which is what a plain ./install.sh gives.
func TestDocsQuoteStatusOutput(t *testing.T) {
	base := func() supervisor.Snapshot {
		return supervisor.Snapshot{
			Version: "v0.5.0",
			Commit:  "8eda50a",
			Machine: "examplemacbo",
			Slots:   7,
			Docker:  true,
			Login:   true,
			Budget:  budget.State{State: "off"},
			Projects: []supervisor.ProjectHealth{
				{Repo: "alice/alpha", Healthy: true},
				{Repo: "alice/beta", Healthy: true},
			},
		}
	}
	quickstart := base()

	hosted := base()
	hosted.Budget = budget.State{State: "on"}
	hosted.Projects[1] = supervisor.ProjectHealth{
		Repo:   "alice/beta",
		Reason: "draining",
		Stuck: []supervisor.StuckJob{{
			RunURL:  "https://github.com/alice/beta/actions/runs/1",
			Job:     "test",
			QueuedS: 720,
		}},
	}

	for _, c := range []struct {
		file string
		snap supervisor.Snapshot
	}{
		{"README.md", quickstart},
		{filepath.Join("docs", "runbook.md"), hosted},
	} {
		doc, err := os.ReadFile(filepath.Join("..", "..", c.file))
		if err != nil {
			t.Fatal(err)
		}
		// The docs indent the block inside a list item; compare line by
		// line with that indent removed.
		want := formatStatus(c.snap)
		if !strings.Contains(dedent(string(doc)), want) {
			t.Errorf("%s does not quote what pool status prints:\n%s", c.file, want)
		}
	}
}

// dedent removes up to three leading spaces from every line, the indent of a
// fenced block inside a numbered list item.
func dedent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		for range 3 {
			if !strings.HasPrefix(l, " ") {
				break
			}
			l = l[1:]
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

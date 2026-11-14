package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

// The README's quickstart and the runbook's "CI went hosted" quote what
// `pool status` prints. A reader compares their own output with those blocks,
// so each must be what formatStatus prints for the state it describes,
// character for character. The quickstart's pool runs without a cap on jobs,
// which is what a plain ./install.sh gives.
func TestDocsQuoteStatusOutput(t *testing.T) {
	base := func() supervisor.Snapshot {
		return supervisor.Snapshot{
			Version: "v0.6.0",
			Commit:  "a4d3acb",
			Machine: "examplemacbo",
			Slots:   3,
			Docker:  true,
			Login:   true,
			Projects: []supervisor.ProjectHealth{
				{Repo: "alice/alpha", Healthy: true},
				{Repo: "alice/beta", Healthy: true},
			},
		}
	}
	quickstart := base()

	starting := base()
	starting.Projects = starting.Projects[:1]
	starting.Projects[0] = supervisor.ProjectHealth{Repo: "alice/alpha", Reason: "starting", Class: "starting"}

	hosted := base()
	hosted.Jobs = supervisor.JobsHealth{Max: 4}
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
		{"README.md", starting},
		{filepath.Join("docs", "runbook.md"), hosted},
	} {
		doc, err := os.ReadFile(filepath.Join("..", "..", c.file))
		if err != nil {
			t.Fatal(err)
		}
		// A block inside a list item is indented with the item; compare
		// with that indent removed too.
		want := formatStatus(c.snap)
		if !strings.Contains(string(doc), want) && !strings.Contains(dedent(string(doc)), want) {
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

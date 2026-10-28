package localcipool

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runBenchReport runs scripts/bench-report.sh over the fixture runs in
// testdata/bench/<dir> (the BENCH_JOBS_DIR seam stands in for gh) and returns
// its standard output, standard error and exit code.
func runBenchReport(t *testing.T, dir string, runs ...string) (string, string, int) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("scripts", "bench-report.sh"))
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := filepath.Abs(filepath.Join("testdata", "bench", dir))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script, "owner/repo"}, runs...)...)
	cmd.Env = append(os.Environ(), "BENCH_JOBS_DIR="+fixtures)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// benchReport is runBenchReport for a report that must succeed.
func benchReport(t *testing.T, dir string, runs ...string) string {
	t.Helper()
	out, errOut, code := runBenchReport(t, dir, runs...)
	if code != 0 {
		t.Fatalf("bench-report.sh exited %d\n%s%s", code, out, errOut)
	}
	return out
}

// Two runs of 3 labels x 2 jobs make n=4 per label, so the medians take the
// lower middle of an even count. Every figure below is worked out by hand from
// the fixtures: queue = started_at - created_at, setup = Set up Go + Download
// modules, in seconds.
func TestBenchReportTable(t *testing.T) {
	want := `| label | n | queue p50 | setup p50 | build p50 | test p50 | test min–max |
|---|---:|---:|---:|---:|---:|---:|
| local-ci-pool-probe-local | 4 | 4 | 12 | 22 | 31 | 30–60 |
| ubuntu-24.04 | 4 | 3 | 6 | 13 | 26 | 25–28 |
| ubuntu-24.04-arm | 4 | 7 | 8 | 16 | 21 | 20–23 |
`
	if got := benchReport(t, "table", "101", "102"); got != want {
		t.Errorf("table mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

// A job that did not succeed is counted in a trailing "failed:" line and kept
// out of every median: the failed local job's 900 s Test step and the
// cancelled arm job's 500 s queue would show if they leaked in.
func TestBenchReportSkipsFailedJobs(t *testing.T) {
	want := `| label | n | queue p50 | setup p50 | build p50 | test p50 | test min–max |
|---|---:|---:|---:|---:|---:|---:|
| local-ci-pool-probe-local | 2 | 4 | 12 | 22 | 30 | 30–34 |
| ubuntu-24.04-arm | 1 | 9 | 9 | 16 | 21 | 21–21 |

failed: local-ci-pool-probe-local 1
failed: ubuntu-24.04-arm 1
`
	if got := benchReport(t, "failed", "201"); got != want {
		t.Errorf("table mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

// A label whose jobs all failed keeps its row, but the table must not pass for
// a valid one: stderr names the label and the exit code is 3.
func TestBenchReportFlagsALabelWithoutASuccessfulJob(t *testing.T) {
	out, errOut, code := runBenchReport(t, "empty", "301")
	wantOut := `| label | n | queue p50 | setup p50 | build p50 | test p50 | test min–max |
|---|---:|---:|---:|---:|---:|---:|
| local-ci-pool-probe-local | 0 | - | - | - | - | - |
| ubuntu-24.04-arm | 1 | 9 | 9 | 16 | 21 | 21–21 |

failed: local-ci-pool-probe-local 1
`
	if out != wantOut {
		t.Errorf("table mismatch\n got:\n%s\nwant:\n%s", out, wantOut)
	}
	if want := "bench-report: local-ci-pool-probe-local has no successful job; its row is empty\n"; errOut != want {
		t.Errorf("stderr = %q, want %q", errOut, want)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

// A run with a queued or in-progress job would give a quiet partial table, so
// the report refuses it and prints nothing.
func TestBenchReportRefusesUnfinishedRuns(t *testing.T) {
	out, errOut, code := runBenchReport(t, "unfinished", "401")
	if out != "" {
		t.Errorf("stdout = %q, want none", out)
	}
	if want := "bench-report: run 401 has unfinished jobs; report it once the run has completed\n"; errOut != want {
		t.Errorf("stderr = %q, want %q", errOut, want)
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

package localcipool

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// benchReport runs scripts/bench-report.sh over the fixture runs in
// testdata/bench/<dir> (the BENCH_JOBS_DIR seam stands in for gh) and returns
// its standard output.
func benchReport(t *testing.T, dir string, runs ...string) string {
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
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bench-report.sh: %v\n%s", err, out)
	}
	return string(out)
}

// Two runs of 3 labels x 2 jobs make n=4 per label, so the medians take the
// lower middle of an even count. Every figure below is worked out by hand from
// the fixtures: queue = started_at - created_at, setup = Set up Go + Download
// modules, in seconds.
func TestBenchReportTable(t *testing.T) {
	want := `| label | n | queue p50 | setup p50 | build p50 | test p50 | test min–max |
|---|---:|---:|---:|---:|---:|---:|
| bench-local | 4 | 4 | 12 | 22 | 31 | 30–60 |
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
| bench-local | 2 | 4 | 12 | 22 | 30 | 30–34 |
| ubuntu-24.04-arm | 1 | 9 | 9 | 16 | 21 | 21–21 |

failed: bench-local 1
failed: ubuntu-24.04-arm 1
`
	if got := benchReport(t, "failed", "201"); got != want {
		t.Errorf("table mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

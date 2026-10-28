#!/usr/bin/env bash
# Print a markdown table of job timings per runs-on label for benchmark runs
# of bench/bench.yml:
#
#   scripts/bench-report.sh OWNER/REPO RUN_ID...
#
# One row per label (the job's first runs-on label): n successful jobs, then
# the median queue time (started_at - created_at of the job), the median of
# Set up Go + Download modules, of Build and of Test, and the Test min-max,
# all in seconds. The median of an even count is the lower middle. A job whose
# conclusion is not success is left out of every figure and counted in a
# trailing "failed: <label> <n>" line. A label with no successful job keeps an
# empty row, and the script says so on stderr and exits 3 after the table. A run
# with a job that has not completed is refused (exit 2).
#
# Needs gh (authenticated for the repository) and jq.
#
# Environment:
#   BENCH_JOBS_DIR  read <RUN_ID>.json from this directory instead of calling
#                   gh; each file is the response of
#                   GET /repos/{owner}/{repo}/actions/runs/{run_id}/jobs
#
# macOS ships bash 3.2, so this script avoids newer bash features.
set -euo pipefail

die() {
  echo "bench-report: $*" >&2
  exit 2
}

[ "$#" -ge 2 ] || die "usage: bench-report.sh OWNER/REPO RUN_ID..."
repo="$1"
shift
case "$repo" in
  */*/* | */ | /* | *[!A-Za-z0-9._/-]*) die "not an OWNER/REPO: $repo" ;;
  */*) ;;
  *) die "not an OWNER/REPO: $repo" ;;
esac
for id in "$@"; do
  case "$id" in
    '' | *[!0-9]*) die "not a run id: $id" ;;
  esac
done
command -v jq > /dev/null || die "jq is required"

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

files=()
for id in "$@"; do
  if [ -n "${BENCH_JOBS_DIR:-}" ]; then
    [ -r "$BENCH_JOBS_DIR/$id.json" ] || die "no $BENCH_JOBS_DIR/$id.json"
    files+=("$BENCH_JOBS_DIR/$id.json")
  else
    command -v gh > /dev/null || die "gh is required"
    # --paginate prints one JSON object per page; jq -s below reads them all.
    gh api --paginate "repos/$repo/actions/runs/$id/jobs?per_page=100" > "$scratch/$id.json" \
      || die "gh api failed for run $id"
    files+=("$scratch/$id.json")
  fi
done

for f in "${files[@]}"; do
  if jq -s -e 'any(.[].jobs[]; .status != "completed")' "$f" > /dev/null; then
    die "run $(basename "$f" .json) has unfinished jobs; report it once the run has completed"
  fi
done

jq -s -r '
  def secs: fromdateiso8601;
  def span: (.completed_at | secs) - (.started_at | secs);
  def step($j; $name):
    ([$j.steps[]? | select(.name == $name)] | first)
      // error("job \($j.name) has no step \($name)") | span;
  def median: sort | .[((length - 1) / 2 | floor)];
  def cell: if length == 0 then "-" else median | tostring end;

  [.[].jobs[]]
  | map(. + {label: (.labels[0] // error("job \(.name) has no label"))})
  | group_by(.label)
  | map({
      label: .[0].label,
      failed: (map(select(.conclusion != "success")) | length),
      ok: (map(select(.conclusion == "success") | {
        queue: ((.started_at | secs) - (.created_at | secs)),
        setup: (step(.; "Set up Go") + step(.; "Download modules")),
        build: step(.; "Build"),
        test: step(.; "Test")
      }))
    }) as $rows
  | "| label | n | queue p50 | setup p50 | build p50 | test p50 | test min–max |",
    "|---|---:|---:|---:|---:|---:|---:|",
    ($rows[] | . as $r | $r.ok as $ok
      | "| \($r.label) | \($ok | length) | \($ok | map(.queue) | cell) | \($ok | map(.setup) | cell) | \($ok | map(.build) | cell) | \($ok | map(.test) | cell) | \(if ($ok | length) == 0 then "-" else "\($ok | map(.test) | min)–\($ok | map(.test) | max)" end) |"),
    (if any($rows[]; .failed > 0) then "" else empty end),
    ($rows[] | select(.failed > 0) | "failed: \(.label) \(.failed)")
' "${files[@]}"

# An empty row looks like a valid table; make it loud.
empty="$(jq -s -r '[.[].jobs[]] | group_by(.labels[0])[] | select(all(.conclusion != "success")) | .[0].labels[0]' "${files[@]}")"
if [ -n "$empty" ]; then
  while IFS= read -r label; do
    echo "bench-report: $label has no successful job; its row is empty" >&2
  done <<< "$empty"
  exit 3
fi

Copy `bench/bench.yml` to `.github/workflows/bench.yml` of a private repository your pool serves, and change `bench-local` in its matrix to that repository's pool label.
Dispatch it (`gh workflow run bench.yml`, or the Actions tab); it runs 5 repetitions on each of the three labels and prints nothing itself.
Report the timings with `scripts/bench-report.sh OWNER/REPO RUN_ID...` (needs `gh` and `jq`): a markdown table per label, in seconds.
`failed:` counts every job that did not succeed (failed, cancelled, skipped); step times have one-second resolution.

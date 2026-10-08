#!/usr/bin/env bash
# Routing decision for local-ci-pool (shared-label mode).
#
# Reads the repository variables as JSON, finds the fresh machine heartbeats
# and writes exactly three lines to $GITHUB_OUTPUT: label, shards and local.
#
# Inputs, all through the environment:
#   VARS_JSON      toJSON(vars)
#   IDENTITY       repository identity, [a-z0-9-]
#   HOSTED_LABEL   label to use when no machine is fresh
#   HOSTED_SHARDS  shard count to use when no machine is fresh
#   MODE           auto | hosted
#   NOW            unix time override (tests only; defaults to the clock)
set -euo pipefail

die() {
  echo "route: $*" >&2
  exit 1
}

command -v jq > /dev/null 2>&1 || die "jq is required but was not found on PATH"

# Every value below ends up in $GITHUB_OUTPUT, so each is validated against a
# strict pattern first: a newline in any of them could inject an output line.
identity="${IDENTITY-}"
hosted_label="${HOSTED_LABEL-}"
hosted_shards="${HOSTED_SHARDS-}"
mode="${MODE-auto}"
now="${NOW-}"
if [[ -z "$now" ]]; then
  now="$(date +%s)"
fi

[[ "$identity" =~ ^[a-z0-9-]{1,40}$ ]] || die "identity must match [a-z0-9-]{1,40}"
[[ "$hosted_label" =~ ^[a-z0-9.-]{1,64}$ ]] || die "hosted-label must match [a-z0-9.-]{1,64}"
[[ "$hosted_shards" =~ ^[1-9][0-9]?$ ]] || die "hosted-shards must be a positive integer below 100"
[[ "$mode" == "auto" || "$mode" == "hosted" ]] || die "mode must be auto or hosted"
[[ "$now" =~ ^[0-9]{1,12}$ ]] || die "NOW must be a unix time in seconds"
[[ -n "${GITHUB_OUTPUT-}" ]] || die "GITHUB_OUTPUT is not set"

fresh=0
total=0

if [[ "$mode" == "auto" ]]; then
  # \A and \z, not ^ and $: with a trailing newline in a key or value, ^...$
  # still matches.
  if ! entries="$(printf '%s' "${VARS_JSON-}" | jq -r '
      to_entries[]
      | select(.key | test("\\ACI_POOL_HB_[A-Z0-9]{1,12}\\z"))
      | select(.value | type == "string")
      | select(.value | test("\\A[0-9]{1,12} [1-8]\\z"))
      | "\(.key) \(.value)"' 2> /dev/null)"; then
    echo "route: vars-json is not a valid JSON object; treating it as no machines" >&2
    entries=""
  fi

  if [[ -n "$entries" ]]; then
    while read -r _ value; do
      epoch="${value% *}"
      slots="${value#* }"
      age=$((10#$now - 10#$epoch))
      # -30 tolerates clock skew between the runner and the machine.
      if ((age >= -30 && age <= 300)); then
        fresh=$((fresh + 1))
        total=$((total + 10#$slots))
      fi
    done <<< "$entries"
  fi
fi

if ((fresh > 0)); then
  label="${identity}-local"
  shards=$((total < 4 ? total : 4))
  is_local=true
else
  label="$hosted_label"
  shards="$hosted_shards"
  is_local=false
fi

printf 'label=%s\nshards=%s\nlocal=%s\n' "$label" "$shards" "$is_local" >> "$GITHUB_OUTPUT"
echo "selected: label=$label shards=$shards local=$is_local"

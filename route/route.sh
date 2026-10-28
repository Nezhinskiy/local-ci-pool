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
#   ROUTE_TEST_NOW unix time override (tests only; defaults to the clock). Not named NOW
#                  so that a caller workflow's own NOW cannot skew freshness.
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
now="${ROUTE_TEST_NOW-}"
if [[ -z "$now" ]]; then
  now="$(date +%s)"
fi

[[ "$identity" =~ ^[a-z0-9-]{1,40}$ ]] || die "identity must match [a-z0-9-]{1,40}"
[[ "$hosted_label" =~ ^[a-z0-9.-]{1,64}$ ]] || die "hosted-label must match [a-z0-9.-]{1,64}"
[[ "$hosted_shards" =~ ^[1-9][0-9]?$ ]] || die "hosted-shards must be a positive integer below 100"
[[ "$mode" == "auto" || "$mode" == "hosted" ]] || die "mode must be auto or hosted"
[[ "$now" =~ ^[0-9]{1,12}$ ]] || die "ROUTE_TEST_NOW must be a unix time in seconds"
[[ -n "${GITHUB_OUTPUT-}" ]] || die "GITHUB_OUTPUT is not set"

# fmt_age SECONDS: whole minutes under an hour, "<h>h <m>m" under a day,
# otherwise "<d>d <h>h". Integer division, no padding.
fmt_age() {
  local s=$1
  if ((s < 3600)); then
    echo "$((s / 60))m"
  elif ((s < 86400)); then
    echo "$((s / 3600))h $((s % 3600 / 60))m"
  else
    echo "$((s / 86400))d $((s % 86400 / 3600))h"
  fi
}

fresh=0
total=0
# The newest entry that passed the key/value filters, fresh or not; it names
# the machine in the fallback annotation.
newest_epoch=-1
newest_key=""

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
    while read -r key value; do
      epoch="${value% *}"
      slots="${value#* }"
      age=$((10#$now - 10#$epoch))
      if ((10#$epoch > newest_epoch)); then
        newest_epoch=$((10#$epoch))
        newest_key="$key"
      fi
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

# Annotations only when auto mode had to fall back. Every interpolated value is
# validated above (the key by the jq filter, the label by its input pattern),
# so no %, : or newline can reach a workflow command.
if [[ "$mode" == "auto" && "$is_local" == "false" ]]; then
  if [[ -z "$newest_key" ]]; then
    printf '::notice title=local-ci-pool::no Mac heartbeat found; running on %s\n' "$label"
  else
    newest_age=$((10#$now - newest_epoch))
    if ((newest_age < 0)); then
      newest_desc="is in the future"
    else
      newest_desc="is $(fmt_age "$newest_age") old"
    fi
    printf '::warning title=local-ci-pool::no fresh Mac heartbeat (the newest, %s, %s); running on %s\n' "$newest_key" "$newest_desc" "$label"
  fi
fi

printf 'label=%s\nshards=%s\nlocal=%s\n' "$label" "$shards" "$is_local" >> "$GITHUB_OUTPUT"
echo "selected: label=$label shards=$shards local=$is_local"

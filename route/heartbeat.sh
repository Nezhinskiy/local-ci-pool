#!/usr/bin/env bash
# Heartbeat writer for local-ci-pool's routing action, for runners that are not
# this pool (deckhand, runscaler, mac-runner, a plain self-hosted runner).
#
#   heartbeat.sh --repo OWNER/NAME --machine NAME --slots N [--check CMD]
#
# It sets the repository variable CI_POOL_HB_<MACHINE> to "<unix epoch> <slots>",
# the value route.sh accepts as a fresh heartbeat for five minutes. Run it every
# two minutes; when the machine goes quiet the routing falls back to the hosted
# label by itself. Pair it with the route action's `local-label` input, set to
# the label your runner registers with.
#
# Public repositories are refused, as they are by the pool itself: a fork PR
# would run on your machine. The check is the repository's visibility, not its
# owner, so a private repository owned by an organization is served. There is
# no override flag.
#
#   --machine NAME  the machine's name; upper-cased, [A-Z0-9]{1,12}
#   --slots N       how many jobs the machine takes at once, 1-8
#   --check CMD     run with `bash -c` first; if it fails nothing is written
#                   (and the exit status is 0), so a machine whose Docker is
#                   down or whose runner app has quit stops advertising itself.
#                   The check runs without the token (GH_TOKEN, GITHUB_TOKEN
#                   and the two ENTERPRISE variants are unset for it), so a
#                   command of yours cannot read or leak it
#
# Authenticate with GH_TOKEN set to a fine-grained personal access token limited
# to that one repository, with "Variables: read and write" (Metadata: read is
# implied). Do not rely on the ambient `gh` login: it is usually a broad
# `repo`-scoped OAuth token, and this script runs unattended.
#
# launchd and cron start with a minimal PATH (/usr/bin:/bin[:/usr/sbin:/sbin]), so
# gh (Homebrew: /opt/homebrew/bin) and docker (/usr/local/bin) are not found
# unless PATH is set below; the script then stops with "gh is required" on every
# run. The token sits in the job's definition, so keep that file private
# (chmod 600) and never put the token in the command's arguments.
#
# launchd, every 120 seconds (/Users/YOU/Library/LaunchAgents/dev.example.heartbeat.plist,
# chmod 600):
#
#   <key>ProgramArguments</key>
#   <array>
#     <string>/path/to/heartbeat.sh</string>
#     <string>--repo</string><string>OWNER/NAME</string>
#     <string>--machine</string><string>MYMAC</string>
#     <string>--slots</string><string>2</string>
#     <string>--check</string><string>docker info >/dev/null</string>
#   </array>
#   <key>EnvironmentVariables</key>
#   <dict>
#     <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
#     <key>GH_TOKEN</key><string>github_pat_...</string>
#   </dict>
#   <key>StandardErrorPath</key><string>/Users/YOU/Library/Logs/heartbeat.err</string>
#   <key>StartInterval</key><integer>120</integer>
#
# cron, every two minutes (crontab -e; the variable lines go at the top of the
# crontab, which is readable only by you):
#
#   PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin
#   GH_TOKEN=github_pat_...
#   */2 * * * * /path/to/heartbeat.sh --repo OWNER/NAME --machine MYMAC --slots 2 --check 'docker info >/dev/null'
#
# HEARTBEAT_TEST_NOW overrides the clock (tests only), like route.sh's
# ROUTE_TEST_NOW.
set -euo pipefail

die() {
  echo "heartbeat: $*" >&2
  exit 2
}

repo=""
machine=""
slots=""
check=""
have_check=false

while (($# > 0)); do
  case "$1" in
    --repo | --machine | --slots | --check)
      (($# >= 2)) || die "$1 needs a value"
      case "$1" in
        --repo) repo="$2" ;;
        --machine) machine="$2" ;;
        --slots) slots="$2" ;;
        --check)
          check="$2"
          have_check=true
          ;;
      esac
      shift 2
      ;;
    *) die "unknown argument: $1" ;;
  esac
done

# tr, not ${machine^^}: macOS still ships bash 3.2.
machine="$(printf '%s' "$machine" | tr '[:lower:]' '[:upper:]')"
now="${HEARTBEAT_TEST_NOW-}"
if [[ -z "$now" ]]; then
  now="$(date +%s)"
fi

[[ "$repo" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "--repo must be OWNER/NAME"
# "." and ".." would turn repos/<repo> into a different API path.
[[ "/$repo/" != */./* && "/$repo/" != */../* ]] || die "--repo must be OWNER/NAME, without . or .. segments"
[[ "$machine" =~ ^[A-Z0-9]{1,12}$ ]] || die "--machine must match [A-Z0-9]{1,12} (case-insensitive)"
[[ "$slots" =~ ^[1-8]$ ]] || die "--slots must be 1 to 8"
[[ "$now" =~ ^[0-9]{1,12}$ ]] || die "HEARTBEAT_TEST_NOW must be a unix time in seconds"
command -v gh > /dev/null 2>&1 || die "gh is required but was not found on PATH"

# Visibility first. Only the literal answer "true" continues; an empty or
# unexpected answer is not taken for private, and not reported as public.
status=0
private="$(gh api "repos/$repo" --jq .private)" || status=$?
if ((status != 0)); then
  exit "$status"
fi
if [[ "$private" == "false" ]]; then
  die "$repo is public; self-hosted runners must not serve public repositories"
elif [[ "$private" != "true" ]]; then
  die "could not tell whether $repo is private; not writing"
fi

if [[ "$have_check" == true ]]; then
  if ! env -u GH_TOKEN -u GITHUB_TOKEN -u GH_ENTERPRISE_TOKEN -u GITHUB_ENTERPRISE_TOKEN bash -c "$check"; then
    echo "heartbeat: check failed; not writing" >&2
    exit 0
  fi
fi

exec gh variable set "CI_POOL_HB_${machine}" --repo "$repo" --body "$now $slots"

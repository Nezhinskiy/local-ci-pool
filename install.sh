#!/usr/bin/env bash
# Install, upgrade or remove the pool on this Mac.
#
#   install.sh [--version vX.Y.Z] [--dry-run]
#   install.sh --uninstall [--dry-run]
#
# The release archive is downloaded with gh and its build provenance is verified
# before anything is placed; a failed verification installs nothing. The script
# writes the binary, one launchd agent and its log directory, and no
# configuration of any kind.
#
# macOS ships bash 3.2, so this script avoids newer bash features.
set -euo pipefail

# sysctl lives in /usr/sbin, which a plain user PATH may lack.
PATH="$PATH:/usr/sbin:/sbin"

REPO="Nezhinskiy/local-ci-pool"
LABEL="com.local-ci-pool.pool"

SUPPORT_DIR="$HOME/Library/Application Support/local-ci-pool"
BIN_DIR="$SUPPORT_DIR/bin"
BIN="$BIN_DIR/pool"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG_DIR="$HOME/Library/Logs/local-ci-pool"
DOMAIN="gui/$(id -u)"

# A stopping pool drains for at most 40 minutes plus 3 to clean up. The
# installer sends it SIGTERM itself and waits up to WAIT_LIMIT for its process
# to exit, then as long again for launchd to let go of the agent and for the
# health address to come free. launchd is not trusted with the drain: macOS
# clamps a LaunchAgent's ExitTimeOut to 60 s (the plist's 2700 is reported as
# "exit timeout = 60" by launchctl print on macOS 27.0.1), and SIGKILLs the
# pool then. The knobs below exist so that the tests do not wait for real.
WAIT_LIMIT="${LOCAL_CI_INSTALL_WAIT_LIMIT:-2760}"
WAIT_POLL="${LOCAL_CI_INSTALL_POLL:-2}"
WAIT_PROGRESS="${LOCAL_CI_INSTALL_PROGRESS:-60}"
HEALTH_POLL="${LOCAL_CI_INSTALL_HEALTH_POLL:-2}"

DRY=0
UNINSTALL=0
VERSION=""
# NEW_VERSION is the version the downloaded binary reports; /healthz must
# report the same once it is loaded.
NEW_VERSION=""
TMP=""
# STOPPED is 1 from the moment the installer asks the old pool to stop until
# the new one is loaded; a failure in between leaves no pool running.
STOPPED=0

say() { printf '%s\n' "$*"; }
warn() { printf 'install.sh: %s\n' "$*" >&2; }
die() {
  warn "$*"
  exit 1
}

usage() {
  cat <<'EOF'
usage: install.sh [--version vX.Y.Z] [--dry-run]
       install.sh --uninstall [--dry-run]

  --version vX.Y.Z  install this release instead of the latest
  --dry-run         verify the release, then print what would be placed or run
  --uninstall       stop the pool (SIGTERM, then wait until it has finished
                    its jobs and exited, up to 46 minutes), unload it from
                    launchd, then delete this Mac's heartbeat variables
                    (pool forget), then remove the agent and the binary

An upgrade stops the running pool the same way before it replaces anything.
The installer sends the signal itself because launchd gives an agent at most
60 seconds to exit before it kills it, whatever the plist asks for.
EOF
}

# act DESCRIPTION COMMAND [ARG...] prints what it does, and does it unless
# --dry-run was given.
act() {
  local desc="$1"
  shift
  if [ "$DRY" = 1 ]; then
    say "would: $desc"
  else
    say "$desc"
    "$@"
  fi
}

cleanup() {
  local rc=$?
  if [ -n "$TMP" ]; then
    rm -rf "$TMP"
  fi
  rm -f "$BIN.new" "$PLIST.new"
  if [ "$rc" -ne 0 ] && [ "$STOPPED" = 1 ]; then
    warn "local-ci-pool: the old pool is stopped and the new one is not running; rerun install.sh (CI falls back to hosted runners meanwhile)"
  fi
}
trap cleanup EXIT
# Without these, bash 3.2 runs the EXIT trap after a SIGTERM with $? = 0, so
# cleanup would not say that the old pool is stopped, and a SIGINT sent to the
# script alone does not stop it at all. Exiting from the signal's own trap
# hands cleanup the signal's status.
trap 'exit 130' INT
trap 'exit 143' TERM

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --dry-run) DRY=1 ;;
      --uninstall) UNINSTALL=1 ;;
      --version)
        [ $# -ge 2 ] || die "--version needs a value such as v0.1.0"
        VERSION="$2"
        shift
        ;;
      --version=*) VERSION="${1#--version=}" ;;
      -h | --help)
        usage
        exit 0
        ;;
      *)
        usage >&2
        exit 2
        ;;
    esac
    shift
  done
  if [ -n "$VERSION" ] && ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$ ]]; then
    die "--version must look like v0.1.0, not '$VERSION'"
  fi
  if [ "$UNINSTALL" = 1 ] && [ -n "$VERSION" ]; then
    die "--version does not apply to --uninstall"
  fi
  if [ "$(id -u)" = 0 ]; then
    die "do not run this as root: the pool is a per-user launchd agent (gui/<uid>) and uses your gh login"
  fi
}

# machine_name prints the name the pool derives from LocalHostName: lower case,
# letters and digits only, at most 12 characters.
machine_name() {
  local host name
  host="$(scutil --get LocalHostName 2>/dev/null)" || die "cannot read LocalHostName (scutil --get LocalHostName)"
  name="$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9' | cut -c1-12)"
  [ -n "$name" ] || die "LocalHostName '$host' has no letters or digits; set one in System Settings > General > Sharing"
  printf '%s' "$name"
}

# slots_for MEM_BYTES CPUS prints the pool's slot count: memory rounded to the
# nearest GiB (Docker Desktop's VM reports a little under its slider), then
# min((GiB - 2) / 4, CPUs / 2, 8). The pool computes the same number; this copy
# only lets the installer say why it refuses.
slots_for() {
  local mem="$1" cpus="$2" gib rounded by_mem by_cpu slots
  gib=$((1 << 30))
  rounded=$(((mem + gib / 2) / gib))
  by_mem=0
  if [ "$rounded" -ge 2 ]; then
    by_mem=$(((rounded - 2) / 4))
  fi
  by_cpu=$((cpus / 2))
  slots="$by_mem"
  if [ "$by_cpu" -lt "$slots" ]; then slots="$by_cpu"; fi
  if [ "$slots" -gt 8 ]; then slots=8; fi
  printf '%s' "$slots"
}

# check_prerequisites refuses early, before anything is downloaded.
check_prerequisites() {
  [ "$(uname -s)" = Darwin ] || die "the pool runs on macOS only"
  command -v gh > /dev/null 2>&1 || die "gh is not installed; run: brew install gh"
  gh auth status > /dev/null 2>&1 || die "gh is not logged in; run: gh auth login"
  command -v docker > /dev/null 2>&1 || die "docker is not installed; install Docker Desktop"

  local info mem cpus
  info="$(docker info --format '{{.MemTotal}} {{.NCPU}}' 2> /dev/null)" || die "Docker is not answering; start Docker Desktop"
  read -r mem cpus <<< "$info"
  if ! [[ "$mem" =~ ^[0-9]+$ && "$cpus" =~ ^[0-9]+$ ]]; then
    die "unexpected output of docker info: '$info'"
  fi
  SLOTS="$(slots_for "$mem" "$cpus")"
  MACHINE="$(machine_name)"
  if [ "$SLOTS" -lt 1 ]; then
    die "Docker has $((mem / (1 << 30))) GiB of memory and $cpus CPUs, which is too little for one runner slot (6 GB and 2 CPUs); raise Docker Desktop's memory limit"
  fi
  say "machine: $MACHINE ($SLOTS slots; heartbeat variable CI_POOL_HB_$(printf '%s' "$MACHINE" | tr '[:lower:]' '[:upper:]'))"

  # uname -m says x86_64 in a shell running under Rosetta on Apple Silicon;
  # hw.optional.arm64 is 1 on every Apple Silicon Mac, whatever the shell.
  if [ "$(sysctl -n hw.optional.arm64 2> /dev/null || true)" = 1 ]; then
    ARCH=arm64
  else
    case "$(uname -m)" in
      arm64) ARCH=arm64 ;;
      x86_64) ARCH=amd64 ;;
      *) die "unsupported architecture '$(uname -m)'; releases exist for arm64 and x86_64" ;;
    esac
  fi
}

# fetch downloads the release archive for this architecture, verifies its
# provenance and unpacks it into $TMP/x. Nothing outside $TMP is touched.
fetch() {
  TMP="$(mktemp -d)"
  if [ -z "$VERSION" ]; then
    # Name the release once, so that the download and the provenance check
    # below are about the same tag.
    VERSION="$(gh release view --repo "$REPO" --json tagName --jq .tagName)" || die "cannot find the latest release of $REPO"
    if ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$ ]]; then
      die "the latest release is called '$VERSION', which is not a version tag"
    fi
  fi
  say "downloading $VERSION for darwin_$ARCH"
  gh release download "$VERSION" --repo "$REPO" --pattern "*darwin_${ARCH}*" --dir "$TMP/dl"
  set -- "$TMP"/dl/*darwin_"${ARCH}"*
  if [ $# -ne 1 ] || [ ! -f "$1" ]; then
    die "expected exactly one darwin_$ARCH archive in the release"
  fi
  local archive="$1"

  say "verifying the build provenance of $(basename "$archive")"
  # The archive must come from this repository's release workflow, run for
  # this very tag, on a GitHub-hosted runner when this gh can check that.
  local help deny=""
  help="$(gh attestation verify --help 2>&1 || true)"
  case "$help" in
    *--deny-self-hosted-runners*) deny="--deny-self-hosted-runners" ;;
  esac
  if ! gh attestation verify "$archive" --repo "$REPO" \
    --signer-workflow "$REPO/.github/workflows/release.yml" \
    --source-ref "refs/tags/$VERSION" ${deny:+"$deny"}; then
    die "the attestation of $(basename "$archive") did not verify; nothing was installed"
  fi

  mkdir "$TMP/x"
  tar -xzf "$archive" -C "$TMP/x"
  [ -f "$TMP/x/pool" ] || die "the archive has no pool binary"
  [ -f "$TMP/x/launchd/$LABEL.plist.tmpl" ] || die "the archive has no launchd template"
  chmod 0755 "$TMP/x/pool"
  local release
  release="$("$TMP/x/pool" version)"
  NEW_VERSION="${release%% *}"
  [ -n "$NEW_VERSION" ] || die "the downloaded pool does not report its version"
  say "release: $release"
}

# stage_binary and stage_plist prepare the new files next to the old ones, so
# that everything that can fail does so before the running pool is touched.
stage_binary() {
  mkdir -p "$BIN_DIR"
  install -m 0755 "$TMP/x/pool" "$BIN.new"
}

# commit_staged is the only step between the unload and the load: two renames.
commit_staged() {
  mv -f "$BIN.new" "$BIN"
  mv -f "$PLIST.new" "$PLIST"
}

# xml_sed_escape makes a value safe inside an XML text node and as the
# replacement text of a sed s||| command.
xml_sed_escape() {
  printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/[\\|&]/\\&/g'
}

stage_plist() {
  mkdir -p "$(dirname "$PLIST")" "$LOG_DIR"
  sed -e "s|@BIN@|$(xml_sed_escape "$BIN")|g" \
    -e "s|@LOG_DIR@|$(xml_sed_escape "$LOG_DIR")|g" \
    "$TMP/x/launchd/$LABEL.plist.tmpl" > "$PLIST.new"
  chmod 0644 "$PLIST.new"
  if command -v plutil > /dev/null 2>&1 && ! plutil -lint "$PLIST.new" > /dev/null; then
    rm -f "$PLIST.new"
    die "the rendered agent is not a valid property list; nothing was changed"
  fi
}

# agent_pid prints the pid launchd reports for the agent ("pid = N" in
# launchctl print), or nothing when the agent is not loaded or not running. It
# never fails: a print of an agent that is not loaded exits non-zero.
agent_pid() {
  launchctl print "$DOMAIN/$LABEL" 2> /dev/null | sed -n 's/^[[:space:]]*pid = \([0-9][0-9]*\)[[:space:]]*$/\1/p' | head -n 1 || true
}

# stop_pool sends the running pool SIGTERM and waits until its process has
# exited, for at most WAIT_LIMIT seconds, saying so every WAIT_PROGRESS
# seconds. The pool then finishes its running jobs (up to 40 minutes, plus 3 to
# clean up), deletes its scale sets and exits 0, which launchd does not restart
# (KeepAlive SuccessfulExit=false). The signal comes from here and not from
# launchctl bootout: launchd clamps a LaunchAgent's exit timeout to 60 s and
# SIGKILLs a pool still draining after that, losing its running jobs.
stop_pool() {
  local pid began next waited
  pid="$(agent_pid)"
  if [ -z "$pid" ]; then
    if launchctl print "$DOMAIN/$LABEL" 2> /dev/null | grep -q 'state = running'; then
      warn "launchd reports the pool running but no pid; unloading it with launchctl bootout, which gives it at most 60 s to finish its jobs"
    else
      say "no pool is running"
    fi
    return 0
  fi
  say "sending the pool (pid $pid) SIGTERM; it finishes its running jobs, then exits"
  if ! kill -TERM "$pid" 2> /dev/null; then
    kill -0 "$pid" 2> /dev/null || return 0 # it exited meanwhile
    die "cannot signal the pool (pid $pid); nothing was changed"
  fi
  began="$SECONDS"
  next="$WAIT_PROGRESS"
  while kill -0 "$pid" 2> /dev/null; do
    waited=$((SECONDS - began))
    if [ "$waited" -ge "$WAIT_LIMIT" ]; then
      STOPPED=0 # the pool is still running, so the "stopped" line would be false
      die "the pool (pid $pid) is still finishing its jobs ${waited}s after it was asked to stop. Nothing was replaced or removed. It exits once its drain ends, and launchd does not restart it then (workflows fall back to hosted runners). Do not unload it with launchctl bootout, which kills it within 60 s; check $LOG_DIR/pool.log, then run this script again"
    fi
    if [ "$waited" -ge "$next" ]; then
      say "still waiting for the pool to finish its jobs and exit (${waited}s)"
      next=$((next + WAIT_PROGRESS))
    fi
    sleep "$WAIT_POLL"
  done
  say "the pool has exited"
}

# bootout_agent asks launchd to unload the agent, whose process stop_pool has
# already seen exit, so this is quick. It still does not rely on the call to
# block: wait_gone is what decides. "Not loaded" is the normal case on a first
# install.
bootout_agent() {
  local err
  if ! err="$(launchctl bootout "$DOMAIN/$LABEL" 2>&1)"; then
    case "$err" in
      *"No such process"* | *"Could not find"*) ;;
      *) warn "launchctl bootout: $err" ;;
    esac
  fi
}

# wait_gone polls launchd until it no longer knows the agent, for at most
# WAIT_LIMIT seconds, and says so every WAIT_PROGRESS seconds.
wait_gone() {
  local began="$SECONDS" next="$WAIT_PROGRESS" waited
  while launchctl print "$DOMAIN/$LABEL" > /dev/null 2>&1; do
    waited=$((SECONDS - began))
    if [ "$waited" -ge "$WAIT_LIMIT" ]; then
      STOPPED=0 # the message below says what is running
      die "launchd still has $DOMAIN/$LABEL ${waited}s after the bootout. No installed file was changed, and the pool it ran was asked to stop, so nothing serves this Mac (workflows fall back to hosted runners). Check 'launchctl print $DOMAIN/$LABEL' and $LOG_DIR/pool.log, then run this script again"
    fi
    if [ "$waited" -ge "$next" ]; then
      say "still waiting for launchd to unload the agent (${waited}s)"
      next=$((next + WAIT_PROGRESS))
    fi
    sleep "$WAIT_POLL"
  done
}

# wait_port_free polls the health address until nothing accepts a connection
# on it, for at most WAIT_LIMIT seconds, saying so every WAIT_PROGRESS seconds.
# launchd can stop listing an agent whose process is still draining; a new pool
# started then cannot bind the address, exits with "terminal: already running"
# and is not restarted. curl's exit status 7 is "could not connect".
wait_port_free() {
  local addr="$1" began="$SECONDS" next="$WAIT_PROGRESS" waited rc
  while :; do
    rc=0
    curl -s -o /dev/null --max-time 2 "http://$addr/healthz" > /dev/null 2>&1 || rc=$?
    if [ "$rc" = 7 ]; then
      return 0
    fi
    waited=$((SECONDS - began))
    if [ "$waited" -ge "$WAIT_LIMIT" ]; then
      STOPPED=0 # a process still holds the address, so the "stopped" line would be false
      die "something still answers on $addr ${waited}s after launchd let go of the pool: the old pool may still be finishing its jobs. No installed file was changed, but nothing restarts the pool once it exits. Check 'lsof -iTCP@$addr -sTCP:LISTEN' and $LOG_DIR/pool.log, then run this script again"
    fi
    if [ "$waited" -ge "$next" ]; then
      say "still waiting for the old pool to release $addr (${waited}s)"
      next=$((next + WAIT_PROGRESS))
    fi
    sleep "$WAIT_POLL"
  done
}

# bootstrap_agent loads the agent. A short retry covers launchd still tearing
# the old job down after it stopped being visible.
bootstrap_agent() {
  local attempt=0
  until launchctl bootstrap "$DOMAIN" "$PLIST"; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 5 ]; then
      die "launchctl bootstrap $DOMAIN $PLIST failed"
    fi
    sleep "$WAIT_POLL"
  done
}

# health_addr prints the address the pool serves /healthz on, as the given
# pool binary reports it.
health_addr() {
  local addr
  addr="$("$1" print-defaults 2> /dev/null | sed -n 's/.*"health_addr":"\([^"]*\)".*/\1/p')"
  [ -n "$addr" ] || die "cannot read the health address from '$1 print-defaults'"
  printf '%s' "$addr"
}

# log_size prints the size of the pool's log, so that what the new pool
# writes can be told from what earlier runs wrote.
log_size() {
  if [ -f "$LOG_DIR/pool.log" ]; then
    wc -c < "$LOG_DIR/pool.log" | tr -d ' '
  else
    printf '0'
  fi
}

# terminal_since prints the first "terminal:" line the pool logged after byte
# offset $1, if any.
terminal_since() {
  [ -f "$LOG_DIR/pool.log" ] || return 0
  tail -c +"$(($1 + 1))" "$LOG_DIR/pool.log" | grep -m 1 'terminal: ' || true
}

# wait_new_pool waits until /healthz reports the version just installed. An
# answer from another version means an old pool still holds the address; a
# "terminal:" line in the log since the load means the new pool gave up (it
# exits 0, so launchd does not restart it). Either way the installer says so
# and restarts the agent once with launchctl kickstart -k. A first start
# downloads the runner and builds images, so a slow answer is not a failure.
wait_new_pool() {
  local addr="$1" want="$2" mark="$3" kicked=0 body got problem
  for _ in $(seq 1 30); do
    got=""
    if body="$(curl -sS --max-time 2 "http://$addr/healthz" 2> /dev/null)"; then
      got="$(printf '%s' "$body" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"
    fi
    if [ "$got" = "$want" ]; then
      say "the pool $want answers on http://$addr/healthz"
      return 0
    fi
    problem=""
    if [ -n "$got" ]; then
      problem="http://$addr/healthz reports version $got, not $want: an old pool still answers there"
    else
      problem="$(terminal_since "$mark")"
      [ -z "$problem" ] || problem="the new pool stopped at once: $problem"
    fi
    if [ -n "$problem" ]; then
      if [ "$kicked" = 1 ]; then
        warn "WARNING: $problem, even after a restart. The new pool is not serving; read $LOG_DIR/pool.log, stop whatever holds $addr, then run: launchctl kickstart -k $DOMAIN/$LABEL"
        return 1
      fi
      warn "WARNING: $problem. Restarting the agent once: launchctl kickstart -k $DOMAIN/$LABEL"
      mark="$(log_size)"
      launchctl kickstart -k "$DOMAIN/$LABEL" || warn "launchctl kickstart failed"
      kicked=1
    fi
    sleep "$HEALTH_POLL"
  done
  warn "the pool does not answer on http://$addr/healthz yet; read $LOG_DIR/pool.log"
}

install_pool() {
  check_prerequisites
  fetch
  local addr
  addr="$(health_addr "$TMP/x/pool")"
  # Everything that can fail without touching the running pool comes first:
  # the new binary and the checked plist are staged beside the old files. After
  # the unload only two renames and the load remain.
  act "stage the binary as $BIN.new" stage_binary
  act "render the launchd agent as $PLIST.new (logs in $LOG_DIR) and check it with plutil" stage_plist
  if [ "$DRY" != 1 ]; then
    STOPPED=1
  fi
  act "stop a running pool, if any: kill -TERM <the pid launchctl print reports>, then wait until it has exited (it finishes its jobs first, up to 43 minutes; gives up after $WAIT_LIMIT s and changes nothing)" stop_pool
  act "unload the agent: launchctl bootout $DOMAIN/$LABEL" bootout_agent
  act "wait until launchd no longer lists it (gives up after $WAIT_LIMIT s)" wait_gone
  act "wait until nothing answers on $addr (gives up after $WAIT_LIMIT s)" wait_port_free "$addr"
  act "replace $BIN and $PLIST with the staged files" commit_staged
  local mark
  mark="$(log_size)"
  act "load the agent: launchctl bootstrap $DOMAIN $PLIST" bootstrap_agent
  STOPPED=0
  if [ "$DRY" = 1 ]; then
    say "would: wait for http://$addr/healthz to report $NEW_VERSION"
  else
    wait_new_pool "$addr" "$NEW_VERSION" "$mark"
  fi
}

forget_heartbeats() {
  if ! "$BIN" forget; then
    warn "could not delete the heartbeat variables; they go stale within five minutes and workflows then use hosted runners"
  fi
}

remove_files() {
  rm -f "$PLIST" "$BIN" "$BIN.new"
  rmdir "$BIN_DIR" "$SUPPORT_DIR" 2> /dev/null || true
}

# uninstall_pool stops the pool first and waits until its process has exited
# and launchd has let go of it: the heartbeat dies with the process, so only
# then does deleting the variables stick.
uninstall_pool() {
  act "stop the pool, if it runs: kill -TERM <the pid launchctl print reports>, then wait until it has exited (it finishes its jobs first, up to 43 minutes; gives up after $WAIT_LIMIT s and changes nothing)" stop_pool
  act "unload the agent: launchctl bootout $DOMAIN/$LABEL" bootout_agent
  act "wait until launchd no longer lists it (gives up after $WAIT_LIMIT s)" wait_gone
  if [ -x "$BIN" ]; then
    act "delete this Mac's heartbeat variables ($BIN forget)" forget_heartbeats
  else
    warn "no binary at $BIN; skipping the heartbeat cleanup (the variables go stale within five minutes)"
  fi
  act "remove $PLIST and $BIN" remove_files
  say "left in place: logs in $LOG_DIR and the cache in $HOME/Library/Caches/local-ci-pool"
}

main() {
  parse_args "$@"
  if [ "$UNINSTALL" = 1 ]; then
    uninstall_pool
  else
    install_pool
  fi
}

main "$@"

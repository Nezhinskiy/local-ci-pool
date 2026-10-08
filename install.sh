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

REPO="Nezhinskiy/local-ci-pool"
LABEL="com.local-ci-pool.pool"

SUPPORT_DIR="$HOME/Library/Application Support/local-ci-pool"
BIN_DIR="$SUPPORT_DIR/bin"
BIN="$BIN_DIR/pool"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG_DIR="$HOME/Library/Logs/local-ci-pool"
DOMAIN="gui/$(id -u)"

DRY=0
UNINSTALL=0
VERSION=""
TMP=""

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
  --uninstall       delete this Mac's heartbeat variables, stop the pool and
                    remove the agent and the binary
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
  if [ -n "$TMP" ]; then
    rm -rf "$TMP"
  fi
}
trap cleanup EXIT

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

  case "$(uname -m)" in
    arm64) ARCH=arm64 ;;
    x86_64) ARCH=amd64 ;;
    *) die "unsupported architecture '$(uname -m)'; releases exist for arm64 and x86_64" ;;
  esac
}

# fetch downloads the release archive for this architecture, verifies its
# provenance and unpacks it into $TMP/x. Nothing outside $TMP is touched.
fetch() {
  TMP="$(mktemp -d)"
  local what="${VERSION:-the latest release}"
  say "downloading $what for darwin_$ARCH"
  if [ -n "$VERSION" ]; then
    gh release download "$VERSION" --repo "$REPO" --pattern "*darwin_${ARCH}*" --dir "$TMP/dl"
  else
    gh release download --repo "$REPO" --pattern "*darwin_${ARCH}*" --dir "$TMP/dl"
  fi
  set -- "$TMP"/dl/*darwin_"${ARCH}"*
  if [ $# -ne 1 ] || [ ! -f "$1" ]; then
    die "expected exactly one darwin_$ARCH archive in the release"
  fi
  local archive="$1"

  say "verifying the build provenance of $(basename "$archive")"
  if ! gh attestation verify "$archive" --repo "$REPO"; then
    die "the attestation of $(basename "$archive") did not verify; nothing was installed"
  fi

  mkdir "$TMP/x"
  tar -xzf "$archive" -C "$TMP/x"
  [ -f "$TMP/x/pool" ] || die "the archive has no pool binary"
  [ -f "$TMP/x/launchd/$LABEL.plist.tmpl" ] || die "the archive has no launchd template"
  chmod 0755 "$TMP/x/pool"
  say "release: $("$TMP/x/pool" version)"
}

place_binary() {
  mkdir -p "$BIN_DIR"
  install -m 0755 "$TMP/x/pool" "$BIN.new"
  mv -f "$BIN.new" "$BIN"
}

# xml_sed_escape makes a value safe inside an XML text node and as the
# replacement text of a sed s||| command.
xml_sed_escape() {
  printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/[\\|&]/\\&/g'
}

render_plist() {
  mkdir -p "$(dirname "$PLIST")" "$LOG_DIR"
  sed -e "s|@BIN@|$(xml_sed_escape "$BIN")|g" \
    -e "s|@LOG_DIR@|$(xml_sed_escape "$LOG_DIR")|g" \
    "$TMP/x/launchd/$LABEL.plist.tmpl" > "$PLIST.new"
  chmod 0644 "$PLIST.new"
  mv -f "$PLIST.new" "$PLIST"
  if command -v plutil > /dev/null 2>&1; then
    plutil -lint "$PLIST" > /dev/null || die "the rendered $PLIST is not a valid property list"
  fi
}

# restart_agent stops a running pool (bootout waits for its drain, which can
# take up to the plist's ExitTimeOut) and loads the agent again.
restart_agent() {
  launchctl bootout "$DOMAIN/$LABEL" 2> /dev/null || true
  local attempt=0
  until launchctl bootstrap "$DOMAIN" "$PLIST"; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 5 ]; then
      die "launchctl bootstrap $DOMAIN $PLIST failed"
    fi
    sleep 2
  done
}

# wait_healthy reports whether the pool answers. A first start downloads the
# runner and builds images, so a slow answer is not a failure.
wait_healthy() {
  local addr
  addr="$("$BIN" print-defaults 2> /dev/null | sed -n 's/.*"health_addr":"\([^"]*\)".*/\1/p')"
  addr="${addr:-127.0.0.1:8737}"
  for _ in $(seq 1 30); do
    if curl -fsS --max-time 2 "http://$addr/healthz" > /dev/null 2>&1; then
      say "the pool answers on http://$addr/healthz"
      return 0
    fi
    sleep 2
  done
  warn "the pool does not answer on http://$addr/healthz yet; read $LOG_DIR/pool.log"
}

install_pool() {
  check_prerequisites
  fetch
  act "place the binary at $BIN" place_binary
  act "render the launchd agent at $PLIST (logs in $LOG_DIR)" render_plist
  act "stop a running pool, if any (waits for its jobs, up to 45 minutes), then load $DOMAIN/$LABEL" restart_agent
  if [ "$DRY" = 1 ]; then
    say "would: wait for http://127.0.0.1:8737/healthz"
  else
    wait_healthy
  fi
}

forget_heartbeats() {
  if ! "$BIN" forget; then
    warn "could not delete the heartbeat variables; they go stale within five minutes and workflows then use hosted runners"
  fi
}

stop_agent() {
  launchctl bootout "$DOMAIN/$LABEL" 2> /dev/null || true
}

remove_files() {
  rm -f "$PLIST" "$BIN" "$BIN.new"
  rmdir "$BIN_DIR" "$SUPPORT_DIR" 2> /dev/null || true
}

uninstall_pool() {
  if [ -x "$BIN" ]; then
    act "delete this Mac's heartbeat variables ($BIN forget)" forget_heartbeats
  else
    warn "no binary at $BIN; skipping the heartbeat cleanup (the variables go stale within five minutes)"
  fi
  act "stop the pool (waits for its jobs, up to 45 minutes) and unload $DOMAIN/$LABEL" stop_agent
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

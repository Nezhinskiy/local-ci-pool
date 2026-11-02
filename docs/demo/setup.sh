# Sourced by demo.tape (inside its Hide block) from the repository root; not executed.
#
# Everything runs in a scratch directory with a scratch HOME, so nothing of the real machine, its
# pool or its launchd is read or written. Names are the neutral ones of CONTRIBUTING.md.

DEMO_REPO="$PWD"
DEMO_DIR="$(mktemp -d "${TMPDIR:-/tmp}/local-ci-pool-demo.XXXXXX")"
DEMO_PORT=18477

mkdir -p "$DEMO_DIR/bin" "$DEMO_DIR/home" "$DEMO_DIR/work"

# The real pool binary, built from this checkout (before HOME changes: go needs its own caches).
go build -o "$DEMO_DIR/bin/pool" ./cmd/pool

# The real routing script, linked into the working directory.
ln -s "$DEMO_REPO/route/route.sh" "$DEMO_DIR/work/route.sh"

# A fixture standing in for the pool's health endpoint.
python3 -I "$DEMO_REPO/docs/demo/fake-healthz.py" "$DEMO_PORT" "$DEMO_REPO/docs/demo/healthz.json" &
DEMO_PID=$!
disown

export HOME="$DEMO_DIR/home"
unset XDG_CONFIG_HOME
export PATH="$DEMO_DIR/bin:/usr/bin:/bin:/opt/homebrew/bin"
export PS1='$ '

# The route step's inputs, as the action passes them. ROUTE_TEST_NOW pins the clock so the
# heartbeat ages below are exact; the script's output goes to the terminal instead of a file.
export IDENTITY=alpha HOSTED_LABEL=ubuntu-24.04-arm HOSTED_SHARDS=4
export ROUTE_TEST_NOW=1793000000 GITHUB_OUTPUT=/dev/stdout

# The Mac goes to sleep: nothing answers on the health address any more.
sleep_mac() { kill "$DEMO_PID" 2>/dev/null; wait "$DEMO_PID" 2>/dev/null; }

# Remove the scratch directory.
demo_cleanup() { sleep_mac; rm -rf "$DEMO_DIR"; }

cd "$DEMO_DIR/work"

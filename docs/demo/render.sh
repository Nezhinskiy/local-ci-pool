#!/usr/bin/env bash
# Renders docs/demo/demo.gif from docs/demo/demo.tape. Needs vhs (with ttyd and ffmpeg), go, python3
# and jq. Run it from anywhere; pass a path to write the GIF elsewhere.
set -euo pipefail
cd "$(dirname "$0")/../.."
out="${1:-docs/demo/demo.gif}"
vhs --quiet --output "$out" docs/demo/demo.tape
ls -l "$out"

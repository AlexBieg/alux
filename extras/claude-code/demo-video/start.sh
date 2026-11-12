#!/usr/bin/env bash
# Usage: start.sh OUT.mp4 [URL] — sets a 16:10 viewport and records to OUT.raw.webm, which finish.sh turns into OUT.mp4.
set -euo pipefail
out=${1:?usage: start.sh OUT.mp4 [URL]}
[[ $out == *.mp4 ]] || { echo "OUT must end in .mp4" >&2; exit 2; }
agent-browser set viewport "${DEMO_WIDTH:-1280}" "${DEMO_HEIGHT:-800}" >/dev/null
rm -f "${TMPDIR:-/tmp}/demo-video-pointer"
agent-browser record start "${out%.mp4}.raw.webm" ${2:+"$2"}

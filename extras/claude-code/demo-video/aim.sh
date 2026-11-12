#!/usr/bin/env bash
# Usage: aim.sh @REF — draws the pointer, scrolls REF into view and glides the pointer onto it; run before each click or fill.
set -euo pipefail
ref=${1:?usage: aim.sh @REF}
here=$(cd "$(dirname "$0")" && pwd)
state=${TMPDIR:-/tmp}/demo-video-pointer

drawn=$(agent-browser eval "$(cat "$here/cursor.js")")
agent-browser scrollintoview "$ref" >/dev/null
read -r x y < <(agent-browser --json get box "$ref" | tail -n 1 |
  python3 -c 'import json,sys; b=json.load(sys.stdin)["data"]; print(round(b["x"]+b["width"]/2), round(b["y"]+b["height"]/2))')

# A freshly loaded page has lost the pointer, so put it back where it was before gliding.
if [[ $drawn == *fresh* && -s $state ]]; then
  read -r px py < "$state"
  agent-browser mouse move "$px" "$py" >/dev/null
fi
agent-browser mouse move "$x" "$y" >/dev/null
echo "$x $y" > "$state"
sleep "${DEMO_GLIDE:-0.4}"

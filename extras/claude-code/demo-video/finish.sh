#!/usr/bin/env bash
# Usage: finish.sh OUT.mp4 — stops the recording start.sh began and saves OUT.mp4 with still stretches cut, OUT-full.mp4 uncut, and OUT-frames.png.
set -euo pipefail
out=${1:?usage: finish.sh OUT.mp4}
base=${out%.mp4}
raw=$base.raw.webm

agent-browser eval '(() => { window.__demoCursorOn = false; document.getElementById("__demo_cursor")?.remove(); })()' >/dev/null || true
agent-browser record stop >/dev/null

# Stillness of 2 s or more is cut down to its first 1 s; under 2 s of cuts in total is not worth it.
# The default -60dB threshold misses a moving pointer or typed text on a full page and cuts the action.
spans=$(ffmpeg -hide_banner -nostats -i "$raw" -an -vf "freezedetect=n=-90dB:d=2,metadata=mode=print:file=-" -f null - 2>/dev/null |
  awk -F= '/freeze_start/ { s = $2 + 1 }
           /freeze_end/ && $2 > s { e = e sep sprintf("gte(t,%.3f)*lt(t,%.3f)", s, $2); sep = "+"; cut += $2 - s }
           END { if (cut >= 2) print e }')

encode() {
  # yuv420p, which players need, takes only even dimensions.
  ffmpeg -v error -y -i "$raw" -an -vf "${1:+$1,}scale=trunc(iw/2)*2:trunc(ih/2)*2" \
    -c:v libx264 -preset veryfast -crf 20 -pix_fmt yuv420p -movflags +faststart "$2"
}

if [[ -n $spans ]]; then
  encode "select='not($spans)',setpts=N/FRAME_RATE/TB" "$out"
  encode "" "$base-full.mp4"
  echo "Saved $out with still stretches cut; uncut copy at $base-full.mp4"
else
  encode "" "$out"
  echo "Saved $out"
fi
rm -f "$raw"

seconds=$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$out")
ffmpeg -v error -y -i "$out" -vf "fps=9/$seconds,scale=640:-1,tile=3x3" -frames:v 1 "$base-frames.png"
echo "Frame sheet (9 frames across ${seconds%.*} s, in order): $base-frames.png"

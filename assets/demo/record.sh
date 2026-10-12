#!/usr/bin/env bash
# record.sh: regenerate assets/crash-demo.gif, the README's hero: the bide-demo terminal
# (crash-demo.tape, recorded with charmbracelet/vhs) followed by the run's bide-view page
# (a headless Chromium screenshot, scrolled from the audit proof down to the timeline).
#
#   assets/demo/record.sh
#
# Needs vhs (which brings ttyd and ffmpeg) and a Chromium-based browser; set CHROME to its
# binary if it is not found.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$work"' EXIT

for c in "${CHROME:-}" \
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  "/Applications/Chromium.app/Contents/MacOS/Chromium" \
  "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge" \
  "$(command -v chromium || true)" "$(command -v google-chrome || true)"; do
  [ -n "$c" ] && [ -x "$c" ] && { CHROME=$c; break; }
done
[ -n "${CHROME:-}" ] || { echo "record.sh: no Chromium-based browser found; set CHROME" >&2; exit 1; }

(cd "$root/cmd/bide-demo" && go build -o "$work/bin/bide-demo" .)

# 1. The terminal.
(cd "$work" && PATH="$work/bin:$PATH" vhs "$here/crash-demo.tape")

# 2. The page: a second run, served on another port, captured in dark mode.
"$work/bin/bide-demo" -fast -no-browser -addr 127.0.0.1:18487 > "$work/view.log" 2>&1 &
for _ in $(seq 50); do grep -q "Ctrl-C to quit" "$work/view.log" && break; sleep 0.2; done
"$CHROME" --headless=new --disable-gpu --hide-scrollbars --force-dark-mode --window-size=1100,1840 \
  --screenshot="$work/view.png" "http://127.0.0.1:18487/run?id=order-42" > /dev/null 2>&1

# 3. Hold the page's top (the audit proof and the gate) for 2s, scroll to the timeline's end
# (the crashed attempt and its ResolveHalt result) over 1.5s and hold 2s; then join, at 1100x760.
ffmpeg -loglevel error -y -loop 1 -framerate 24 -t 5.5 -i "$work/view.png" \
  -vf "crop=1100:760:0:'min(1080, max(0, (t-2)/1.5*1080))',format=yuv420p" "$work/view.mp4"
ffmpeg -loglevel error -y -i "$work/crash-demo-terminal.mp4" -i "$work/view.mp4" \
  -filter_complex "[0:v]fps=12,scale=1100:760,setsar=1[a];[1:v]fps=12,setsar=1[b];[a][b]concat=n=2:v=1,split[x][y];[x]palettegen=max_colors=96:stats_mode=diff[p];[y][p]paletteuse=dither=none:diff_mode=rectangle" \
  "$root/assets/crash-demo.gif"
ls -l "$root/assets/crash-demo.gif"

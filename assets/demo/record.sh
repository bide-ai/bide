#!/usr/bin/env bash
# record.sh: regenerate assets/crash-demo.gif, the README's hero. The terminal (crash-demo.tape,
# recorded with charmbracelet/vhs) is on the left and the run's bide-view page on the right: a
# placeholder while the agent runs, the halted run once the restart halts, and the completed,
# verified run once the demo has checked the proofs. The page is captured with a headless
# Chromium; the demo reports when each state is reached (BIDE_DEMO_RECORD).
#
#   assets/demo/record.sh
#
# Needs vhs (which brings ttyd and ffmpeg) and a Chromium-based browser; set CHROME to its binary
# if it is not found.
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

# The panes: the terminal is the tape's size, the page 600x900 (450 CSS pixels wide at 4/3 scale,
# so its text stays legible when the README shows the GIF about 800 pixels wide).
TW=720 H=900 VW=600
ENTER=2.6 # seconds into the recording at which the tape presses Enter

(cd "$root/cmd/bide-demo" && go build -o "$work/bin/bide-demo" .)
(cd "$root/cmd/bide-view" && go build -o "$work/bin/bide-view" .)
mkdir -p "$work/rec"

shot() { # url out: a screenshot of the page's top, the pane's size
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars --force-dark-mode --force-device-scale-factor=1.3333 \
    --window-size=450,675 --screenshot="$2" "$1" > /dev/null 2>&1
  ffmpeg -loglevel error -y -i "$2" -vf "scale=$VW:-1,crop=$VW:$H:0:0" "$2.tmp.png" && mv "$2.tmp.png" "$2"
}
serve() { # log command...: start a server in the background and wait for its address
  local log=$1; shift
  "$@" > "$log" 2>&1 &
  for _ in $(seq 100); do grep -q "http://" "$log" && return; sleep 0.2; done
  echo "record.sh: the server did not start:" >&2; cat "$log" >&2; exit 1
}

# 1. The terminal, with the demo reporting its marks and keeping the halted journal.
(cd "$work" && PATH="$work/bin:$PATH" BIDE_DEMO_RECORD="$work/rec" vhs "$here/crash-demo.tape")
halted=$(awk '$1 == "halted" {print $2}' "$work/rec/marks.txt")
verified=$(awk '$1 == "verified" {print $2}' "$work/rec/marks.txt")

# 2. The page in each state.
cat > "$work/waiting.html" <<'HTML'
<!doctype html><meta charset="utf-8"><style>
body{margin:0;height:100vh;display:flex;flex-direction:column;align-items:center;justify-content:center;
background:#1b1b1f;color:#8b8b93;font:16px -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
b{color:#e6e6ea;font-size:20px;margin-bottom:8px}</style>
<b>bide-view</b><span>the run is in progress ...</span>
HTML
shot "file://$work/waiting.html" "$work/waiting.png"
serve "$work/halted.log" "$work/bin/bide-view" -db "$work/rec/halted.db" -run order-42 -addr 127.0.0.1:18487
shot "http://127.0.0.1:18487/run?id=order-42" "$work/halted.png"
serve "$work/final.log" "$work/bin/bide-demo" -fast -no-browser -addr 127.0.0.1:18488
shot "http://127.0.0.1:18488/run?id=order-42" "$work/final.png"

# 3. Side by side, the page switching state as the terminal reaches it; then a GIF.
# Each still lasts as long as the terminal: palettegen reads its input to the end.
a=$(echo "$ENTER + $halted" | bc) b=$(echo "$ENTER + $verified" | bc)
d=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$work/terminal.mp4")
ffmpeg -loglevel error -y -i "$work/terminal.mp4" -loop 1 -t "$d" -i "$work/waiting.png" -loop 1 -t "$d" -i "$work/halted.png" -loop 1 -t "$d" -i "$work/final.png" \
  -filter_complex "[0:v]scale=$TW:$H,setsar=1,pad=$((TW + VW + 4)):$H:0:0:color=0x44475a[t];\
[t][1:v]overlay=$((TW + 4)):0:shortest=1[w];[w][2:v]overlay=$((TW + 4)):0:enable='gte(t,$a)'[h];\
[h][3:v]overlay=$((TW + 4)):0:enable='gte(t,$b)',fps=12,split[x][y];\
[x]palettegen=max_colors=128:stats_mode=diff[p];[y][p]paletteuse=dither=none:diff_mode=rectangle" \
  "$root/assets/crash-demo.gif"
ls -l "$root/assets/crash-demo.gif"

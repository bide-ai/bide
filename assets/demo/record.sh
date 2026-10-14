#!/usr/bin/env bash
# record.sh: regenerate assets/crash-demo.gif, the README's hero. The terminal (crash-demo.tape,
# recorded with charmbracelet/vhs) is on the left and the run's bide view page on the right, live
# from the first frame: a card for the run without bide, the gate waiting (nothing signed, then
# ops and finance signed, kept through the crash), the halt, the resolved run, the verified run, and
# the tampered copy of the journal failing verification. Each page is a real `bide view` over a copy of the journal at that
# state, captured with a headless Chromium; the demo reports when it reaches each state and keeps
# the copy (BIDE_DEMO_RECORD: marks.txt and <state>.db).
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

# The panes: the terminal is the tape's size, the page 600x1050 (520 CSS pixels wide, scaled by 600/520,
# so its text stays legible when the README shows the GIF about 800 pixels wide).
TW=790 H=1050 VW=600
ENTER=1.1 # seconds into the recording at which the tape presses Enter

(cd "$root/cmd/bide" && go build -o "$work/bin/bide" .)
mkdir -p "$work/rec"

shot() { # url out: a screenshot of the page's top, the pane's size
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars --force-dark-mode --force-device-scale-factor=1.1538 \
    --window-size=520,910 --screenshot="$2" "$1" > /dev/null 2>&1
  ffmpeg -loglevel error -y -i "$2" -vf "scale=$VW:-1,crop=$VW:$H:0:0" "$2.tmp.png" && mv "$2.tmp.png" "$2"
}
serve() { # log command...: start a server in the background and wait for its address
  local log=$1; shift
  "$@" > "$log" 2>&1 &
  for _ in $(seq 100); do grep -q "http://" "$log" && return; sleep 0.2; done
  echo "record.sh: the server did not start:" >&2; cat "$log" >&2; exit 1
}

# 1. The terminal, with the demo reporting its marks and keeping the journal at each state.
(cd "$work" && PATH="$work/bin:$PATH" BIDE_DEMO_RECORD="$work/rec" vhs "$here/crash-demo.tape")

# 2. The page in each state. While the run is without bide there is no journal to view: a static card.
cat > "$work/nobide.html" <<'HTML'
<!doctype html><meta charset="utf-8"><style>
body{margin:0;height:100vh;display:flex;flex-direction:column;align-items:center;justify-content:center;text-align:center;
background:#1b1b1f;color:#8b8b93;font:16px -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;padding:0 32px;box-sizing:border-box}
b{color:#e6e6ea;font-size:20px;margin-bottom:8px}</style>
<b>without bide</b><span>no journal: nothing records that the charge went through</span>
HTML
shot "file://$work/nobide.html" "$work/nobide.png"
# The rest are real bide view pages, each served on its own port.
port=18490
state() { # name db-args...: serve the page and shoot it to <name>.png
  local name=$1; shift
  port=$((port + 1))
  serve "$work/$name.log" "$work/bin/bide" view "$@" -addr 127.0.0.1:$port
  shot "http://127.0.0.1:$port/${URLPATH-run?id=order-42}" "$work/$name.png"
}
for s in pending approved halted resolved; do
  state "$s" -db "$work/rec/$s.db" -run order-42
done
# demo -fast exits when it is done; -keep prints the bide view command that reopens the finished run
# (and leaves the tampered copy of the journal beside it).
"$work/bin/bide" demo -fast -no-browser -keep > "$work/final-demo.log" 2>&1
view=$(sed -n 's/^ *bide view //p' "$work/final-demo.log")
kept=$(sed -n 's/^kept *//p' "$work/final-demo.log")
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$work" "$kept"' EXIT
state verified $view
state tampered ${view/journal.db/tampered.db}
cp "$work/verified.png" "$work/final.png"

# 3. Side by side, the page switching state as the terminal reaches it; then a GIF.
# Each still lasts as long as the terminal: palettegen reads its input to the end.
d=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$work/terminal.mp4")
inputs=() chain="[0:v]scale=$TW:$H,setsar=1,pad=$((TW + VW + 4)):$H:0:0:color=0x44475a[v0];"
inputs+=(-loop 1 -t "$d" -i "$work/nobide.png")
chain+="[v0][1:v]overlay=$((TW + 4)):0:shortest=1[v1];"
n=1
for s in pending approved halted resolved verified tampered final; do
  at=$(awk -v s="$s" '$1 == s {print $2}' "$work/rec/marks.txt")
  [ -n "$at" ] || { echo "record.sh: the demo reported no mark for $s" >&2; exit 1; }
  at=$(echo "$ENTER + $at" | bc)
  inputs+=(-loop 1 -t "$d" -i "$work/$s.png")
  chain+="[v$n][$((n + 1)):v]overlay=$((TW + 4)):0:enable='gte(t,$at)'[v$((n + 1))];"
  n=$((n + 1))
done
chain+="[v$n]fps=12,split[x][y];[x]palettegen=max_colors=128:stats_mode=diff[p];[y][p]paletteuse=dither=none:diff_mode=rectangle"
ffmpeg -loglevel error -y -i "$work/terminal.mp4" "${inputs[@]}" -filter_complex "$chain" "$root/assets/crash-demo.gif"
ls -l "$root/assets/crash-demo.gif"

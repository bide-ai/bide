#!/usr/bin/env bash
# live.sh: regenerate the run viewer guide's live mode images (docs/guides/img/run-viewer-live-*).
# It drives examples/approval step by step against a SQLite journal while bide view serves the run,
# and captures the page as it follows the run: in progress, waiting for approval (none, then one, of
# the two approvals its 2-of-3 gate needs), in progress again once the refund runs, and completed.
# Each capture opens the page, keeps it open while the next step lands, and then photographs it, so it
# shows the page updated in place by its script, with the new timeline rows marked. The frames are
# joined into a GIF.
#
#   assets/demo/live.sh                          # docs/guides/img/run-viewer-live-dark.gif
#   THEME=light FRAMES=/tmp/frames IMG=/tmp/gif assets/demo/live.sh   # light, keeping each frame as a PNG, GIF into /tmp/gif
#
# Needs a Chrome (CHROME_BIN), node 20 or later and ffmpeg.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
img=${IMG:-$root/docs/guides/img}
theme=${THEME:-dark}
port=${PORT:-8593}
work=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null; rm -rf "$work"' EXIT

. "$here/lib.sh"
require_free "$port"
shot_init

(cd "$root/cmd/bide" && go build -o "$work/bide" .)
(cd "$root/examples/approval" && go build -o "$work/approval" .)
cd "$work"
url="http://127.0.0.1:$port/run?id=refund-42"
delay=6s # each model turn is held this long, so the run is seen in progress

wait_for() { # cmd...: retry for up to 30 s
  for _ in $(seq 300); do "$@" > /dev/null 2>&1 && return 0; sleep 0.1; done
  echo "live.sh: timed out waiting for: $*" >&2; exit 1
}
view() { curl -sf "http://127.0.0.1:$port/run.json?id=refund-42"; }
state() { view | grep -q "\"state\": \"$1\""; }

# capture name ms: open the run page in the background, keep it open ms milliseconds (its script
# polling) and photograph it (browser-shots/live-capture.mjs); cap is the capture's PID. Returns once
# the page has loaded, so the step the caller takes next lands while the page is open.
capture() {
  (cd "$(shot_dir)" && node live-capture.mjs "$work/$1.png" "$url" "$2" "$theme") > "$work/$1.log" 2>&1 &
  cap=$!
  for _ in $(seq 300); do grep -q loaded "$work/$1.log" 2>/dev/null && break; sleep 0.1; done
}

# 1. The run starts, and its first model turn is held: in progress.
./approval run -db a.db -turn-delay "$delay" > /dev/null &
run=$!
wait_for test -f a.db
./bide view -db a.db -addr "127.0.0.1:$port" > /dev/null &
wait_for state started
capture 1-in-progress 500
wait "$cap"
# 2. A page opened in progress follows the run to its gate: waiting for approval, 0 of 2.
capture 2-waiting 9000
wait "$run" "$cap"
# 3. One approver decides: 1 of 2.
capture 3-one-approval 4000
./approval approve -db a.db -as ops > /dev/null
wait "$cap"
# 4. The second approver decides and the run resumes: the refund runs, and the closing model turn
#    is held, so the run is in progress again.
capture 4-refunded 5000
./approval approve -db a.db -as finance > /dev/null
./approval run -db a.db -turn-delay "$delay" > /dev/null &
run=$!
wait "$cap"
# 5. The closing turn lands: completed. The page stops following the run.
capture 5-completed 8000
wait "$run" "$cap"

if [ -n "${FRAMES:-}" ]; then
  mkdir -p "$FRAMES"
  for f in "$work"/[1-5]-*.png; do cp "$f" "$FRAMES/run-viewer-live-$(basename "$f" .png)-$theme.png"; done
fi
ffmpeg -loglevel error -y -framerate 0.5 -pattern_type glob -i "$work/[1-5]-*.png" \
  -vf "scale=720:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=96[p];[b][p]paletteuse=dither=none" \
  -loop 0 "$img/run-viewer-live-$theme.gif"
echo "live.sh: wrote $img/run-viewer-live-$theme.gif"

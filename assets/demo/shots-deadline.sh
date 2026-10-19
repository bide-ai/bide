#!/usr/bin/env bash
# shots-deadline.sh: regenerate the run viewer guide's screenshots of gates with a deadline:
# docs/guides/img/run-viewer-deadline-attention (the Needs attention list: a gate overdue, one due
# soon and one due tomorrow) and run-viewer-gate-expired (a run whose gate expired, with one of two
# approvals and a second one written after the expiry, from the approvers down through the timeline). assets/demo/setup/deadline.go writes the journal with the real agent runtime and a
# controllable clock, so the times are relative to now; the pages are captured with a headless
# Chromium-based browser at 1200 pixels wide, as shots.sh does. The pages refresh themselves every
# few seconds, so the capture runs on a virtual clock short of the first refresh.
#
#   assets/demo/shots-deadline.sh              # dark, into docs/guides/img
#   THEME=light assets/demo/shots-deadline.sh  # the light variants, which the guide embeds with <picture>
#
# Needs a Chromium-based browser (set CHROME to its binary if it is not found). The pages hold
# times, so the images differ from run to run in those digits only.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
theme=${THEME:-dark}
out=${OUT:-$root/docs/guides/img}
port=${PORT:-8641}
work=$(mktemp -d /tmp/bide-deadline.XXXXXX) # a short path: the Needs attention list shows it
trap 'kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null; rm -rf "$work"' EXIT

. "$here/lib.sh"
shot_init

(cd "$root/cmd/bide" && go build -o "$work/bide" .)
mkdir -p "$out"
cd "$work"

wait_for() { # url: retry for up to 30 s
  for _ in $(seq 300); do curl -sf -o /dev/null "$1" && return 0; sleep 0.1; done
  echo "shots-deadline.sh: timed out waiting for $1" >&2; exit 1
}
(cd "$root/cmd/bide" && GOWORK=off go run ../../assets/demo/setup/deadline.go -dir "$work")

serve() { # port args...: bide view, read-only, on a loopback port
  local p=$1; shift
  ./bide view "$@" -addr "127.0.0.1:$p" > /dev/null &
  wait_for "http://127.0.0.1:$p/"
}
serve "$port" -db deadline.db
shot run-viewer-deadline-attention "http://127.0.0.1:$port/attention" 1450
shot run-viewer-gate-expired "http://127.0.0.1:$port/run?id=refund-2004" 2800
# Keep the page from the Approvers heading down to the end of the timeline, as a palette PNG.
python3 - "$out/run-viewer-gate-expired-$theme.png" <<'PY'
import sys
from PIL import Image, ImageChops
f = sys.argv[1]
im = Image.open(f).convert("RGB")
bg = Image.new("RGB", im.size, im.getpixel((2, im.height - 2)))
bottom = ImageChops.difference(im, bg).getbbox()[3]
im.crop((0, 650, im.width, min(im.height, bottom + 24))).quantize(colors=256, dither=Image.Dither.NONE).save(f, optimize=True)
PY

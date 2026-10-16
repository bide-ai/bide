#!/usr/bin/env bash
# shots-identity.sh: regenerate the run viewer guide's screenshots of approver identity:
# docs/guides/img/run-viewer-identity-run (a run page: one approver counted final on an independent
# time, one counted only provisionally, shown in a different style, never green) and
# run-viewer-identity-attention (the Needs attention list: gates paused for identity status, waiting
# for a covering signed head, and one with a disputed count). assets/demo/setup/identity.go writes the journal
# with the real agent runtime; the pages are captured with a headless Chromium-based browser at 1200
# pixels wide, as shots.sh does.
#
#   assets/demo/shots-identity.sh              # dark, into docs/guides/img
#   THEME=light assets/demo/shots-identity.sh  # the light variants, which the guide embeds with <picture>
#
# Needs a Chromium-based browser (set CHROME to its binary if it is not found). The pages hold
# times, so the images differ from run to run in those digits only.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
theme=${THEME:-dark}
out=${OUT:-$root/docs/guides/img}
port=${PORT:-8651}
work=$(mktemp -d /tmp/bide-identity.XXXXXX) # a short path: the Needs attention list shows it
trap 'kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null; rm -rf "$work"' EXIT

for c in "${CHROME:-}" \
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  "/Applications/Chromium.app/Contents/MacOS/Chromium" \
  "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge" \
  "$(command -v chromium || true)" "$(command -v google-chrome || true)"; do
  [ -n "$c" ] && [ -x "$c" ] && { CHROME=$c; break; }
done
[ -n "${CHROME:-}" ] || { echo "shots-identity.sh: no Chromium-based browser found; set CHROME" >&2; exit 1; }

(cd "$root/cmd/bide" && go build -o "$work/bide" .)
mkdir -p "$out"
cd "$work"

shot() { # name url height
  local flags=(--headless=new --disable-gpu --hide-scrollbars --virtual-time-budget=3000 --window-size=1200,"$3"
    --user-data-dir="$work/profile-$1" --screenshot="$out/$1-$theme.png")
  if [ "$theme" = dark ]; then flags+=(--force-dark-mode); else flags+=(--blink-settings=preferredColorScheme=1); fi
  "$CHROME" "${flags[@]}" "$2" > /dev/null 2>&1
  echo "$out/$1-$theme.png"
}
wait_for() { # url: retry for up to 30 s
  for _ in $(seq 300); do curl -sf -o /dev/null "$1" && return 0; sleep 0.1; done
  echo "shots-identity.sh: timed out waiting for $1" >&2; exit 1
}
(cd "$root/cmd/bide" && GOWORK=off go run ../../assets/demo/setup/identity.go -dir "$work")

serve() { # port args...: bide view, read-only, on a loopback port
  local p=$1; shift
  ./bide view "$@" -addr "127.0.0.1:$p" > /dev/null &
  wait_for "http://127.0.0.1:$p/"
}
serve "$port" -db identity.db
shot run-viewer-identity-attention "http://127.0.0.1:$port/attention" 1500
shot run-viewer-identity-run "http://127.0.0.1:$port/run?id=refund-3001" 2200
# Keep the page from the Approval gate cards to the Approvers section's end, as a palette PNG.
python3 - "$out/run-viewer-identity-run-$theme.png" <<'PY'
import sys
from PIL import Image, ImageChops
f = sys.argv[1]
im = Image.open(f).convert("RGB")
bg = Image.new("RGB", im.size, im.getpixel((2, im.height - 2)))
bottom = ImageChops.difference(im, bg).getbbox()[3]
im.crop((0, 0, im.width, min(im.height, bottom + 24))).quantize(colors=256, dither=Image.Dither.NONE).save(f, optimize=True)
PY

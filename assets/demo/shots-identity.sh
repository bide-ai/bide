#!/usr/bin/env bash
# shots-identity.sh: regenerate the run viewer guide's screenshots of approver identity:
# docs/guides/img/run-viewer-identity-run (a run page: one approver counted final as recorded on the log
# signer's clock, one counted only provisionally, shown in a different style, never green) and
# run-viewer-identity-attention (the Needs attention list: gates paused for identity status, waiting
# for a covering signed head, and a call paused before execution for current status). cmd/bide/internal/identitydemo writes the journal
# with the real agent runtime and a real certificate-bound gate, so every state shown is one the gate produces; the pages are captured with a headless Chromium-based browser at 1200
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
work=/tmp/bide-identity # a fixed, short path: the Needs attention list shows it, so the images are reproducible
rm -rf "$work" && mkdir -p "$work"
trap 'kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null; rm -rf "$work"' EXIT

. "$here/lib.sh"
shot_init

(cd "$root/cmd/bide" && go build -o "$work/bide" .)
mkdir -p "$out"
cd "$work"

wait_for() { # url: retry for up to 30 s
  for _ in $(seq 300); do curl -sf -o /dev/null "$1" && return 0; sleep 0.1; done
  echo "shots-identity.sh: timed out waiting for $1" >&2; exit 1
}
(cd "$root/cmd/bide" && BIDE_IDENTITY_DEMO_DIR="$work" GOWORK=off go test -tags identitydemo ./internal/identitydemo -run TestWriteDemoJournal -count=1 > /dev/null)

serve() { # port args...: bide view, read-only, on a loopback port
  local p=$1; shift
  ./bide view "$@" -addr "127.0.0.1:$p" > /dev/null &
  wait_for "http://127.0.0.1:$p/"
}
serve "$port" -db identity.db
shot run-viewer-identity-attention "http://127.0.0.1:$port/attention" 1900
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

#!/usr/bin/env bash
# shots-approval.sh: regenerate the run viewer guide's screenshots of the examples/approval run:
# docs/guides/img/run-viewer-agent-halted (the halt, with the gate approved 2 of 3),
# run-viewer-agent-verified (after ResolveHalt, with the evidence verifying) and
# run-viewer-timeline (the timeline, with the times the journal records) and run-viewer-share (the card
# the page's Share this run button saves for the verified run). It drives the example
# with the commands the guide lists (run, approve twice, a run that crashes after the refund, a
# resume that halts, resolve, evidence), then captures bide view's pages with a headless
# Chromium-based browser at 1200 pixels wide, as shots.sh does.
#
#   assets/demo/shots-approval.sh              # dark, into docs/guides/img
#   THEME=light OUT=/tmp/shots assets/demo/shots-approval.sh
#
# Needs a Chromium-based browser (set CHROME to its binary if it is not found). The pages hold
# times, so the images differ from run to run in those digits only.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
theme=${THEME:-dark}
out=${OUT:-$root/docs/guides/img}
port=${PORT:-8631}
work=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null; rm -rf "$work"' EXIT

. "$here/lib.sh"
shot_init

(cd "$root/cmd/bide" && go build -o "$work/bide" .)
(cd "$root/examples/approval" && go build -o "$work/approval" .)
mkdir -p "$out"
cd "$work"

wait_for() { # url: retry for up to 30 s
  for _ in $(seq 300); do curl -sf -o /dev/null "$1" && return 0; sleep 0.1; done
  echo "shots-approval.sh: timed out waiting for $1" >&2; exit 1
}
serve() { # port args...: bide view, read-only, on a loopback port
  local p=$1; shift
  ./bide view "$@" -addr "127.0.0.1:$p" > /dev/null &
  wait_for "http://127.0.0.1:$p/"
}

./approval run -db a.db > /dev/null 2>&1 || true            # the gate pauses
./approval approve -db a.db -as ops > /dev/null
./approval approve -db a.db -as finance > /dev/null
./approval run -db a.db -crash > /dev/null 2>&1 || true     # the refund fires; the process dies
./approval run -db a.db > /dev/null 2>&1 || true            # the resume halts: outcome unknown

serve "$port" -db a.db
shot run-viewer-agent-halted "http://127.0.0.1:$port/run?id=refund-42" 1300

./approval resolve -db a.db > /dev/null
pub=$(./approval evidence -db a.db -out ev.json 2>&1 | grep -o '[0-9a-f]\{64\}' | head -1)
serve $((port + 1)) -db a.db -evidence ev.json -pubkey "$pub" -approver-keys approver-keys.json
shot run-viewer-agent-verified "http://127.0.0.1:$((port + 1))/run?id=refund-42" 1300
# The image the page's Share this run button saves for this verified run, as the browser saves it.
(cd "$(shot_dir)" && node share.mjs "http://127.0.0.1:$((port + 1))/run?id=refund-42" "$out/run-viewer-share-$theme.png" "$theme")
echo "$out/run-viewer-share-$theme.png"
if command -v ffmpeg > /dev/null; then
  saved=$out; out=$work
  shot full "http://127.0.0.1:$((port + 1))/run?id=refund-42" 3500 > /dev/null
  out=$saved
  ffmpeg -loglevel error -y -i "$work/full-$theme.png" -vf "crop=1200:1720:0:${TIMELINE_Y:-1590}" "$out/run-viewer-timeline-$theme.png"
  echo "$out/run-viewer-timeline-$theme.png"
else
  echo "shots-approval.sh: no ffmpeg; run-viewer-timeline not retaken" >&2
fi

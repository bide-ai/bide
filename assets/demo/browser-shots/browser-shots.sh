#!/usr/bin/env bash
# browser-shots.sh: regenerate the v0.17.0 browser images in docs/guides/img from real runs, in a real
# Chrome, at 2x, dark and light: the approve page (approve-page-waiting, approve-page-signed), bide
# view's Needs attention list (run-viewer-needs-attention) and the Slack payloads bide notify relay
# delivers (notify-relay-slack-approval, notify-relay-slack-resolved). See README.md.
#
#   assets/demo/browser-shots/browser-shots.sh                  # into docs/guides/img
#   OUT=/tmp/shots CHROME_BIN=/path/to/chrome assets/demo/browser-shots/browser-shots.sh
#   ONLY=needs-attention assets/demo/browser-shots/browser-shots.sh   # one part: approve, needs-attention or relay
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
export OUT=${OUT:-$root/docs/guides/img}
mkdir -p "$OUT"
work=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null; rm -rf "$work"' EXIT

for c in "${CHROME_BIN:-}" \
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  "$(command -v google-chrome-stable || true)" "$(command -v google-chrome || true)" "$(command -v chromium || true)" \
  "$(ls -d "$HOME"/.cache/puppeteer/chrome/*/chrome-*/"Google Chrome for Testing.app"/Contents/MacOS/"Google Chrome for Testing" 2>/dev/null | tail -1)"; do
  [ -n "$c" ] && [ -x "$c" ] && { CHROME_BIN=$c; break; }
done
[ -n "${CHROME_BIN:-}" ] || { echo "browser-shots.sh: no Chrome found; set CHROME_BIN" >&2; exit 1; }
export CHROME_BIN
[ -d "$here/node_modules/puppeteer-core" ] || (cd "$here" && npm ci --ignore-scripts)

(cd "$root/cmd/bide" && GOWORK=off go build -o "$work/bide" .)
(cd "$root" && GOWORK=off go build -o "$work/intake" ./assets/demo/browser-shots/intake)
. "$here/../lib.sh" # require_free
only=${ONLY:-approve needs-attention relay}
for part in $only; do
  case $part in needs-attention) require_free 8631 ;; relay) require_free 8641 8642 ;; esac
done
want() { case " $only " in *" $1 "*) return 0;; esac; return 1; }
wait_for() { for _ in $(seq 300); do curl -sf -o /dev/null "$1" && return 0; sleep 0.1; done; echo "timed out waiting for $1" >&2; exit 1; }

# 1. The approve page: a real intake with an agent paused at a refund, finance already approved.
if want approve; then
for theme in dark light; do
  export THEME=$theme ENROL_DIR=$(mktemp -d "$work/enrol.XXXX")
  "$work/intake" > "$ENROL_DIR/log" 2>&1 &
  pid=$!
  for _ in $(seq 100); do [ -f "$ENROL_DIR/url" ] && break; sleep 0.1; done
  (cd "$here" && node approve.mjs) || { cat "$ENROL_DIR/log" >&2; exit 1; }
  kill $pid; wait $pid 2>/dev/null || true
done
fi

# 2. bide view's Needs attention list, from bide sample.
if want needs-attention; then
(cd "$work" && ./bide sample -out sample -no-browser -addr 127.0.0.1:8631 > /dev/null) &
wait_for http://127.0.0.1:8631/
(cd "$here" && node shot.mjs http://127.0.0.1:8631/attention run-viewer-needs-attention 1200)
kill %% 2>/dev/null || true
fi

if want relay; then
# 3. The relay: a stub stands in for Slack and PagerDuty and records what the relay sends.
cap=$work/cap; mkdir -p "$cap"
node "$here/stub.mjs" "$cap" > "$cap/stub.log" 2>&1 &
wait_for http://127.0.0.1:8641/ || true
export BIDE_WEBHOOK_SECRET=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
export SLACK_WEBHOOK_URL=http://127.0.0.1:8641/slack/T000/B000/XXXX PAGERDUTY_ROUTING_KEY=0123456789abcdef0123456789abcdef
"$work/bide" notify relay -addr 127.0.0.1:8642 -pagerduty-url http://127.0.0.1:8641/pd -view-url http://127.0.0.1:8621/ > "$cap/relay.log" 2>&1 &
sleep 1
"$work/bide" notify test -url http://127.0.0.1:8642/ -kind approval_pending -run refund-1043 -view-url http://127.0.0.1:8621/ > /dev/null
"$work/bide" notify test -url http://127.0.0.1:8642/ -kind resolved -run refund-1043 > /dev/null
sleep 1
(cd "$here" && node relay.mjs "$cap" "$cap/relay.log")
fi
# The screenshots are 2x; a 256-color palette is visually the same and several times smaller.
for part in $only; do
  case $part in approve) pat=approve-page;; relay) pat=notify-relay-slack;; needs-attention) pat=run-viewer-needs-attention;; *) echo "browser-shots.sh: unknown part $part" >&2; exit 2;; esac
  for f in "$OUT"/$pat-*.png; do
  python3 "$here/../optimize-png.py" "$f" "$f.tmp" && mv "$f.tmp" "$f"
  done
done
ls -1 "$OUT" | grep -E '^(approve-page|notify-relay-slack|run-viewer-needs-attention)'

#!/usr/bin/env bash
# shots.sh: regenerate the run viewer guide's screenshots whose content comes from bide sample
# (docs/guides/img/run-viewer-*): the run list, the Needs attention list, the halted plan flow, and the flow config's risk
# map and violated assertion. It builds bide, runs bide sample, and captures each page with a
# headless Chromium-based browser, at 1200 pixels wide, so the screenshots are what any user gets by
# running bide sample.
#
#   assets/demo/shots.sh                       # dark, into docs/guides/img
#   THEME=light OUT=/tmp/shots assets/demo/shots.sh
#
# Needs a Chromium-based browser (set CHROME to its binary if it is not found).
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
theme=${THEME:-dark}
out=${OUT:-$root/docs/guides/img}
port=${PORT:-8621}
work=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null; rm -rf "$work"' EXIT

for c in "${CHROME:-}" \
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  "/Applications/Chromium.app/Contents/MacOS/Chromium" \
  "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge" \
  "$(command -v chromium || true)" "$(command -v google-chrome || true)"; do
  [ -n "$c" ] && [ -x "$c" ] && { CHROME=$c; break; }
done
[ -n "${CHROME:-}" ] || { echo "shots.sh: no Chromium-based browser found; set CHROME" >&2; exit 1; }

(cd "$root/cmd/bide" && go build -o "$work/bide" .)
mkdir -p "$out"
cd "$work"

shot() { # name url height
  local flags=(--headless=new --disable-gpu --hide-scrollbars --window-size=1200,"$3"
    --user-data-dir="$work/profile-$1" --screenshot="$out/$1-$theme.png")
  if [ "$theme" = dark ]; then flags+=(--force-dark-mode); else flags+=(--blink-settings=preferredColorScheme=1); fi
  "$CHROME" "${flags[@]}" "$2" > /dev/null 2>&1
  echo "$out/$1-$theme.png"
}
wait_for() { # url: retry for up to 30 s
  for _ in $(seq 300); do curl -sf -o /dev/null "$1" && return 0; sleep 0.1; done
  echo "shots.sh: timed out waiting for $1" >&2; exit 1
}

./bide sample -out sample -no-browser -addr "127.0.0.1:$port" > /dev/null &
wait_for "http://127.0.0.1:$port/"
shot run-viewer-run-list "http://127.0.0.1:$port/" 1250
shot run-viewer-attention "http://127.0.0.1:$port/attention" 1680
shot run-viewer-flow-halted "http://127.0.0.1:$port/run?id=triage-3003" 1400

# The flow configs, opened by their relative paths, as a user runs them from the sample directory.
cd sample
../bide view -config flows/refund-v1.yaml -addr 127.0.0.1:$((port + 1)) > /dev/null &
../bide view -config flows/refund-bypass.yaml -addr 127.0.0.1:$((port + 2)) > /dev/null &
wait_for "http://127.0.0.1:$((port + 1))/"
wait_for "http://127.0.0.1:$((port + 2))/"
shot run-viewer-config-risk "http://127.0.0.1:$((port + 1))/" 1450
shot run-viewer-config-assert "http://127.0.0.1:$((port + 2))/" 1450

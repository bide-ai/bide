#!/usr/bin/env bash
# live.sh: regenerate the run viewer guide's live mode images (docs/guides/img/run-viewer-live-*).
# It drives examples/approval step by step against a SQLite journal while bide view serves the run,
# and captures the page as it follows the run: in progress, waiting for approval (none, then one, of
# the two approvals its 2-of-3 gate needs), in progress again once the refund runs, and completed.
# Each capture opens the page, keeps it open while the next step lands, and then captures it, so it
# shows the page updated in place by its script, with the new timeline rows marked. (A headless
# browser captures a page once it has loaded, so the page is framed by a holder page whose load a
# small local server delays.) The frames are joined into a GIF.
#
#   assets/demo/live.sh                          # docs/guides/img/run-viewer-live-dark.gif
#   THEME=light FRAMES=/tmp/frames assets/demo/live.sh   # light, keeping each frame as a PNG
#
# Needs a Chromium-based browser (set CHROME to its binary if it is not found), ffmpeg and python3.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
img=$root/docs/guides/img
theme=${THEME:-dark}
port=${PORT:-8593}
hold=$((port + 1))
work=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null; rm -rf "$work"' EXIT

for c in "${CHROME:-}" \
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  "/Applications/Chromium.app/Contents/MacOS/Chromium" \
  "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge" \
  "$(command -v chromium || true)" "$(command -v google-chrome || true)"; do
  [ -n "$c" ] && [ -x "$c" ] && { CHROME=$c; break; }
done
[ -n "${CHROME:-}" ] || { echo "live.sh: no Chromium-based browser found; set CHROME" >&2; exit 1; }

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

# The holder: /frame?ms=<ms> frames the run page and finishes loading after ms milliseconds.
python3 - "$hold" "$url" "$theme" <<'PY' > /dev/null 2>&1 &
import http.server, sys, time, urllib.parse
port, url, theme = int(sys.argv[1]), sys.argv[2], sys.argv[3]
bg = "#18181a" if theme == "dark" else "#fafaf8"
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        q = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        ms = int(q.get("ms", ["0"])[0])
        if self.path.startswith("/hold"):
            time.sleep(ms / 1000)
            self.send_response(204)
            self.end_headers()
            return
        body = (f'<!doctype html><style>html,body{{margin:0;background:{bg};overflow:hidden}}'
                f'iframe{{border:0;width:100vw;height:100vh}}</style>'
                f'<iframe src="{url}"></iframe><img alt="" src="/hold?ms={ms}">').encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass
http.server.ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
PY

# capture name ms: open the run page in the background, keep it open ms milliseconds (its script
# polling) and capture it; cap is the browser's PID. Returns once the page has loaded, so the step
# the caller takes next lands while the page is open.
capture() {
  local flags=(--headless=new --disable-gpu --hide-scrollbars --window-size=900,1500
    --user-data-dir="$work/profile-$1" --screenshot="$work/$1.png")
  if [ "$theme" = dark ]; then flags+=(--force-dark-mode); else flags+=(--blink-settings=preferredColorScheme=1); fi
  "$CHROME" "${flags[@]}" "http://127.0.0.1:$hold/frame?ms=$2" > /dev/null 2>&1 &
  cap=$!
  sleep 2
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

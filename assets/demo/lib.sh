# shellcheck shell=bash
# lib.sh: what the shots*.sh scripts share. Source it after setting root (the repository), out (where
# the images go) and theme (dark or light).
#
#   find_chrome       sets and exports CHROME_BIN (and CHROME): $CHROME_BIN or $CHROME, a Chrome on the
#                     usual paths, or a puppeteer-installed Chrome for Testing
#   shot NAME URL H   photographs URL at 1200 x H in $theme into $out/NAME-$theme.png, with
#                     browser-shots/capture.mjs (puppeteer-core, installed from the lockfile on first
#                     use); a Chrome that fails is tried three times
find_chrome() {
  local c
  for c in "${CHROME_BIN:-}" "${CHROME:-}" \
    "$(ls -d "$HOME"/.cache/puppeteer/chrome/*/chrome-*/"Google Chrome for Testing.app"/Contents/MacOS/"Google Chrome for Testing" 2>/dev/null | tail -1)" \
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
    "/Applications/Chromium.app/Contents/MacOS/Chromium" \
    "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge" \
    "$(command -v google-chrome-stable || true)" "$(command -v google-chrome || true)" "$(command -v chromium || true)"; do
    [ -n "$c" ] && [ -x "$c" ] && { CHROME_BIN=$c; CHROME=$c; export CHROME_BIN CHROME; return 0; }
  done
  echo "$(basename "$0"): no Chrome found; set CHROME_BIN" >&2
  return 1
}

shot_dir() { printf '%s' "$root/assets/demo/browser-shots"; }

shot_init() {
  find_chrome || exit 1
  [ -d "$(shot_dir)/node_modules/puppeteer-core" ] || (cd "$(shot_dir)" && npm ci --ignore-scripts > /dev/null)
}

# free ports: nothing may already answer on a port a script is about to serve on, or a stale server
# could answer for the one the script started (and be photographed instead of it).
require_free() { # port...
  local p
  for p in "$@"; do
    if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2> /dev/null; then
      echo "$(basename "$0"): port $p is in use; set PORT to a free base port" >&2
      exit 1
    fi
  done
}

# wait_for URL: wait up to 30 s for the server the script just started in the background ($!) to answer
# 200, and fail if that process has exited (it could not bind its port, say) instead of letting another
# server on the port answer.
wait_for() { # url
  local pid=$! _
  for _ in $(seq 300); do
    kill -0 "$pid" 2> /dev/null || { echo "$(basename "$0"): the server for $1 exited" >&2; exit 1; }
    curl -sf -o /dev/null "$1" && return 0
    sleep 0.1
  done
  echo "$(basename "$0"): timed out waiting for $1" >&2
  exit 1
}

# private_dir PATH: an empty directory at a fixed path that only this user can touch: an existing one
# must be a real directory (not a link) of ours, and is emptied; it is made with mode 700, and made
# only if nothing is there. The path is fixed because the Needs attention list shows it, and a random
# suffix would put one in the picture.
private_dir() { # path
  local d=$1
  if [ -e "$d" ] || [ -L "$d" ]; then
    if [ -L "$d" ] || [ ! -d "$d" ] || [ ! -O "$d" ]; then
      echo "$(basename "$0"): $d exists and is not a directory of yours; remove it" >&2
      exit 1
    fi
    chmod 700 "$d" && rm -rf "$d"
  fi
  mkdir -m 700 "$d" || { echo "$(basename "$0"): cannot make $d" >&2; exit 1; }
  if [ -L "$d" ] || [ ! -O "$d" ]; then
    echo "$(basename "$0"): $d is not ours" >&2
    exit 1
  fi
}

# shot NAME URL HEIGHT [TEXT]: photograph URL; capture.mjs fails on a response that is not 2xx, on a
# page with no <main>, and, when TEXT is given, on a page whose <main> does not hold it. A run page
# (URL ending run?id=ID) must name its run.
shot() { # name url height [text]
  local file="$out/$1-$theme.png" n text=${4:-}
  case $2 in *"/run?id="*) [ -n "$text" ] || text=${2##*run?id=} ;; esac
  rm -f "$file"
  for n in 1 2 3; do
    (cd "$(shot_dir)" && node capture.mjs "$file" "$2" 1200 "$3" "$theme" "$text") > /dev/null 2>&1 && [ -s "$file" ] && break
  done
  [ -s "$file" ] || { echo "$(basename "$0"): no screenshot of $2 (a non-2xx, a page without <main>, or one without $text)" >&2; exit 1; }
  echo "$file"
}

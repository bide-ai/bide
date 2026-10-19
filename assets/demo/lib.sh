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

shot() { # name url height
  local file="$out/$1-$theme.png" n
  rm -f "$file"
  for n in 1 2 3; do
    (cd "$(shot_dir)" && node capture.mjs "$file" "$2" 1200 "$3" "$theme") > /dev/null 2>&1 && [ -s "$file" ] && break
  done
  [ -s "$file" ] || { echo "$(basename "$0"): no screenshot of $2" >&2; exit 1; }
  echo "$file"
}

#!/usr/bin/env bash
# scripts/shots.sh: regenerate the docs' browser screenshots (docs/guides/img) from real runs, in one
# Chrome, in one way: assets/demo/browser-shots/capture.mjs (puppeteer-core, from a lockfile) at 1x with the
# page in a named color scheme and reduced motion. Run it after a change to what the pages look like, and
# commit the images with the change. See assets/demo/SHOTS.md for what each script makes and what is
# and is not reproducible.
#
#   scripts/shots.sh                         # every image, into docs/guides/img
#   scripts/shots.sh viewer editor           # some groups: viewer, approval, deadline, identity, flows, editor,
#                                            #   needs-attention, live (the GIF)
#   OUT=/tmp/shots scripts/shots.sh viewer   # elsewhere
#
# Needs Go, Node 20 or later, Chrome (CHROME_BIN, or a Chrome for Testing in ~/.cache/puppeteer), ffmpeg
# (the timeline crop and the GIF), python3 with PIL (the gate-expired and identity crops) and curl.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
demo=$(cd "$here/../assets/demo" && pwd)
groups=("$@")
[ ${#groups[@]} -gt 0 ] || groups=(viewer approval deadline identity flows editor needs-attention live)

run() { echo "== $*" >&2; "$@"; }
for g in "${groups[@]}"; do
  case $g in
    viewer)          THEME=dark run "$demo/shots.sh" ;;
    approval)        THEME=dark run "$demo/shots-approval.sh" ;;
    deadline)        THEME=dark run "$demo/shots-deadline.sh"; THEME=light run "$demo/shots-deadline.sh" ;;
    identity)        THEME=dark run "$demo/shots-identity.sh"; THEME=light run "$demo/shots-identity.sh" ;;
    flows)           THEME=dark run "$demo/shots-flows.sh" ;;
    editor)          run "$demo/shots-edit.sh" ;;
    needs-attention) ONLY=needs-attention run "$demo/browser-shots/browser-shots.sh" ;;
    live)            THEME=dark run "$demo/live.sh" ;;
    *) echo "scripts/shots.sh: unknown group $g" >&2; exit 2 ;;
  esac
done

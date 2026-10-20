#!/usr/bin/env bash
# scripts/shots.sh: regenerate the docs' browser screenshots (docs/guides/img) from real runs, in one
# Chrome, in one way: assets/demo/browser-shots/capture.mjs (puppeteer-core, from a lockfile) at 1x with the
# page in a named color scheme and reduced motion. Run it after a change to what the pages look like, and
# commit the images with the change. See assets/demo/SHOTS.md for what each script makes and what is
# and is not reproducible.
#
#   scripts/shots.sh                         # every image, into docs/guides/img
#   scripts/shots.sh viewer editor           # some groups: viewer, approval, deadline, identity, flows, editor,
#                                            #   editor-typed, themes, approve-page, needs-attention, live (the GIF)
#   OUT=/tmp/shots scripts/shots.sh viewer   # elsewhere
#
# Needs Go, Node 20 or later, Chrome (CHROME_BIN, or a Chrome for Testing in ~/.cache/puppeteer), ffmpeg
# (the timeline crop and the GIF), python3 with PIL (the gate-expired and identity crops) and curl.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
demo=$(cd "$here/../assets/demo" && pwd)
groups=("$@")
[ ${#groups[@]} -gt 0 ] || groups=(viewer approval deadline identity flows editor editor-typed themes approve-page needs-attention live)

run() { echo "== $*" >&2; "$@"; }
for g in "${groups[@]}"; do
  case $g in
    viewer)          THEME=dark run "$demo/shots.sh" ;;
    approval)        THEME=dark run "$demo/shots-approval.sh" ;;
    deadline)        THEME=dark run "$demo/shots-deadline.sh"; THEME=light run "$demo/shots-deadline.sh" ;;
    identity)        THEME=dark run "$demo/shots-identity.sh"; THEME=light run "$demo/shots-identity.sh" ;;
    flows)           THEME=dark run "$demo/shots-flows.sh" ;;
    editor)          run "$demo/shots-edit.sh" ;;
    editor-typed)    run "$demo/shots-edit-typed.sh" ;;
    themes)          run "$demo/shots-themes.sh" ;;
    approve-page)    ONLY=approve run "$demo/browser-shots/browser-shots.sh" ;;
    needs-attention) ONLY=needs-attention run "$demo/browser-shots/browser-shots.sh" ;;
    live)            THEME=dark run "$demo/live.sh" ;;
    *) echo "scripts/shots.sh: unknown group $g" >&2; exit 2 ;;
  esac
done

# Every image the run wrote is compared with the committed one: a changed size, or a file that shrank by
# more than half (a 404 page or a blank render is a few kilobytes against a page's hundred), is a failure
# until looked at (SHOTS_ALLOW_CHANGE=1 accepts what you looked at).
out=${OUT:-$(cd "$here/.." && pwd)/docs/guides/img}
repo=$(cd "$here/.." && pwd)
bad=0
for f in "$out"/*.png "$out"/*.gif; do
  [ -e "$f" ] || continue
  rel=${f#"$repo"/}
  old=$(git -C "$repo" cat-file -s "HEAD:$rel" 2> /dev/null) || continue   # a new image has nothing to compare with
  git -C "$repo" diff --quiet -- "$rel" 2> /dev/null && continue            # unchanged
  new=$(wc -c < "$f" | tr -d ' ')
  dims() { python3 -c 'import sys; from PIL import Image; print("x".join(map(str, Image.open(sys.stdin.buffer).size)))'; }
  od=$(git -C "$repo" show "HEAD:$rel" | dims) || od=?
  nd=$(dims < "$f") || nd=?
  if [ "$od" != "$nd" ]; then echo "shots: $rel is $nd, was $od" >&2; bad=1
  elif [ $((new * 2)) -lt "$old" ]; then echo "shots: $rel shrank from $old to $new bytes: is it the page meant?" >&2; bad=1; fi
done
if [ $bad = 1 ] && [ "${SHOTS_ALLOW_CHANGE:-}" != 1 ]; then
  echo "shots: look at the images above; SHOTS_ALLOW_CHANGE=1 accepts them" >&2
  exit 1
fi

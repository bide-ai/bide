#!/usr/bin/env bash
# shots-themes.sh: regenerate the themes guide's pictures, docs/guides/img/themes-light, themes-dark,
# themes-high-contrast and themes-acme-teal (1200 x 900): the page bide view serves for examples/plan/flows/refund-policy.yaml
# (the flow's risk map) in each shipped theme and in a custom one (browser-shots/acme-teal.json, passed with -theme).
# The page is real and nothing is mocked; browser-shots/themes-shots.mjs chooses the theme as the page's Theme button does.
#
#   assets/demo/shots-themes.sh                     # into docs/guides/img
#   OUT=/tmp/shots assets/demo/shots-themes.sh
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
theme=light
out=${OUT:-$root/docs/guides/img}
port=${PORT:-8691}
work=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null; rm -rf "$work"' EXIT

. "$here/lib.sh"
require_free "$port"
shot_init

(cd "$root/cmd/bide" && GOWORK=off go build -o "$work/bide" .)
"$work/bide" theme check "$here/browser-shots/acme-teal.json" > /dev/null
mkdir -p "$out"
cd "$root/examples/plan"
"$work/bide" view -config flows/refund-policy.yaml -theme "$here/browser-shots/acme-teal.json" -addr "127.0.0.1:$port" > /dev/null &
wait_for "http://127.0.0.1:$port/"
(cd "$(shot_dir)" && node themes-shots.mjs "http://127.0.0.1:$port/" "$out")
for n in light dark high-contrast acme-teal; do echo "$out/themes-$n.png"; done

#!/usr/bin/env bash
# cli.sh: regenerate the guides' colored terminal screenshots (docs/guides/img/cli-*-dark.png):
# bide check on a violated policy assertion, bide check -diff -check on a weakened approval gate,
# and bide audit verify-evidence on the sample's evidence, verified and then edited (the files come
# from bide sample), plus go run . in a
# project bide init scaffolded (docs/guides/img/cli-init-run-dark.png). Each is recorded
# with charmbracelet/vhs in a real terminal, so the colors are the ones bide picks on its own
# (-color=auto), 1200 pixels wide.
#
#   assets/demo/cli.sh                                   # all four, dark
#   THEME=light ONLY=cli-diff-weakened assets/demo/cli.sh   # one, in the light variant the guides embed with <picture>
#
# Needs vhs (which brings ttyd and ffmpeg) and python3.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
img=$root/docs/guides/img
theme=${THEME:-dark}
only=${ONLY:-}
case $theme in
  dark) vhs_theme=Dracula ;;
  light) vhs_theme="Builtin Tango Light" ;;
  *) echo "cli.sh: THEME is dark or light" >&2; exit 1 ;;
esac
work=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$work"' EXIT

(cd "$root/cmd/bide" && go build -o "$work/bin/bide" .)
mkdir -p "$work/flows"

# bide sample writes the journal, evidence, keys and flow configs these screenshots show, the same
# runs every time (bide sample -h). It serves its viewer once the files are written, so it is
# stopped when it prints its address.
"$work/bin/bide" sample -no-browser -out "$work/sample" > "$work/sample.out" 2>&1 &
for _ in $(seq 300); do grep -q '^viewer' "$work/sample.out" && break; sleep 0.1; done
grep -q '^viewer' "$work/sample.out" || { echo "cli.sh: bide sample did not finish:" >&2; cat "$work/sample.out" >&2; exit 1; }
kill %1 2>/dev/null || true
wait 2>/dev/null || true

# The check screenshot reads the sample's flow configs. The diff one needs nodes added and removed
# as well as a weakened gate, which the sample's pair does not have, so it takes the older pair in
# examples/plan/flows.
cp "$root"/examples/plan/flows/refund-v1.yaml "$root"/examples/plan/flows/refund-v2.yaml "$work/flows/"

# The evidence of the approved refund and the log key, and the same package with one byte of a
# proven record changed (the refund's result: $120 -> $12).
mkdir -p "$work/run"
cp "$work/sample/refund-1049.evidence.json" "$work/run/evidence.json"
cp "$work/sample/log.pub" "$work/run/log.pub"
python3 - "$work/run" <<'PY'
import base64, json, sys
d = json.load(open(sys.argv[1] + "/evidence.json"))
b = d["actions"][0]["bundle"]
b["record_bytes"] = base64.b64encode(base64.b64decode(b["record_bytes"]).replace(b"refunded $120", b"refunded $12")).decode()
json.dump(d, open(sys.argv[1] + "/evidence-edited.json", "w"), indent=2)
PY

shot() { # name height dir command...: one screenshot of the command's output (SHOT_WAIT: seconds)
  local name=$1 height=$2 dir=$3; shift 3
  [ -z "$only" ] || [ "$only" = "$name" ] || return 0
  name=$name-$theme
  local wait=${SHOT_WAIT:-1.5}
  cat > "$work/$name.tape" <<TAPE
Output "$work/$name.gif"
Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height $height
Set Padding 20
Set Theme "$vhs_theme"
Set TypingSpeed 5ms
Hide
Type "cd $dir && export PATH=$work/bin:\$PATH && PS1='\$ ' && clear"
Enter
Sleep 300ms
Show
Type "$*"
Enter
Sleep ${wait}s
Screenshot "$work/$name.raw.png"
Sleep 1s
TAPE
  rm -f "$img/$name.png"
  vhs "$work/$name.tape" > /dev/null
  [ -s "$work/$name.raw.png" ] || { echo "cli.sh: vhs wrote no $name.png" >&2; exit 1; }
  python3 "$here/optimize-png.py" "$work/$name.raw.png" "$img/$name.png"
  echo "$img/$name.png"
}

shot cli-check-violation 250 "$work/sample" "bide check flows/refund-bypass.yaml flows/refund-v1.yaml"
shot cli-diff-weakened 300 "$work" "bide check flows/refund-v1.yaml -diff flows/refund-v2.yaml -check"
shot cli-audit-verified 330 "$work/run" "bide audit verify-evidence -evidence evidence.json -pubkey log.pub"
shot cli-audit-failed 350 "$work/run" "bide audit verify-evidence -evidence evidence-edited.json -pubkey log.pub"

# A project bide init scaffolded, built against this checkout; built once first so go run . is quick.
if [ -z "$only" ] || [ "$only" = cli-init-run ]; then
  (cd "$work" && "$work/bin/bide" init -dev "$root" refund > /dev/null)
  (cd "$work/refund" && GOWORK=off GOFLAGS=-mod=mod go mod tidy > /dev/null 2>&1 && GOWORK=off GOFLAGS=-mod=mod go build ./...)
  SHOT_WAIT=6 shot cli-init-run 330 "$work/refund" "go run ."
fi

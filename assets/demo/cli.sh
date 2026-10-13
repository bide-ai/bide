#!/usr/bin/env bash
# cli.sh: regenerate the guides' colored terminal screenshots (docs/guides/img/cli-*-dark.png):
# bide check on a violated policy assertion, bide check -diff -check on a weakened approval gate,
# and bide audit verify-evidence on the demo's evidence, verified and then edited. Each is recorded
# with charmbracelet/vhs in a real terminal, so the colors are the ones bide picks on its own
# (-color=auto), 1200 pixels wide.
#
#   assets/demo/cli.sh
#
# Needs vhs (which brings ttyd and ffmpeg) and python3.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
img=$root/docs/guides/img
work=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$work"' EXIT

(cd "$root/cmd/bide" && go build -o "$work/bin/bide" .)
mkdir -p "$work/flows" "$work/run"
cp "$root"/examples/plan/flows/*.yaml "$work/flows/"

# The demo's evidence and log key, kept in a directory of their own; then the same package with
# one byte of a proven record changed (the charge's result: $120 -> $12).
"$work/bin/bide" demo -fast -no-browser -keep > "$work/demo.out" 2>&1 &
for _ in $(seq 300); do grep -q '^kept' "$work/demo.out" && break; sleep 0.2; done
kill %1 2>/dev/null || true
wait 2>/dev/null || true
kept=$(awk '$1 == "kept" {print $2}' "$work/demo.out")
[ -n "$kept" ] || { echo "cli.sh: the demo did not finish:" >&2; cat "$work/demo.out" >&2; exit 1; }
cp "$kept/evidence.json" "$work/run/"
grep -o 'ed25519:[0-9a-f]*' "$work/demo.out" | head -1 > "$work/run/log.pub"
rm -rf "$kept"
python3 - "$work/run" <<'PY'
import base64, json, sys
d = json.load(open(sys.argv[1] + "/evidence.json"))
b = d["actions"][0]["bundle"]
b["record_bytes"] = base64.b64encode(base64.b64decode(b["record_bytes"]).replace(b"charged $120", b"charged $12")).decode()
json.dump(d, open(sys.argv[1] + "/evidence-edited.json", "w"), indent=2)
PY

shot() { # name height dir command...: one screenshot of the command's output
  local name=$1 height=$2 dir=$3; shift 3
  cat > "$work/$name.tape" <<TAPE
Output "$work/$name.gif"
Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height $height
Set Padding 20
Set Theme "Dracula"
Set TypingSpeed 5ms
Hide
Type "cd $dir && export PATH=$work/bin:\$PATH && PS1='\$ ' && clear"
Enter
Sleep 300ms
Show
Type "$*"
Enter
Sleep 1500ms
Screenshot "$img/$name.png"
Sleep 1s
TAPE
  rm -f "$img/$name.png"
  vhs "$work/$name.tape" > /dev/null
  [ -s "$img/$name.png" ] || { echo "cli.sh: vhs wrote no $name.png" >&2; exit 1; }
  echo "$img/$name.png"
}

shot cli-check-violation-dark 250 "$work" "bide check flows/refund-policy-bypass.yaml flows/refund-policy.yaml"
shot cli-diff-weakened-dark 300 "$work" "bide check flows/refund-v1.yaml -diff flows/refund-v2.yaml -check"
shot cli-audit-verified-dark 330 "$work/run" "bide audit verify-evidence -evidence evidence.json -pubkey log.pub"
shot cli-audit-failed-dark 350 "$work/run" "bide audit verify-evidence -evidence evidence-edited.json -pubkey log.pub"

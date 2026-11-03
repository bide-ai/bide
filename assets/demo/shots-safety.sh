#!/usr/bin/env bash
# shots-safety.sh: regenerate the screenshots of the four safety controls, dark and light:
#
#   Kill switch (docs/guides/kill-switch.md)
#     cli-stop-set            bide stop -tool refund, signed by ops under the deployment's stop policy
#     cli-stop-lift           bide stop -lift, signed by finance (one lift of the one the policy needs)
#     cli-audit-stops         bide audit stops: the signed trail, anchored by a signed head, verifies
#     run-viewer-stop-banner  bide view with the stop in force: the banner and the run it holds
#   Untrusted content (docs/guides/untrusted-content.md)
#     cli-approve-taint-list  bide approve -list: the held refund, its reason, the 2-of-3 gate
#     run-viewer-taint-hold   bide view, the run page: the refund held because the run read inbound-email
#     run-viewer-taint-attention  bide view, Needs attention: the same hold
#   Amount caps (docs/guides/amount-caps.md)
#     run-viewer-cap-refused  bide view, the run page: a $4,800 refund refused by the per-call cap
#     cli-audit-caps          bide audit caps: the day's ledger, two refunds counted
#   Earned authority (docs/guides/delegation.md)
#     cli-earned-authority    examples/govern/earned-authority: the ladder, the ceiling, the reset
#
# Everything is real: assets/demo/setup/safety.go drives the real agent runtime to write the journal,
# bide stop, bide approve and bide audit are the real commands recorded with charmbracelet/vhs in a
# real terminal (colors as bide picks them with -color=auto), and the viewer is photographed with
# browser-shots/capture.mjs, as shots.sh does.
#
# One thing differs from a stock bide: bide stop records the writer's host name, which would put this
# machine's name in the images, so the bide that sets and lifts the stops is built with that one value
# replaced ("ops-workstation"), through go build -overlay. Nothing else changes.
#
#   assets/demo/shots-safety.sh                 # dark and light, into docs/guides/img
#   VARIANTS=dark OUT=/tmp/shots assets/demo/shots-safety.sh
#
# Needs Go, vhs (which brings ttyd and ffmpeg), Node 20 or later, a Chrome (CHROME_BIN), python3 with
# Pillow and curl. The pages and the stop's age hold times, so a new picture differs in those digits.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
out=${OUT:-$root/docs/guides/img}
port=${PORT:-8661}
work=/tmp/bide-demo # a fixed, neutral path: the Needs attention list shows it, so no user name or random suffix is in the picture
. "$here/lib.sh"
require_free "$port"
private_dir "$work"
trap 'kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null; rm -rf "$work"' EXIT
shot_init
mkdir -p "$out" "$work/bin"

# bide, with the stop command's host name fixed (see above).
cat > "$work/overlay.json.in" <<'JSON'
{"Replace": {"@SRC@": "@DST@"}}
JSON
src=$root/cmd/bide/internal/stopcmd/stopcmd.go
sed 's/hostName = os.Hostname$/hostName = func() (string, error) { return "ops-workstation", nil }/' "$src" > "$work/stopcmd.go"
grep -q ops-workstation "$work/stopcmd.go" || { echo "shots-safety.sh: stopcmd.go no longer has the hostName seam" >&2; exit 1; }
sed -e "s|@SRC@|$src|" -e "s|@DST@|$work/stopcmd.go|" "$work/overlay.json.in" > "$work/overlay.json"
(cd "$root/cmd/bide" && go build -overlay "$work/overlay.json" -o "$work/bin/bide" .)
(cd "$root/examples/govern" && go build -o "$work/bin/earned-authority" ./earned-authority)
bide=$work/bin/bide

# The approvers' keys, the stop policy (ops sets; finance or risk lifts, one lift, and not the setter),
# the log key that signs the trail's heads, and the journal.
base=$work/base
mkdir -p "$base"
for id in ops finance risk; do "$bide" keygen -out "$base/$id.key" -as "$id" > /dev/null; done
"$bide" keygen -out "$base/log.key" > /dev/null
mv "$base/log.key.pub" "$base/log.pub"
python3 - "$base" <<'PY'
import json, sys
d = sys.argv[1]
pub = {i: open(f"{d}/{i}.key.pub").read().strip() for i in ("ops", "finance", "risk")}
json.dump(pub, open(f"{d}/approver-keys.json", "w"), indent=2)
json.dump({"deployment_id": "prod-91c2",
           "approvers": {i: {"alg": "ed25519", "key": p.split(":", 1)[1]} for i, p in pub.items()},
           "setters": ["ops"], "lifters": ["finance", "risk"], "lift_quorum": 1, "lift_needs_other": True},
          open(f"{d}/stop-policy.json", "w"), indent=2)
PY
rm -f "$base"/*.key.pub
(cd "$root/cmd/bide" && GOWORK=off go run ../../assets/demo/setup/safety.go -dir "$base")

stopargs=(-stop-policy stop-policy.json)
# Three more states, from the same journal, made with the real command: stopped; stopped and lifted;
# and lifted with a signed head over the trail.
stopped=$work/stopped lifted=$work/lifted
cp -R "$base" "$stopped"
(cd "$stopped" && "$bide" stop -db safety.db -tool refund -reason "provider incident" "${stopargs[@]}" -as ops -key ops.key -yes > /dev/null)
id=$(cd "$stopped" && "$bide" stop -db safety.db "${stopargs[@]}" -list -json | python3 -c 'import json,sys; d=json.load(sys.stdin); print((d.get("stops") or d.get("active"))[0]["id"][:8])')
cp -R "$stopped" "$lifted"
(cd "$lifted" && "$bide" stop -db safety.db -lift "$id" -reason "provider recovered" "${stopargs[@]}" -as finance -key finance.key -yes > /dev/null \
  && "$bide" audit stops -db safety.db "${stopargs[@]}" -sign-head -log-key log.key -pubkey log.pub -head-out stop-heads.jsonl > /dev/null)

term() { # name theme height dir line...: one screenshot of the lines' output (each line is typed and entered)
  local name=$1 theme=$2 height=$3 dir=$4; shift 4
  local variant=$name-$theme work2=$work/vhs-$name-$theme
  local vt=Dracula
  [ "$theme" = light ] && vt="Catppuccin Latte"
  mkdir -p "$work2"
  {
    echo "Output \"$work2/$variant.gif\""
    echo 'Set Shell "bash"'
    echo "Set FontSize ${FONT:-15}"
    echo 'Set Width 1200'
    echo "Set Height $height"
    echo 'Set Padding 20'
    echo "Set Theme \"$vt\""
    echo 'Set TypingSpeed 4ms'
    echo 'Hide'
    echo "Type \"cd $dir && export PATH=$work/bin:\$PATH && PS1='\$ ' && clear\""
    echo 'Enter'
    echo 'Sleep 300ms'
    echo 'Show'
    local l
    for l in "$@"; do
      echo "Type \`$l\`"
      echo 'Enter'
    done
    echo 'Sleep 2500ms'
    echo "Screenshot \"$work2/$variant.raw.png\""
    echo 'Sleep 500ms'
  } > "$work2/$variant.tape"
  vhs "$work2/$variant.tape" > /dev/null
  [ -s "$work2/$variant.raw.png" ] || { echo "shots-safety.sh: vhs wrote no $variant.png" >&2; exit 1; }
  python3 "$here/optimize-png.py" "$work2/$variant.raw.png" "$out/$variant.png"
  echo "$out/$variant.png"
}

serve() { # port args...: bide view, read-only, on a loopback port
  local p=$1; shift
  "$bide" view "$@" -addr "127.0.0.1:$p" > /dev/null &
  wait_for "http://127.0.0.1:$p/"
}
cd "$work"
serve "$port" -db base/safety.db
serve $((port + 1)) -db stopped/safety.db -stop-policy stopped/stop-policy.json

for theme in ${VARIANTS:-dark light}; do
  export theme
  # Kill switch
  rm -rf "$work/t" && cp -R "$base" "$work/t"
  FONT=13 term cli-stop-set "$theme" 330 "$work/t" \
    'bide stop -db safety.db -tool refund -reason "provider incident" \' \
    '  -stop-policy stop-policy.json -as ops -key ops.key -yes'
  FONT=13 term cli-stop-lift "$theme" 330 "$stopped" \
    "bide stop -db safety.db -lift $id -reason \"provider recovered\" \\" \
    '  -stop-policy stop-policy.json -as finance -key finance.key -yes'
  FONT=13 term cli-audit-stops "$theme" 330 "$lifted" \
    'bide audit stops -db safety.db -stop-policy stop-policy.json \' \
    '  -sth stop-heads.jsonl -pubkey log.pub'
  shot run-viewer-stop-banner "http://127.0.0.1:$((port + 1))/run?id=refund-4801" 1500
  # Untrusted content
  term cli-approve-taint-list "$theme" 460 "$base" \
    'bide approve -db safety.db -list -approver-keys approver-keys.json'
  shot run-viewer-taint-hold "http://127.0.0.1:$port/run?id=refund-4801" 1500
  shot run-viewer-taint-attention "http://127.0.0.1:$port/attention" 900
  # Amount caps
  shot run-viewer-cap-refused "http://127.0.0.1:$port/run?id=refund-4813" 1500
  term cli-audit-caps "$theme" 250 "$base" 'bide audit caps -db safety.db -unanchored'
  # Earned authority
  term cli-earned-authority "$theme" 880 "$base" 'earned-authority'
done

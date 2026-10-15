#!/usr/bin/env bash
# approve.sh: regenerate the bide approve recordings (docs/guides/img/cli-approve-dark.gif and
# cli-approve-light.gif). An approver reviews a refund that waits at a 2-of-3 approval gate with
# separation of duties: the waiting calls (-list), the requester (ops) refused, then finance reviews
# the call, its arguments and the SHA-256 the signature binds, types their id, and gets the signed,
# recorded decision and its receipt; risk's decision completes the gate. Everything comes from real
# runs: bide keygen makes the keys, assets/demo/setup/approve.go drives the real agent runtime to
# the gate, and bide approve is the real command, recorded with charmbracelet/vhs in a real
# terminal, so the colors are the ones bide picks on its own (-color=auto).
#
#   assets/demo/approve.sh              # both
#   VARIANTS=light assets/demo/approve.sh
#
# Needs vhs (which brings ttyd and ffmpeg) and python3.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
img=$root/docs/guides/img
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

(cd "$root/cmd/bide" && go build -o "$work/bin/bide" .)

# The approvers' keys (bide keygen), the file that enrols their public keys, and the journal with the
# waiting refund. ops started the run (the requester), so the gate does not count ops.
mkdir -p "$work/base"
for id in ops finance risk; do
  "$work/bin/bide" keygen -out "$work/base/$id.key" -as "$id" > /dev/null
done
python3 - "$work/base" <<'PY'
import json, sys
d = sys.argv[1]
json.dump({i: open(f"{d}/{i}.key.pub").read().strip() for i in ("ops", "finance", "risk")},
          open(f"{d}/approver-keys.json", "w"), indent=2)
PY
rm -f "$work"/base/*.key.pub
(cd "$root/cmd/bide" && GOWORK=off go run ../../assets/demo/setup/approve.go -dir "$work/base")

record() { # variant theme
  local variant=$1 theme=$2
  local dir=$work/$variant
  rm -rf "$dir" && cp -R "$work/base" "$dir"
  cat > "$work/$variant.tape" <<TAPE
Output "$work/$variant.gif"
Set Shell "bash"
Set FontSize 15
Set Width 1200
Set Height 820
Set Padding 20
Set Theme "$theme"
Set TypingSpeed 18ms
Set Framerate 20
Hide
Type "cd $dir && export PATH=$work/bin:\$PATH && PS1='\$ ' && clear"
Enter
Sleep 300ms
Show
Sleep 600ms
Type "bide approve -db refund.db -list -approver-keys approver-keys.json"
Sleep 400ms
Enter
Sleep 4s
Type "bide approve -db refund.db -run refund-1049 -call c2 -as ops -key ops.key -approver-keys approver-keys.json"
Sleep 400ms
Enter
Sleep 4s
Ctrl+L
Sleep 500ms
Type "bide approve -db refund.db -run refund-1049 -call c2 -as finance -key finance.key -approver-keys approver-keys.json"
Sleep 400ms
Enter
Sleep 5s
Type "finance"
Sleep 700ms
Enter
Sleep 5s
Ctrl+L
Sleep 500ms
Type "bide approve -db refund.db -run refund-1049 -call c2 -as risk -key risk.key -approver-keys approver-keys.json"
Sleep 400ms
Enter
Sleep 5s
Type "risk"
Sleep 700ms
Enter
Sleep 5s
TAPE
  vhs "$work/$variant.tape" > /dev/null
  [ -s "$work/$variant.gif" ] || { echo "approve.sh: vhs wrote no $variant.gif" >&2; exit 1; }
  # One shared palette per recording: terminal text needs few colors, so the file stays small.
  ffmpeg -loglevel error -y -i "$work/$variant.gif" -vf "fps=10,split[a][b];[a]palettegen=max_colors=64:stats_mode=diff[p];[b][p]paletteuse=dither=none:diff_mode=rectangle" "$img/cli-approve-$variant.gif"
  ls -l "$img/cli-approve-$variant.gif"
}

for v in ${VARIANTS:-dark light}; do
  case $v in dark) record dark "Dracula" ;; light) record light "Catppuccin Latte" ;; esac
done

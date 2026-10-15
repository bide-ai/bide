#!/usr/bin/env bash
# notifications.sh: regenerate the bide audit notifications screenshots in docs/guides/img:
# cli-audit-notifications-pass-{dark,light}.png (refund-2001: its waiting approval was delivered by
# the real webhook notifier, to an HTTPS receiver that verified the signature, and acknowledged,
# on a trail with a signed head) and cli-audit-notifications-fail-{dark,light}.png (refund-2002:
# its approval waits and nobody was notified). assets/demo/setup/notify.go drives the real agent
# runtime and notifier to write the journal and the signed heads; bide audit notifications is the
# real command, recorded with charmbracelet/vhs in a real terminal, so the colors are the ones
# bide picks on its own (-color=auto).
#
#   assets/demo/notifications.sh        # both
#   VARIANTS=light assets/demo/notifications.sh
#
# Needs vhs (which brings ttyd and ffmpeg) and python3 with Pillow.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
img=$root/docs/guides/img
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

(cd "$root/cmd/bide" && go build -o "$work/bin/bide" .)
mkdir -p "$work/run"
(cd "$root/cmd/bide" && GOWORK=off go run ../../assets/demo/setup/notify.go -dir "$work/run")
heads=$(cd "$work/run/heads" && ls)
day=$(ls "$work/run/heads" | sed -n 's/^open-\([0-9]\{4\}-[0-9][0-9]-[0-9][0-9]\)\.json$/\1/p')
[ -n "$day" ] || { echo "notifications.sh: no open-trail head in $heads" >&2; exit 1; }
year=${day%%-*}

shot() { # name theme height line1 line2: one screenshot of the two-line command's output
  local name=$1 theme=$2 height=$3 line1=$4 line2=$5
  cat > "$work/$name.tape" <<TAPE
Output "$work/$name.gif"
Set Shell "bash"
Set FontSize 15
Set Width 1200
Set Height $height
Set Padding 20
Set Theme "$theme"
Set TypingSpeed 4ms
Hide
Type "cd $work/run && export PATH=$work/bin:\$PATH && PS1='\$ ' && clear"
Enter
Sleep 300ms
Show
Type \`$line1 \\\`
Enter
Type \`$line2\`
Enter
Sleep 2s
Screenshot "$work/$name.raw.png"
Sleep 500ms
TAPE
  vhs "$work/$name.tape" > /dev/null
  [ -s "$work/$name.raw.png" ] || { echo "notifications.sh: vhs wrote no $name.png" >&2; exit 1; }
  python3 "$here/optimize-png.py" "$work/$name.raw.png" "$img/$name.png"
  ls -l "$img/$name.png"
}

# The command is typed over two lines, with a backslash continuation, as an operator would write it.
pass1="bide audit notifications -db notify.db -pubkey log.pub -run refund-2001"
pass2="  -sth heads/refund-2001.json -sth heads/open-$day.json -sth heads/open-index-$year.json"
fail1="bide audit notifications -db notify.db -pubkey log.pub -run refund-2002"
fail2="  -sth heads/open-$day.json -sth heads/open-index-$year.json"
for t in "dark Dracula" "light Catppuccin Latte"; do
  set -- $t; v=$1; shift; th="$*"; set -- "$v" "$th"
  case " ${VARIANTS:-dark light} " in *" $1 "*) ;; *) continue ;; esac
  shot "cli-audit-notifications-pass-$1" "$2" 330 "$pass1" "$pass2"
  shot "cli-audit-notifications-fail-$1" "$2" 330 "$fail1" "$fail2"
done

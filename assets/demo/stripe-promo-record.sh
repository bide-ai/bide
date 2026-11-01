#!/usr/bin/env bash
# stripe-promo-record.sh: a split-screen promo recording of examples/stripe-refund, as a 1080x1350
# (4:5) GIF and MP4. The terminal is on top: two approvers sign, the process is killed right after
# the refund is sent, the restart halts (outcome unknown), the reconciling restart finds the refund at
# Stripe and finishes, and a curl asks Stripe how many refunds carry the run's key. The run's bide
# view page is below, changing at each state. Each page is a real `bide view` over a copy of the
# journal taken when the terminal reached that state, captured with a headless Chromium; the script
# checks that bide view left every journal database file (the .db, and its -wal when it has content;
# the -shm is index memory, and an empty -wal holds no data, so both are left out by design) byte for
# byte unchanged.
#
#   STRIPE_SECRET_KEY=sk_test_... assets/demo/stripe-promo-record.sh [pi_...]
#   assets/demo/stripe-promo-record.sh --self-test      # proves the key guard, no key needed
#
# The payment intent is made for you when none is given (a 50.00 test payment, test mode only).
# Output goes to $OUT (default assets/demo/out, which git ignores), as
# stripe-refund-split.gif and stripe-refund-split.mp4. The key is only ever in the environment: it is
# not typed into the recording or printed, and the script refuses to finish if it finds it in any
# file it kept. That guard is a byte match (grep -rF) over the logs, tapes and journals of the run,
# not a look at pixels: the protection for the frames is that nothing prints the key. Curl gets the
# key from a config file (-K), so it is not in any process's argument list.
#
# STRIPE_PROMO_REHEARSE=1 runs the same pipeline against a local stand-in for Stripe
# (assets/demo/stripe-promo/fakestripe.go) with a placeholder key, for layout and timing work.
#
# macOS only (it uses script, shasum and BSD date/sed behavior). Needs vhs (which brings ttyd and
# ffmpeg), a Chromium-based browser (set CHROME if it is not found), Go, bc, perl, curl, and python3
# with Pillow (it crops one screenshot).
set -euo pipefail

# key_guard dir key: succeeds only when grep ran cleanly and found the key nowhere under dir. A match
# (status 0) and a grep error (status 2) both refuse. The bin directory (programs built before the key
# is used; the stand-in's own default key is compiled into one) is not searched.
key_guard() {
  local rc=0
  grep -rqF --exclude-dir=bin -- "$2" "$1" || rc=$?
  [ "$rc" -eq 1 ]
}
if [ "${1:-}" = --self-test ]; then
  st=$(mktemp -d)
  trap 'rm -rf "$st"' EXIT
  k=sk_test_SELFTESTplantedkey0123456789
  for i in $(seq 1 320); do mkdir -p "$st/d$((i % 7))"; printf 'log line %s\nnothing here\n' "$i" > "$st/d$((i % 7))/f$i.txt"; done
  key_guard "$st" "$k" || { echo "self-test: the guard refused a clean tree" >&2; exit 1; }
  for i in $(seq 1 320); do
    f="$st/d$((i % 7))/f$i.txt"
    cp "$f" "$f.orig"
    printf 'x %s y\n' "$k" >> "$f"
    if key_guard "$st" "$k"; then echo "self-test: the guard missed the key planted in $f" >&2; exit 1; fi
    mv "$f.orig" "$f"
  done
  if key_guard "$st/does-not-exist" "$k" 2>/dev/null; then echo "self-test: the guard accepted a grep error" >&2; exit 1; fi
  key_guard "$st" "$k" || { echo "self-test: the guard refused the restored clean tree" >&2; exit 1; }
  echo "key guard self-test: refused all 320 planted copies and a grep error, accepted the clean tree"
  exit 0
fi

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
OUT=${OUT:-$root/assets/demo/out}
work=$(mktemp -d)
pids=()
keydir=$(mktemp -d) # holds the curl config with the key; outside $work so the guard scans only what is kept
chmod 700 "$keydir"
cleanup() { for p in "${pids[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done; rm -rf "$keydir"; [ -n "${KEEP_WORK:-}" ] || rm -rf "$work"; }
trap cleanup EXIT

for c in "${CHROME:-}" \
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  "/Applications/Chromium.app/Contents/MacOS/Chromium" \
  "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge" \
  "$(command -v chromium || true)" "$(command -v google-chrome || true)"; do
  [ -n "$c" ] && [ -x "$c" ] && { CHROME=$c; break; }
done
[ -n "${CHROME:-}" ] || { echo "stripe-promo-record.sh: no Chromium-based browser found; set CHROME" >&2; exit 1; }

# ---- Stripe, real or the rehearsal stand-in
write_curl_cfg() { (umask 077; printf 'user = "%s:"\n' "$STRIPE_SECRET_KEY" > "$keydir/curl.cfg"); }
if [ -n "${STRIPE_PROMO_REHEARSE:-}" ]; then
  export STRIPE_SECRET_KEY=sk_test_rehearsal STRIPE_API_BASE=http://127.0.0.1:18599
  ! (exec 3<>/dev/tcp/127.0.0.1/18599) 2>/dev/null || { echo "stripe-promo-record.sh: port 18599 is already in use" >&2; exit 1; }
  mkdir -p "$work/bin"
  (cd "$here/stripe-promo" && go build -o "$work/bin/fakestripe" fakestripe.go)
  "$work/bin/fakestripe" -addr 127.0.0.1:18599 -key "$STRIPE_SECRET_KEY" > "$work/fake.log" 2>&1 &
  pids+=($!)
  up=
  for _ in $(seq 50); do (exec 3<>/dev/tcp/127.0.0.1/18599) 2>/dev/null && { up=1; break; }; sleep 0.1; done
  [ -n "$up" ] || { echo "stripe-promo-record.sh: the local Stripe stand-in did not start" >&2; cat "$work/fake.log" >&2; exit 1; }
  PI=pi_3RehearsalDemo
else
  case "${STRIPE_SECRET_KEY:-}" in sk_test_*) ;; *) echo "stripe-promo-record.sh: set STRIPE_SECRET_KEY to a test-mode key (sk_test_...)" >&2; exit 1 ;; esac
  unset STRIPE_API_BASE
  PI=${1:-}
  if [ -n "$PI" ] && ! [[ "$PI" =~ ^pi_[A-Za-z0-9]+$ ]]; then echo "stripe-promo-record.sh: the argument is not a payment intent id (pi_...)" >&2; exit 1; fi
  if [ -z "$PI" ]; then
    write_curl_cfg
    PI=$(curl -sS -K "$keydir/curl.cfg" https://api.stripe.com/v1/payment_intents \
      -d amount=5000 -d currency=usd -d payment_method=pm_card_visa -d confirm=true \
      -d "automatic_payment_methods[enabled]=true" -d "automatic_payment_methods[allow_redirects]=never" |
      python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
  fi
fi
write_curl_cfg
case "$PI" in pi_*) ;; *) echo "stripe-promo-record.sh: no payment intent id (got '$PI')" >&2; exit 1 ;; esac

# ---- geometry: a 1080x1350 frame, the terminal on top, the page below
W=1080 TH=600 PH=750 H=1350
FONT=28 SPEED=0.018 # seconds per typed character (the tape's TypingSpeed)
DSF=1.8 # the page is laid out 600 CSS pixels wide and scaled to 1080
SKIP=56 # CSS pixels of the page's top bar left out, to show more of the run

mkdir -p "$work/bin" "$work/snap" "$work/rec"
(cd "$root/examples/stripe-refund" && GOWORK=off go build -o "$work/bin/stripe-refund" .)
(cd "$root/cmd/bide" && go build -o "$work/bin/bide" .)

export PATH="$work/bin:$PATH" PI
stamp=$(date +%H%M%S)
export R=demo-$stamp # a fresh run id: Stripe replays an Idempotency-Key for 24 hours
export DB="$work/stripe-refund.db" SNAP="$work/snap" MARKS="$work/marks.txt"

# The shell helpers the recording uses: they shorten the commands, wrap long lines at spaces (the
# text itself is unchanged) and take the page snapshots.
cat > "$work/rc.sh" <<'RC'
now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
mark() { echo "$1 $(now)" >> "$MARKS"; }
snap() { mkdir -p "$SNAP/$1"; cp "$DB"* "$SNAP/$1/"; mark "$1"; }
# run a command on a pseudo-terminal (so it keeps its colors), breaking lines at spaces
wrapped() { script -q /dev/null "$@" | tr -d '\r\004\010' | fold -s -w 58; return "${PIPESTATUS[0]}"; }
n=0
refund() {
  [ "$n" = 0 ] && mark start
  wrapped stripe-refund run -db "$DB" -payment-intent "$PI" -run "$R" -amount 2500 -approval-threshold 1000 "$@"
  local rc=$?
  n=$((n + 1))
  case $n in 1) snap pending ;; 2) snap crashed ;; 3) snap halted ;; 4) snap resolved ;; esac
  return $rc
}
approve() { wrapped stripe-refund approve -db "$DB" -run "$R" -as "$1"; snap "$1"; }
RC
cat >> "$work/rc.sh" <<RC
URL="${STRIPE_API_BASE:-https://api.stripe.com}/v1/refunds?payment_intent=$PI&limit=100"
CFG="$keydir/curl.cfg"
RC

# ---- the tape, generated so the time of the first Enter is known
tape="$work/promo.tape"
t=0
add() { printf '%s\n' "$1" >> "$tape"; }
sleepfor() { add "Sleep $1"; t=$(echo "$t + $1" | bc -l); }
typeit() { add "Type \"$1\""; t=$(echo "$t + ${#1} * $SPEED" | bc -l); }
cat > "$tape" <<TAPE
Output "terminal.mp4"
Set Shell "bash"
Set FontSize $FONT
Set Width $W
Set Height $TH
Set Padding 22
Set Theme "Dracula"
Set TypingSpeed ${SPEED}s
Set Framerate 24
Hide
Type "PS1='\$ '; source ./rc.sh; clear"
Enter
Show
TAPE
sleepfor 0.5
typeit "# 1. refund 25.00: needs 2 approvals"; add Enter; t=$(echo "$t + 0.05" | bc -l)
sleepfor 0.6
typeit "refund"
ENTER=$t
add Enter
add 'Wait+Screen@30s /status: paused/'
sleepfor 1.6
add 'Type "clear"'; add Enter
typeit "# 2. two approvers sign"; add Enter
sleepfor 0.5
add 'Type "approve ops"'; add Enter; add 'Wait+Screen@30s /ops approved/'; sleepfor 1.2
add 'Type "approve finance"'; add Enter; add 'Wait+Screen@30s /finance approved/'; sleepfor 0.4
typeit "# 2 of 3 is enough: risk is not needed"; add Enter
sleepfor 1.2
add 'Type "clear"'; add Enter
typeit "# 3. kill it right after the refund is sent"; add Enter
sleepfor 0.5
add 'Type "refund -crash-after-send"'; add Enter; add 'Wait+Screen@30s /dies before/'; sleepfor 1.5
add 'Type "clear"'; add Enter
typeit "# 4. restart: outcome unknown, so it halts"; add Enter
sleepfor 0.5
add 'Type "refund -reconcile=false"'; add Enter; add 'Wait+Screen@30s /not reconciling/'; sleepfor 2.0
add 'Type "clear"'; add Enter
typeit "# 5. restart: it asks Stripe what happened"; add Enter
sleepfor 0.5
add 'Type "refund"'; add Enter; add 'Wait+Screen@40s /Receipt/'; sleepfor 2.0
add 'Type "clear"'; add Enter
typeit "# check Stripe: refunds carrying this run's key"; add Enter
sleepfor 0.4
typeit 'curl -sK $CFG $URL | grep -c bide_key'; add Enter; sleepfor 3
add Hide
add 'Type "mark end"'; add Enter; add 'Sleep 500ms'

(cd "$work" && vhs promo.tape > vhs.log 2>&1) || { sed 's/sk_test_[A-Za-z0-9_]*/[masked]/g' "$work/vhs.log" | tail -20 >&2; exit 1; }
[ -s "$work/terminal.mp4" ] || { echo "stripe-promo-record.sh: vhs wrote no recording" >&2; exit 1; }

# ---- the page in each state, from the journal copies
shot() { # url out [skip]: a screenshot of the page from skip CSS pixels down, the pane's size
  local skip=${3:-$SKIP}
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars --force-dark-mode --force-device-scale-factor=$DSF \
    --window-size=600,$(echo "$PH / $DSF + $skip + 2" | bc) --screenshot="$2" "$1" > /dev/null 2>&1
  ffmpeg -loglevel error -y -i "$2" -vf "crop=$W:$PH:0:$(echo "$skip * $DSF / 1" | bc)" "$2.tmp.png" && mv "$2.tmp.png" "$2"
}
# The timeline's record of the reconciled step ("reconciled by ResolveHalt", with Stripe's refund as
# evidence) is record 12 of 14: shoot the whole page tall and crop that
# record, measured up from the page's end.
recon_shot() { # url out
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars --force-dark-mode --force-device-scale-factor=$DSF \
    --window-size=600,4400 --screenshot="$work/tall.png" "$1" > /dev/null 2>&1
  python3 - "$work/tall.png" "$2" "$DSF" "$W" "$PH" <<'PY'
import sys
from PIL import Image
src, out, dsf, w, ph = sys.argv[1], sys.argv[2], float(sys.argv[3]), int(sys.argv[4]), int(sys.argv[5])
im = Image.open(src).convert("RGB")
bg = im.getpixel((im.width - 2, im.height - 2))
last = im.height - 1
while last > 0 and all(im.getpixel((x, last)) == bg for x in range(0, im.width, 7)):
    last -= 1
top = max(0, int(last - (608 + 40) * dsf))
im.crop((0, top, w, top + ph)).save(out)
PY
}
port=18700
state() { # name [view flags...]
  local name=$1; shift
  port=$((port + 1))
  "$work/bin/bide" view -db "$SNAP/$name/stripe-refund.db" -run "$R" -addr 127.0.0.1:$port "$@" > "$work/view-$name.log" 2>&1 &
  local pid=$!; pids+=($pid)
  up=
  for _ in $(seq 100); do grep -q "http://" "$work/view-$name.log" && { up=1; break; }; sleep 0.2; done
  [ -n "$up" ] || { echo "stripe-promo-record.sh: bide view did not start for $name:" >&2; cat "$work/view-$name.log" >&2; exit 1; }
  sk=$SKIP
  shot "http://127.0.0.1:$port/run?id=$R" "$work/$name.png" "$sk"
  [ "$name" = resolved ] && recon_shot "http://127.0.0.1:$port/run?id=$R" "$work/reconciled.png"
  kill $pid 2>/dev/null || true
}
# bide view only reads: the journal and every snapshot (the .db, and the -wal when it has content) must be byte for byte the same after the pages are made
# An empty -wal holds no data: SQLite creates one when it opens a journal that has none, so only a -wal with
# content is hashed. A .db is always hashed.
hashes() { local f; for f in "$DB" "$SNAP"/*/stripe-refund.db; do shasum -a 256 "$f"; done; for f in "$DB-wal" "$SNAP"/*/stripe-refund.db-wal; do [ -s "$f" ] && shasum -a 256 "$f"; done; return 0; }
before=$(hashes)
for s in pending ops finance crashed halted resolved; do state "$s"; done
after=$(hashes)
[ "$before" = "$after" ] || { echo "stripe-promo-record.sh: a journal changed while its page was made:" >&2; diff <(echo "$before") <(echo "$after") >&2; exit 1; }
echo "journal and snapshots unchanged by bide view (sha256 of .db and -wal files)" >&2
# Before the first journal exists: a static card.
cat > "$work/card.html" <<'HTML'
<!doctype html><meta charset="utf-8"><style>
body{margin:0;height:100vh;display:flex;flex-direction:column;align-items:center;justify-content:center;text-align:center;
background:#121214;color:#9a9aa3;font:20px -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;padding:0 32px;box-sizing:border-box}
b{color:#ececf0;font-size:34px;margin-bottom:10px}</style>
<b>bide view</b><span>the run's journal, read live</span>
HTML
shot "file://$work/card.html" "$work/card.png"

# ---- compose: the terminal over the page, the page switching state as the terminal reaches it
d=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$work/terminal.mp4")
t0=$(awk '$1 == "start" {print $2}' "$work/marks.txt")
# vhs records frames slower or faster than the wall clock: scale the marks so the last one (the tape's end)
# lands at the video's end
tend=$(awk '$1 == "end" {print $2}' "$work/marks.txt")
k=$(echo "($d - $ENTER) / ($tend - $t0)" | bc -l)
echo "clock scale $k" >&2
inputs=(-i "$work/terminal.mp4" -loop 1 -t "$d" -i "$work/card.png")
chain="[0:v]scale=$W:$TH,setsar=1[term];[1:v]setsar=1[b0];"
n=1; prev=b0
for s in pending ops finance crashed halted resolved reconciled; do
  if [ "$s" = reconciled ]; then m=$(echo "$(awk '$1 == "resolved" {print $2}' "$work/marks.txt") + ${RECON_AFTER:-2.2}" | bc -l); else
  m=$(awk -v s="$s" '$1 == s {print $2}' "$work/marks.txt"); fi
  [ -n "$m" ] || { echo "stripe-promo-record.sh: no mark for $s" >&2; exit 1; }
  at=$(echo "$ENTER + $k * ($m - $t0) + ${LAG:-0.35}" | bc -l)
  n=$((n + 1))
  inputs+=(-loop 1 -t "$d" -i "$work/$s.png")
  chain+="[$prev][$n:v]overlay=0:0:enable='gte(t,$at)'[b$n];"
  prev=b$n
  echo "$s at ${at}s" >&2
done
chain+="[term][$prev]vstack,format=yuv420p[v]"
ffmpeg -loglevel error -y "${inputs[@]}" -filter_complex "$chain" -map "[v]" -r 24 -c:v libx264 -crf 20 -preset slow -movflags +faststart "$work/out.mp4"
# the GIF: 720 wide keeps it under about 5 MB
ffmpeg -loglevel error -y -i "$work/out.mp4" -filter_complex "[0:v]fps=10,scale=720:-1:flags=lanczos,split[x][y];[x]palettegen=max_colors=96:stats_mode=diff[p];[y][p]paletteuse=dither=none:diff_mode=rectangle" "$work/out.gif"

# ---- the key must not be in anything kept
key_guard "$work" "$STRIPE_SECRET_KEY" || { echo "stripe-promo-record.sh: the Stripe key appears in a recording file, or the check failed; nothing was written" >&2; exit 1; }
mkdir -p "$OUT"
cp "$work/out.mp4" "$OUT/stripe-refund-split.mp4"
cp "$work/out.gif" "$OUT/stripe-refund-split.gif"
ffmpeg -loglevel error -y -i "$work/out.mp4" -vf "fps=1/2,scale=360:-1,tile=6x3" -frames:v 1 "$OUT/contact-sheet.png"
ls -l "$OUT"/stripe-refund-split.*
echo "duration $d s"

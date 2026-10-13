#!/usr/bin/env bash
# stripe-refund-record.sh: regenerate examples/stripe-refund/img/stripe-refund-dark.gif, a real
# Stripe TEST-mode run: approve, crash mid-refund, the halted restart, the reconciling restart, and
# Stripe's own count of refunds (1).
#
#   STRIPE_SECRET_KEY=sk_test_... assets/demo/stripe-refund-record.sh
#
# Needs vhs (which brings ttyd and ffmpeg). The key is only ever in the environment: it is not typed
# into the recording or printed. Check the frames before committing: no key, and no account id.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
case "${STRIPE_SECRET_KEY:-}" in sk_test_*) ;; *) echo "stripe-refund-record.sh: set STRIPE_SECRET_KEY to a test-mode key (sk_test_...)" >&2; exit 1 ;; esac
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

(cd "$root/examples/stripe-refund" && GOWORK=off go build -o "$work/stripe-refund" .)
export PATH="$work:$PATH"
export PI=$(curl -sS https://api.stripe.com/v1/payment_intents -u "$STRIPE_SECRET_KEY:" \
  -d amount=5000 -d currency=usd -d payment_method=pm_card_visa -d confirm=true \
  -d "automatic_payment_methods[enabled]=true" -d "automatic_payment_methods[allow_redirects]=never" |
  python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
# A fresh run id: Stripe replays an Idempotency-Key for 24 hours.
export R=demo-$(date +%H%M%S)
export DB="$work/stripe-refund.db"

cd "$work"
vhs "$here/stripe-refund.tape" > vhs.log 2>&1 || { sed 's/sk_test_[A-Za-z0-9_]*/[masked]/g' vhs.log | tail -20 >&2; exit 1; }
ffmpeg -loglevel error -y -i stripe.mp4 -vf "fps=12,palettegen=max_colors=128:stats_mode=full" pal.png
ffmpeg -loglevel error -y -i stripe.mp4 -i pal.png -lavfi "fps=12 [x]; [x][1:v] paletteuse=dither=none" \
  "$root/examples/stripe-refund/img/stripe-refund-dark.gif"
[ -z "${KEEP_MP4:-}" ] || cp stripe.mp4 "$KEEP_MP4"
echo "wrote examples/stripe-refund/img/stripe-refund-dark.gif"

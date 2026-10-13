#!/usr/bin/env bash
# Kill the refund agent mid-refund against Stripe TEST mode, restart it, and show that exactly one
# refund exists. Usage: STRIPE_SECRET_KEY=sk_test_... ./demo.sh pi_...
set -euo pipefail
cd "$(dirname "$0")"

pi="${1:?usage: STRIPE_SECRET_KEY=sk_test_... ./demo.sh pi_...}"
: "${STRIPE_SECRET_KEY:?set STRIPE_SECRET_KEY to a test-mode secret key (sk_test_...)}"
case "$STRIPE_SECRET_KEY" in sk_test_*) ;; *) echo "STRIPE_SECRET_KEY must start with sk_test_: this example never uses live mode" >&2; exit 1 ;; esac

# A fresh run id and journal each time: Stripe replays an Idempotency-Key for 24 hours, so reusing
# one would return the old refund instead of making a new one.
run="demo-$(date +%s)"
db="$(mktemp -d)/stripe-refund.db"
go build -o "$(dirname "$db")/stripe-refund" .
bin="$(dirname "$db")/stripe-refund"
common=(-db "$db" -payment-intent "$pi" -run "$run" -amount 100 -approval-threshold 1)

echo "== 1. Start a refund of 100 (minor units); it is over the threshold of 1, so it needs 2 of 3 approvers"
"$bin" run "${common[@]}"
echo; echo "== 2. Two approvers sign off"
"$bin" approve -db "$db" -run "$run" -as ops
"$bin" approve -db "$db" -run "$run" -as finance
echo; echo "== 3. Resume, and kill the process right after the refund request is sent"
"$bin" run "${common[@]}" -crash-after-send || echo "(process exited with status $?: killed mid-refund)"
echo; echo "== 4. Restart WITHOUT the reconciler: it halts instead of refunding again"
"$bin" run "${common[@]}" -reconcile=false
echo; echo "== 5. Restart with the reconciler: it asks Stripe for the refund, resolves the halt, finishes"
"$bin" run "${common[@]}" -min-halt-age 2s
echo; echo "== 6. Stripe's own record: refunds of $pi carrying this run's key (expect exactly 1)"
curl -sS -u "$STRIPE_SECRET_KEY:" -G "${STRIPE_API_BASE:-https://api.stripe.com}/v1/refunds" -d payment_intent="$pi" -d limit=100 \
  | { grep -o "\"bide_key\": *\"$run/refund-1\"" || true; } | wc -l | sed 's/^ *//; s/^/matching refunds: /'
echo; echo "Journal: $db"
echo "View it:  bide view -db $db     (go install github.com/bide-ai/bide/cmd/bide@latest)"

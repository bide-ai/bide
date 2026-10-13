# The terminal recording behind examples/stripe-refund/img/stripe-refund-dark.gif. Run it through
# stripe-refund-record.sh, which builds the example, makes a fresh test payment intent and has the
# test key in the environment before recording starts, so the key is never typed or shown.
Output "stripe.mp4"

Set Shell "bash"
Set FontSize 14
Set Width 1200
Set Height 310
Set Padding 20
Set Theme "Dracula"
Set TypingSpeed 28ms
Set Framerate 20

Hide
Type `PS1='$ '; refund() { stripe-refund run -db "$DB" -payment-intent "$PI" -run "$R" -amount 2500 -approval-threshold 1000 "$@"; }; approve() { stripe-refund approve -db "$DB" -run "$R" -as "$1"; }; stripe_refunds() { n=$(curl -sS -u "$STRIPE_SECRET_KEY:" -G https://api.stripe.com/v1/refunds -d payment_intent="$PI" | grep -c '"bide_key"'); if [ "$n" = 1 ]; then printf '\033[1;32m✓ Stripe holds exactly 1 refund for this run\033[0m\n'; else printf '\033[1;31m✗ Stripe holds %s refunds for this run\033[0m\n' "$n"; fi; }; clear`
Enter
Show

Sleep 500ms
Type "refund   # refund 25.00 of a 50.00 test payment: over the threshold, so it needs 2 of 3 approvers"
Enter
Sleep 2500ms
Type "approve ops; approve finance"
Enter
Sleep 2s
Type "clear"
Enter
Type "refund -crash-after-send; echo exit status $?"
Enter
Sleep 3s
Type "clear"
Enter
Type "refund -reconcile=false   # restart: the outcome is unknown, so it does not refund again"
Enter
Sleep 3s
Type "clear"
Enter
Type "refund   # restart with the reconciler"
Enter
Wait+Screen@30s /receipt/
Sleep 3s
Type "stripe_refunds   # ask Stripe how many refunds this run made"
Enter
Wait+Screen@30s /Stripe holds/
Sleep 3s

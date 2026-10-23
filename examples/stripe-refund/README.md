# stripe-refund: kill an agent mid-refund on a real payment API

A refund agent that talks to Stripe's **test mode** API. It sends a refund, is killed after the
request goes out and before it can write down the answer, and on restart does **not** refund
twice. It halts (the outcome is unknown), asks Stripe for its own record of the refund, resolves
the halted step from that record, and finishes. One refund exists.

It is the crash demo behind `bide demo` (a fake provider) against a real API, with a 2-of-3
approval gate for large refunds. Stripe is called with `net/http` directly: no Stripe SDK, no new
dependency.

<p align="center"><img src="img/stripe-refund-dark.gif" width="100%" alt="A real Stripe test-mode run: two approvers sign off, the process is killed right after the refund request is sent (exit status 3), the restart halts because the outcome is unknown, the reconciling restart finds the refund in Stripe's own record and completes, and Stripe holds exactly 1 refund"></p>

The recording is a real run against Stripe test mode (`assets/demo/stripe-refund-record.sh` regenerates it).

## The flow

1. **Look up** the payment intent and its charge (`GET /v1/payment_intents/{id}`, `GET /v1/charges/{id}`): read-only, safe to re-run.
2. **Approval gate.** A refund over `-approval-threshold` (default 10000 minor units, 100.00) goes through the `refund_large` tool, which needs 2 of 3 approvers (`ops`, `finance`, `risk`), each signing the exact call. At or under the threshold, the `refund` tool runs without a gate. Same code, same journal.

   `-max-refund N` adds a hard ceiling: one amount-cap rule (`agent.WithCaps`, see [amount caps](../../docs/guides/amount-caps.md)) covers both refund tools, so a refund over N minor units (USD) is refused whichever tool is picked, before any approver is asked. The approval gate stays on `refund_large`, because a cap that holds a call for approval has not shipped. The ceiling is in USD and the rule reads the amount alone (the tool's arguments have no currency member for a rule to check), so with `-max-refund` set the example itself refuses a payment in any other currency, before any request to Stripe: 4000 would otherwise be 40 EUR, 4000 yen or 4.000 KWD. A real tool with a currency member in its arguments would write `currency=` in the rule instead.
3. **Refund** (`POST /v1/refunds`): a side effect, so bide writes an attempt marker first and never retries it blindly. The request carries an `Idempotency-Key` and metadata naming the run and call.
4. **Receipt**: the agent's final answer, from the refund Stripe reports.

The model here is scripted (no LLM key needed); the tools, the journal, the gate and the
reconciler are the real thing.

## Five-minute setup

You need Go 1.27 and a Stripe account in **test mode** (no real money moves).

1. Get a test secret key from the Stripe dashboard (Developers, API keys, with the "test mode" toggle on). It starts with `sk_test_`.

   ```sh
   export STRIPE_SECRET_KEY=sk_test_...
   ```

   The program refuses any other key, with a clear error, before it sends a request. It never prints or logs the key.

2. Create a payment to refund. One `curl` makes and confirms a test payment with Stripe's test card:

   ```sh
   curl -sS https://api.stripe.com/v1/payment_intents -u "$STRIPE_SECRET_KEY:" \
     -d amount=5000 -d currency=usd \
     -d payment_method=pm_card_visa -d confirm=true \
     -d "automatic_payment_methods[enabled]=true" \
     -d "automatic_payment_methods[allow_redirects]=never" | grep '"id": "pi_'
   ```

   Or with the Stripe CLI: `stripe payment_intents create --amount 5000 --currency usd --payment-method pm_card_visa --confirm --automatic-payment-methods.enabled=true --automatic-payment-methods.allow-redirects=never`.

   Note the `pi_...` id it prints.

## Kill it, restart it

The one-command version (a fresh run and journal each time, a 100-cent refund so one payment
intent serves many runs):

```sh
cd examples/stripe-refund
./demo.sh pi_...
```

By hand, to see each step. The refund is 25.00 of the 50.00 payment, and the threshold is lowered
to 10.00 so it needs approval. Pass the same flags on every restart: they are part of what the
call is.

```sh
export STRIPE_SECRET_KEY=sk_test_...
F=(-payment-intent pi_... -run r1 -amount 2500 -approval-threshold 1000)   # an array, so it works in bash and zsh

# 1. Start the refund: over the threshold, so it pauses for 2 of 3 approvers.
go run . run "${F[@]}"
go run . approve -run r1 -as ops
go run . approve -run r1 -as finance

# 2. Resume and kill the process right after the request is sent (exit status 3).
go run . run "${F[@]}" -crash-after-send

# 3. Restart without the reconciler: it halts and sends nothing.
go run . run "${F[@]}" -reconcile=false

# 4. Restart with it (the default): it asks Stripe, resolves the halt, finishes.
go run . run "${F[@]}"
```

### What to look for

- **Step 2** prints `-crash-after-send: the refund request was sent; the process dies before its result is journaled`. Stripe has the refund; the journal does not know. Check the dashboard: one refund.
- **Step 3** prints `status: halted ... the outcome is unknown, so it is not sent again`. This is the guarantee: an attempted side effect with no recorded result is never fired a second time on a guess.
- **Step 4** prints `reconcile: found re_... at Stripe`, then `resolved refund-1 from Stripe's record`, `status: done` and the receipt. Look at the dashboard again: still one refund.

## How the reconciler works

Stripe cannot look a refund up by its `Idempotency-Key`, so the refund also carries metadata,
`bide_key=<run>/<call>` (plus `bide_run` and `bide_call`), and the key is deterministic: the same
for every attempt of this step, different for any other. After a halt, the reconciler:

1. waits until the halt is at least `-min-halt-age` old (default 2s), so Stripe's record has had time to appear (asking early would read "absent");
2. lists `GET /v1/refunds?payment_intent=...` (following pagination) and picks the refund whose `metadata.bide_key` is this step's key, not merely the first refund it sees;
3. calls `agent.ResolveHalt` with the refund as the step's outcome, the exact result the tool would have returned, and what it read as **evidence**. The journal marks the result *reconciled* and signs the evidence with it, so a later reader tells a reconciled step from a clean one.

If no refund carries the key, it resolves **nothing**: an absent record does not prove the request never left, so a person decides. If Stripe's refund is `failed` or `canceled`, the step resolves as a failure.

A request that fails in a way that leaves the outcome unknown (a dropped connection, a timeout, a 5xx) returns `agent.ErrToolOutcomeUnknown`, so the next run halts and reconciles by the same path, no crash needed. A 4xx is Stripe refusing: the refund did not happen, and it is recorded as a plain failure.

## See the run

The journal is SQLite (`-db`, default `stripe-refund.db`). View the run, its halt, the reconciled
result with its evidence, and the approval signatures:

```sh
go install github.com/bide-ai/bide/cmd/bide@latest
bide view -db stripe-refund.db
```

## Tests

`go test ./...` needs no Stripe key and no network. `fake_test.go` is a fake Stripe over
`httptest` that checks the bearer token, creates and lists refunds, and honors `Idempotency-Key`
the way Stripe does. The tests run the real binary as separate processes on one journal:

- crash after send, then a halted restart, then a reconciling restart: exactly one refund at the fake, one `POST /v1/refunds` in all, and a reconciled result with the matching refund as evidence in the journal;
- the same story behind the 2-of-3 gate; a reconciler that waits out a young halt, that adopts only its own refund among others on the payment intent, and that refuses to guess when Stripe has no such refund;
- a non-`sk_test_` key (live, restricted, publishable, empty) is refused before any request, and is never printed; the API base is limited to Stripe or loopback;
- `demo.sh` itself, end to end.

The real-API path (`demo.sh` against Stripe) is manual only.

## Notes

- `STRIPE_API_BASE` points the program at a loopback address instead of `https://api.stripe.com`; the tests use it for the fake. Any other host is refused.
- **The approvers' keys are predictable demo keys** (each is SHA-256 of a public name), as in `examples/approval`: anyone can sign as any approver. Never reuse them. Real approvers hold their own keys.
- Stripe keeps an idempotency key for 24 hours and replays the first response for a repeat of it. That is why `demo.sh` uses a fresh run id each time: reusing a run id with the same payment intent and amount would be answered with the earlier refund rather than making a new one.
- Amounts are in minor units (cents), as Stripe counts them.

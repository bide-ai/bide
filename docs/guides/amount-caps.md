# Amount caps: limit the money one tool call can move

A model that is steered, or simply wrong, can ask a tool for any amount. A cap is a rule on one
number in a tool's arguments: over this limit the call is **refused**, and the rule lives in your Go
registration, not in anything the model writes. The design, with the
failure modes, is in the [design note](../design/untrusted-content-and-amount-caps.md) (section 6);
this page is how to use what has shipped.

**What has shipped, and what has not.** v0.23.0 has the per-call form (`> N CUR`) with the action
`-> refuse`. A cumulative rule (`sum N CUR per day`) is refused at registration with a "not yet
supported" error, because it needs the cap ledger, which has not shipped; so is `-> gate`, which
arrives with the escalation gate. **Stability:** caps are experimental, so the Go names
(`agent.WithCaps`) may change in a minor release with a changelog entry.

## A rule

<!-- docsnip: setup type RefundIn struct { Amount int64 `json:"amount"`; Currency string `json:"currency"` }; refund func(context.Context, RefundIn) (string, error) -->
```go
tool, err := agent.Func("refund", "refund a payment", refund,
	agent.WithCaps(`big_refund: refund.amount[minor, currency=currency] > 500 USD -> refuse`),
)
```

A rule is one line: `name: tools.argument[unit, currency=member] > limit CUR -> action`.

- `big_refund` is the rule's name: required, and stable (a later release keys the ledger by it).
  Names are compared without case, and a name is one rule: every tool that carries it carries the
  same line.
- `refund` is the set of tools the rule covers (`refund|refund_large`). A registered tool a rule
  names must carry the rule, so naming a tool cannot leave it uncapped. The argument is an object
  member, or a path of them (`payment.amount`).
- `[minor, currency=currency]` says the argument is an integer count of minor units (cents) and which
  argument holds the currency. `[major, ...]` takes a decimal in major units. A rule with a fixed
  currency and no `currency=` is refused on a tool whose schema has a member with "currency" in its name,
  so the model cannot choose another currency.
- The limit is written in major units with at most the currency's exponent of fraction digits
  (`500 USD`, `500 JPY`, `5.125 KWD`), from the ISO 4217 table. The call is over when its amount is
  **greater than** the limit: a call for exactly the limit passes.
- `-> refuse` is the action (`-> gate` is not yet supported). A name with a `.` (an MCP tool `stripe.refund`) is written
  in double quotes: `"stripe.refund".amount[...]`.

## Over the cap: refuse

The call is not run and not asked. The model reads a tool error naming the rule and saying a limit
was exceeded. It does not say the limit or the amount, because a steered model should not be told how
to stay just under it; the figures are in the journal, in the `gate-reason:<call>` record (rule,
action, why, currency, amount and limit in minor units). A refusal is final for the call: a second
attempt by the model is a new call with its own decision.

## What is checked, and when

At registration (`agent.New`, `agent.WithTools`) every failure is `agent.ErrConfig`: a line that does
not parse; a rule that does not name its tool; an argument the tool's input schema lacks or whose type is
not integer, number or string (a schema reached through `$ref`, `anyOf`, `oneOf` or `allOf` cannot be
checked and is refused); a currency member that is not a string; a limit above 2^53 minor units (a
client that reads numbers as doubles cannot compare it exactly, and no realistic limit is lost);
a rule that gates; and a cumulative rule.

At run time every call is decided **twice**. First on the arguments the model sent, before any gate
or claim: a refused call is answered then, with no attempt marker. Then again on the arguments the
tool will receive, after any tool middleware, immediately before the call begins. A middleware that
raises the amount is refused there, as a known failure the model reads, and the tool is not called.

An amount that cannot be read with certainty is **not assessable** and gets the rule's action, so it
never passes silently: negative, absent, null, not a number or string, an exponent form (`1e3`), a
leading `+`, more fraction digits than the currency allows, or beyond int64. So do arguments that are
not strict JSON anywhere in the document: a repeated member or two members that differ only in case
(`amount` and `Amount`), since a tool that decodes arguments its own way may read the other one. No
float is used anywhere.

A call whose currency no rule of the argument covers is not assessable too, with the **strictest**
action among the argument's rules (refuse if any rule refuses). The currency is compared byte for
byte against the rule's upper-case code: `usd` is not `USD`. bide does not convert currencies.

The `gate-reason` record only adds: the engine recomputes a call's reasons from its arguments and
the registered rules on every evaluation, and enforces the union of those and the recorded ones, so a
record planted in the store can add a requirement and never lift one (a recorded reason refuses the
call even if the rules would let it through now).

## What a cap does not do

- **Array shapes.** A rule reads an object member. A bulk tool that takes
  `payments: [{amount: ...}, ...]` cannot be capped by a line, and registration cannot tell that such a
  tool carries amounts elsewhere in its arguments. Give such a tool one call per payment, or cap it
  with a rule on a total member the tool also takes.
- **No conversion, no totals.** A cap is per call and per currency. Splitting a payment into many
  calls under the limit is the cumulative form's job, which has not shipped.
- **Compensation.** A saga's rollback is not cap-checked, and the rollback's re-run of a call that
  already ran is not either.
- **Plan flows.** A flow node has no cap: the plan registry refuses a tool that carries caps
  (`RegisterTool`, `Builder.Tool`), so a flow cannot run it uncapped by accident.
- **The model is not told.** The rule lines never go into the model's request, but the refusal
  names the rule, so a rule name should not say its limit.

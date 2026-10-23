# Amount caps: limit the money one tool call can move

A model that is steered, or simply wrong, can ask a tool for any amount. A cap is a rule on one
number in a tool's arguments: over this limit the call is **refused**, and the rule lives in your Go
registration, not in anything the model writes. The design, with the
failure modes, is in the [design note](../design/untrusted-content-and-amount-caps.md) (section 6);
this page is how to use what has shipped.

**What has shipped, and what has not.** v0.23.0 has the per-call form (`> N CUR`) and the cumulative
form (`sum N CUR per run|hour|day [by key]`), both with the action `-> refuse`. `-> gate` is refused at
registration with a "not yet supported" error: it arrives with the escalation gate. **Stability:** caps are experimental, so the Go names
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
a rule that gates; and a `by` key member that is not a string or integer member of the schema.

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

## Cumulative rules

`refund_total: refund|refund_large.amount[minor, currency=currency] sum 5000 USD per day by customer_id -> refuse`
counts the amounts of the calls the engine lets begin in a window and refuses the call that would take the
window over the limit. The window is `run` (one per run tree), `hour` or `day` (UTC, fixed, not rolling). The
count is kept per rule, per currency and per key in a **cap ledger**: a reserved journal run under `@cap/`
in the same store as the runs, with no table and no new store method. `agent.WithCapNamespace` names the
deployment's ledgers (default `default`); renaming a namespace or a rule starts new ledgers and so resets
the window's total.

- **When it reserves.** After the call's attempt is claimed and before the tool begins, on the arguments the
  tool will receive (a middleware that raises the amount reserves the raised amount). A call that may have
  run stays counted whatever happens after, so a window only over-counts. A reservation that would exceed the
  limit refuses the call: a known failure the model reads (no figures), the tool not called.
- **Retries and resumes.** A tool that is not retry-safe is one attempt per attempt marker, so a re-attempt
  after a "not started" reserves anew; an `Idempotent` tool has no marker and one attempt per tool-use ID, so
  a middleware that calls `next` twice, a resume and a second driver count once. A model that calls the tool
  again is a new tool-use ID and a new reservation.
- **Fail closed.** If the ledger cannot be read or written, the tool is not run: the run fails, nothing is
  recorded for the call, and a re-drive reaches it again.
- **Amounts and keys.** An amount of zero reserves nothing. A negative, absent or unparseable amount is not
  assessable and is refused. `by customer_id` reads the key from the arguments (strictly: a repeated or
  case-variant member is not assessable), `lower` and `trim` normalise it, and `by principal` uses the run's
  bound principal (`OnBehalfOf`); a missing or empty key, or one with a byte outside ASCII or a control byte,
  is refused (without Unicode normalisation two spellings of one customer would be two windows; normalise the key
  in your tool, or make it ASCII, before it reaches the rule). A key the model
  chooses is only as strong as its canonical form: pair every per-key rule with an unkeyed total.
- **Windows.** A tumbling window allows up to twice the limit across a boundary (spend the limit at 23:59 and
  again at 00:00); processes with skewed clocks put a reservation in either window. A call that crashed and
  resumes after midnight keeps the window it started in: the window is fixed once, in the call's `cap-intent`.
  The clock is checked at that first intent only: a driver whose window is earlier than the newest recorded one,
  or more than one window later, is refused.
- **Cost.** One read of the ledger tail and one insert per reservation. On an `AuditedStore` a reservation
  costs more as the ledger grows (the store anchors the run's head on growth): set `WithMinInterval` on a store
  that holds ledgers, prefer per-key rules (many small ledgers) to one global total with thousands of
  reservations a window, and see [KNOWN-LIMITATIONS](../KNOWN-LIMITATIONS.md).
- **Release.** Only the loop releases, and only a *marked* (not retry-safe) attempt, and only once it has proved
  the tool never began: after the tool chain returned, it seals the call's began word (a tool that began cannot be
  sealed), releases each of the call's reservations, and only then records the attempt as not started. A call
  refused by a second rule after the first reserved, or failed by the store before the tool began, therefore
  gives its room back. `Recover` also runs a reconcile pass over every ledger, releasing the slot of an attempt
  whose marker the journal records as not started (a release that failed, a handler that reserved after the
  chain returned). **Never released:** a call that began or may have (a crash after the claim leaves the marker
  standing and the slot counted), an `Idempotent` call (any number of drivers share its tool-use ID, so one
  driver's proof says nothing about the others: its slot stays counted until the window ends), a call
  `ResolveHalt` resolved, and a saga's rollback (a compensation reserves nothing and credits nothing). A window
  only over-counts.

## What a cap does not do

- **Array shapes.** A rule reads an object member. A bulk tool that takes
  `payments: [{amount: ...}, ...]` cannot be capped by a line, and registration cannot tell that such a
  tool carries amounts elsewhere in its arguments. Give such a tool one call per payment, or cap it
  with a rule on a total member the tool also takes.
- **No conversion.** A cap is per currency; bide does not convert. A per-call rule does not stop a
  payment split into many calls under the limit: that is what a cumulative rule is for.
- **Compensation.** A saga's rollback is not cap-checked, and the rollback's re-run of a call that
  already ran is not either.
- **Plan flows.** A flow node has no cap: the plan registry refuses a tool that carries caps
  (`RegisterTool`, `Builder.Tool`), so a flow cannot run it uncapped by accident.
- **The model is not told.** The rule lines never go into the model's request, but the refusal
  names the rule, so a rule name should not say its limit.

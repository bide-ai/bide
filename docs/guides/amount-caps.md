# Amount caps: limit the money one tool call can move

A model that is steered, or simply wrong, can ask a tool for any amount. A cap is a rule on one
number in a tool's arguments: over this limit the call is **refused**, and the rule lives in your Go
registration, not in anything the model writes. The design, with the
failure modes, is in the [design note](../design/untrusted-content-and-amount-caps.md) (section 6);
this page is how to use what has shipped.

**What has shipped, and what has not.** v0.23.0 has the per-call form (`> N CUR`) and the cumulative
form (`sum N CUR per run|hour|day [by key]`), both with the action `-> refuse`. `-> gate` is refused at
registration with a "not yet supported" error: a cap gate is not built on the taint policy's escalation gate yet. **Stability:** caps are experimental, so the Go names
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
- **Cost.** One read of the ledger tail and one insert per reservation on a plain store. On an
  `AuditedStore` a reservation costs more as its ledger grows, because the store anchors the run's head on
  every growth and reads the whole run to do it: in the benchmarks (`audit/capledger_bench_test.go`, SQLite)
  the bytes allocated per reservation grew from about 340 KB at 0 slots in the ledger to 2.5 MB at 1000,
  about 7 times; a `MemStore` stays flat at 29 to 75 microseconds. Extrapolating the growth, a ledger of
  10,000 slots costs about 25 MB allocated per reservation. So:
  - set `WithMinInterval` on an `AuditedStore` that holds ledgers (`WithMinInterval(time.Minute)` lowered the
    cost at 0 and 1000 slots from 340 KB and 2.5 MB to 240 KB and 1.78 MB: it lowers the constant, not the
    growth);
  - prefer per-key rules (`by customer_id`: many small ledgers) to one global total, and keep a global
    window total below about 1000 reservations a window on an audited store;
  - listing the reserved runs is cheap by comparison: 0.9 to 5 ms per `Runs` pass over 1000 ledger runs and
    19 to 38 ms over 10,000.

  There is no hard limit. If a deployment needs a large global total on an audited store, the remedy is a
  ledger layout that rotates the run (a new ledger run every N slots, the last `cum_after` carried into the
  next); that changes the protocol (model 17), has not been measured, and is not planned unless a user asks.
  See [KNOWN-LIMITATIONS](../KNOWN-LIMITATIONS.md) and [Retention](retention.md).
- **Release.** Only the loop releases, and only a *marked* (not retry-safe) attempt, and only once it has proved
  the tool never began: after the tool chain returned, it seals the call's began word (a tool that began cannot be
  sealed), journals why in a `cap-release:` record (a write that fails releases nothing), releases each of the call's reservations, and only then records the attempt as not started. A call
  refused by a second rule after the first reserved, or failed by the store before the tool began, therefore
  gives its room back. `Recover` and every pass of `RecoverLoop` also run a reconcile pass over the ledgers of open
  windows (a day or hour window until a window length after it ended, a run window until its root run ends),
  releasing the slot of an attempt whose marker the journal records as not started (a release that failed, a
  handler that reserved after the chain returned). A deployment that never calls either keeps those slots
  counted until the window ends. **Never released:** a call that began or may have (a crash after the claim leaves the marker
  standing and the slot counted), an `Idempotent` call (any number of drivers share its tool-use ID, so one
  driver's proof says nothing about the others: its slot stays counted until the window ends, **even when a
  later rule of the same call refuses it, so an Idempotent tool under two rules can lose room in the first
  window to calls that never ran**; give a money-moving tool that is Idempotent one rule, or the rules it needs
  and no more), a call
  `ResolveHalt` resolved, and a saga's rollback (a compensation reserves nothing and credits nothing). A window
  only over-counts.

## Auditing the ledgers: `bide audit caps`

A window's total is the sum of the live reservations in its ledger, and the ledger is rows in your store.
`bide audit caps` reads them and tells you whether they hold together. It never writes.

```
bide audit caps -db journal.db -sth heads.jsonl -pubkey log.pub
bide audit caps -db journal.db -unanchored
bide audit caps -postgres-env BIDE_DSN -sth heads.jsonl -pubkey log.pub -rule refund_total -window 2026-10-22
```

For each ledger run (`@cap/...`) it checks:

1. **The ledger's own rules.** The slots are dense (0, 1, 2, ...), each `cum_after` follows from the one
   before it, a release voids an earlier live reserve of the same attempt and amount, the over flag is
   what the limit implies, and the currency is the ledger's. Anything else is `CORRUPT`.
2. **The window total.** The sum of the live reserves is recomputed and must equal the last `cum_after`
   (`TOTAL`), and no attempt may hold two live reserves (`DOUBLE`). The slots counted over the limit under a
   waiver are listed as `OVER LIMIT`.
3. **The runs.** A reservation leaves a `cap-reserve:` record in the run that made the call, naming the
   ledger, the slot and its `cum_after`. A record whose slot is missing is a `DELETED` slot; one whose
   slot differs (its `cum_after`, its attempt, or it is a release) is `CHANGED`; a reserve slot whose run
   holds no record is `UNRECORDED`: a failure when that run has ended, a warning when it has not (a
   reservation interrupted between its slot and its record looks the same, and a re-drive of the run
   writes the record). A ledger run the store no longer holds but a record or a signed head names is
   `LEDGER_MISSING`.
4. **The intents.** Every reserve slot's attempt must have exactly one `cap-intent:` record, in the run
   that made the call, and the intent must send the attempt to this ledger with this amount, currency
   and marking (`INTENT_MISSING`, `INTENT_DUPLICATE`, `INTENT_MISMATCH`, `INTENT_UNREADABLE`). An intent whose
   window differs from its slot's is the window-pinning attack: the writer chose the ledger the attempt is
   counted in. For a `day` or `hour` rule the window must also be in the rule's index run (`INDEX_MISSING`)
   and not earlier than the newest window the index held when the intent was written (`INTENT_CLOCK`). The
   window must also fit the call's own time (`INTENT_WINDOW`, whatever ledger the intent names and whatever
   `-window` says): it must be the window that contains the attempt marker's `recorded_at`, or the next one. The
   engine writes it when it claims the attempt, on the clock the run was driven with, a moment before the
   first intent, and a caller cannot set it (the marker's `attempted_at` is the wall clock, which a run driven with `WithClock` does not share).
   A marker written before `recorded_at` existed dates nothing and counts like an unmarked attempt. One window forward is allowed, for a boundary that falls between the
   claim and the intent; one window back never is, because that is how a planted intent gains room. An
   attempt with no marker (a retry-safe tool) has only its run's start to date it by: its window must not be
   earlier than the window of `run:start`, and a long-lived run can pin to any later one, so the report counts
   those intents. A `per run` window must be the call's root run. An intent with no slot is information: the attempt was interrupted before its slot, or the reservation was
   refused, and it counts nothing.
5. **The releases.** Only the loop releases, and only a call it proved never began. Before it releases, the
   loop journals why in a `cap-release:` record in the run that made the call; the reconcile pass releases a
   voided attempt and writes none, the void being its reason. A release of a reservation whose run holds a
   successful tool result for the call is `RELEASE_OF_A_CALL_THAT_RAN` and fails, note or no note, whoever
   wrote the slot and whether or not a signed head covers it. Otherwise a release must be explained by its
   `cap-release:` record (which must read and name the attempt) or by the attempt's void
   (`RELEASE_UNCONFIRMED`). With neither, it fails when the audit is anchored and warns when not: a crash
   between the release and the void looks the same, and a re-drive voids it, but a writer of an unsigned
   store can leave exactly this. A release of a call journaled before the record existed (its marker has no
   `recorded_at`) cannot be told from a forgery: it warns, anchored or not, and the report counts it. The
   record shows that the release was the loop's, not that the call did not run: a writer who plants the
   record beside a forged release of a call that ran and failed passes. With signed heads, a release whose
   explanation lies past the run's newest head (or in a run no head covers) fails (`RELEASE_UNATTESTED`).
6. **The signed heads** (`-sth`, with `-pubkey`). An `AuditedStore` anchors a head for every ledger run it
   writes; pass the heads from your anchor log (one JSON head per line, any number, for any runs). Each
   must verify, and the ledger must be the one the newest head signs: a slot changed or deleted since is a
   `DIVERGENT_HEAD`. The runs' own records are held to the same: the `cap-intent` and `cap-reserve`
   records of each run must lie inside a signed head of that run, or the run is `RUN_NOT_ATTESTED` (the heads
   file should hold the heads of those runs too; an `AuditedStore` anchors every run it writes). Entries **after** the newest head are `NOT_ATTESTED` and fail: whoever can write the
   store could have deleted a slot or appended a release there unseen. A signature covers what the store was
   given at signing, not that it was legitimate: a slot written through the signing store is signed. Run the audit after the store has
   anchored (with `WithMinInterval`, after the interval or a `Flush`).

It also reports, without failing, the **reconcile exposure**: per window, the slots still counted for
attempts the journal records as never started, and their amount. Those are the slots the next `Recover`
releases; a figure that does not go down after a recovery pass means recovery is not running.

**Anchored or not.** With `-unanchored` the first line of the report says that whoever can write the store
can delete a slot or append a release undetected, and that the audit only finds inconsistencies it can see.
That is the limit, not a modesty: an attacker who edits a slot and the run's `cap-reserve` record to match,
or who appends a valid release of a reserve whose call also has no result or ended in an error (and edits that
run's journal to match), leaves a ledger every check above accepts. Only a signed head, held outside the store,
shows a change made after the head. The audit does find the common forgery, a release of a call that ran. See [KNOWN-LIMITATIONS](../KNOWN-LIMITATIONS.md).

**Filters.** `-namespace`, `-rule` and `-window` narrow the ledgers read, and `-run` narrows the audit to
one run's calls and its sub-runs' (their slots and records, and the ledgers that hold them). A filtered
report lists what it did not check and ends `PASS (filtered)`, never a bare `PASS`; a filter that matches no
ledger exits 4. Amounts are in minor units, as the ledger holds them.

**Exit status** is `bide audit`'s: 0 when every ledger checked reads and agrees (with `-sth`, and is
unchanged since its newest signed head); warnings do not change it, 1 for any finding, 2 for a usage error (neither `-sth` nor `-unanchored`, or both), 4 when the
store cannot be opened or its runs cannot be listed. A single run that cannot be read (a corrupt row, a
record that does not decode) is not that: it is an `UNREADABLE` finding on the run (exit 1), the slots that
run made are left out of the cross-checks that need it, and the audit goes on with the rest. A read that fails because the store does (a lost connection, a timeout, a cancelled context) ends the audit with exit 4 and "store error, re-run the audit"; the stores report a corrupt row and an I/O error alike, so the audit tells them apart by the causes only an I/O failure has, and an error it cannot place is reported as `UNREADABLE`: re-run the audit before treating one as tampering. The `-json` report carries the findings and the per-ledger figures.

It does not check the index runs for entries no intent uses (they cost nothing), and it does not re-apply
the writer's clock bound (a setting that is not recorded); the time check above stands in for it.
**What remains.** Someone who can write the store and sign heads (a process that writes through the signing
store, or holds the log key) can write a history that is consistent with itself, and the audit finds only
contradictions: it cannot tell that history from a legitimate one. A head signs whatever the run held when it
was signed: a record planted in a run before a later, legitimate write to the same run is covered by the head
that write anchors, so the head attests the plant and `RUN_NOT_ATTESTED` cannot see it. Detection by heads works
only for what was written after the newest head; the time check above is what catches a plant of that kind. `bide sample` writes a small ledger to
try it on: `bide audit caps -db sample.db -unanchored`.

## A worked configuration

One tool, three rules, and a namespace for the deployment: a ceiling on one call, a total for the day, and a
total per customer for the day.

<!-- docsnip: setup type RefundIn struct { Amount int64 `json:"amount"`; Currency string `json:"currency"`; CustomerID string `json:"customer_id"` }; refund func(context.Context, RefundIn) (string, error); model agent.Model; journal *agent.Journal -->
```go
tool, err := agent.Func("refund", "refund a payment", refund,
	agent.WithCaps(
		`big_refund: refund.amount[minor, currency=currency] > 500 USD -> refuse`,
		`refund_total: refund.amount[minor, currency=currency] sum 5000 USD per day -> refuse`,
		`refund_by_customer: refund.amount[minor, currency=currency] sum 1000 USD per day by customer_id -> refuse`,
	),
)
if err != nil {
	panic(err)
}
a, err := agent.New(model, journal, agent.WithTools(tool), agent.WithCapNamespace("payments-prod"))
```

A call is refused if it is over 500 USD, would take the UTC day over 5000 USD for all customers, or would
take that customer over 1000 USD for the day. The per-customer rule is paired with the unkeyed total, as
advised above: a key the model chooses is only as strong as its canonical form. The journal can be any store,
an `AuditedStore` included; the ledgers are runs under `@cap/payments-prod/`.

`bide sample` writes a small ledger like this one: its two evidence runs (`refund-1041` and `refund-1049`)
refund 120 USD each under `sum 500 USD per day`, so the sample journal holds one cap ledger with two slots.

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

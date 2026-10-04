---
title: "Introducing bide: durable agents in Go"
description: "Agents that resume from a journal after a crash, run side effects at most once, and stop when the outcome is unknown."
date: 2026-10-04
author: Dayna Blackwell
---

# Introducing bide: durable agents in Go

*Dayna Blackwell, 2026-10-04*

An agent that only answers questions can crash and start over. One that charges a card, sends an
email or updates a record cannot: if the process dies after the effect fired but before anyone
wrote down that it did, a naive restart fires it again. bide is a Go library for the second kind of
agent.

## What bide is

bide runs an agent loop (a model, some tools, a store) on top of an append-only journal. Every model
turn and every tool result is a named record. When a process dies and you call `Run` again with the
same run ID and input, the run replays its journal and continues where it stopped, without redoing the
work it already recorded.

A tool that changes the world is, by default, a side effect, and bide runs it **at most once**. Before calling it, the run writes an attempt marker. On
resume there are three cases:

1. Nothing was recorded: the call never started, so it runs.
2. The result was journaled: the run reads it and does not call the tool again.
3. The marker is there but the result is not: the effect may or may not have happened. bide does
   not guess. The run halts with `OutcomeUnknown` and waits for a person or a reconciler to record
   what happened.

The guarantee is at-most-once, not exactly-once: a crash in
that window can leave an effect fired once but unconfirmed, and the run stops rather than pretend it
knows. A tool you declare `ReadOnly` or `Idempotent` skips the marker and is retried instead, so
the halt is reserved for effects that are neither.

The [guarantee page](../GUARANTEE.md) states its conditions: the store must
survive the crash (SQLite or Postgres; the in-memory store is for development), its writes must be
atomic, and each tool must declare its safety accurately. It promises safety, not liveness: a run
can still end up halted and need a decision.

## Your first agent

bide needs Go 1.27. In a new module:

```
go mod init example.com/hello
go get github.com/bide-ai/bide/agent@latest
```

This is the first agent from [Getting started](../getting-started.md), unchanged. It compiles against
v0.11.0:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/openai"
)

type WeatherArgs struct {
	City string `json:"city" desc:"city name"`
}

func main() {
	// Any OpenAI-compatible endpoint; get a key at openrouter.ai, or see "No API key?" below.
	model := openai.New(os.Getenv("OPENROUTER_API_KEY"),
		openai.WithBaseURL("https://openrouter.ai/api/v1"),
		openai.WithModel("openai/gpt-4o-mini"),
	)
	weather := agent.MustFunc("get_weather", "Get the weather for a city",
		func(_ context.Context, in WeatherArgs) (string, error) {
			return "72F and clear in " + in.City, nil
		}, agent.WithSafety(agent.Safety{ReadOnly: true}))

	journal, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		log.Fatal(err)
	}
	a, err := agent.New(model, journal, agent.WithTools(weather))
	if err != nil {
		log.Fatal(err)
	}
	out, err := a.Run(context.Background(), "run-1", agent.UserText("Weather in SF?"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out.Message.Text())

	// The same run ID again: the run is finished, so its journaled answer comes back, and the
	// model is not called.
	again, err := a.Run(context.Background(), "run-1", agent.UserText("Weather in SF?"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(again.Message.Text() == out.Message.Text()) // true
}
```

It prints the model's answer, then `true`: the second `Run` of `run-1` read the answer back from
the journal instead of calling the model. Without a key, the guide's
[agenttest section](../getting-started.md#no-api-key-use-agenttest) swaps in a scripted model.
`NewMemStore` keeps the journal in memory; to survive a restart, pass the SQLite store
(`store/sqlite`) or the Postgres store (`store/postgres`) to `NewJournal` instead.

## One crash, end to end

One run we did by hand had two tools: `charge_card`, a side effect that appends a
line to a file, and `write_report`, a slow tool that overwrites a marker file, declared
`ReadOnly` because re-running it is harmless. The model was
`openai/gpt-4o-mini` through OpenRouter, and the journal was on `store/sqlite`. While
`write_report` was running we killed the process with `SIGKILL`, started it again, and called
`Run` with the same run ID and input. The report tool, cut off mid-call, ran again; the
charge, already recorded, did not, and the run finished. The charge file held one line.

That is one schedule, not a proof. Had the kill landed between the attempt marker and the journaled
result, the resumed run would have halted with `OutcomeUnknown` instead of finishing, as
intended. `examples/recover` shows both sides offline, with a scripted model:
it resumes without firing its tool again, then halts on a lost outcome and resolves it with
`ResolveHalt`:

```
go run github.com/bide-ai/bide/examples/recover@latest
```

## What else is in the box

**A verifiable audit trail.** The `audit` package commits the journal to an RFC 6962 Merkle tree, the structure
Certificate Transparency uses. An inclusion proof shows one action is in the log; a consistency
proof shows the log was only appended to. The `bide-audit` CLI checks these offline, from an exported
bundle and a public key, with no access to your database:

```
brew install bide-ai/tap/bide-audit
```

Verifying the hashes detects accidental corruption, but it detects
deliberate tampering only when the root is also committed somewhere the operator cannot
rewrite (signed with a key the application cannot use freely, or published to a separate system).
See [Audit](../guides/audit.md).

**Human approval, including k-of-n.** A tool can carry an approval gate that pauses the run before
the tool executes. A k-of-n gate needs k signed decisions from a fixed, named set of n approvers,
each signed over the exact call, and `bide-audit verify-approvals` checks offline that k named
approvers approved that call. bide checks signatures against the keys you provide; tying a key to a
person is your identity provider's job, and bide cannot tell when one person holds two keys. See
[Human approval](../guides/hitl-approval.md).

**Formal models of the coordination protocols.** The claim protocol, the approval gate, tool
calls, the run lifecycle and recovery, sessions and more are TLA+ models (nine in all). The TLC
model checker explores every interleaving within each configuration's bounds; the models run in
CI on every pull request that changes them, and all of them in the merge queue and on main. They have
found 28 bugs in bide's own design or code, and confirmed one more found in review. Most
were caught before release; four (L1, S1, S2, S4) shipped in releases up to v0.9.0 and were fixed in v0.10.0. Nightly, the Apalache
model checker proves an inductive invariant of the claim protocol: for two drivers over two
processes, with attempts 0 to 3 and a fixed pool of claim ids, at-most-once holds at any depth and
for any number and mix of faults within those bounds. That proof excludes the approval gate and
rests on a stated assumption about the lease check. Nothing is
proven beyond the stated bounds, and the models describe the design, not the Go code. CI checks
that marked code changes with its model; review checks that the code does what the model says.
Details: [How bide is verified](../testing/verification.md)
and the [formal verification overview](https://github.com/bide-ai/bide/blob/main/docs/formal-verification.md).

**Certified convergence for shared state.** The optional `govern` module describes shared state as
variables, invariants and events, built on the gsm engine. gsm's `Build` checks a machine against
the conditions of a machine-checked convergence theorem, and returns it only after an oracle
generated from the Rocq proof re-checks it in-process. `CertifyConvergence` emits a certificate you
can re-check offline. The claim is order-independent convergence of the replay for a machine that
meets the theorem's conditions; with event pairs declared `Independent`, it covers only reorderings
across those pairs, and a federation's own conditions are checked by gsm's Go code, not an oracle.
bide requires gsm v0.12.0, and a verdict recorded under gsm v0.11.0 is not covered by its fix. See
[Governance](../guides/governance.md).

## Limits

bide is pre-1.0. Breaking API changes are marked **Breaking** in the
[changelog](https://github.com/bide-ai/bide/blob/main/CHANGELOG.md). The journal format can change
between pre-releases without a version bump, so finish or resolve runs before you upgrade. Durability needs a durable store and
waker, and recovery needs a `Resumer` you supply. The full list is on
[Known limitations](../KNOWN-LIMITATIONS.md); read it before you rely on any guarantee above.

## Get started

- [Getting started](../getting-started.md): install, the first agent, examples.
- [Concepts](../CONCEPTS.md) and [the guarantee](../GUARANTEE.md): the vocabulary and the exact promise.
- Repository: [github.com/bide-ai/bide](https://github.com/bide-ai/bide) (Apache-2.0).
- Install: `go get github.com/bide-ai/bide/agent@latest`, and for the auditor CLI,
  `brew install bide-ai/tap/bide-audit`.

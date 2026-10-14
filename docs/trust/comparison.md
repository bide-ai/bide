# Comparison with Temporal, LangGraph, and retries with idempotency keys

This page compares how bide and three other approaches treat side effects across crashes, human approval, audit evidence, and operations. It is scoped to those four questions. It does not rank products, and the others are stronger than bide on several axes that matter in production; those are stated first.

## Where the others are stronger

- **Temporal** is a mature, widely deployed system with years of production use at scale. It has SDKs in several languages (Go, Java, TypeScript, Python, .NET and others, per [docs.temporal.io](https://docs.temporal.io)), a clustered server that scales horizontally, a managed cloud offering, a visibility and operations UI, and a large body of operational knowledge. bide is a pre-1.0 Go library with one maintainer, no cluster, and no other-language SDKs yet (a wire protocol is [designed, not implemented](../design/protocol.md)). If you need polyglot workers, very high throughput across many nodes, or a vendor you can buy support from, Temporal is the stronger choice today.
- **LangGraph** has a large ecosystem: integrations with many models, tools and vector stores, a large community, hosted tooling and tracing, and a Python and JavaScript developer base. bide has a small set of model adapters (Anthropic, OpenAI-compatible, Gemini), MCP support and bring-your-own retrieval. If you want to prototype quickly against many providers in Python, LangGraph is the stronger choice.
- **Plain retries with idempotency keys** are the simplest design and need no new runtime. When the downstream API honors idempotency keys, this is often enough, and it is the right answer for many payment integrations. bide does not replace it where the downstream supports keys; the two combine (see below).
- **All three** are older and more widely reviewed than bide. bide has had no third-party security audit; its guarantees are specified, model-checked within bounds and tested ([formal verification](../formal-verification.md)), which is not the same as independent review.

## Side effects across crashes

| | bide | Temporal | LangGraph | Retries + idempotency keys |
|---|---|---|---|---|
| Model | Journal with an attempt marker written before a non-idempotent effect; resume replays results | Event history with deterministic workflow replay; activities are retried | Checkpoints of graph state at steps; resume from the last checkpoint | Caller retries; the downstream de-duplicates by key |
| A crash after the effect ran and before its result was recorded | Resume sees a marker with no result and **halts** with `OutcomeUnknown` instead of re-running | The activity may run again: Temporal "guarantees that the Activity will be observed as completed exactly once. However, the Activity may be executed multiple times" and recommends idempotent activities ([activity definition](https://docs.temporal.io/activity-definition)) | A task that started but did not finish may run again on resume; the docs say to design side effects to be idempotent and use idempotency keys ([functional API, idempotency](https://docs.langchain.com/oss/python/langgraph/functional-api#idempotency)) | Safe only if the downstream honors the key for the whole retry window; otherwise a duplicate |
| Requires the effect to be idempotent | No for the at-most-once path; a tool declared `Idempotent` or `ReadOnly` opts out of the halt | Recommended (see quote) | Recommended (see quote) | Yes, that is the mechanism |
| Cost of the guarantee | A halt needs resolution (a person, or a reconciler that queries the provider and calls `ResolveHalt`). Safety, not liveness | Retries complete automatically; duplicates are the downstream's problem | Same | Retries complete automatically |

The measured result for crash points is in [benchmarks/README.md](../../benchmarks/README.md). In the crash-injection benchmark, a non-idempotent `charge` driven through every seeded crash schedule fired at most once under bide and more than once under every other benchmarked Go SDK. LangGraph (Python, SQLite and Postgres checkpointers, every documented way to write the side effect and every durability mode) re-fired in every configuration it was run in, with its "sync" mode being the most durable. The benchmark did not include Temporal, and a side effect written with an idempotency key would not double-charge in any of them, as the benchmark notes. Method, fairness checks and limits are in that file; the maximum fire count is not a ranking.

**Idempotency keys are complementary.** bide's at-most-once rests on its own marker, and a key sent to the downstream (as in `examples/stripe-refund`) covers the case where a human or reconciler decides to retry after a halt. Using both is stronger than either.

## Human approval

| | bide | Temporal | LangGraph | Plain retries |
|---|---|---|---|---|
| Pause for a person | Durable gate before the call (`WithApproval`), or `Interrupt` inside a tool | Workflows can wait for signals or updates, which is how approval is typically built | Interrupts pause a graph at a node, resumable from a checkpoint | Not provided |
| Multiple approvers | Built-in m-of-n gate; each approver signs the exact call (run, call id, tool, canonical arguments) | Build it in workflow code | Build it in graph code | Not provided |
| Proof that the approval happened before the action | `bide audit verify-approvals` checks offline that k named approvers signed before the call ran | The event history records signals received; no signature scheme is part of the product | Checkpoints record state; no signature scheme is part of the product | Not provided |
| Binding keys to people | Not provided; your identity provider's job ([threat model](threat-model.md)) | Your auth layer | Your auth layer | Not applicable |

Where bide is limited: a fixed named approver set, no weighted votes, roles or deadlines, and unsigned `recorded_at` times ([known limitations](../KNOWN-LIMITATIONS.md#approval-and-quorum)).

## Audit evidence

| | bide | Temporal | LangGraph | Plain retries |
|---|---|---|---|---|
| Record of what happened | Append-only journal, Merkle-committed (RFC 6962) | Per-workflow event history, retained per namespace policy | Checkpoint history in the checkpointer's store | Application logs |
| Tamper-evidence | Signed tree heads; consistency and absence proofs; verifiable offline by a third party with a public key. Against the operator only when heads are anchored outside their control | Provided by access control on the cluster and its database; no cryptographic commitment is part of the product | Same | Same |
| Portable evidence for an outside auditor | `EvidencePackage`, `bide audit verify-evidence`, stdlib-only verifier | Export of history is possible; verification is trust in the exporter | Same | Same |
| Confidentiality | Not provided; leaves are not encrypted | Payloads can be encrypted with a data converter you supply (per Temporal docs) | Your storage | Your logs |

The cryptographic features are bide's differentiator on this axis; they prove that a record was committed and not altered, not that it is true or that the model reasoned well ([threat model](threat-model.md)).

## Operations

| | bide | Temporal | LangGraph | Plain retries |
|---|---|---|---|---|
| What you run | Your processes plus SQLite (one machine) or Postgres | A Temporal cluster or Temporal Cloud, plus workers | Your processes plus a checkpointer store (or the vendor's hosted platform) | Your services plus a queue or scheduler |
| Scale | Many concurrent durable runs per process; nodes coordinate through Postgres leases. Not benchmarked against a cluster scheduler | Designed and run at large scale | Depends on deployment | Depends on deployment |
| Maturity | Pre-1.0; formats may change between releases ([stability](stability.md)) | Mature | Mature for its scope | Not applicable |
| Languages | Go only | Several | Python, JavaScript | Any |
| Recovery of dead holders | `RecoverLoop` and leases; the attempt claim, not the lease, keeps effects at most once | Server reschedules | Application-driven resume | Queue redelivery |
| Observability | OTel spans (`trace.Instrument`), `bide view` | UI and metrics | Tracing integrations | Your tooling |

## Choosing

- Money moves, the downstream has no idempotency key, and an auditor will ask you to prove what happened: bide fits, accepting a pre-1.0 Go library and the cost of resolving halts.
- The downstream honors idempotency keys and you need polyglot workers or very large scale: Temporal, or plain retries with keys, is the better fit; bide's audit layer is not available there.
- A Python prototype against many providers: LangGraph. Treat side effects as at-least-once and use idempotency keys.

Sources: bide's [guarantee](../GUARANTEE.md) and [benchmarks](../../benchmarks/README.md); the Temporal and LangGraph pages linked above, read on 2026-10-13. Their products change; check their current documentation. If something here is wrong, please open an issue and it will be corrected.

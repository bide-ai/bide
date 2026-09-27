# Harness vs SDK: the operational axis

## Why this note exists

go-agents ships as an embeddable SDK: you import it, and your process calls the runtime. A recurring
question is whether it should instead be a harness, a long-running runtime that drives your agents for
you. This note records where the project sits, what a harness would add, and the rule that keeps a
harness a front-end over the one substrate rather than a fork of it. It is the operational-axis
companion to the authoring-axis note in [expression-surfaces.md](expression-surfaces.md).

## What it is today: an embeddable SDK

The runtime is a library you call in your own process. You provide a `Model`, a `Durable` store, and
tools, and you invoke the loop; the durability and recovery primitives (`Recover`, `Lease`, `Waker`)
are functions you call, not a daemon that calls you. There is no server, no control plane, and no
supervisor: recovery is caller-driven. This is deliberate. It is the "embeddable, no cluster"
position, and it is the sharpest contrast with the durable-execution servers (Temporal's cluster,
Restate's runtime) that a user must stand up and operate. An SDK embeds in a Go backend with no
operational burden.

It also means go-agents shares, as an SDK, the property we note against ADK and LangGraph: nothing
watches for a crashed run on its own; the caller decides when to `Recover`. For a library that is
correct. Supervision is exactly what a harness adds.

## What a harness adds: inversion of control

A harness drives you. It is a long-running process that:

- owns the agent lifecycle and supervises it (a watchdog that detects a crashed or stalled run and
  re-drives it automatically, rather than waiting for a caller to invoke `Recover`),
- delivers timers and signals continuously (runs the `Waker` and the lease loop as a service),
- exposes a control plane (submit a run, query its status, fetch its result, stream its events) over
  an API,
- dispatches work across workers and hosts the deployment.

These are the pieces that turn a library into a platform. None of them exists today, and that absence
is the whole of what makes go-agents "not a harness."

## Harness-ready by construction

Nothing structural prevents a harness, because the substrate already provides exactly the primitives a
harness runs continuously. A harness is a thin, supervised daemon plus a control plane over the same
journal-backed runs: it runs `Recover` and `Lease` in a loop instead of leaving them to a caller, it
runs the `Waker` as a service, and it drives the same `Durable`-backed journal. Because every run still
lowers to the journal, a harness inherits at-most-once, halt-on-ambiguity, the offline-verifiable audit
trail, and conformance for free. The store stays pluggable; the harness adds operation, not execution.

## The operational invariant

The authoring axis has a rule: a surface may add a way to *author*, never a way to *execute*
(see [expression-surfaces.md](expression-surfaces.md)). The operational axis has the mirror rule:

> A harness may add a way to *run and operate* an agent (supervise, deliver, host, dispatch, expose an
> API), never a new way to *execute* it. It must drive the same journal-backed runs. A harness that
> reimplements execution or durability is a fork, not an operational front-end, and is out of bounds.

This is the same "many front-ends, one substrate" compiler pattern, applied to operation instead of
authoring. An in-process caller and a hosted supervisor are two front-ends over one backend; both
produce the same journal, so both are auditable and conformable in exactly the same way.

Two consequences follow. A polyglot harness (an API that lets a non-Go application submit and drive a
run) is in bounds, because the agent still executes on the one runtime and the API is a transport, not
a second executor. A harness that grew its own retry or state model beside the journal would be a fork,
however convenient, and is forbidden.

## The gate

A harness is subject to the same two-question gate as any new surface:

1. Does it lower to the core? A supervisor that calls `Recover`/`Lease` and a control plane that submits
   runs both drive the existing journal-backed runtime. Yes, by construction, as long as it adds no
   second executor.
2. Does the substrate stay the headline? The runtime and its guarantees are the product; the harness is
   the way you operate them at scale or reach them from another language. If the harness (the hosting,
   the dashboard) becomes the pitch and the guarantees become a detail, that is the failure mode.

Both gates protect the architecture. When to build a harness is a priority call, not a gate.

## Why SDK-first, and where the harness fits

The two forms serve different ends and stack.

- The SDK is the adoption vector: it embeds in a Go service, in the user's process, with no cluster to
  run. It is the low-friction on-ramp and the thing that proves the wedge.
- The harness is the operational and commercial surface. A supervised, hosted runtime with a control
  plane is the natural home for a managed accountability layer (the runtime that runs your agents and
  hands you offline-verifiable proof they behaved), and its API is what lets a non-Go application drive
  the same at-most-once, audited, conformable runs. The substrate is the free foundation; the hosted
  harness is a plausible product on top of it.

So the harness is not a different project. It is the operational front-end over the same substrate, the
way rung-1 `plan` is an authoring front-end (expression-surfaces.md) and governed effects are a
governance front-end ([governed-flows.md](governed-flows.md)). All of them lower to the journal.

## Current status

go-agents is an embeddable SDK, deliberately. No daemon, control plane, or hosting exists, and none is
a work item yet. The substrate is harness-ready: the recovery, lease, and waker primitives are the
pieces a harness would supervise, and the pluggable `Durable` store is the seam a hosted deployment
would use. A harness is an additive layer built through the gate above and the operational invariant;
the substrate is ready for it whenever hosting or polyglot reach is the priority.

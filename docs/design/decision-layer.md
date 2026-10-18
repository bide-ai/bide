# Design: the decision layer (System One deciders that can only make bide stricter)

Status: draft for approval; nothing is built. The direction (target the `/v1/systemone` protocol, not a vendor) was chosen by the user. Decisions that remain are in section 14. Target release: v0.22.0, after the v0.21.0 bank pilot.

## 1. Problem

A bide agent that moves money is governed by rules that people wrote down: approval gates, an authority matrix, a kill switch. Those rules are exact, and they are blind to meaning. "Refund over 500 needs two approvers" cannot notice that the refund memo reads like a social-engineering attempt, that a payee name is a near-miss of a sanctioned party, or that a customer message is asking the agent to ignore its instructions. Today the only judge of such things inside bide is the agent's own model, which is the component an attacker is trying to steer.

System One decision models are a better fit for that gap than a chat model. A caller sends a block of state and typed questions and gets typed answers with probabilities, in about 100 ms, instead of prose. Three question kinds exist: Choice (one option from a supplied set), Score (a position on an ordered rubric) and Noul (a yes/no probability). That output is easy to journal, easy to threshold and cheap enough to run on every call. It is also:

- **not deterministic and not correct by construction.** The same state can score differently on two runs, option order moves probabilities, and "typed is not correct" (vendor wording);
- **steerable.** The state is an attack surface; text inside it can move an answer;
- **often hosted.** The reference service is hosted only, so the state leaves the institution; and
- **versioned by name.** `jev-1.13.0` is a pinned model; `jev-latest` moves.

This document designs how bide can use such a model without letting it become a new way to lose money. The core idea: a decider's output may make bide **stricter**, and may never make it less strict.

Goals:

- a vendor-neutral Go API for typed decisions, one HTTP adapter for every server that serves the `/v1/systemone` shape, and a deterministic rules adapter (OPA) behind the same API;
- every decision journaled as a durable step: a replay reuses the recorded answer and never asks again, and the record names the exact model, endpoint, input and every probability;
- the escalate-only rule, as a type-level and a runtime guarantee, composed with the existing approval gates, notifier and kill switch;
- model risk management (SR 11-7) evidence: a pinned version in signed policy, a calibration check before a version change, and a measured human-overturn rate;
- a plan that checks the "same schema" claim against real servers before any adapter depends on it.

Not goals: letting a decider approve, skip or select anything (that is the point); validating that any model is sound for a use (bide is a runtime, and `eval` is not a validation); a decider for generation or arithmetic (vendor documentation says they are unsuitable); and a deterministic claim about model output (the guarantee is about what bide does with an output, section 7).

## 2. Summary of the design

- **A port, `agent.Decider`**, in the core module: a request of state and typed questions, a result of typed answers with the probability of every option, plus the identity of what answered. The core imports no adapter (`agent/architecture_test.go` enforces this today for the model adapters and would cover these).
- **Adapters**: `decide/systemone` (HTTP, standard library only, so it can live in the root module like `model/anthropic`), `decide/opa` (a nested module, because OPA's dependency tree is large, as for `store/sqlite`), `decide/decidertest` (a scripted fake and a conformance suite, root module). Cedar is deferred (section 14).
- **A decision step** per decision, recorded under the reserved name `decision:<call>:<set>` in the run's journal, with the evidence in section 6 and the state in a separate, redactable record.
- **A signed decision policy** names the deciders (endpoint, pinned model, calibration reference), the rules that map answers to escalations, and the ladder of gates each tool may be escalated through. Every escalation target is declared in it; no answer can name one.
- **A sealed `Escalation` type and a pure `Resolve`** turn recorded answers into a rung on that ladder, and the engine takes the maximum of the base rung and the decided rung. There is no value, constructor or field that means "allow", "approve" or "less gated".
- **Failures fail closed**: a timeout, an error, a malformed or unpinned answer is treated as every rule bound to that call firing.
- **Model risk**: a model version change is a signed policy update that must cite a passing calibration report from `bide decide calibrate` over the customer's labelled cases; `bide decide report` tracks the overturn rate.

## 3. What is known about the protocol (and what is not)

The claim that several servers share one schema comes from vendor and community pages, not from a specification or from a test of ours. This section records what those pages say; section 10 is the plan to check it. Fetched 2026-10-17.

Request and response, as shown by the DigitalOcean and LiteLLM pages (sources below):

```json
{ "model": "jev-1.13.0",
  "state": "string, object or array",
  "questions": { "<name>": { "type": "choice|score|noul", "instructions": "...", "criteria": {"opt": "description"} } } }
```

`criteria` is an object of option to description for `choice`, an ordered list of level names for `score`, and absent for `noul`. The response carries `model`, `answers` keyed by question name (each with `type`, and `choice` plus `probabilities` plus `confidence`, or `score`, or `noul`) and `usage` (`input_tokens`, `output_tokens`). Authentication is a bearer token. Streaming is not supported. A choice has at most 255 options, a score 2 to 10 levels, and Jev takes 64k tokens per request (32k for the state plus the longest question). Jev runs 70 to 500 ms end to end and costs about $0.042 per million input tokens.

Already visible differences between pages: the model id is `typesafe-jev-1.13.0` in DigitalOcean's example and `jev-1.13.0` in LiteLLM's; DigitalOcean's response example also shows `score` and `noul` fields beside `choice`. These are the kind of divergence the conformance suite exists to find.

| Server | Source says | Notes relevant to bide |
|---|---|---|
| Jev (TypeSafe) | hosted only; versioned `jev-1.13.0`; `jev-latest` moves | state is an attack surface; "confidence is not authorization"; add explicit unknown or human-review options; enterprise zero-data-retention on request (wavect review) |
| Kev (Jared Palmer) | Apache-2.0; 0.8B, 4B, 9B on Qwen3.5 and 27B on Qwen3.8; CUDA, ROCm, Apple MLX; "the server exposes POST /v1/systemone with the same request and response shape as TypeSafe's API" | trained on at most 384 state tokens; single-request handling; questions per call and state budget not documented; option order affects results; 4B also on OpenRouter |
| LitJev (zhengxuyu) | "independent reproduction"; `/v1/systemone`: yes | any Qwen model (per the lead's brief; not confirmed by the alternatives page) |
| Von (wfzyx) | Apache-2.0; "serves the same /v1/systemone API as Jev"; CPU, CUDA, ROCm, MPS | ModernBERT encoder; a different architecture from the LoRA-on-LLM models |
| Clef, Clef-flash | listed as Apache-2.0 and by Cloudflare, on Workers AI and OpenRouter; endpoint "not specified" | reads images; the brief says it serves the schema, the page does not; treat as unverified |
| jevos | CPU (per the lead's brief) | not found on the pages read; unverified |
| Laya | self-hosted; `/v1/systemone`: no | has its own router |
| NanoJev | MIT; `/v1/systemone` not specified | its boolean type differs from Noul |
| AnyJev | Nokia researchers; not specified | logit method, no fine-tuning |
| poorjev | named in the brief | not found on the pages read |

Independent claims about accuracy (for example Kev 0.822 and Von 0.720 on one benchmark in the local-ai-zone article) are one blog's numbers and are not used by this design: bide decides nothing from a published benchmark, only from the customer's calibration (section 9).

Sources: [systemonemodels.org alternatives](https://systemonemodels.org/examples/alternatives/), [systemonemodels.org Kev](https://systemonemodels.org/models/kev/), [local-ai-zone deep dive](https://local-ai-zone.github.io/blog/system-one-decision-models-deep-dive.html), [wavect Jev review](https://wavect.io/blog/jev-ai-decision-model-review/), [DigitalOcean System One API](https://docs.digitalocean.com/products/inference/how-to/use-system-one-api/), [LiteLLM TypeSafe pass-through](https://docs.litellm.ai/docs/pass_through/typesafe), [Kev on Hugging Face](https://huggingface.co/jaredpalmer/kev-9b).

## 4. What exists today that this builds on

- `Journal.Step` (`agent/step.go`) runs a named function once, records its JSON result, and on resume returns the recorded result without running it. `WithSafety{ReadOnly: true}` skips the attempt marker, so a step whose call can be repeated simply runs again after a crash. Step names with a reserved prefix are refused (`IsReservedStepName`, `agent/keys.go`), which is how engine-owned names stay engine-owned.
- A gate is `ToolSpec.Approval`, an `ApprovalPolicy` (`agent/tool.go`); the gate a run enforces is the registered tool's, never one read back from the journal. Gates are evaluated at `toolHandler`/`preExecute` (`agent/exec_gate.go`) and for plan flow nodes (`agent/flowgate.go`). A gate pauses as `*ApprovalPending`; the pause contract is sealed (`agent/pause.go`).
- The policy diff gate (`bide check -diff ... -fail-on-weaken`, [plan flows guide](../guides/plan-flows.md)) already defines "weaker" for gates: removing a gate, lowering `need`, removing or lengthening a timeout.
- The kill switch ([design](kill-switch.md)): `StopControl.SetStop` writes a signed or basic stop with a scope (global, tool, flow, agent); `StopSwitch` makes every check point pause covered runs. Its policy chain (`RotatePolicy`: each next policy approved by a quorum of the one before) is the precedent for a signed, rotating policy.
- Redaction ([design](journal-redaction.md)): tombstones keep a record's position and leaf hash; a leaf's salt is random and erased with the content; `agent.RedactionProtected` lists records no redaction may touch.
- Model turns already journal the model, finish reason and digests of the prompt and tool set (`ModelTurn`, `agent/record.go`); the [control mapping](../trust/control-mapping.md) row "Model identity and evaluation" cites it.
- Model adapters are packages of the root module (`model/anthropic`, `model/openai`, `model/gemini`) that the core may not import; adapters with heavy dependencies are nested modules (`store/sqlite`, `signer/awskms`, `anchor/awss3`, `govern/redislog`, and others, each with its own `go.mod` and a `replace` to the root).

## 5. API and module layout

### 5.1 The port (core module, package `agent`)

Names are provisional; the shape is the commitment.

<!-- docsnip: skip design proposal: the types are not built yet -->
```go
// QuestionKind is Choice, Score or Noul.
type Question struct {
    Name         string
    Kind         QuestionKind
    Instructions string
    Options      []Option // Choice: key and description, in a fixed order; at most 255
    Levels       []string // Score: ordered, 2 to 10
}

type Request struct {
    State     json.RawMessage // already projected and minimised by the caller (section 11)
    Questions []Question
}

type Answer struct {
    Name          string
    Kind          QuestionKind
    Choice        string        // Choice: the selected option
    Probabilities []Probability // Choice and Score: every option or level, in the question's order
    Score         float64       // Score: the expected position
    Noul          float64       // Noul: P(yes)
    Confidence    float64       // as the server reports it; recorded, never trusted
}

type Decision struct {
    Answers  []Answer
    Identity DeciderIdentity // what actually answered (section 6)
    Usage    Usage
}

type Decider interface {
    Identity() DeciderIdentity // what the decider will claim to be, before any call
    Decide(ctx context.Context, req Request) (Decision, error)
}
```

Slices, not maps, so canonical JSON and digests are stable. Validation lives in the core, not in each adapter, and runs on every `Decision` before it is used: every question answered exactly once; a choice answer names an option that was supplied; probabilities are finite, within [0, 1] and sum to 1 within a stated tolerance; a score has one probability per level. A violation is a failure (section 7.4), never a repair.

`decidertest.Fake` returns scripted decisions, injected errors, latencies and a call counter; the engine tests use it, and it is the "fake" of PR 1.

### 5.2 Where code lives

| Piece | Location | Module | Why |
|---|---|---|---|
| `Decider`, `Question`, `Answer`, `Decision`, validation | `agent` | root | a port, like `Model` and `Store` |
| `Escalation`, `Rung`, `Resolve`, the decision policy and its loader | `agent` | root | enforced by the engine; must not depend on an adapter |
| The decision step, the engine hook | `agent` | root | same code paths as gates |
| `systemone` HTTP adapter | `decide/systemone` | root | standard library only, same footing as `model/openai` |
| `decidertest` (fake, conformance suite) | `decide/decidertest` | root | tests for any adapter |
| OPA adapter | `decide/opa` | new nested module | OPA's dependencies are large; the core module's graph stays small |
| Cedar adapter | not planned | | section 14 |
| `bide decide calibrate`, `report` | `cmd/bide` | nested, existing | CLI commands live there |

The architecture test (allowlist of the core's runtime import graph) gets a case that fails if `agent` imports `decide/*`.

## 6. Journaling

### 6.1 A decision is a durable step

Each decision runs as an engine-owned step under `decision:<call>:<set>` (`<call>` is the tool-use ID or the plan node key, `<set>` the rule set's ID from the signed policy), added to the reserved prefixes so user code cannot write or collide with it. It runs with `ReadOnly` safety, which is correct for this step and is why:

- **Replay never asks again.** A completed step returns its recorded value; the decider is not called. The conformance test counts calls on the fake across a crash sweep.
- **A crash before the record re-asks, which is safe only because of one invariant**: nothing acts on an answer until the record containing it is durable. The first execution therefore resolves the escalation from the value the step returned (the decoded record), never from the in-memory response. The first answer a crash threw away was never used, so the second may differ without consequence. A test asserts that no gate, route or stop is created from a decision before its record is in the store.
- An error that is not recovered by the retry budget (section 7.4) is itself recorded, as a decision with `outcome` of the failure class and no answers, so the fail-closed result is also replay-stable.

### 6.2 What the record holds

```
decision:<call>:<set>   (protected, never redactable)
  decider:      kind ("systemone", "opa"), the policy's decider id
  model:        the pinned model id the policy names, and the id the server echoed, and whether they match
  endpoint:     scheme, host, port and path (no credentials, no query), the operator's label, and, when the
                server reports one, its build or revision
  policy:       digest of the decision policy in force
  questions:    digest of the canonical question set (including option order)
  input:        SHA-256(salt || canonical state), the salt held only in the input record below
  answers:      every question's kind, selected choice, every option's probability, score, noul, confidence
  outcome:      ok, or the failure class (timeout, transport, status, schema, version, endpoint)
  rung:         the rung Resolve derived, for display only (section 7.3: it is recomputed, never trusted)
  latency_ms, usage
decision-input:<call>:<set>   (redactable)
  salt, the state sent
```

The model identity matters for two reasons. An audit or an SR 11-7 review asks which model produced a decision, and TypeSafe itself recommends storing the versioned id with each logged decision. And the echo is the only evidence that the server did not substitute a different model. The echo is checked against the pin (section 9.1); a server that does not echo is recorded as "not echoed" and the policy chooses whether that is acceptable (`require_echo`, default true in audited mode).

The endpoint identity is the part bide can attest. TLS is verified with the system roots or a pinned CA from the policy; the policy lists the allowed endpoints, and the adapter refuses any other (a configuration or environment change cannot redirect state to a new host), refuses cross-host redirects, and sends the credential only to the listed host. A decision recorded against an endpoint that the policy did not list cannot occur.

### 6.3 Personal data and redaction

The state is the sensitive part and is kept apart from the evidence:

- The outcome record holds no state: a digest, the question set, and probabilities. It holds nothing a customer supplied unless a developer put it in a question's option labels or instructions, which the policy fixes, so they are institution text.
- The digest is salted with a salt that exists only in the redactable input record, so after the input record is redacted a digest of a low-entropy state (an account number, a name) cannot be brute forced from the surviving hash. This follows the redaction design's reasoning about random, erased salts.
- The input record is redactable with the existing machinery once the run is over (`bide journal redact`, `-payloads`). Redaction keeps the outcome record, so "what was decided, by which model, with what probabilities" survives erasure of "about what". A reader that needs the input for calibration or an appeal must read it before redaction, and the guide says so next to the redaction guide's evidence-first rule.
- `agent.RedactionProtected` gains `decision:*`; `decision-input:*` is added to the `-payloads` preset.
- Sending state to a hosted decider is a disclosure to a third party, and redaction does not reach the provider's copy (the existing limitation for model providers applies and is restated in KNOWN-LIMITATIONS). Hence section 11: the caller projects the minimum state, and the policy records whether a decider is hosted.

## 7. The escalate-only rule

This is the core safety property. Stated exactly:

> For any call, the disposition bide enforces with a decider is at least as strict as the disposition it enforces without one, whatever the decider returned (or failed to return). A decider's output can add a human gate, route to a person, or request a stop; it cannot remove a gate, supply an approval, or select an action that the signed policy and the registered tool do not already allow.

### 7.1 The ladder

The signed decision policy declares, per tool (or flow node), a **ladder**: an ordered list of rungs, from 0 up.

- Rung 0 is the base: exactly the tool's registered gate (`ToolSpec.Approval`, or none). The loader compares the policy's rung 0 with the registered tool and refuses a mismatch, so the policy cannot silently weaken a gate declared in code, and code cannot silently skip a gate the policy lists.
- Each higher rung names a gate (an `ApprovalPolicy`, with its approvers), optionally a notifier route, and optionally a stop scope, declared in the policy. The loader validates every rung is **not weaker than the one below it** with the same comparison the policy diff gate uses (removing a gate, lowering `need`, lengthening a timeout are weakenings), and that a stop scope is no wider than the tool, flow or agent the ladder belongs to unless the policy sets `allow_global` explicitly.
- The decider cannot create a rung, a gate or an approver. A rule picks a rung index, and a rung is whatever the signed policy says it is.

A `Rung` is a small integer. The effective rung of a call is the **maximum** of the base (0) and every rung any rule derived. A maximum cannot go down: adding a decider, a rule, or a second decider can only raise it.

### 7.2 Type-level guarantee

- `Escalation` is a struct with unexported fields; the only constructors are `Resolve` (below) and `None`. There is no "allow" or "approve" value, and `Answer` has no method that returns an `Escalation`.
- The engine never accepts an `Answer`, a `Decision` or a string from a decider in a position that selects a gate, an approver, a route or a scope. Those come only from the signed policy by rung index.
- `Resolve(policy, set, answers) Escalation` is a pure function. It evaluates each rule's condition (`noul >= t`, `score >= t`, `choice in {...}`, or `P(option) >= t`) against the answers. Conditions are monotone by construction: the loader accepts only "at least" comparisons and set membership of designated high-risk options, and refuses conditions that fire when risk is low (an `<=` threshold on a risk score, a membership test on the benign option), because such a rule would escalate on the answer that the benign branch trusts.
- `Rung.Join` is `max`. The engine applies a decision only through `Join`; there is no assignment.

What the types cannot say, because the type system does not express it: that a *rule's own meaning* is monotone (a policy author could name the benign option as the escalating one and escalate every safe case, which is stricter and a nuisance, not a weakening), and that application code which reads an answer for its own purposes behaves. The guarantee covers the authority path, not every use of a number a decider returned. Section 7.5 narrows the second.

### 7.3 Runtime guarantee

- The hook sits where gates are evaluated, so every path that can run a call passes it: `toolHandler` and `preExecute` (first execution, recovery, RecoverLoop, a middleware retry) and the plan flow node gate. This is the lesson of the kill switch design (section 6.1 there): a check missing from one path is a path to a tool. A mutation check removes the hook from each path in turn, and each removal must fail a test.
- The decision step runs before the attempt claim, so a call held for a decision is not marked as run.
- On every read, the rung is **recomputed** with `Resolve` from the recorded answers and the policy digest the record names, and the engine uses the maximum of the recomputed and any other source. The stored `rung` field is for display. A forged lower `rung` is ignored; forging the recorded answers needs write access to the store, which is the existing integrity boundary (unsigned records are forgeable by a writer; the audit trail detects it) and is not claimed to be solved here.
- Because the rung is a pure function of the recorded inputs, replay and audit reproduce it, and a stricter policy later does not rewrite what a past run was held to: the record names the policy digest it was decided under.
- The effective gate is read from the signed policy and the registered tool, never from the journal, as the gate comment in `agent/tool.go` requires.
- A call whose tool has a ladder, in a process that has no decider of the policy's id configured, **pauses at the highest gate of its ladder** (fail closed), not runs.

### 7.4 What each rung does

| Rung content | Effect | Reuses |
|---|---|---|
| Gate | the call pauses as `*ApprovalPending` until the rung's gate is satisfied, its reason naming the decision; no new Pause type, so the sealed pause contract is unchanged | gate counting, deadlines, certificate-bound approvers, `bide approve` |
| Route | the notifier routes the pending item to the named channel | notifier routing |
| Stop | `StopControl.SetStop` for the rung's scope. Basic mode needs no key; audited mode needs an enrolled setter whose key the decision service holds (KMS or HSM), and lifting stays a human quorum | kill switch |

A rung is a gate **in place of** the lower gate, not beside it; that is why rungs must not be weaker than the one below, and why the existing tally and exec-hold code, which counts one gate per call, is unchanged. The alternative of a second gate counted in conjunction with the base gate is in section 13; it needs a second tally namespace per call in the most reviewed code in the repository.

Failure handling, all fail closed. A timeout (default 2 s, a policy field), a transport error, a non-success status, a response that fails the section 5.1 validation, a `model` echo that does not match the pin, an unlisted endpoint, an oversized body (bounded, strict JSON, duplicate keys refused), and a missing decider all produce a failed decision, which `Resolve` treats as **every rule of the set firing**. One retry is allowed inside the budget of the timeout; a retry that succeeds is recorded as ok. A failure is recorded once and replay reuses it.

### 7.5 Using a Choice as data

A Choice question can also route work (which queue, which category). Such a use cannot widen authority because it feeds a decision that the gates still make, but bide gives a helper that makes the safe use easy: `Select` builds the option set from what the signed policy allows for the call before sending it, and treats an answer outside that set as a failure (escalation). For a decision whose result is consumed by application code, the guarantee is only that the authority path is unchanged; the docs say so, and the recipe shows routing as the example.

### 7.6 Several deciders

A rule set may name more than one decider (a local Kev and a hosted Jev, say). Rungs from each are joined with `max`, so any one of them can escalate and none can cancel another. This is the safe form of "two models must agree": disagreement escalates. A recorded decision per decider.

## 8. Threat model: a manipulated or wrong answer

Assume the decider is adversarially steerable (the state contains attacker text), or simply wrong, or the vendor changed its model, or the network returns garbage. The question is what that can do.

| Scenario | Outcome |
|---|---|
| Injected text pushes a Noul toward "no risk" | No escalation. The call runs under its base gate exactly as it would with no decider. This is the residual risk: a decider that is fooled adds no protection, which is why the base gate is never removed and why bide's documentation must not present a decider as a control that replaces one |
| Injected text pushes toward "high risk" | A gate or a route: a person is asked, a call is held. Stricter; costs time |
| The response names an option that was not asked, a probability outside [0, 1], NaN, or an unknown question | Validation fails; every rule fires |
| The answer is right but for a different model than the pin | The echo mismatch fails the decision; every rule fires; the record shows both ids |
| The vendor moves to a new model under the same name (an alias) | Only if it echoes the pinned id; otherwise as above. A silent change under an unchanged id is not detectable by bide; calibration drift and the overturn rate (section 9.3) are the detectors, and the docs say so |
| The endpoint is slow or down | Timeout; every rule fires; a human sees more items. Availability cost, stated |
| Config or environment points the adapter elsewhere | The policy's endpoint list refuses it; the call is held |
| A forged `rung` in the store | Ignored; recomputed |
| A raw store writer forges answers | Out of scope here, as for every unsigned record; the signed audit trail and `bide audit` are the detection |
| Attacker crafts state to trip a Stop repeatedly | Stricter (stops, needs a human lift) and a denial of service. Mitigations: stop rungs are scoped no wider than the ladder's tool unless `allow_global`; the recommended default is gate and route rungs per call and Stop only from the aggregate monitor (section 9.3). Open question 6 |
| An operator relaxes a base gate because the decider "covers it" | Refused by design: a rung 0 that differs from the registered gate fails to load, and `-fail-on-weaken` flags any gate change. Social pressure is not solved by software |
| The decider sees personal data it should not | Section 11: projection, disclosure recorded, the provider's copy not reachable by redaction |

So the worst case of a wrong answer is one of two outcomes: an unnecessary escalation, or no escalation, which is the world without the decider. The first is a cost in people's time (and, if too frequent, in their attention, which is why the overturn rate is monitored). It is never a call that runs which would not have run. This is a claim about bide's handling of an answer; it says nothing about the decider's accuracy.

## 9. Model risk (SR 11-7)

A runtime does not validate models; the [control mapping](../trust/control-mapping.md) already says so. What it can do is make the evidence for inventory, change control and monitoring cheaper to produce and hard to alter.

### 9.1 Version pinned in signed policy

The decision policy lists each decider as `{id, kind, endpoint, model: "jev-1.13.0", require_echo, calibration: {report_digest, date, cases_digest}}`. `jev-latest` and any other alias the policy loader recognises as moving (`*-latest`, an empty model) are refused in audited mode and warned in basic mode. The policy is a signed document adopted like the stop policy: a loaded trust root, then later versions approved by a quorum of the previous policy's approvers (the `RotatePolicy` pattern), kept in a reserved control run (`@decide`) so each decision record's policy digest resolves to a policy and who approved it. Reusing the stop control's chain machinery rather than inventing another is open question 2.

### 9.2 A version change needs a calibration check

A policy update that changes a decider's `model`, `endpoint`, thresholds, option order or question set is a **change** and must carry a `calibration.report_digest` for the new configuration. The loader refuses a changing update whose report is missing, is for a different model or question set, or records a failure against the policy's acceptance criteria. The report comes from:

```
bide decide calibrate -policy policy.json -decider jev-prod -cases cases.jsonl [-repeats 5] [-permutations 4] -out report.json
```

`cases.jsonl` is the customer's labelled cases: a state, and per question the expected escalation outcome (must-escalate, must-not-escalate, either). The tool runs each case several times and with permutations of option order, because order moved probabilities in one published test (0.84 to 0.96) and because repeat runs may differ, and reports per rule:

- the **miss rate**: must-escalate cases that did not escalate, with a Wilson interval (the number a risk owner needs first);
- the **escalation rate** on must-not-escalate cases, which is the human workload;
- reliability of the probabilities (a calibration curve and its error), and the threshold the cases support;
- repeat variance and permutation sensitivity, and the worst case across permutations;
- the digest of the cases and of the policy it ran under.

The report is a signed-able JSON document, and the policy's acceptance criteria (a maximum miss rate with a minimum number of must-escalate cases, a maximum escalation rate) are checked by the tool and again by the loader. Limits, stated in the guide: the check is as good as the cases; a small set proves little (the interval says how little); and it does not tell whether the model is sound for the use, which remains the institution's validation. The tool never sends case data anywhere the policy's endpoint is not.

### 9.3 Monitoring

Each escalation a decision caused ends with a human outcome that the journal already records (approved as asked, approved with changes where applicable, denied, expired). `bide decide report` reads the journal and reports, per decider, model version and rule, over a window:

- **overturn rate**: the share of escalations the human approved unchanged (false alarms), with its interval; a high rate means alert fatigue, a sudden drop or rise means drift or a changed input mix;
- **sampled review**: the policy may set `sample_rate` so a random share of **non-escalated** decisions is routed to a person anyway (recorded as `decision-review`), which is the only way to estimate the miss rate in production;
- latency, error and timeout rates, and the fail-closed rate.

The policy may set bounds. A bound breach raises a notification and, if the policy says so, switches the set to "escalate all" (every rule fires) until a signed policy update resets it, which is the same fail-closed state as an outage. Whether the breach may trip a Stop is open question 6.

### 9.4 Control mapping

Adds a feature row "Decision layer" to the table at the top of `docs/trust/control-mapping.md`, and rows to the SR 11-7 table (all marked "planned" until merged, since that page counts only what is on main):

| SR 11-7 control | The layer provides | The institution provides |
|---|---|---|
| Model inventory and documentation | every decision record names decider kind, pinned version, echoed version, endpoint and policy digest; the policy lists the deciders | the inventory entry, purpose, limits, owner |
| Change control | a version change is a signed policy update that must cite a passing calibration report | the approval authority, the labelled cases, the acceptance criteria |
| Ongoing monitoring | overturn rate, sampled review, error and latency rates, bounds that fail closed | thresholds, cadence, acting on a breach |
| Outcomes analysis | every probability journaled and verifiable offline; input kept until redaction | back-testing design and analysis |
| Governance, controls over use | escalate-only: a decider cannot add authority; fail closed | the model risk policy |
| Effective challenge, validation | nothing (unchanged) | independent validation |

It also adds rows to the SOC 2 and other tables only where an existing row's wording changes; the docs sweep decides.

## 10. Compatibility check plan

The shared-schema claim comes from blogs, so the **first implementation step is a conformance suite, run before the adapter is written**, against Kev and at least one other server, recording field differences. The adapter carries per-server **shims** (a small struct of normalisations selected by a `profile` in the policy: `typesafe`, `kev`, and so on) if they diverge.

Step 0, before any code: confirm what actually runs. Kev's documented backends are CUDA, ROCm and Apple MLX; a hosted CI runner has no GPU, so it is not yet known that a Kev server runs there at all. Von is documented as running on CPU and serving the same API, and jevos is reported to run on CPU; neither is verified here. Step 0 finds a backend that runs in CI on a hosted Linux runner, with a pinned weights revision (a Hugging Face commit hash, a size budget for the CI cache, and the weights' license checked). If none does, CI runs the conformance suite against the in-repo fake and a recorded transcript of real responses, and the live suites run on the maintainer's laptop and a self-hosted runner; the design does not depend on which.

The suite (`decide/decidertest`, exported so a third party can run it against its server) checks:

- the request: field names, `criteria` as object (choice) versus list (score), `noul` with none, state as a string, an object and an array, the 255-option and 10-level limits, unicode, and the largest state the server accepts;
- the response: `model` format (`jev-1.13.0` versus `typesafe-jev-1.13.0`), echo of the requested id, `answers` shape per kind, probabilities that sum to 1 and their tolerance, what `confidence` and `score` mean, extra or missing fields;
- errors: HTTP status and body shape for a bad request, a bad key, an over-limit state, an unknown model, a server overload; whether a timeout leaves the server in a usable state;
- behaviour: repeat determinism on one server, sensitivity to option order, concurrent requests (Kev is documented as handling one at a time), a state containing an instruction aimed at the decider (recorded, not asserted);
- authentication: bearer header, no key.

Each difference found becomes either a shim, a documented unsupported feature, or an upstream issue. The output is a table in the guide: server, version tested, what differs. A server is "supported" only for the versions in that table.

CI runs against a local server with a pinned version, so no paid key is needed. An optional live check against the hosted Jev API runs only when a key is present; the key is read from environment variables loaded from `~/.config/bide-test/` by the developer's shell, is never printed, never placed in argv, a URL or an error message, and never committed. The test skips when it is absent. The adapter redacts the `Authorization` header from every error and log, with a test that plants a canary key and searches all outputs for it.

## 11. Data handling and bank-grade requirements

- **Projection.** The decision step takes a `State func(call) json.RawMessage` the developer writes, so what leaves the process is the minimum; the default is an error, not "the whole run". The record says whether the endpoint is hosted.
- **Keys.** The API key comes from the process environment or a secret store handle in the policy, never from argv, the journal, an error or a log. A mutual-TLS client certificate option is part of PR 1, since banks put such calls through an internal gateway.
- **Endpoint allowlist** in signed policy, no cross-host redirects, a pinned CA option.
- **No data retention is a vendor setting** (enterprise zero retention on request, per the wavect review); bide records the policy's statement of it and cannot verify it.
- **Separation of duties.** The people who change a decision policy are the quorum of the previous policy's approvers, not the developers who ship the process; a model change and a threshold change are the same kind of change.
- **Audit.** Policy adoption goes to the signed trail like stop policy changes (follow-up, as for the kill switch).

## 12. Formal model

A new small model (the next number after Model 16) in `spec/tla/decide/`, checked in PR 1, with a `// protocol:` region around the engine hook so `modelsync` ties them. State: a call, a base gate, a decision record with answers, the derived rung, the gate's satisfaction, and a crash. Invariants: the call runs only when the gate of the maximum of the base and recorded-derived rung is satisfied; no escalation exists before its record is durable; a replayed decision does not call the decider; a failed decision derives the top rung; the effective rung never decreases under any step. A trace-validation test replays the engine's journals against it. The claim stays scoped: the model checks the rules within its bounds; it says nothing about decider accuracy.

## 13. Alternatives considered

| Alternative | Why not (or not alone) |
|---|---|
| **Do nothing** | The semantic gap in section 1 stays. Rules cannot read intent, and the agent's own model is the steered component. Doing nothing is the baseline the escalate-only rule guarantees we never fall below, which is also why the decision layer is optional and off by default |
| **LLM as judge** (the `eval.Judge` pattern at runtime) | Free-text output must be parsed; the judge is steerable by the same text with no probability to threshold or calibrate; seconds not 100 ms; a failure mode is a parse error. It fits offline evaluation, where bide already uses it, not a per-call control |
| **OPA (or Cedar) only** | Deterministic and the right tool for the authority matrix, and it is in this design (`decide/opa`). It cannot judge unstructured text such as a memo or a message |
| **A vendor SDK for Jev** | A lock-in the user chose against; the protocol covers the hosted and the local servers. The `typesafe-sdk` is not needed for a JSON POST |
| **A second gate counted with the base gate** (conjunction) | Same safety, more flexibility (independent gates), but a second tally namespace per call in the approval code, which is the riskiest and most reviewed code. The ladder reuses the existing single-gate counting and the weaken comparison. Revisit if a real case needs two independent gates |
| **A decider that can deny** | Stricter than a gate, but a denial by a model has no accountable person behind it, and a manipulated answer would deny legitimate payments with no record of a human reason. A person can deny after escalation. Open question 5 |
| **Auto-approve on a high-confidence "safe"** | The exact thing the rule forbids. Confidence is not authorization |
| **A trained in-house classifier** | Out of scope; it would use the same `Decider` port |

## 14. Decisions (for approval)

1. **Port in the core, adapters outside it**: `agent.Decider` in `agent`, `decide/systemone` and `decide/decidertest` in the root module, `decide/opa` in a new nested module. Recommend yes.
2. **Policy mechanism**: a signed decision policy with a quorum-approved chain in a reserved `@decide` control run, reusing the stop control's chain code rather than the plan-flow governed policy digest. Recommend yes; the chain is the proven pattern, and a model change then has an auditable approver set.
3. **Gate composition**: the ladder (a rung is a gate in place of the lower gate, each not weaker than the one below) rather than a second gate in conjunction. Recommend the ladder for v1.
4. **Journal shape**: protected `decision:*` outcome record plus a redactable `decision-input:*` with the salt, and `ReadOnly` step safety. Recommend yes.
5. **No denial by a decider in v1**: rungs are gate, route and stop; a person denies. Recommend yes.
6. **Stop rungs**: allowed per rule but off by default (`allow_stop` in policy), because a steerable decider could be used to cause outages; the aggregate monitor (overturn or error bounds) may trip a Stop. Recommend this.
7. **Monotone conditions only**: the loader refuses rules that escalate on low-risk answers. Recommend yes.
8. **Fail closed means every rule of the set fires**, with a 2 s default timeout and one retry. Recommend yes; the timeout is a policy field.
9. **Pins**: aliases refused in audited mode, `require_echo` default true there. Recommend yes.
10. **Sampled review** (`sample_rate`) to estimate the miss rate. Recommend yes, default off, documented as the only way to measure it.
11. **Cedar**: defer. OPA covers the matrix; Cedar can use the same port later. Recommend defer; confirm the Go implementation's maturity then.
12. **OPA embedded** (the Go library, bundle digest pinned in the record as the "model version") rather than a sidecar. Recommend embedded; revisit if the dependency cost bites.
13. **CI backend**: step 0 picks a CPU-capable pinned server if Kev will not run on a hosted runner. Recommend that, and Kev's conformance run on the maintainer's machine until a self-hosted runner exists. This departs from "CI against a local Kev" only if Kev cannot run there, which is not yet known.
14. **Multiple deciders** joined by `max` in v1. Recommend yes (it is free in the lattice).
15. **Release**: v0.22.0 after the v0.21.0 pilot, labelled experimental until a bank pilot uses it. Recommend yes.

## 15. PR plan

Small PRs, one at a time, each with tests first, mutation checks, docs, a changelog fragment and a break-it review before merge, because each touches approvals and the journal. This document is PR 0.

0. **This document** (docs only; fast path).
1. **Interface, journaled decisions, systemone adapter with a fake.** Step 0 of section 10 first (the conformance suite and its findings, committed with the shim table), then: `agent` port and validation, `Escalation`, `Rung`, `Resolve`, the decision policy loader and its chain, the decision step and records, the engine hook in every gate path, fail-closed handling, redaction protection, `decide/systemone` with profiles, `decidertest` (fake and suite), the architecture test case, the formal model and its trace validation. Tests: crash sweep with call counting, no escalation before the record, forged-rung ignored, monotonicity property test over random ladders and answers, canary key never appears, endpoint allowlist, mutation checks of each hook position. Break-it review: can any sequence of answers lower a rung or skip a gate; can a crash between the call and the record let a different answer act; can a path to a tool skip the hook; can the endpoint be redirected.
2. **OPA adapter and the local CI backend.** `decide/opa` nested module (its answers are probability 0 or 1 through the same port, so escalate-only applies to OPA identically), the bundle digest as identity, the CI job against the pinned local server, the optional live Jev check, docs. Break-it review: can an OPA result lift a gate.
3. **Calibration and monitoring.** `bide decide calibrate` and `report`, the loader's calibration check, sampled review, the monitoring bounds, notifier kinds, the control-mapping rows, the guide, a recipe and an example, README and the four i18n READMEs, GUARANTEE, KNOWN-LIMITATIONS, CONCEPTS, getting-started, godoc, CLI reference regenerated, docs-site sidebar. This is a third PR beyond the "about two" in the brief, because the calibration tool does not touch approvals and the PRs stay small; fold it into 2 if preferred.

Docs for each PR follow the standing rule (every PR updates every doc it affects); PR 1 carries the guide's first version so the guarantee is never described without its limits.

## 16. Claims and limits to state in the docs

- The guarantee is about what bide does with a decider's output: no output makes a call less gated. It is not a claim that a decider detects fraud or injection.
- A fooled decider offers no protection; the base gates are never replaced.
- bide cannot see inside a hosted model: a silent change under an unchanged id is not detectable by bide.
- State sent to a hosted decider is disclosed to the provider; redaction does not reach it.
- Option order and repeated runs change probabilities; calibrate with permutations and repeats.
- A Stop is a denial-of-service lever for anyone who can steer a decider; it is off by default.
- Not a compliance certification; the institution validates the model.

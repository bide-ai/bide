# Deep teardown: agenticenv/agent-sdk-go (our closest competitor)

Code-level examination at pinned commits (`durable-go @ 424c9b5`, `agent-sdk-go @ df0d172` /
v0.3.7, Sept 2026). This is the ONE framework sharing our exact thesis — embedded durable
execution, no cluster, plain Go, provider-neutral — so it's the rival that most tests our moat.
Verdict: **the "durable-as-library" *headline* is contested; safe/correct/lean durability is
genuinely ours, verified in their source.**

## Where we WIN (verified in their code; several already shipped by us)

1. **Side-effect safety — they have NONE (the moat).** `durable-go` blindly re-runs unjournaled
   steps: a STARTED record is best-effort (`step.go:203`) and *excluded from the replay cache*
   (`journal.go:626`), side effect fires, then result is journaled — crash between → tool re-runs.
   Their doc: *"Side effects on re-run are the caller's problem."* No idempotency key, no
   read/write distinction, no halt. **We ship `Safety{ReadOnly/Idempotent}` + `ResumeHalt`.**
2. **Content-stable step identity.** They key tool steps positionally (`tool-exec-<iter>-<idx>`)
   and *ignore input args* for cache identity, so LLM-driven divergence on resume returns the
   **wrong tool's cached result** (`step.go:77`). **We key by the provider `tool_call` ID
   (content-derived, stable).** Already better by construction.
3. **Typed content parts + reasoning signatures.** They use flat `Content string`; **extended-
   thinking signatures are never preserved** (`grep signature` → nothing; Anthropic converter
   drops thinking blocks, `anthropic/client.go:265`), which *breaks Claude thinking + tools*.
   **We ship `[]Part` with `Reasoning.Signature` round-tripped (verified).**
4. **Provider-aware schema.** They hand-build schema and ship **one generic schema to every
   provider** — no OpenAI strict mode, no Gemini dialect (400 risk), no auto-reflection. **We
   ship `schema.For` (inline reflection, no `$ref`) + `OpenAIStrict`.**
5. **Raw-JSON tool args.** Their `ToolCall.Args` is `map[string]any` (lossy). **We keep
   `json.RawMessage`.**
6. **Lean dependencies — their single biggest weakness.** Importing their core `pkg/agent`
   transitively compiles **Temporal + Restate + gRPC + protobuf + Weaviate + pgx + Redis + OTel**
   — `go list -deps` shows **84 temporal/restate/weaviate packages for a hello-world**, ~380
   total, 197 go.sum module entries. No build tags. **Our entire dep tree is `modernc.org/sqlite`
   (pure Go) + its handful of transitives.** Demonstrable win on binary size, build time,
   supply-chain surface.
7. **One agent loop, not three.** They maintain **triple-duplicated loops** — local (1467 LOC),
   restate (1378), temporal (2136) — ~5k LOC that drifts (proven: two different max-iteration
   defaults, 5 vs 10). **We have one loop over the pluggable `Durable` interface.**
8. **Shared-storage HA (planned).** They enforce single-writer `flock`, one process per dataDir;
   a dead node's runs can't resume elsewhere (`engine.go:268`). Our pluggable substrate
   (Postgres/DBOS backend) beats this on failover.

## Where THEY win (shipped; we haven't yet — catch-up, not moat)

- **Observability:** real, well-wired OTel across llm/tool/memory/retriever spans, 30+ metrics,
  no-op defaults. CLOSED: `trace.Model` (`.Use`) + `trace.Tool` (`.UseTool`) emit gen_ai.* spans,
  opt-in (core has no OTel dep). And our tool span NESTS across the sub-agent boundary — their
  documented gap — because sub-agents are just tools sharing the ctx. (We don't yet have their
  breadth of metrics; spans + usage are there.)
- **Middleware:** true *mutating, short-circuiting* hooks at 8 lifecycle points. Genuinely good.
  CLOSED: we now have two mutating/short-circuiting `func(Handler) Handler` chains at the two
  boundaries that matter — model (`Use`) and tool (`UseTool`) — plus the `Agent.Stream` event
  feed for run/turn observation. One idiom, not 8 bespoke slots; tool middleware runs inside the
  durable step so short-circuits are journaled. (Their gap remains: no span across sub-agents.)
- **HITL:** comprehensive tool/MCP/sub-agent/budget approvals with tokens + policies. CLOSED:
  declarative approve/deny (`RequiresApproval` → `*PendingApproval` → `Approve`) AND imperative
  typed `Interrupt[T]`/`Resume` — both DURABLE (the decision/value is a journaled step, survives a
  crash), on our at-most-once substrate (theirs relies on idempotency). Plus `WithMaxTurns` safety.
- **Provider breadth:** 5 providers (OpenAI/Anthropic/Gemini/DeepSeek/Ollama) via official SDKs.
  PARTLY CLOSED: Anthropic (native) + one OpenAI-compatible adapter that via `WithBaseURL` runs on
  OpenAI/Groq/DeepSeek/Ollama/Mistral/Together/vLLM/... — "1 adapter, N providers." Still missing
  native Gemini/Bedrock. (Prompt caching + sampling params shipped since.)

Most are now CLOSED (observability, middleware, HITL). Remaining catch-up: native Gemini/Bedrock and
multimodal. Still *features*, not *moats* — we add them; they can't add side-effect safety (an
engine-level redesign, now DST-proven) or shed 380 deps. And we've since built moats they can't
match at all: adversarial crash-safety DST + an RFC 6962 tamper-evident audit trail.

## Their other footguns (ammunition)

Interior journal corruption fails the whole run closed; `Close` hangs forever on a non-ctx-aware
step (never releases the flock); approval-timeout leaves a non-terminal state patched call-site by
call-site; sub-agent routes aren't durable across restart; **tests never run with `-race`** despite
default parallel tool execution + concurrent event emit; silent `json.Unmarshal` discards (malformed
args → `{}` → Execute runs anyway) in 4+ places; Gemini stream hand-rolls a byte-offset delta →
invalid UTF-8 on split runes; eventbus blocking-send backpressure under a slow subscriber;
approval-required-by-default surprises new users; requires Go 1.26; 56 stars, single-maintainer
(vinodvx ~95% of commits), pre-1.0 churn, last dependabot CI failed.

## Positioning line

*"Durable agents as a library isn't new — doing it **safely** is. We won't double-charge on a
crash (side-effect-safe resume), we don't drag Temporal + Weaviate into your hello-world (one pure-
Go dep), and we preserve Claude's reasoning across turns. One agent loop, not three."*

Sources: cloned `durable-go/{engine,step,journal,task}.go`, `agent-sdk-go/{pkg/agent/config.go,
pkg/llm/{anthropic,openai,gemini}/client.go,internal/runtime/local/agent_loop.go,
internal/eventbus/inmem.go}`; agent-sdk-go issues #92 (guardrails requested), #93 (router
requested), #94/PR#100 (durability). Examined 2026-09-23.

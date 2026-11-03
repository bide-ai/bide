// All homepage content lives here. Edit this file to change benchmark rows, add screenshots,
// reword a card or add a recipe; the components only lay it out.
//
// Text fields marked "html" are rendered with v-html. Keep markup to <b> and
// <span class="mono">, and write > as &gt;. Every sentence here was checked against the code, so
// do not strengthen a claim without checking it again.

export interface Img {
  /** File name under /home/ (copied by sync-docs.mjs). */
  src: string
  alt: string
  width: number
  height: number
}

// ---- Hero -------------------------------------------------------------------------------

export const hero = {
  title: 'Durable, accountable AI agents in Go.',
  lede: 'A Go framework for AI agents that move money: risky calls approved, every action recorded and verifiable.',
  installHtml:
    '<span class="p">$</span> brew install bide-ai/tap/bide\n<span class="c"># crash an agent mid-payment, no API key</span>\n<span class="p">$</span> bide demo',
  note: 'No Go install needed for the demo with Homebrew. Or:',
  noteCmd: 'go run github.com/bide-ai/bide/cmd/bide@latest demo',
  sampleLinkText: 'Explore a sample run in your browser →',
  sampleLinkHref: '/sample/',
  figure: {
    src: 'hero.gif',
    alt: 'bide demo: an agent is killed mid-charge and the restart halts with outcome unknown instead of charging again, and ops is notified; then a refund requested by untrusted inbound email is held with the reason; the requester is refused by separation of duties; two approvers sign, the refund runs once, and the audit verifies',
    width: 1280,
    height: 720,
  } as Img,
  caption:
    'The demo kills an agent after the charge and before the journal write, halts instead of charging again, then edits one amount in a copy of the journal to show verification fail.',
}

export const audiences = [
  {
    title: 'Move money',
    text: 'Refunds, payouts, purchases, credits. A side effect that must not happen twice, and that someone may need to approve first.',
  },
  {
    title: 'Change records',
    text: 'Customer records, invoices, tickets, entitlements. A write that must not run twice, even if the process dies mid-write.',
  },
  {
    title: 'Act under audit',
    text: 'Work a reviewer will ask about later: who approved it, before it ran, and was anyone told. Answered from evidence, offline.',
  },
]

// ---- Problem and benchmark --------------------------------------------------------------

export const problem = {
  eyebrow: 'The problem',
  title: 'Your agent crashed mid-refund. Did the customer get charged twice?',
  p1: 'Most agent frameworks recover by re-running. That is fine for a model call. For a refund, an email or a wire, it is a second refund, a second email, a second wire.',
  p2: 'We ran a non-idempotent charge through 204 to 209 crash schedules per framework, then recovered the way each one supports.',
  linkText: 'Reproduce the benchmark →',
  linkHref: 'https://github.com/bide-ai/bide/tree/main/benchmarks',
  note: 'Every adapter passes a fairness test: a run with no crash charges exactly once, and where a framework has resume, it works. LangGraph runs in its most durable documented mode. These frameworks document that steps must be idempotent; this measures that documented model. Worst-case counts track how often a framework saves state, so they are not a ranking: anything above 1 is a double charge.',
}

export interface BenchRow {
  name: string
  meta: string
  verdict: string
  worst: string
  /** The highlighted (bide) row. */
  us?: boolean
}

// Measured 2026-11-02; the source is assets/demo/chaos-card/results.json (see benchmarks/README.md).
export const benchmark: BenchRow[] = [
  { name: 'bide', meta: 'Go · main at 72ffb78 · 209 schedules', verdict: 'Never repeated', worst: 'at most once held', us: true },
  { name: 'LangGraph', meta: 'Python · 1.2.19 · SQLite and Postgres', verdict: 'Charged again', worst: 'worst case: 3 charges' },
  { name: 'adk-go', meta: 'Go · v2.7.0', verdict: 'Charged again', worst: 'worst case: 4 charges' },
  { name: 'trpc-agent-go', meta: 'Go · v1.11.2', verdict: 'Charged again', worst: 'worst case: 6 charges' },
  { name: 'langchaingo', meta: 'Go · v0.1.15 · no crash durability', verdict: 'Charged again', worst: 'worst case: 64 charges' },
  { name: 'eino', meta: 'Go · v0.9.22 · no crash durability', verdict: 'Charged again', worst: 'worst case: 64 charges' },
]

// ---- Resolution ladder ------------------------------------------------------------------

export const ladder = {
  eyebrow: 'What bide does instead',
  title: 'When the outcome is unknown, it stops.',
  lede: 'bide writes a durable attempt marker before every side effect, so a resumed run can tell "never started" from "started, outcome unknown". The unknown case is resolved in a fixed order.',
  quote: "A pause you can clear beats a double-fire you can't.",
  steps: [
    {
      title: 'Retry-safe',
      html: 'Declared read-only or idempotent, or sent with an idempotency key. Retried automatically; the provider dedupes.',
      result: '→ auto',
    },
    {
      title: 'Queryable record',
      html: 'A sent-message id, a row, a provider record. A reconciler resolves it with <span class="mono">ResolveHalt</span> after a grace period, with evidence.',
      result: '→ auto, reconciler',
    },
    {
      title: 'Unknowable',
      html: 'Nothing can establish what happened. The run halts and a person is notified.',
      result: '→ HALT, wait for a human',
      hot: true,
    },
  ],
}

// ---- API and code tabs ------------------------------------------------------------------

export const api = {
  eyebrow: 'The API',
  title: 'Gate the risky call. The rest is plain Go.',
  lede: 'Write the agent in ordinary Go. Mark the calls that move money, and bide holds them for the people your policy names.',
  checks: [
    '<b>k of n named approvers</b>, each signing the exact tool and arguments',
    '<b>Separation of duties:</b> with <span class="mono">ExcludePrincipal</span>, the requester\'s decision never counts',
    '<b>Deadlines:</b> set a <span class="mono">Timeout</span>, and a gate nobody decides in time expires; the call never runs',
    '<b>Signing keys can live in an HSM or a cloud KMS</b>, behind one interface',
    '<b>Or author it as a flow</b> in YAML, with the policy checked in CI',
  ],
}

// Highlighted with the classes c (comment), s (string), k (keyword/number), n (function), p (prompt).
export const codeTabs = [
  {
    id: 'code-go',
    label: 'main.go',
    html: `<span class="c">// A refund that needs 2 of 3 named approvers before it runs.</span>
refund := agent.<span class="n">MustFunc</span>(<span class="s">"refund"</span>, <span class="s">"Refund an order"</span>, doRefund,
	agent.<span class="n">WithApproval</span>(&amp;agent.<span class="n">ApprovalPolicy</span>{
		Need:             <span class="k">2</span>,
		Approvers:        []<span class="k">string</span>{<span class="s">"ops"</span>, <span class="s">"finance"</span>, <span class="s">"risk"</span>},
		ExcludePrincipal: <span class="k">true</span>,           <span class="c">// the requester never counts</span>
		Timeout:          <span class="k">24</span> * time.Hour, <span class="c">// undecided in time: never runs</span>
	}))

store, _ := sqlite.<span class="n">Open</span>(<span class="s">"run.db"</span>) <span class="c">// pure Go, no cgo; Postgres for many nodes</span>
journal, _ := agent.<span class="n">NewJournal</span>(store)

a, _ := agent.<span class="n">New</span>(model, journal,
	agent.<span class="n">WithTools</span>(refund),
	agent.<span class="n">WithApproverVerifiers</span>(lookupApprover), <span class="c">// id -&gt; key, from your PKI</span>
)

<span class="c">// Crash anywhere. Run the same ID again: it resumes from</span>
<span class="c">// the journal, and the refund fires at most once.</span>
res, err := a.<span class="n">Run</span>(ctx, <span class="s">"refund-1042"</span>, agent.<span class="n">UserText</span>(ticket),
	agent.<span class="n">WithIdentity</span>(agent.<span class="n">Identity</span>{OnBehalfOf: requester}))
<span class="k">if</span> pend, ok := errors.<span class="n">AsType</span>[*agent.<span class="n">ApprovalPending</span>](err); ok {
	ask(pend) <span class="c">// paused at the gate until 2 of 3 sign</span>
}`,
  },
  {
    id: 'code-yaml',
    label: 'flow.yaml',
    html: `<span class="k">version</span>: <span class="s">1</span>
<span class="k">flow</span>: refund
<span class="k">entry</span>: assess
<span class="k">nodes</span>:
  - {name: assess, block: assess, safety: readonly}
  - <span class="k">name</span>: refund
    <span class="k">block</span>: refund
    <span class="k">safety</span>: side_effect
    <span class="k">approval</span>: {need: <span class="s">2</span>, approvers: [ops, finance, risk], exclude_principal: <span class="s">true</span>}
  - {name: notify, block: notify, safety: readonly}
  - {name: refund_small, block: refund_small, safety: side_effect}
<span class="k">wiring</span>:
  - <span class="k">switch</span>: assess
    <span class="k">when</span>: [{pred: over_threshold, to: refund}]
    <span class="k">else</span>: refund_small
  - <span class="k">edge</span>: [refund, notify]
<span class="k">assert</span>:
  - <span class="s">every path to refund passes approval &gt;= 2 of 3 excluding the requester</span>

<span class="c"># bide check flow.yaml exits 1 if any path reaches the refund</span>
<span class="c"># without that gate. With -diff main -check -fail-on-weaken it fails</span>
<span class="c"># a pull request that lowers need or drops exclude_principal.</span>`,
  },
  {
    id: 'code-run',
    label: 'run it',
    html: `<span class="p">$</span> bide init refund &amp;&amp; cd refund &amp;&amp; go mod tidy &amp;&amp; go run .
<span class="k">paused before node:refund</span>: 0 of 2 approvals (pending ops, finance, risk)
ops approved {"order":"order-42","amount":120,"reason":"arrived damaged"}
<span class="k">paused before node:refund</span>: 1 of 2 approvals (pending finance, risk)
finance approved {"order":"order-42","amount":120,"reason":"arrived damaged"}
<span class="s">done: refunded: arrived damaged (120)</span>
refunds the provider holds: <span class="s">1</span>
wrote evidence.json, approver-keys.json and keys/log.pub

<span class="p">$</span> go run .   <span class="c"># again: reads the finished run back, refunds nothing</span>`,
  },
]

// ---- Full framework ---------------------------------------------------------------------

export const framework = {
  eyebrow: 'A full agent framework',
  title: 'Everything you build agents with, on one journal.',
  lede: 'Models, tools, flows, sub-agents and long-running ambient work all run on the same durable journal, so each keeps the guarantees.',
  cards: [
    {
      title: 'Models',
      html: 'Native Anthropic and Gemini, and any OpenAI-compatible endpoint: OpenAI, Ollama, vLLM, DeepSeek, Groq, OpenRouter, Azure, xAI. Extended thinking with tools, prompt caching, images, one tool schema emitted per provider dialect.',
    },
    {
      title: 'Tools and MCP',
      html: 'Typed Go tools with schemas generated from the type. MCP servers as tool sources, with their annotations mapped to safety and approval gates.',
    },
    {
      title: 'Composition',
      html: 'Sub-agents that share the journal, durable <span class="mono">Parallel</span> fan-in, sagas that roll back in reverse and stop on an unknown outcome, and typed <span class="mono">plan</span> flows with switch, join and loop.',
    },
    {
      title: 'Ambient agents',
      html: 'Durable <span class="mono">Sleep</span> and <span class="mono">WaitUntil</span> that survive restarts, a pluggable <span class="mono">Waker</span>, signals applied once even when redelivered, and typed <span class="mono">Interrupt</span> for a human decision.',
    },
    {
      title: 'Reliability',
      html: 'Per-attempt timeouts, retry that classifies transient and terminal errors, hedged model calls, rate limits, token budgets across an agent tree, and spend read from the journal, discarded retries included.',
    },
    {
      title: 'Run and observe',
      html: 'Streaming, typed output, durable sessions, OTel GenAI spans in one line, a scripted test model for offline tests, and SQLite or Postgres with leases and continuous recovery.',
    },
  ],
  callouts: [
    '<b>Already on ADK, Eino, trpc-agent-go or langchaingo?</b> Wrap the side effects in <span class="mono">agent.Step</span> and they get the journal: at most once across a crash, and on the audit trail.',
    '<b>Nothing in your go.sum you did not ask for.</b> The core module requires no other module, and a test fails the build if that changes. SQLite, Postgres, OTel and the cloud SDKs are separate modules.',
  ],
}

// ---- Five guarantees --------------------------------------------------------------------

export const guarantees = {
  eyebrow: 'One journal, five guarantees',
  title: 'The loop is ordinary Go. The guarantees come from the journal under it.',
  cards: [
    {
      n: '01',
      title: 'At most once',
      text: 'A side effect fires at most once, even across a crash, so it does not have to be idempotent.',
      bullets: [
        'Calling a finished run again returns the stored answer, with no new model call',
        'A resume reads its limits, prompt and tool filter from the journal; a redeploy that changes them is refused',
      ],
      link: 'The guarantee →',
      href: '/GUARANTEE',
    },
    {
      n: '02',
      title: 'A library, not a cluster',
      text: 'Durable execution from a store you already run: SQLite on one machine, Postgres for many. No server, no worker fleet.',
      bullets: [
        'With Postgres, any node resumes any run; a per-run lease keeps one driver at a time',
        '20,000 runs, 5,000 in flight, in about a second on a 4-vCPU CI runner (in-memory store, simulated model latency)',
      ],
      link: 'Durable steps →',
      href: '/guides/durable-steps',
    },
    {
      n: '03',
      title: 'A verifiable audit trail',
      text: 'The journal is committed to an RFC 6962 Merkle tree. A third party checks proofs offline without trusting your database or logs, and against the operator once heads are anchored outside their control.',
      bullets: ['Inclusion, consistency, and proofs that an action never happened', 'Anchoring to S3 Object Lock or a locked Google Cloud Storage bucket'],
      link: 'Audit →',
      href: '/guides/audit',
    },
    {
      n: '04',
      title: 'People in control',
      text: 'k-of-n signed approvals with separation of duties and deadlines, and a notifier that records each delivery and whether your endpoint acknowledged it.',
      bullets: [
        'Approvers bound to certificates from your own CA; a revoked one does not count',
        'Signing keys in PKCS#11, AWS KMS, Google Cloud KMS or Azure Key Vault',
      ],
      link: 'Human approval →',
      href: '/guides/hitl-approval',
    },
    {
      n: '05',
      title: 'Provably convergent state',
      experimental: true,
      text: 'Shared state many agents change converges to one valid state in any event order, for state that meets conditions checked at build time.',
      bullets: ['The convergence theory is proved in Rocq, axiom-free', 'Each machine is re-checked by a checker extracted from that proof'],
      link: 'Scope and limits →',
      href: '/guides/governance',
    },
  ],
}

// ---- Approvals --------------------------------------------------------------------------

export const approvals = {
  eyebrow: 'Approvals a reviewer can check',
  title: 'Who approved, before it ran, over exactly these arguments.',
  lede: 'Every decision is signed over the run, the call, the tool and its canonical arguments, then counted by the gate and kept on the audited trail.',
  checks: [
    '<b>Decide in a terminal or a browser.</b> <span class="mono">bide approve</span> shows the call and the SHA-256 the signature binds. The browser page signs with a key it cannot export.',
    '<b>Certificate-bound approvers.</b> A seat can be a person your CA certified; revocation is checked just before the call runs.',
    '<b>Decision receipts.</b> Each approver keeps one, and a later check notices a decision deleted after a signed head committed to it.',
    "<b>Earned authority.</b> An agent's limit rises one rung per clean streak, up to a ceiling the root signed, and drops on any anomaly.",
    '<b>Escalation.</b> An approval nobody acknowledges escalates, up to four steps, until someone decides.',
  ],
  note: "bide compares ids; tying a key to a person is your identity provider's job. Signers are tested against SoftHSM2 and emulators, not a live cloud.",
  image: {
    src: 'approve-page-waiting-dark.png',
    alt: "The browser approve page for a waiting refund call: the approver's public key and fingerprint, every argument, the arguments' SHA-256, and Approve and Reject buttons",
    width: 1720,
    height: 2668,
  } as Img,
}

// ---- Untrusted input --------------------------------------------------------------------

export const untrusted = {
  eyebrow: 'Bound what injected text can do',
  title: 'An email tells your agent to refund $4,800. Now what?',
  lede: 'These controls bound the consequences of instructions hidden in content the agent reads. They do not detect or prevent the injection itself.',
  cards: [
    {
      title: 'Taint policy',
      href: '/guides/untrusted-content',
      html: 'After a run reads content you marked untrusted, every later side effect needs an escalation gate, such as 2 of 3 with the requester excluded. The approver sees why the call was held. Works in agents and in plan flows.',
    },
    {
      title: 'Amount caps',
      href: '/guides/amount-caps',
      html: 'A rule in your Go code refuses a refund over a limit, per call or per run, hour or day, whatever the model sent. <span class="mono">bide audit caps</span> checks the ledger.',
    },
    {
      title: 'Kill switch',
      href: '/guides/kill-switch',
      html: '<span class="mono">bide stop</span> pauses every agent, or one tool, flow or agent, before its next side effect; runs resume when it is lifted. Audited mode signs stops and needs a quorum to lift.',
    },
  ],
  // Screenshots from docs/guides (assets/demo/shots-safety.sh). `position` picks the part of a tall
  // image the frame shows.
  shots: [
    {
      id: 'u-taint',
      label: 'taint hold',
      caption:
        'The agent read an inbound email, then asked for a refund. The call waits for 2 of 3 approvers, and the gate says why.',
      image: {
        src: 'run-viewer-taint-hold-dark.png',
        alt: 'bide view run page for refund-4801, waiting for approval: the approval gate card is open on why this gate, untrusted content from inbound-email read by tool read_email, with the three pending approvers',
        width: 1200,
        height: 1000,
        position: 'center 45%',
      },
    },
    {
      id: 'u-cap',
      label: 'amount cap',
      caption:
        'A $4,800 refund over the big_refund rule (500 USD) is refused before the tool runs; the model reads an error that names the rule.',
      image: {
        src: 'run-viewer-cap-refused-dark.png',
        alt: 'bide view run page for refund-4813: the model calls refund for 480000 USD, a gate-reason record names rule big_refund with action refuse and limit 50000, and the tool result is an error saying the call exceeds a configured amount limit',
        width: 1200,
        height: 1500,
        position: 'bottom center',
      },
    },
    {
      id: 'u-stop',
      label: 'kill switch',
      caption:
        'A stop on the refund tool, in audited mode: the run is held before its next side effect, and the banner shows who set it, why, and the lifts still needed.',
      image: {
        src: 'run-viewer-stop-banner-dark.png',
        alt: 'bide view with a tool stop in force: a red STOPPED banner naming tool refund, the reason provider incident, who set it and 0 of 1 lifts held; run refund-4801 carries a held by stop pill',
        width: 1200,
        height: 1000,
        position: 'top center',
      },
    },
  ] as { id: string; label: string; caption: string; image: Img & { position: string } }[],
}

// ---- Tools ------------------------------------------------------------------------------

export interface ToolTab {
  id: string
  label: string
  title: string
  experimental?: boolean
  html: string
  cmd: string
  linkText: string
  href: string
  /** 'frame' crops a tall screenshot into a fixed-height box; 'shot' shows it whole. */
  images: (Img & { mode: 'frame' | 'shot' })[]
}

export const tools = {
  eyebrow: 'The tools',
  title: 'See it, sign it, prove it.',
  ledeHtml:
    'One <span class="mono" style="color:#ededed">bide</span> binary: a local, read-only page for runs and flows that makes no outside request, a terminal for approvers, a flow editor, and 17 offline checks an auditor can run.',
  tabs: [
    {
      id: 't-view',
      label: 'bide view',
      title: "See a flow's risk before it ships",
      html: 'Each node is colored by what a crash does to it: a side effect halts and asks, a read-only step re-runs. Approval gates are marked, and policy assertions are checked on the graph.',
      cmd: 'bide view -config flow.yaml',
      linkText: 'Read the guide →',
      href: '/guides/run-viewer',
      images: [
        {
          mode: 'frame',
          src: 'run-viewer-config-risk-dark.png',
          alt: "bide view -config: a refund flow's risk map, each node colored by what a crash does to it, with all four policy assertions holding",
          width: 1200,
          height: 1450,
        },
      ],
    },
    {
      id: 't-check',
      label: 'bide check',
      title: 'Policy as a test',
      html: 'An assertion such as "every path to charge passes approval &gt;= 2 of 3" is decided on the graph, without running anything. A violation comes with its shortest counterexample path, in red.',
      cmd: 'bide check flow.yaml',
      linkText: 'Read the guide →',
      href: '/guides/run-viewer',
      images: [
        {
          mode: 'frame',
          src: 'run-viewer-config-assert-dark.png',
          alt: 'bide view -config: a violated policy assertion with its shortest counterexample path drawn in red',
          width: 1200,
          height: 1450,
        },
      ],
    },
    {
      id: 't-diff',
      label: 'policy diff in CI',
      title: 'A pull request that weakens a gate fails CI',
      html: 'Compare a flow with any git revision. A removed gate, a lower need, a dropped requester exclusion, a longer or removed deadline, a new ungated side effect or a loosened assertion is flagged, and the check exits 1.',
      cmd: 'bide check flow.yaml -diff main -check -fail-on-weaken',
      linkText: 'CLI reference →',
      href: '/reference/cli',
      images: [
        {
          mode: 'frame',
          src: 'flow-diff-weakened-dark.png',
          alt: 'bide view -diff: a flow compared with main, the removed approval gate drawn in dashed red with a weakened callout',
          width: 1200,
          height: 1250,
        },
      ],
    },
    {
      id: 't-approve',
      label: 'bide approve',
      title: 'Decide from a terminal',
      html: 'Approvers see the call read from the journal, its arguments and the SHA-256 their signature binds. A requester whose decision would not count is refused before anything is recorded, and every decision leaves a receipt.',
      cmd: 'bide approve -db run.db -list',
      linkText: 'Read the guide →',
      href: '/guides/hitl-approval',
      images: [
        {
          mode: 'shot',
          src: 'cli-approve-dark.gif',
          alt: 'bide approve in a terminal: a refund waiting at a 2-of-3 gate; the requester refused; finance shown the call, its arguments and the SHA-256 the signature binds, then signing',
          width: 1200,
          height: 820,
        },
      ],
    },
    {
      id: 't-audit',
      label: 'bide audit',
      title: 'Prove people were told',
      html: 'Checked offline against the journals and the signed trail: every halt and every waiting approval was delivered, and your endpoint acknowledged it in time. One that nobody was sent fails. An acknowledgement means your endpoint answered, not that a person read it.',
      cmd: 'bide audit notifications -db run.db -pubkey keys/log.pub -sth notify-run.sth.json',
      linkText: 'Read the guide →',
      href: '/guides/notifications',
      images: [
        {
          mode: 'shot',
          src: 'cli-audit-notifications-pass-dark.png',
          alt: 'bide audit notifications PASS: the approval_pending event was delivered and acknowledged, on a trail with a signed head',
          width: 1200,
          height: 330,
        },
        {
          mode: 'shot',
          src: 'cli-audit-notifications-fail-dark.png',
          alt: 'bide audit notifications FAIL never_notified: an approval was waiting and the trail has no record that anyone was told',
          width: 1200,
          height: 330,
        },
      ],
    },
    {
      id: 't-edit',
      label: 'bide edit',
      title: 'Build flows visually',
      experimental: true,
      html: 'A local visual editor for flow configs: the graph, an outline and an inspector. Add, rename and reorder nodes, wire edges, switches and joins by form or by dragging between nodes, and set each node\'s approval gate in a form, with the assertions\' verdicts live as you edit. An edit that weakens a gate against the committed policy raises a banner that cannot be closed, and every save shows a diff first.',
      cmd: 'bide edit flow.yaml',
      linkText: 'Read the guide →',
      href: '/guides/flow-editor',
      images: [
        {
          mode: 'frame',
          src: 'flow-edit-structure.png',
          alt: 'bide edit on a refund flow: the graph of four nodes colored by crash behavior with an edge being dragged from charge, the outline with an Add a node form, and the inspector setting the charge node\'s approval gate to 2 of 3 excluding the requester; the banner reads All assertions hold',
          width: 1280,
          height: 1150,
        },
      ],
    },
    {
      id: 't-attn',
      label: 'needs attention',
      title: 'Know what is waiting on a person',
      html: 'Every run waiting for a person, nearest approval deadline first: approvals pending, halts with an unknown outcome, and whether someone was notified and acknowledged. Try it with no code: <span class="mono">bide sample</span> opens 14 runs in every state. <a href="/sample/" target="_self">Or open the sample in your browser →</a>',
      cmd: 'bide view -db run.db',
      linkText: 'Read the guide →',
      href: '/guides/run-viewer',
      images: [
        {
          mode: 'frame',
          src: 'run-viewer-needs-attention-dark.png',
          alt: 'bide view Needs attention: approvals waiting, halts whose outcome is unknown, each with what to do and whether a person was notified',
          width: 2400,
          height: 3344,
        },
      ],
    },
  ] as ToolTab[],
}

// ---- Comparison -------------------------------------------------------------------------

export const compare = {
  eyebrow: 'Compared',
  title: 'Where bide differs, and where others are stronger.',
  columns: ['', 'bide', 'Temporal', 'LangGraph', 'Go agent SDKs'],
  rows: [
    [
      'Crash after the effect ran, before its result was saved',
      'Halts with outcome unknown instead of re-running',
      'The activity may run again; idempotent activities recommended',
      'A started task may run again; idempotency keys recommended',
      'Re-ran in our benchmark (worst case 4 to 64 charges)',
    ],
    [
      'Multiple approvers',
      'Built-in m-of-n gate; each approver signs the exact call',
      'Build it in workflow code on signals',
      'Build it in graph code on interrupt',
      'Not compared',
    ],
    [
      'Proof the approval came first',
      '<span class="mono">bide audit verify-approvals</span>, offline',
      'Not described in the docs we cite',
      'Not described in the docs we cite',
      'Not compared',
    ],
    [
      'Deployment',
      'A library and a database you already run',
      'A server and a worker fleet, or Temporal Cloud',
      'A library, with a hosted platform',
      'A library',
    ],
  ],
  callouts: [
    '<b>Temporal is stronger</b> if you need workers in several languages (it has eight SDKs), a clustered scheduler, or a managed service.',
    '<b>LangGraph is stronger</b> if you work in Python or TypeScript, or want hosted tooling such as LangSmith.',
    '<b>bide is pre-1.0 and Go only,</b> with a small set of model adapters, and has had no third-party security audit.',
  ],
  sourcesText: 'Sources, quotes and the idempotency-key comparison:',
  sourcesLink: 'bide-ai.com/trust/comparison',
  sourcesHref: '/trust/comparison',
}

// ---- Proofs and trust -------------------------------------------------------------------

export const proofs = {
  eyebrow: 'Claims you can check',
  title: 'Each claim links to its evidence and its limits.',
  lede: 'Built toward what a bank asks of software that moves money: keys outside the application, separation of duties, a tamper-evident trail, explicit failure modes. Not certified against any standard.',
  stats: [
    // Derived at build time from docs/formal-verification.md (see version.data.ts); the key says which.
    { value: '', from: 'models', html: 'TLA+ models of the runtime\'s protocols' },
    { value: '', from: 'bugs', html: "bugs the models found in bide's own design or code" },
    { value: '', from: 'configs', html: 'model configurations checked in the merge queue' },
    // Literal. Source: the `bide audit` verbs in docs/reference/cli.md.
    { value: '17', html: 'offline <span class="mono">bide audit</span> checks' },
    // Literal. Source: agent/architecture_test.go TestCoreModuleRequiresNothing.
    { value: '0', html: 'modules the core requires' },
  ],
  cards: [
    {
      title: 'TLA+ in CI',
      href: '/formal-verification',
      text: 'Claims, approvals, the tool-call state machine, recovery, notifications, the kill switch and more. Every model runs on main and in the merge queue; bounded, and they check the design, not the Go code.',
    },
    {
      title: 'Rocq, axiom-free',
      href: 'https://github.com/blackwell-systems/normalization-confluence',
      text: 'The convergence theory is proved without axioms and checked on Coq 8.18, 8.20 and Rocq 9.3. gsm re-checks every machine it builds with a checker extracted from that proof.',
    },
    {
      title: 'Stdlib-only verifier',
      href: '/guides/audit',
      text: "An auditor verifies a run's evidence with the Go standard library alone, from one file and a public key. Tree hashes match the published Certificate Transparency reference roots.",
    },
    {
      title: 'Release trust',
      href: '/trust/verify-releases',
      text: 'Signed archives, SBOMs and SLSA v1 provenance built to Build Level 3 requirements in a separate builder repository. Builds are reproducible and rechecked on another OS every release.',
    },
    {
      title: 'Erase, keep the proofs',
      experimental: true,
      href: '/guides/retention',
      text: 'Redact personal data and every other proof still verifies; a signed trail records each erasure, and a legal hold blocks it.',
    },
    {
      title: 'Tested at scale',
      href: '/testing/testing',
      text: '10,000,000 governed agents converge, each with an audit proof verified offline (stub model, in-memory store). Plus crash-sweep and reference-model tests against a plain-Go model of the runtime.',
    },
    {
      title: 'Post-quantum options',
      href: '/guides/key-custody',
      text: 'Audit heads and approver keys can be signed with ML-DSA-65 or an Ed25519 hybrid, in software. FIPS 140-3 mode is tested in CI, not validated.',
    },
    {
      title: 'For risk reviewers',
      href: '/trust/threat-model',
      text: 'Threat model, a control mapping to SOC 2, ISO 27001, PCI DSS and DORA that says what you still provide, operations guidance and stability policy.',
    },
  ],
  note: 'No third-party security audit yet.',
  links: [
    { text: 'Known limitations', href: '/KNOWN-LIMITATIONS' },
    { text: 'Control mapping', href: '/trust/control-mapping' },
    { text: 'Stability', href: '/trust/stability' },
    { text: 'Operations', href: '/trust/operations' },
  ],
}

// ---- Start, recipes ---------------------------------------------------------------------

export const start = {
  eyebrow: 'Start',
  title: 'From zero to a verified run.',
  lede: 'No API key needed. Everything runs on your machine, on SQLite.',
  steps: [
    { label: '1 · See it', text: 'Kill an agent mid-payment. It does not charge twice.', cmd: 'bide demo' },
    { label: '2 · Build', text: 'A flow with a 2-of-3 approval gate and a test.', cmd: 'bide init refund && cd refund && go mod tidy && go run .' },
    { label: '3 · Prove', text: "Verify the run's evidence offline.", cmd: 'bide audit verify-evidence -evidence evidence.json -pubkey keys/log.pub' },
  ],
  recipesTitle: 'Recipes to copy',
  recipesLink: 'All recipes →',
  recipes: [
    { title: 'Refund with 2-of-3 approval', text: 'Money moves only after several people sign off, and only above a threshold.', href: '/recipes/refund-approval' },
    { title: 'Invoice approval', text: 'A model reads a document, a person confirms, then a system of record changes.', href: '/recipes/invoice-approval' },
    { title: 'Support ticket triage', text: 'A model decides where work goes, and the work has side effects.', href: '/recipes/ticket-triage' },
    { title: 'Update a record safely', text: 'A write that must not run twice, even if the process dies mid-write.', href: '/recipes/customer-record' },
    { title: 'Ambient agent', text: 'Sleep, wake, check something and act; the sleep survives a restart.', href: '/recipes/ambient-agent' },
    { title: 'Get notified', text: 'A webhook tells a person when a run halts or waits for approval.', href: '/recipes/notify-human' },
  ],
  examplesText: 'Plus 25 runnable examples, including a Stripe refund in test mode with a reconciler, HSM signing, SSO certificates and MCP.',
  examplesLink: 'Browse the examples →',
  examplesHref: 'https://github.com/bide-ai/bide/tree/main/examples',
}

// ---- Blog, status, final CTA, footer ----------------------------------------------------

export const blog = {
  title: 'From the blog',
  link: 'All posts →',
  posts: [
    {
      date: '2026-10-04',
      title: 'Introducing bide: durable agents in Go',
      text: 'What bide is, a first agent, one crash resumed from the journal, and where the guarantees stop.',
      href: '/blog/introducing-bide',
    },
    {
      date: '2026-10-13',
      title: 'Model-checking an agent runtime with TLA+',
      text: 'How the TLA+ models check at-most-once, a bug they caught, and what they cannot see.',
      href: '/blog/model-checking-an-agent-runtime',
    },
  ],
}

export const status = {
  statusTitle: 'Status',
  statusText:
    // {releases} and {since} are filled in at build time from CHANGELOG.md (see version.data.ts).
    'Working v0, released often ({releases} releases since {since}). On 0.x, a minor version may break the API, and journals are not promised to resume across pre-1.0 releases: finish in-flight runs before you upgrade. Go only today; other languages are on the roadmap.',
  talkTitle: 'Talk to us',
  talkText: 'Building agents that move money, change records, or act under audit? Questions, feedback and pilots:',
  email: 'dayna@blackwell-systems.com',
}

export const cta = {
  title: 'Crash an agent mid-payment. It never charges twice.',
  cmdHtml: '<span class="p">$</span> brew install bide-ai/tap/bide &amp;&amp; bide demo',
}

export const footer = {
  license: 'Apache-2.0 licensed. Copyright 2026 Dayna Blackwell / bide-ai.',
  readmes: [
    { text: '简体中文', href: 'https://github.com/bide-ai/bide/blob/main/docs/i18n/README.zh-CN.md' },
    { text: 'Русский', href: 'https://github.com/bide-ai/bide/blob/main/docs/i18n/README.ru.md' },
    { text: 'हिन्दी', href: 'https://github.com/bide-ai/bide/blob/main/docs/i18n/README.hi.md' },
    { text: 'العربية', href: 'https://github.com/bide-ai/bide/blob/main/docs/i18n/README.ar.md' },
  ],
  columns: [
    {
      title: 'Docs',
      links: [
        { text: 'Getting started', href: '/getting-started' },
        { text: 'Tour', href: '/tour' },
        { text: 'CLI reference', href: '/reference/cli' },
        { text: 'pkg.go.dev', href: 'https://pkg.go.dev/github.com/bide-ai/bide/agent' },
      ],
    },
    {
      title: 'Trust',
      links: [
        { text: 'The guarantee', href: '/GUARANTEE' },
        { text: 'Known limitations', href: '/KNOWN-LIMITATIONS' },
        { text: 'Threat model', href: '/trust/threat-model' },
        { text: 'Control mapping', href: '/trust/control-mapping' },
        { text: 'Verify releases', href: '/trust/verify-releases' },
      ],
    },
    {
      title: 'Project',
      links: [
        { text: 'GitHub', href: 'https://github.com/bide-ai/bide' },
        { text: 'Changelog', href: '/CHANGELOG' },
        { text: 'Roadmap', href: 'https://github.com/bide-ai/bide/blob/main/docs/ROADMAP.md' },
        { text: 'Stability', href: '/trust/stability' },
        { text: 'Blog', href: '/blog/' },
      ],
    },
  ],
}

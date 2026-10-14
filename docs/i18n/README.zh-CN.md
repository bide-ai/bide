[English](../../README.md) · **简体中文** · [Русский](README.ru.md) · [हिन्दी](README.hi.md) · [العربية](README.ar.md) &nbsp; [![Coverage](https://codecov.io/gh/bide-ai/bide/branch/main/graph/badge.svg)](https://codecov.io/gh/bide-ai/bide)

<p align="center">
  <img src="../../assets/bide-banner.png" alt="Bide">
</p>

<p align="center">
  <strong>用 Go 构建的持久化 AI 智能体，面向经手资金、修改记录或在审计之下运行的智能体。</strong>
  <br>
  副作用至多触发一次，即使跨越崩溃也是如此。每个动作事后均可证明。
</p>

<p align="center"><img src="../../assets/crash-demo.gif" width="100%" alt="bide demo：智能体在扣款中途被杀死，重启后以 outcome unknown 停下而不是再次扣款，随后根据支付服务商的记录解决，bide view 显示审计证明与 2/3 审批签名均已验证；没有 bide 时重启会重试扣款，服务商扣了两次；停下时会通过签名 webhook 通知；改动日志副本中的一个金额则验证失败"></p>
<p align="center">
  <img src="../../assets/quote-rule-left.png" width="72" alt="">&nbsp;&nbsp;<em>一次你能解除的暂停，胜过一次你无法撤销的重复触发。</em>&nbsp;&nbsp;<img src="../../assets/quote-rule-right.png" width="72" alt="">
</p>

```sh
# 安装 (或: go install github.com/bide-ai/bide/cmd/bide@latest)
brew install bide-ai/tap/bide
# 出问题了？bide doctor 会检查 Go、SQLite、模块缓存和你的项目，并给出修复办法
# 1. 看它运行：在支付中途杀死智能体，它不会重复扣款（无需 API key）
bide demo
# 2. 构建：一个带 2/3 审批门的 plan 流程，基于 SQLite，无需 API key
bide init refund && cd refund && go mod tidy && go run .
# 3. 检查：在本地只读页面中查看运行（先是运行列表，再是每次运行；会打印要打开的地址）
bide view -db run.db
#    可选，无需写代码：浏览一份包含 14 次运行、涵盖各种状态的示例日志
bide sample
#    某个调用停在审批关卡？列出它，并在终端签署你的决定
bide approve -db run.db -list
# 4. 证明：离线验证这次运行的证据
bide audit verify-evidence -evidence evidence.json -pubkey keys/log.pub
```

一条仅追加（append-only）的日志，作为一个纯 Go 库，带来四项保证：副作用至多触发一次，即使跨越崩溃也是如此，因此它们无需是幂等的；在单个进程内承载成千上万个并发的持久化运行，它是一个库，无需运行集群；一条可加密验证的审计轨迹（RFC 6962 Merkle 证明，无需信任厂商即可核验）；以及可证明收敛的共享状态，其收敛条件经过机器核验。

**环境智能体（ambient agent）。** 环境智能体在无人值守下运行：它休眠，直到某个触发条件（一个计划任务或一个事件）将其唤醒，随后工作数小时乃至数天，仅在需要判断时才暂停以询问人类，全程无人盯着每一步。这样的智能体必须做到崩溃安全、重复触发安全、事后可证明。Bide 为此提供了持久化的生命周期：持久化的 `Sleep`/`WaitUntil` 定时器、用于按时间或事件驱动唤醒的可插拔 `Waker`，以及用于类型化人在回路（human-in-the-loop）的持久化 `Interrupt`/`AnswerInterrupt`，全都建立在同一条日志之上。你负责提供触发源和监督 UI；运行时则保证每次运行在休眠、崩溃和节点交接之间始终正确。

状态：可用的 v0，已完成端到端实地验证。需要 Go 1.27。

<p align="center">
  <img src="../../docs/guides/img/run-viewer-config-risk-dark.png" width="49%" alt="bide view -config: a flow's risk map, each node colored by what a crash does to it, with its approval gates marked">
  <img src="../../docs/guides/img/run-viewer-config-assert-dark.png" width="49%" alt="bide view -config: a violated policy assertion, its shortest counterexample path drawn in red">
</p>

**发布前看清流程的风险。** `bide view -config flow.yaml` 按崩溃对每个节点的影响为其着色（副作用会停下并询问；只读步骤会重新运行），并在图上检查流程的 `assert:` 策略，例如 `every path to charge passes approval >= 2 of 3`。违反时会以红色给出最短的反例路径。`bide check flow.yaml` 在 CI 中执行相同的检查，并以退出码 1 结束。参见[运行查看器指南](../../docs/guides/run-viewer.md#check-a-flow-config)。

## 一条日志，四项保证

智能体循环约 40 行。四项保证来自其下那条持久化、仅追加的日志。

### 1 · 至多一次（实测）

Temporal、DBOS、trpc-agent-go、ADK 和 eino 通过重新运行来恢复：活动或步骤必须是幂等的，因此一个非幂等的副作用（一笔扣款、一封邮件、一次发货）可能在崩溃后触发两次。一个崩溃注入基准测试（[`chaos/`](../../chaos)，跨 SDK 结果见 [`benchmarks/`](../../benchmarks/README.md)）驱动一笔非幂等的 `charge`（扣款）穿过每一个崩溃点：

<p align="center"><img src="../../assets/chaos-results.png" width="600" alt="Crash-injection benchmark: bide never repeated the charge; LangGraph (SQLite and Postgres), adk-go, trpc-agent-go, langchaingo and eino each charged again after a crash."></p>

`maxFired` 是同一个副作用实际执行的最多次数。**1 是正确的；更高就是一次重复扣款。** 每个竞品适配器都通过一项公平性测试（证明其恢复机制有效）。所有被测框架都没有的是：在非幂等写入之前写下的持久化尝试标记（attempt marker），以及恢复时的结果未知即停机（halt-on-unknown-outcome）：如果一次写入的结果从未被记入日志，运行就会停下来交由人类决定。

### 结果未知时，它就停下

难点在于这样一个副作用：它的调用已经离开了进程，但它的结果从未抵达日志。尝试标记让一次恢复的运行能区分"从未开始"与"已开始、结果未知"，而未知的情形按一个固定的顺序来解决：

<p align="center">
  <img src="../../assets/resolution-ladder.png" width="820" alt="结果未知的解决阶梯：可安全重试的副作用会自动重试，由提供商去重；留下了可查询记录的副作用由一个对账器（reconciler）自动解决；无法得知的结果则停机并等待人类。在终极的不确定之下，它停下来。">
</p>

一个工具落在哪一层，取决于它声明的 `Safety`：把它标记为只读或幂等，未知结果就会自动重试；这些都不声明，它就会停机。可重试安全是需要主动选择的；没有选择时，默认就是暂停。

大多数未知结果根本不会交到人手里：幂等键让提供商对一次安全的重试去重，而对于没有幂等键的系统（邮件、内部服务），一个对账器会根据该步骤留下的记录来解决它（`agent.ResolveHalt`）。只有两者都不适用时，才会询问人。

> [!IMPORTANT]
> 当一个动作经手资金、修改记录或在审计之下发生，而其结果无法得知时，运行就会停下。人类或对账器能解除一次暂停；一次重复扣款却无法收回。

### 2 · 作为库的持久化执行

Temporal 借助一个服务器和一支 worker 机群提供持久化执行。而这里，它来自一个你本就在运行的存储适配器（本地用 SQLite，生产用 Postgres）。一个 hello-world 只导入标准库：没有 Temporal、gRPC 或向量数据库依赖（由 `architecture_test.go` 强制保证）。

单个进程可以同时让许多持久化运行处于进行中。智能体的工作是 I/O 密集型的（在等待模型和工具调用），而 goroutine 无需集群即可吸收这类等待。[`cmd/bench`](../../cmd/bench/README.md) 测试工具对此做了测量：20,000 次运行，每次同时有 5,000 次处于进行中，每次运行在模型上阻塞约 100ms，在 10 核 Apple silicon Mac 上约半秒（约 470ms，于 v0.7.0 测得）、标准 4 vCPU CI 运行器上约一秒的墙钟时间内完成，跑在几千个 goroutine 和数十 MB 之上（`go run ./cmd/bench -runs 20000 -concurrency 5000 -latency 50ms`）。这测量的是吞吐量；每次调用的延迟由提供商决定。在高扇出下，上限是持久化存储的写入吞吐量。每一个并发运行都保有全部四项保证。为了负载下的可靠性，提供了按尝试计的超时、带退避（backoff）且能区分瞬时错误与终结性错误的重试、对冲式（hedged）模型调用（同时发起一个备份调用，取先到者，用于降低尾延迟并实现提供商故障切换），以及用于模型和工具调用的限流器（rate limiter）（[middleware](../../middleware)、[docs/guides/reliability.md](../../docs/guides/reliability.md)）。

为实现高可用，任意节点都能从共享存储恢复任意运行，而相互竞争的驱动方通过一个按运行计的租约（lease）（`agent.Lease`）来协调：通常同一时刻只有一个进程驱动某个运行，崩溃持有者的租约会过期，从而由另一个节点的 `agent.RecoverLoop` 接管。停顿超过租约期限的持有者可能醒来时仍在驱动该运行，但它无法再次触发副作用：至多一次依靠的是尝试声明（attempt claim），而不是租约。测试覆盖了内存存储上的并发 worker 互斥、崩溃接管，以及并发驱动方下的至多一次（`agent/ha_e2e_test.go`），还有 Postgres 上的跨进程至多一次，即两个存储实例共享同一个数据库（`store/postgres/postgres_test.go` 中的 `TestPostgres_HAAtMostOnceAcrossInstances`；Postgres 后端用一次 DB 时钟 upsert 实现该租约）。

### 3 · 一条可验证的审计轨迹，出自同一条日志

<p align="center"><img src="../../assets/merkle.png" width="820" alt="Merkle 包含性证明：一条日志记录（charge）沿其兄弟路径逐级哈希到签名的根，证明该记录处于已承诺的历史之中，而其他记录保持隐藏。"></p>

那条让恢复变得安全的日志同时也是审计记录，以证书透明度（Certificate Transparency）所用的密码学加以承诺（[RFC 6962](https://datatracker.ietf.org/doc/html/rfc6962)，已对照发布的参考向量核验）。第三方核验一个证明，无需信任你、你的数据库或你的日志：

- **包含性证明（inclusion proof）**：以 O(log n) 证明某一具体动作发生过（这笔扣款、这次批准），且不泄露任何其他记录（只泄露它的位置和该运行的大小）。为审计员提供选择性披露。
- **一致性证明（consistency proof）**：证明历史只被追加，从未被重写或重排。
- **签名的树头（signed tree head）＋ 持续锚定**：`AuditedStore` 为每一步签署一份承诺并将其带外发布到一个外部透明度日志，从而篡改可以被证明。
- **谁在行动，依据什么授权**：同一片叶子可以承诺到行动者身份（谁行动、代表谁、依据哪一份签名授权），并将委派授权作为一项受治理的不变量来强制执行，从而一个证明显示谁为所发生之事被授权。自带你自己的 IdP；这让被授权的动作变得可证明，它并不替代身份认证。

为单个动作生成一份可移植的 `ProofBundle`（`audit.ProveToolCall`），或生成整次运行的 `EvidencePackage`（`audit.Evidence`），将每一个实质性动作的证明打包进一个文件，交给一位审计员，他用 `bide audit verify` / `verify-evidence`，或用一个从不导入该 SDK 的纯标准库验证器，离线核验它。同一条脊柱还承载着问责层的其余部分，全部可离线验证：带证明的运行（一份 `RunCertificate` 证明整次运行的策略合规性）、带衰减式委派的签名能力授权、从一份干净审计轨迹中赢得的权限，以及受治理的 k-of-n 法定人数（quorum）。→ [docs/guides/audit.md](../../docs/guides/audit.md)

### 4 · 可证明收敛的共享状态（gsm）

多个智能体可以并发修改共享状态，即使事件相互冲突并破坏不变量，每个副本都会收敛到同一个有效状态。其思想是**通过补偿实现收敛（convergence by compensation）**：修复总会终止并与事件可交换，因此事件重放的顺序无法改变结果。其理论 [normalization confluence](https://github.com/blackwell-systems/normalization-confluence) 是一份受治理并发状态的精确情形图：每种情形都有经机器核验的精确条件、表明不存在高效条件的困难性结果，或公开声明的缺口。这些证明是无公理的，并在 Coq 8.18、8.20 与 Rocq 9.3 上经 CI 核验（[![verify](https://github.com/blackwell-systems/normalization-confluence/actions/workflows/verify.yml/badge.svg)](https://github.com/blackwell-systems/normalization-confluence/actions/workflows/verify.yml)）。

- **在构建时检查。** 把共享状态描述为由变量、不变量和事件组成的注册表；gsm 的 `Build` 检查收敛条件，否则拒绝构建并给出一个具体反例。
- **由证明本身重新核验。** gsm 返回的每台注册表机器都会在进程内由一个从 Rocq 证明生成的检查器重新认证；不能通过认证，就没有机器。
- **联邦同样适用。** 对于跨团队或跨组织链接的注册表，修复组合为唯一的范式，而跨注册表的事件顺序由每条边两项局部检查（C1 与 C2）保证安全，这两项检查由 gsm 在构建时执行。
- **在规模上测试。** 1,000 万个受治理智能体被驱动经历随机的、违反不变量的顺序，全部收敛到同一个有效状态，每个都附有一份可离线核验的审计证明（这是一个框架级测试：桩模型、内存存储；见 [docs/testing/testing.md](../../docs/testing/testing.md)）。

这个论断是精确的：对满足条件的机器、在离散且确定性的状态上，重放的顺序无关收敛性，而非"智能体总能达成一致"。详情见[治理指南](../../docs/guides/governance.md)；确切范围（因果交付与 `Independent` 事件对、交付义务、哪些检查由证明生成而哪些是 Go 代码，以及 gsm 版本历史）见[已知限制](../../docs/KNOWN-LIMITATIONS.md#governed-state-gsm)。

### 与持久化执行及智能体运行时的对比

| | **Bide** | Temporal / DBOS | ADK · eino · trpc · langchaingo |
|---|---|---|---|
| 崩溃时的非幂等副作用 | **至多一次（结果未知即停机）** | 至少一次；活动/步骤必须幂等 | 至少一次；重新运行（**实测 4–64×**） |
| 部署 | **一个库 + 一个你本就在运行的数据库** | 服务器 + worker 机群 | 库 |
| 防篡改审计 | **RFC 6962 Merkle 脊柱（同一条日志）** | 非内建 | 无 |
| 收敛的共享状态 | **可证明（gsm）**[^gsm] | 不适用 | 无 |

[^gsm]: 收敛定理经过机器核验。gsm 的 `Build` 依据其条件检查每台机器，并且只有在由该证明生成的 Go 表预言机在进程内重新核验之后才返回它；对于处在其片段之内且在成本上限之内的组合子规则，规则预言机还会直接从规则重新核验它。联邦自身的条件，包括事件顺序检查 C1 与 C2，由 gsm 的 Go 代码检查。bide 依赖 gsm v0.14.0；v0.13.0 加入了 C1 与 C2 检查（v0.12.0 两者都没有），而 v0.12.0 修复了 v0.11.0 中 `Build` 的缺口（读取另一个事件所写变量的守卫和效果）。确切范围：[已知限制](https://github.com/bide-ai/bide/blob/main/docs/KNOWN-LIMITATIONS.md#governed-state-gsm)。

### 其他细节

- **图是可选的。** 写纯 Go（`if`、`for`、函数）；`RenderMermaid` 和 `Topology` 派生出图。如果你更喜欢手写拓扑，`plan` 构建器负责这件事，并运行在同一个运行时之上。见 [Graphs（图）](tour.zh-CN.md#graphs图)。
- **Claude 的扩展思考可与工具配合使用。** 思考签名在各轮之间得以保留；当扩展思考与工具使用结合时，API 要求这样做。
- **按提供商生成的工具 schema。** 一个工具的 schema 从其 Go 类型生成一次，并以每个提供商的方言发出，包括 OpenAI 严格模式和 Gemini 的子集。
- **模型适配器。** 原生 Claude 与 Gemini，以及经由 `WithBaseURL` 的任意 OpenAI 兼容端点（OpenAI、Ollama、DeepSeek、Groq、OpenRouter、vLLM、Azure、xAI）。
- **跨节点故障切换。** 使用 Postgres 时，任意节点都能恢复任意运行。按运行计的租约阻止两个节点驱动同一个运行，而 `agent.RecoverLoop` 会在崩溃节点的租约过期时接手它的运行。

**构建者**在自己的模块中添加库：`go get github.com/bide-ai/bide/agent`。然后阅读 API [导览](tour.zh-CN.md)。

## 指南

初来乍到？从 **[入门](../../docs/getting-started.md)** 开始，用 **[文档索引](../../docs/README.md)** 获取完整地图，并参见 **[概念](../../docs/CONCEPTS.md)** 了解词汇（journal、at-most-once、lease、Waker、gsm、ProofBundle）。精确的持久性保证陈述于 **[保证](../../docs/GUARANTEE.md)**，其边界见 **[已知限制](../../docs/KNOWN-LIMITATIONS.md)**。

信任文档（面向安全与风险评审）：[威胁模型](../../docs/trust/threat-model.md)、[稳定性与支持](../../docs/trust/stability.md)、[对比](../../docs/trust/comparison.md)、[运维指南](../../docs/trust/operations.md)、[验证发布版本](../../docs/trust/verify-releases.md)（英文）。

**手写与构建**

- **[配方](../../docs/recipes/index.md)**：六个可复制即用、无需 API 密钥即可运行的完整流程：3 选 2 审批的退款、发票审批、工单分流、安全更新客户记录、会休眠并唤醒的环境智能体，以及在运行需要人工介入时通知相关人员的 webhook。
- **[流程](../../docs/guides/plan-flows.md)**：`plan` 类型化流程构建器。手写下沉到同一条日志的拓扑（`Step`/`Tool`/`Model`/`Switch`/`Join`/`LoopBack`），然后证明一次运行遵循了它（`Conform`）。可在 `examples/plan` 中运行。
- **[持久化步骤](../../docs/guides/durable-steps.md)**：组合你自己的持久化工作：`Step`、`Parallel`/`Task` 扇入、saga（`WithSaga`）以及持久化定时器（`Sleep`/`WaitUntil`）。可在 `examples/parallel` 中运行。
- **[可靠性](../../docs/guides/reliability.md)**：按尝试计的超时、分类的重试、对冲式模型调用、限流和成本跟踪，以及它们如何组合。可在 `examples/hedge` 中运行。
- **[信号与环境运行](../../docs/guides/signals.md)**：把外部事件接收进一次运行：持久化定时器和 `Waker`、人在回路（`Interrupt`/`AnswerInterrupt`），以及持久化信号（传输进来是至少一次，应用出去是恰好一次）。可在 `examples/signals`、`examples/interrupt` 中运行。
- **[模型](../../docs/guides/models.md)**：Anthropic、OpenAI 兼容和 Gemini 适配器：`WithBaseURL`、采样、提示缓存、类型化错误和多模态图像输入。
- **[MCP](../../docs/guides/mcp.md)**：把一个 MCP 服务器作为运行时工具源接入，并具备副作用安全的恢复；一个受信任服务器的工具注解可以把工具标记为可安全重新运行。
- **[可观测性](../../docs/guides/observability.md)**：一行搞定 OTel gen_ai span（`trace.Instrument`）：span 分类法、子智能体嵌套、token 到成本，以及内容捕获的隐私默认值。可在 `examples/observability` 中运行。
- **[消息](../../docs/guides/messaging.md)**：从一个入站 webhook（Slack、Telegram、SMS、Discord）驱动一个智能体，且重投递安全：一个被重试的 webhook 会重放，而不是重复触发。可在 `examples/webhook` 中运行。
- **[调试与恢复](../../docs/guides/debugging.md)**：确定性重放（`Replay`）、事件重建（`ReplayEvents`）、Mermaid 运行图，以及重新驱动被中断运行的崩溃恢复（`Recover`）。

**问责与治理**

- **[审计](../../docs/guides/audit.md)**：带证明的运行。一次运行随附一份可移植的 `RunCertificate`，可用 `bide audit verify-run` 离线核验。可在 `examples/govern/proof-carrying-run` 中运行。
- **[委派](../../docs/guides/delegation.md)**：子智能体只能收窄的签名能力授权（`Grant`/`SignGrant`），离线核验（`VerifyDelegationChain`），外加从一份干净轨迹中赢得的权限。可在 `examples/govern/delegation`、`examples/govern/authority` 中运行。
- **[安全模型](../../docs/guides/security-model.md)**：密码学保证的确切范围（完整性、真实性、防篡改性、不可否认性、选择性披露）以及范围之外的内容（机密性）。在依赖审计轨迹之前请读这个。
- **[密钥托管](../../docs/guides/key-custody.md)**：把审计日志和审批人的签名密钥放在 PKCS#11 HSM（测试用 SoftHSM2）、AWS KMS、Google Cloud KMS 或 Azure Key Vault（ECDSA P-256）中，位于 `audit.Signer` 之后。审批人可用 HSM、AWS KMS 或 Google Cloud KMS 中的密钥通过 `bide-hsm approve`（从源码构建）在终端中决定。可运行示例：`examples/hsm-signing`。
- **[治理](../../docs/guides/governance.md)**：Tier-2 受治理状态底座（gsm）。把共享状态描述为一个注册表，而 `Build()` 检查每一种交错都收敛，否则返回一个反例。可在 `examples/govern/mesh`、`examples/govern/compose` 中运行。
- **[通知](../../docs/guides/notifications.md)**：运行停止或等待批准时通知人：已签名的 HTTPS webhook（`notify.NewWebhook`）、一次调用的接收端验证（`notify.Verify`）、可选的双向 TLS 与密钥固定。
- **[批准](../../docs/guides/hitl-approval.md)**：工具运行之前的持久化人类签核，从 1-of-1 到签名的 m-of-n（`ApprovalPolicy`、`SubmitDecision`），可选职责分离（运行的发起人永不计入，`ExcludePrincipal`），并离线证明 k 位具名批准人在动作之前批准了它（`audit.ApprovalEvidence`、`audit.VerifyApprovals`）。可在 `examples/approval` 中运行。 审批人可用 `bide approve` 在终端中决定（它先显示日志中的调用，再签名），密钥由 `bide keygen` 生成。
- **[Stripe 测试模式退款](../../examples/stripe-refund/README.md)**：在退款中途杀掉智能体；重启时停下而不是退两次款，随后对账器依据 Stripe 自己的记录解决该步骤。可在 `examples/stripe-refund` 中运行。
- **[法定人数](../../docs/guides/quorum.md)**：受治理的 k-of-n 模型一致（`govern.Quorum`），计票锚定在日志中并可离线重新核对（`bide audit verify-quorum`）。可在 `examples/govern/quorum` 中运行。

**参考与内部机制**

- **[扩展点](../../docs/reference/extension-points.md)**：端口与适配器（`Model`、`Store`、`Tool`、`Compensator`、`Retriever`、`Anchor`、`EventStore`），附一个"实现你自己的存储"的演练。
- **[bide 如何被验证](../../docs/testing/verification.md)**：没有失败测试就没有修复、变异检查、崩溃与取消扫描、强制交错、一致性测试套件、协调协议的 TLA+ 模型检验，以及 CI 强制执行的内容。
- **[形式化验证](../../docs/formal-verification.md)**：bide 协调协议的 TLA+ 模型（每个 pull request 用 TLC 检验，每晚用 Apalache 检验：一个归纳不变式证明认领协议的“至多一次”规则在任意深度下成立，适用于两个驱动者、有界的尝试次数和认领 ID）、每个模型保证什么并覆盖哪些代码、模型发现或确认的每个缺陷，以及模型不覆盖的范围。
- **[测试与证据](../../docs/testing/testing.md)**：测试了什么以及如何测试、混沌崩溃注入基准、差分预言机、RFC 6962 一致性，以及 `eval` 包中可证明与统计之间的边界。
- **[日志压紧](../../docs/design/compaction.md)**（设计说明）：在不破坏审计脊柱的包含性和一致性证明的前提下，压紧一条无界的日志。

## 联系方式

问题、反馈，或有意使用 bide：**dayna@blackwell-systems.com**。安全问题请通过 [SECURITY.md](../../SECURITY.md)（私下报告）提交。

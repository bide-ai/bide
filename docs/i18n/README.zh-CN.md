[English](../../README.md) · **简体中文** · [Русский](README.ru.md) · [हिन्दी](README.hi.md) · [العربية](README.ar.md)

<p align="center">
  <img src="../../assets/bide-banner.png" alt="Bide">
</p>

**用 Go 构建持久化 AI 智能体。副作用至多触发一次。**

*一次你能解除的暂停，胜过一次你无法撤销的重复触发。*

一条仅追加（append-only）的日志，带来其他任何智能体框架都无法在单个库中同时提供的四项保证：副作用**至多触发一次**；在**单个进程内、无需集群**即可承载成千上万个并发的持久化运行；一条**可加密验证的审计轨迹**（RFC 6962 Merkle 证明，无需信任厂商即可核验）；以及**可证明收敛**的共享状态。这四项都出自同一套机制，而非四套集成起来的系统，作为一个纯 Go 库交付。专为经手资金、触及记录或在审计之下运行的智能体而生。

**为环境智能体（ambient agent）而生。** 环境智能体在无人值守下运行：它休眠，直到某个触发条件（一个计划任务或一个事件）将其唤醒，随后工作数小时乃至数天，仅在需要判断时才暂停以询问人类，全程无人盯着每一步。恰恰在这种场景下，至多一次、高可用恢复以及可验证的轨迹才从"锦上添花"变成刚需；一个不被观察、独自行动的后台智能体，必须做到崩溃安全、重复触发安全、事后可证明。Bide 为此提供了持久化的生命周期：持久化的 `Sleep`/`WaitUntil` 定时器、用于按时间或事件驱动唤醒的可插拔 `Waker`，以及用于类型化人在回路（human-in-the-loop）的持久化 `Interrupt`/`AnswerInterrupt`，全都建立在同一条日志之上。你负责提供触发源和监督 UI；运行时则保证每一次运行在休眠、崩溃和节点交接之间始终正确。

状态：**可用的 v0**，已完成端到端实地验证。需要 **Go 1.27**。

## 一条日志，四项保证

人人都提供一个智能体循环；我们的循环约 40 行。真正重要的是其下的底座：一条持久化、仅追加的日志，四项保证全都*从中推导而来*，因此你从同一套机制获得它们，而不是集成四套系统。

### 1 · 至多一次，而非至少一次（是实测出来的，不是宣称的）

Temporal、DBOS、trpc-agent-go、ADK、eino 全都通过**重新运行**来恢复：活动/步骤必须是幂等的，因此一个非幂等的副作用（一笔扣款、一封邮件、一次发货）可能在崩溃后触发两次。我们构建了一个**公平的**崩溃注入基准测试（[`chaos/`](../../chaos)，跨 SDK 结果见 [`benchmarks/`](../../benchmarks/README.md)），它驱动一笔非幂等的 `charge`（扣款）穿过每一个崩溃点。这个数字*就是*产品本身：

```
Bide      maxFired=1    ✓ at-most-once held
trpc-agent-go  maxFired=6    ✗ double-charged
adk-go         maxFired=4    ✗
langchaingo    maxFired=64   ✗
eino           maxFired=64   ✗
```

`maxFired` 是同一个副作用实际执行的最多次数。**1 是正确的；更高就是一次重复扣款。** 这些竞品适配器都经过验证*不是*稻草人（每个都带有一个公平性测试，证明其恢复机制确实有效）。它们全都没有的那一块：在非幂等写入之前写下的持久化**尝试标记（attempt marker）**，以及恢复时的**结果未知即停机（halt-on-unknown-outcome）**：如果一次写入的结果从未被记入日志，运行就会停下来交由人类决定，而不是靠猜。

### 结果未知时，它就停下

至多一次的难点不在于你看得见的崩溃，而在于你看不见的那种：一个副作用的调用已经离开了进程，但它的结果从未抵达日志。尝试标记让一次恢复的运行能区分"从未开始"与"已开始、结果未知"，并按一个固定的层级来解决未知的情形，从不靠猜：

<p align="center">
  <img src="../../assets/resolution-ladder.png" width="820" alt="结果未知的解决阶梯：可安全重试的副作用会自动重试，由提供商去重；留下了可查询记录的副作用由一个对账器（reconciler）自动解决；真正无法得知的结果则停机并等待人类。在终极的不确定之下，它停下来。">
</p>

一个工具落在哪一层，取决于它声明的 `Safety`：把它标记为只读或幂等，未知结果就会自动重试；这些都不声明，它就会停机。可重试安全是需要主动选择的；在你没有主动选择时，暂停就是默认行为，因此一个以"绝不重复触发"为全部意义的库，默认偏向安全而非靠猜。

大多数未知结果根本不会交到人手里：幂等键让提供商对一次安全的重试去重，而对于没有幂等键的系统（邮件、内部服务），一个对账器会根据该步骤留下的记录来解决它（`agent.ResolveHalt`）。人类是兜底，而不是默认。

> [!IMPORTANT]
> **其下的规则：** 当一个动作经手资金、触及记录或在审计之下发生，而其结果真正无法得知时，停下来就是正确的结果。一次人类或对账器能解除的暂停，胜过一次无人能收回的重复扣款。

### 2 · 作为库、而非集群的持久化执行

Temporal 拥有这些保证，但需要一个服务器加一支 worker 机群才能运转。而这里，它们来自一个**你本就在运行的存储适配器**（本地用 SQLite，生产用 Postgres）。一个 hello-world 只导入**标准库**：不把 Temporal、gRPC、向量数据库拖进你的二进制文件（由 `architecture_test.go` 强制保证）。导入它，而不是运维它。

而且，因为它是一个 Go 库，单个进程可以同时让数量极其庞大的这类持久化运行处于进行中。智能体的工作是 I/O 密集型的（在等待模型和工具调用），而 goroutine 无需集群即可吸收这类等待。[`cmd/bench`](../../cmd/bench/README.md) 测试工具对此做了测量：20,000 次运行，每次同时有 5,000 次处于进行中，每次运行在模型上阻塞约 100ms，在**10 核 Apple silicon Mac 上约半秒（约 470ms，于 v0.7.0 测得）、标准 4 vCPU CI 运行器上约一秒的墙钟时间**内完成，跑在几千个 goroutine 和数十 MB 之上（`go run ./cmd/bench -runs 20000 -concurrency 5000 -latency 50ms`）。它的优势在于吞吐量和运维简洁性，而不是比模型更低的延迟（每次调用的延迟由提供商决定）；在高扇出下，持久化存储的写入吞吐量才是上限，而非 goroutine。每一个并发运行都保有全部四项保证。这种负载下的可靠性是内建的：按尝试计的**超时**、带退避（backoff）且能**区分**瞬时错误与终结性错误的重试、**对冲式（hedged）**模型调用（同时发起一个备份调用，取先到者，用于降低尾延迟并实现提供商故障切换），以及用于模型和工具调用的**限流器（rate limiter）**（[middleware](../../middleware)、[docs/guides/reliability.md](../../docs/guides/reliability.md)）。

为实现高可用，任意节点都能从共享存储恢复任意运行，而相互竞争的驱动方通过一个按运行计的**租约（lease）**（`agent.Lease`）来协调：通常同一时刻只有一个进程驱动某个运行，崩溃持有者的租约会过期，从而由另一个节点的 `agent.RecoverLoop` 接管。停顿超过租约期限的持有者可能醒来时仍在驱动该运行，但它无法再次触发副作用：至多一次依靠的是尝试声明（attempt claim），而不是租约。与保证 1 一样，这是经过验证的，而非断言的：内存存储上的并发 worker 互斥、崩溃接管，以及并发驱动方下的至多一次（`agent/ha_e2e_test.go`），还有 Postgres 上的跨进程至多一次，即两个存储实例共享同一个数据库（`store/postgres/postgres_test.go` 中的 `TestPostgres_HAAtMostOnceAcrossInstances`；Postgres 后端用一次 DB 时钟 upsert 实现该租约）。

### 3 · 一条可加密验证的审计脊柱，出自同一条日志

<p align="center"><img src="../../assets/merkle.png" width="820" alt="Merkle 包含性证明：一条日志记录（charge）沿其兄弟路径逐级哈希到签名的根，证明该记录处于已承诺的历史之中，而其他记录保持隐藏。"></p>

那条让恢复变得安全的日志*就是*审计记录，并且它以**与证书透明度（Certificate Transparency）所用相同的密码学**加以承诺（[RFC 6962](https://datatracker.ietf.org/doc/html/rfc6962)，已对照发布的参考向量核验）。对受监管买家而言真正重要的区别：这是**可验证的，而非仅仅被记录下来的**。第三方核验一个证明，*无需信任你、你的数据库或你的日志*：

- **包含性证明（inclusion proof）**：以 O(log n) 证明某一具体动作发生过（这笔扣款、这次批准），且不泄露任何其他记录（只泄露它的位置和该运行的大小）。为审计员提供选择性披露。
- **一致性证明（consistency proof）**：证明历史只被追加，从未被重写或重排。
- **签名的树头（signed tree head）＋ 持续锚定**：`AuditedStore` 为每一步签署一份承诺并将其带外发布到一个外部透明度日志；篡改由此变得可证明，而不只是被怀疑。
- **谁在行动，依据什么授权**：同一片叶子可以承诺到行动者身份（谁行动、代表谁、依据哪一份签名授权），并将委派授权作为一项受治理的不变量来强制执行，从而一个证明不仅显示发生了什么，还显示谁为此被授权。自带你自己的 IdP；这让被授权的动作变得可证明，它并不替代身份认证。

**是你自己核验的证明，而非你信任的日志。** 别人提供的都是*可观测性*（你之所以信任那些日志是因为厂商通过了 SOC2）；这里提供的是*一个你亲自核验的密码学证明*。为单个动作生成一份可移植的 `ProofBundle`（`audit.ProveToolCall`），或生成整次运行的 `EvidencePackage`（`audit.Evidence`），将每一个实质性动作的证明打包进一个文件，交给一位审计员，他用 `bide-audit verify` / `verify-evidence`，或用一个从不导入该 SDK 的纯标准库验证器，离线核验它。**其他任何智能体框架都完全没有这个能力。** 同一条脊柱还承载着问责层的其余部分，全部可离线验证：带证明的运行（一份 `RunCertificate` 证明整次运行的策略合规性）、带衰减式委派的签名能力授权、从一份干净审计轨迹中赢得的权限，以及受治理的 k-of-n 法定人数（quorum）。→ [docs/guides/audit.md](../../docs/guides/audit.md)

### 4 · 可证明收敛的共享状态（gsm）

受治理的状态层：多个进程重放同一条持久化日志会**收敛到完全相同的状态**，并有一份**机器核验的证明**作后盾。**gsm** 收敛引擎的规范化重写系统是合流的（confluent），因此各步骤重放的顺序无法改变结果。该证明是无公理的，并在 Coq 8.18、8.20 与 Rocq 9.3 上经 CI 验证（`Print Assumptions` 报告 "Closed under the global context"）：[Coq/Rocq 证明](https://github.com/blackwell-systems/normalization-confluence/tree/main/coq)（[![verify](https://github.com/blackwell-systems/normalization-confluence/actions/workflows/verify.yml/badge.svg)](https://github.com/blackwell-systems/normalization-confluence/actions/workflows/verify.yml)）。gsm 的 `Build` 通过枚举状态空间来检查给定机器是否满足该定理的条件（补偿总会终止，即 WFC；事件在补偿后可交换，即 CC）。把共享状态描述为一个注册表；gsm 在构建时检查智能体动作的每一种交错（在声明了事件对时，即用 `Independent` 声明的事件对的每一种顺序）都到达同一个有效状态，否则就拒绝构建并向你展示一个反例。`Build` 只有在表预言机（table oracle，由 gsm 经机器核验的 Rocq 证明生成的 Go 代码）在进程内重新核验之后才返回机器；对于处在其片段（fragment）之内且在成本上限之内的组合子规则，规则预言机（rules oracle）还会直接从规则重新核验它。联邦自身的条件由 gsm 的 Go 代码检查。在 CI 中，bide 的必需检查 gsm machine gate 会对治理示例构建的每台机器运行该证明的检查器。确切范围，包括 CC 覆盖哪些事件对（全部事件对，或仅用 `Independent` 声明的事件对），见[已知限制](../../docs/KNOWN-LIMITATIONS.md#governed-state-gsm)。规则被表达为**可检视的组合子（combinator）数据**，而非不透明的闭包，正是这一点使它们可序列化、可移植、可重新核验；验证还可以以**局部足迹（footprint-local）**的方式运行（`BuildCompositional`），以认证那些全局状态空间大到无法枚举的机器。这就是独立的智能体在没有单一写者的情况下共享状态的方式。这个论断是精确的：*重放的顺序无关收敛性*，对满足定理条件的机器已被证明，而非"智能体总能达成一致"。这一联邦化的收敛结果已被机械化，包括异步（混沌）顺序无关性。上同调层已机械化至循环基：在可逆片段中，联邦存在全局收敛状态当且仅当每个基本循环的和乐平凡。完整的 H¹ 分类是论文证明的。

**bide 依赖 gsm v0.12.0。** bide 此前依赖的 gsm v0.11.0 的 `Build` 存在一个缺口：它对根据写入内容判定为独立的事件对跳过交换性检查，却不检查它们的守卫（guard）和效果读取了什么，因此当一个事件的守卫或效果读取了另一个事件写入的变量时（例如以 `paid` 为守卫的 `ship` 事件，而 `paid` 由 `pay` 设置），这台机器可能被认证为收敛，而实际上并不收敛。定理本身是正确的；问题在于实现在应用它时没有检查这一前提条件。gsm v0.12.0 的 `Build` 对它为 CC 检查的每一对事件（全部事件对，或仅用 `Independent` 声明的事件对）都做精确检查，不再走足迹捷径。在 v0.11.0 下记录的收敛判定或证书不在该修复的覆盖范围内；参见[已知限制](../../docs/KNOWN-LIMITATIONS.md#governed-state-gsm)。

在规模上具体化：一个集成测试驱动多达 **1,000 万个受治理智能体，每次 2,048 个**，让它们经历*随机的、违反不变量的*顺序（每一次运行都突破一个设了上限的不变量并被补偿），并断言每一个智能体都收敛到同一个有效的规范形式*且*产出一份可离线核验的审计证明，全在单个进程内、以约 3 MB 的扁平活跃堆完成（约 13 分钟，约每秒 1.25 万个智能体）。这是一个框架级的测试（桩模型、内存存储）：它在规模上考验治理和审计机器，而非一个真实的 LLM 或一个生产数据库。见 [docs/testing/testing.md](../../docs/testing/testing.md)。

### 与持久化执行及智能体运行时的对比

| | **Bide** | Temporal / DBOS | ADK · eino · trpc · langchaingo |
|---|---|---|---|
| 崩溃时的非幂等副作用 | **至多一次（结果未知即停机）** | 至少一次；活动/步骤必须幂等 | 至少一次；重新运行（**实测 4–64×**） |
| 部署 | **一个库 + 一个你本就在运行的数据库** | 服务器 + worker 机群 | 库 |
| 防篡改审计 | **RFC 6962 Merkle 脊柱（同一条日志）** | 非内建 | 无 |
| 收敛的共享状态 | **可证明（gsm）**[^gsm] | 不适用 | 无 |

[^gsm]: 收敛定理经过机器核验。gsm 的 `Build` 依据其条件检查每台机器，并且只有在由该证明生成的 Go 表预言机在进程内重新核验之后才返回它；对于处在其片段之内且在成本上限之内的组合子规则，规则预言机还会直接从规则重新核验它。联邦自身的条件由 gsm 的 Go 代码检查。bide 依赖 gsm v0.12.0，它修复了 v0.11.0 中 `Build` 对读取另一个事件所写变量的守卫和效果的缺口。确切范围：[已知限制](../../docs/KNOWN-LIMITATIONS.md#governed-state-gsm)。

### 底层的匠心

在四项保证之外，还有那些让在它之上构建变得愉快的细节：

- **默认纯 Go，附带一个可选的类型化流程构建器。** 你写 `if`/`for`/函数，而图是一个*派生出来*的视图（`RenderMermaid`、`Topology`），不是被迫要去手写的东西。当你确实想要手写拓扑时，`plan` 构建器把它交给你，并下沉到同一个运行时。见 [Graphs（图）](#graphs图)。
- **Claude 的推理在往返之间得以保留。** 扩展思考（extended-thinking）签名被保留；大多数 SDK 会丢弃它们，从而悄无声息地破坏"思考 + 工具使用"。
- **感知提供商的工具 schema。** 一份反射得到的 schema，按方言分别发出（OpenAI 严格模式等），而不是一份被严格模式和 Gemini 拒绝的通用 schema。
- **任意模型，一个适配器。** 原生 Claude、原生 Gemini，以及任意 OpenAI 兼容端点（OpenAI、Ollama、DeepSeek、Groq、OpenRouter、vLLM、Azure、xAI……）经由 `WithBaseURL`。
- **多节点故障切换，协调有序。** 任意节点恢复任意运行（Postgres，无单一写者锁）；按运行计的租约让相互竞争的恢复方与在线 worker 不至于双重驱动，而崩溃持有者的运行会在租约过期时被接管。

## Graphs（图）

大多数智能体框架把图当作*基础*：既是你必须手写的东西，也是被执行的东西，带有节点、边、一个状态对象，有时上面还有一个可视化构建器。Bide 把这一点倒了过来。同样的手写表面都可用，直至并包括一个可视化构建器，但它们是你在一个纯 Go 日志底座之上选择的层，而从来不是基座。其理由是精确的，而非意识形态的。

图不增添任何表达力。图能计算的任何东西，普通控制流都能计算：一个计算图就是一个控制流图，而顺序、选择和迭代足以表达其中任意一个。不存在哪种智能体行为你能用节点-边的图搭出来、却不能用 `if`、`for` 和函数写出来。图增添的不是能力，而是*具象化（reification）*：一份对流程的一等表示，你可以检视它、可视化它、静态校验它，并在代码之外去手写它。那确实有用，但它是一个工具层，而非一个基础，且构建智能体并不需要它。

所以这里的底座是纯 Go，而各项保证（持久性、至多一次、可验证的轨迹）来自日志，而非来自图。图仍然作为一个*派生*视图存在：`RenderMermaid` 从实际运行过的内容中把它重建出来。

如果你想要一个可手写的图，那一层已经存在：**`plan`** 包是一个受约束、经类型检查的流程构建器，它编译下沉到这个运行时，并免费继承至多一次和审计轨迹。你把类型化的节点（`Step`、`Tool`、`Model`、`Switch`、扇入的 `Join`、有界的 `LoopBack`）接入一个 `Flow`，或者把同一份拓扑写成声明式配置（`plan.Load`），供更高的一层（例如一个可视化构建器）来发出。它始终是你选择的一层，而非基座：一个图优先的框架无法提供反向的能力，因为对它来说图是基础而非一个选项。

对一个问责型运行时来说，方向同样重要。一份手写的图是一张你信任的示意图；一份派生的图是从日志中重建的，因此它恰恰是运行过的东西。`plan` 层把两者系在一起：`Topology()` 和 `RenderMermaid()` 暴露所声明的形状，而 `Conform()` 以密码学方式核验一次运行是否遵循了它所声明的拓扑，这与系统其余部分是同一种"核验、不信任"的立场。让任何这样的层都无法分叉运行时的规则是：一个新表面可以增添一种手写方式，绝不增添一种执行方式；每一层都下沉到那一个由日志支撑的运行时。见 [docs/guides/flows.md](../../docs/guides/flows.md)。

### 三种手写方式，一个运行时

同一个订单分拣流程，三种写法。纯 Go 是默认方式：写普通的控制流，并为日志必须保证崩溃安全的那些步骤命名。

<!-- docsnip: setup ctx context.Context; store *agent.Journal; order Order; type Order struct{}; type Receipt struct{}; type Assessment struct{ Rush bool }; type Reservation struct{}; func classify(Order) (Assessment, error); func reserve(Assessment) (Reservation, error); func finalize(Reservation) (Receipt, error); func decline(Assessment) (Receipt, error) -->
```go
// classify, then branch: rush orders reserve-then-finalize, the rest decline.
assess, _ := store.Step(ctx, "order-42", "classify",
    func(ctx context.Context) (Assessment, error) { return classify(order) },
    agent.WithSafety(agent.Safety{ReadOnly: true})) // safe to re-run after a crash

var receipt Receipt
if assess.Rush {
    res, _ := store.Step(ctx, "order-42", "reserve", // a side effect: at most once
        func(ctx context.Context) (Reservation, error) { return reserve(assess) })
    receipt, _ = store.Step(ctx, "order-42", "finalize",
        func(ctx context.Context) (Receipt, error) { return finalize(res) })
} else {
    receipt, _ = store.Step(ctx, "order-42", "decline",
        func(ctx context.Context) (Receipt, error) { return decline(assess) })
}
```

当你想把同一个流程作为一件一等的、可检视的工件时，`plan` 构建器把类型化的节点接入一个下沉到同一运行时的 `Flow`：

<!-- docsnip: setup type Order struct{}; type Receipt struct{}; type Assessment struct{ Rush bool }; type Reservation struct{} -->
```go
b := plan.New[Order, Receipt]("order-triage")
classify := b.Step("classify", func(ctx context.Context, o Order) (Assessment, error) { ... })
reserve  := b.Step("reserve",  func(ctx context.Context, a Assessment) (Reservation, error) { ... }) // non-idempotent
finalize := b.Step("finalize", func(ctx context.Context, r Reservation) (Receipt, error) { ... })
decline  := b.Step("decline",  func(ctx context.Context, a Assessment) (Receipt, error) { ... })

b.Switch(classify,
    plan.When(func(a Assessment) bool { return a.Rush }, reserve).Named("rush"),
    plan.Else(decline),
)
b.Edge(reserve, finalize)

flow, err := b.Build() // inherits at-most-once and the audit trail
```

或者把同一份拓扑写成声明式配置，供更高的一层（一个可视化构建器）发出，并用 `plan.Load` 加载：

```json
{
  "version": 1,
  "flow": "order-triage",
  "entry": "classify",
  "nodes": [
    {"name": "classify", "block": "classify"}, {"name": "reserve", "block": "reserve"},
    {"name": "finalize", "block": "finalize"}, {"name": "decline", "block": "decline"}
  ],
  "wiring": [
    {"switch": "classify", "when": [{"pred": "rush", "to": "reserve"}], "else": "decline"},
    {"edge": ["reserve", "finalize"]}
  ]
}
```

<!-- docsnip: setup type Order struct{}; type Receipt struct{}; configBytes []byte; reg *plan.Registry -->
```go
flow, err := plan.Load[Order, Receipt](configBytes, reg) // same topology, same Digest()
```

三者都下沉到那一个由日志支撑的运行时，因此无论你选哪个表面，至多一次、高可用恢复和可验证的轨迹都免费获得。

## 环境运行：持久化的休眠、唤醒与中断

上文的四项保证是底座；这是它们所使能的生命周期。一次环境运行并不待在一个同步的聊天循环里。它休眠，在某个触发下唤醒，为一个人类暂停，而其中每一次转换都是日志上一个持久化、至多一次的步骤，因此运行能在它们之间幸免于崩溃和节点交接。

- **休眠至某个截止时刻。** `Sleep`/`WaitUntil` 暂停一次运行并把它的唤醒时间记入日志，从而暂停能挺过一次重启。在唤醒时刻重新调用会恰好一次地恢复。
- **按时间或事件唤醒。** 一个可插拔的 `Waker`（默认是进程内的 `MemWaker`）重新调用一个到期的运行；触发源是你的（一个进程内循环、一个 cron、一个队列、一个入站 webhook），因此同一底座既驱动计划型、也驱动事件驱动型的智能体。
- **为一个人类持久化地中断。** `Interrupt[T]`/`AnswerInterrupt` 在任意点暂停一次运行以请求一个类型化的决定，并以人类的回答作为一个记入日志的步骤来恢复（见 [Human-in-the-loop（人在回路）](#人在回路human-in-the-loop)）。批准/拒绝是那个布尔的特例。

你提供触发源和监督表面；运行时保证运行在每一次休眠、唤醒、中断、崩溃和交接之间始终正确。可在 `examples/signals`（把一个事件投递进一个等待中的运行）、`examples/interrupt`（人在回路的暂停/恢复）和 `examples/recover`（持久化恢复）中运行。见[信号与环境指南](../../docs/guides/signals.md)。

## 保证 1，在代码里：它不会重复扣款

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string; type ChargeArgs struct{}; type Receipt struct{} -->
```go
// A tool that moves money is a write: not ReadOnly, not Idempotent.
charge := agent.MustFunc("charge_card", "Charge the customer", func(ctx context.Context, in ChargeArgs) (Receipt, error) { /* ... */ })

// If the process crashes after the charge fires but before its result is journaled,
// resume does NOT run it again: it returns *OutcomeUnknown so you confirm, not double-charge:
_, err := a.Run(ctx, runID, agent.UserText(input))
if halt, ok := errors.AsType[*agent.OutcomeUnknown](err); ok {
	// halt.Op.ToolName == "charge_card": outcome unknown, a human decides, no double side effect.
}
```

## 快速上手

需要 Go 1.27（核心用到了泛型方法）。如果 `go version` 更旧，请升级或设置 `GOTOOLCHAIN=go1.27.0`。

核心包是 `agent`，从 `github.com/bide-ai/bide/agent` 导入（如下面的代码块所示）。在你自己的模块中，用 `go get github.com/bide-ai/bide/agent@latest github.com/bide-ai/bide/store/sqlite@latest` 安装它和 SQLite 存储（或先写代码再运行 `go mod tidy`）。没有 API 密钥？[入门指南](../getting-started.md#no-api-key-use-agenttest) 用脚本化模型离线运行同一个智能体。

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/openai"
	"github.com/bide-ai/bide/store/sqlite"
)

type WeatherArgs struct {
	City string `json:"city" desc:"city name"`
}
type Weather struct {
	TempF int    `json:"temp_f"`
	Sky   string `json:"sky"`
}

func main() {
	// Any OpenAI-compatible endpoint (here OpenRouter); swap the base URL for Ollama, etc.
	model := openai.New(os.Getenv("OPENROUTER_API_KEY"),
		openai.WithBaseURL("https://openrouter.ai/api/v1"),
		openai.WithModel("openai/gpt-4o-mini"))

	// A tool is a typed Go function; its schema is derived automatically.
	weather := agent.MustFunc("get_weather", "Current weather for a city",
		func(_ context.Context, in WeatherArgs) (Weather, error) {
			return Weather{TempF: 68, Sky: "sunny"}, nil
		}, agent.WithSafety(agent.Safety{ReadOnly: true}))

	// Durable on-disk store: a crash mid-run resumes from here.
	store, err := sqlite.Open("agent.db")
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	j, err := agent.NewJournal(store)
	if err != nil {
		log.Fatal(err)
	}

	a, err := agent.New(
		model,
		j,
		agent.WithTools(weather),
		agent.WithSystemPrompt("You are a concise weather assistant."),
	)
	if err != nil {
		log.Fatal(err)
	}
	out, err := a.Run(context.Background(), "run-1", agent.UserText("Weather in SF? Use the tool."))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out.Message.Text())
}
```

运行实地冒烟示例：`OPENROUTER_API_KEY=sk-... go run github.com/bide-ai/bide/examples/smoke@latest`

`Run` 接受一个 `Message` 输入（文本，或文本加图像）和每次运行的选项，并返回一个 `Result`：最终消息、整次运行的 token 用量（含缓存与子智能体）、模型轮次计数和墙钟时长。`Turns` 只计本次调用的模型轮次，因此恢复的运行，或再次驱动的已完成运行，显示的轮次可能少于整次运行的轮次（`Usage` 和 `Spend` 是整次运行的）。只要运行 ID 有效，`Result` 在任何错误时都不为 nil：暂停、停机、失败、saga 中止、取消：

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string -->
```go
res, err := a.Run(ctx, runID, agent.UserText(input),
	agent.WithTokenBudget(50_000), agent.WithSystemPrompt("You are terse."))
// res.Message, res.Usage, res.Spend, res.Turns, res.Duration, res.RunID
_ = err
```

运行的首次驱动把它的输入和选项记入 `run:start`，之后的每次驱动（包括恢复）都在其下运行：不同的限额（`WithMaxTurns`、`WithTokenBudget`）作为修订 `run:limits:<n>` 记入日志，其他任何不同的设置都是 `ErrConfig`。`agent.Cancel` 取消一次运行（saga 会先回滚），`agent.Status` 从日志读取它的状态。

## 流式（Streaming）

`Run` 会阻塞并返回最终答案。要观察智能体工作（token 增量、轮次边界、工具开始/结束），请用 `Stream`。它驱动的是**同一个循环**（`Stream(...).Result()` 返回的就是 `Run` 返回的），所以持久性、恢复和副作用安全性是完全一致的：

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string -->
```go
stream := a.Stream(ctx, runID, agent.UserText(input))
for ev := range stream.Events() {
	switch e := ev.(type) {
	case agent.ModelEvent: // live token/reasoning/tool-call deltas
		if d, ok := e.Event.(agent.TextDelta); ok {
			fmt.Print(d.Text)
		}
	case agent.ToolStarted:
		fmt.Printf("\n[calling %s]\n", e.Name)
	case agent.ToolCompleted:
		fmt.Printf("[%s done]\n", e.Name)
	}
}
res, err := stream.Result() // what Run returns: the Result and an error (incl. a pause: *ApprovalPending, *OutcomeUnknown, ...)
```

事件：`TurnStarted`、`ModelEvent`（token 流）、`AssistantTurn`、`ToolStarted` / `ToolCompleted`、`ApprovalRequired`、`Finished`。为 UI 而 range `Events()` 然后调用 `Result()`，或者单独调用 `Result()` 以表现得与 `Run` 完全一样（它会替你把事件排空）。

有两件事值得知道，两者都是持久性的后果：
- **token 增量在中间件链之下抵达**（Retry / Cost 仍然看到整条组装好的消息），而且**只在一次全新的模型调用上**出现。
- **恢复时，记入日志的记录（transcript）会被重新发出**，作为 `AssistantTurn{Replayed: true}` + `ToolCompleted`，在实时进度之前，因此一个全新的 UI 能在一次崩溃之后重建整个故事，而一个被重放的轮次不产生 token 增量（它已经被决定了）。

`Stream` 接受与 `Run` 相同的选项（saga 用 `WithSaga()`），它的 `RunStream.Result()` 返回 `Run` 会返回的内容。

## 类型化输出

`RunTyped[T]` 返回一个类型化的 `T`，而非一条自由格式的消息。它注入一个合成的 `final_answer` 工具，其 JSON schema 由 `T` 派生而来（经由 `schema` 包），并引导模型在其工作完成后调用它一次，从而一个使用工具的智能体可以先做真正的工作、*然后*给出类型化的回答。与提供商无关（构建在原生工具调用之上，而非某个提供商的 JSON 模式）。

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string -->
```go
type Weather struct {
	City  string `json:"city"`
	TempF int    `json:"temp_f"`
}

w, _, err := a.RunTyped[Weather](ctx, runID, agent.UserText("weather in SF?"))
// w.City == "SF", w.TempF == 68
```

它是一个 Go 1.27 泛型方法；它也返回运行的 `Result`（其 `Output` 是记入日志的答案）。该值是从*记入日志的*工具调用中解码出来的，所以它是**恢复安全的**：一次运行中途的崩溃会在恢复时从日志中把类型化的答案找回来。该工具接受的第一次 `final_answer` 调用即结束本次运行。只有当模型从未发出这样的调用（而是以纯 JSON 文本回复）时，`RunTyped` 才会解析该运行最后一轮的文本。`T` 必须是一个 JSON 对象（结构体、指向结构体的指针或 map），因为提供商只接受对象形式的工具参数；其他任何 `T` 都是 `ErrConfig`。

在支持严格结构化输出的 OpenAI 兼容提供商上，带 `agent.WithOutputMode(agent.OutputNative)` 的 `RunTyped[T]` 使用提供商原生的 JSON-schema 响应格式而非工具（schema 在提供商侧强制执行，无需工具往返）；Anthropic 适配器不支持它并返回 `ErrConfig`，所以在那里请用默认的工具模式以获得与提供商无关的输出。

## 采样（Sampling）

生成控制项是与提供商无关的，且一次性设定；每个适配器把它们映射到自己的传输格式（并丢弃它做不到的，例如 Anthropic 没有 `seed`）：

<!-- docsnip: setup model agent.Model; store *agent.Journal; tools []agent.Tool -->
```go
a, err := agent.New(
	model,
	store,
	agent.WithTools(tools...),
	agent.WithSampling(agent.Temperature(0), agent.MaxTokens(500), agent.TopP(0.9), agent.Seed(42)),
)
if err != nil {
	panic(err)
}
```

字段在设计上是可选的：一个未设置的字段使用提供商默认值，因此一个显式的 `Temperature(0)` 有别于"未指定"。请求级的 `MaxTokens` 会覆盖适配器构造时的默认值。

## 提示缓存（Prompt caching）

一个智能体循环每一轮都重新发送一大段恒定的前缀（系统提示 + 工具 schema）。Anthropic 提示缓存把这些重复部分按缓存读取费率计费：

<!-- docsnip: setup key string -->
```go
model := anthropic.New(key, anthropic.WithPromptCache())
```

这会在系统块和工具定义上放置 `cache_control` 断点。OpenAI 自动缓存前缀（无需标志）。无论哪种方式，缓存效果都会体现在 `agent.Usage` 中（`CacheReadTokens`，自缓存供给；`CacheWriteTokens`，写入缓存），因此成本核算、追踪以及本次运行的 token 预算看到的都是真实数字。

## 会话（多轮）

`Run` 是一轮。一个 `Session` 是一场持久化的多轮对话：每一次 `Send` 都是一次完整的智能体运行（工具、恢复、副作用安全），并以迄今为止的记录为种子，因此智能体记得先前的各轮。

<!-- docsnip: setup ctx context.Context; a *agent.Agent -->
```go
s, _ := a.Session(ctx, "user-42")   // reopens + rebuilds the transcript from the store
r1, err := s.Send(ctx, agent.UserText("what's the capital of France?"))
if err != nil {
	panic(err)
}
r2, err := s.Send(ctx, agent.UserText("and its population?")) // sees turn 1 in context
if err != nil {
	panic(err)
}
fmt.Println(r1.Message.Text(), r2.Message.Text())
```

记录按会话 id 逐轮记入日志，因此一个重启的进程 `a.Session(ctx, "user-42")` 会把它重建出来并继续。第 N 轮在 `"<id>>@turn/N"` 之下运行（它自己的持久化日志处理该轮*之内*的崩溃恢复）；对话记忆是问答记录：一轮的中间工具调用留在那一轮里，不会泄漏进后面的轮次。如果一轮暂停了（批准 / `Interrupt`），`Send` 会返回那个错误；解决它，然后用相同的输入再次调用 `Send` 以恢复。在此之前，用一条不同的消息调用 `Send` 会返回 `ErrConfig`：那个未完成的轮次属于它自己的消息。对于可能被重投递的入站消息，`SendOnce(ctx, id, text)` 对每个消息 id 只回答一次。同一个会话上的多个句柄既不会丢失任何一轮，也不会把一轮记录两次，也不会用另一条消息的回复来回答某条消息。在支持租约的存储上（`MemStore`、SQLite、Postgres），一轮在其运行的租约下同一时间只由一个 worker 驱动，因此它的 token 预算在多个 worker 之间依然成立；在此期间收到同一条消息的第二个 worker 会得到 `ErrTurnContended`，稍后再发送一次即可。运行被取消的 Send 轮次会被记为关闭：该消息的 `Send` 返回 `ErrRunCancelled`，下一条消息的 `Send` 把该轮记为关闭（没有回答）并运行自己的轮次。

## 可审计性（防篡改日志）

那条持久化日志已经记录了一次运行的每一步。`audit` 包用一条哈希链承诺那段历史，从而一次运行的执行是可验证的：

<!-- docsnip: setup ctx context.Context; store *agent.Journal; runID string; priv ed25519.PrivateKey -->
```go
head, _ := audit.Head(ctx, store, runID)                     // SHA-256 chain over the stored journal bytes
sig, _ := audit.Sign(head, audit.Ed25519Signer{Priv: priv}) // anchor it: sign / publish out-of-band
```

对一条记录的任何修改 / 插入 / 删除 / 重排都会改变链头。**安全模型：** 这无条件地给出完整性，并在*你把链头带外锚定时*给出防篡改性（一条与攻击者所控制的数据库处于同一数据库中的链，可以被重写并重新哈希）；见包文档。它是合规/企业级的接缝：可证明的至多一次副作用*外加*一份关于智能体究竟做了什么的可验证记录。

对于**选择性披露**，`audit.Root` / `Prove` / `VerifyInclusion` 构建一棵 **RFC 6962**（证书透明度）Merkle 树，因此你能经由一个 O(log n) 的包含性证明证明某条记录是一次已承诺运行的一部分，*而不泄露其他记录*（例如向审计员表明发生过某一笔扣款，却不暴露任何其他客户或提示）。而 `ProveConsistency` / `VerifyConsistency` 证明一个较早的根是一个较晚的根的**仅追加前缀**：历史只被追加，从未被重写或重排（透明度日志的保证）。该实现已对照发布的 RFC 6962 测试向量核验。

`SignTreeHead` 产出 CT 风格的**签名树头（Signed Tree Head）**，即 `{Kind, RunID, Size, Root, TimestampNanos}` 连同其签名方案一起，由任意 `audit.Signer`（Ed25519、ML-DSA-65 或二者混合）签名，就是你要发布的那件工件。完整流程：签署一个 STH，稍后用一个包含性证明披露单条记录，审计员对照签名过的根来核验它，并证明两个 STH 之间的仅追加式增长。关于该模型、API 以及端到端的合规流程，见 [docs/guides/audit.md](../../docs/guides/audit.md)。

## RAG 与记忆（自带）

Bide **不附带任何向量存储、嵌入器或记忆后端**：它给你*接缝*，你插入你本就在运行的存储。针对你自己的基础设施实现一个接口：

<!-- docsnip: api agent -->
```go
type Retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]agent.Doc, error)
}
```

然后用两种方式之一把它接上：

<!-- docsnip: setup model agent.Model; journal *agent.Journal; myStore agent.Retriever -->
```go
// Agentic RAG: the model searches on demand:
a, err := agent.New(model, journal,
	agent.WithTools(agent.MustRetrievalTool("search_kb", "Search the knowledge base.", myStore, 5)))

// Classic RAG: top-k auto-injected as context on each user turn:
a, err = agent.New(model, journal, agent.WithRetrieval(myStore, 5))
```

对话记忆已经内建（`Session`）；动态上下文经由 `WithSystemPromptFunc`；这条接缝覆盖语义/长期记忆。具体的存储适配器（如果真有需要的话）会是独立的模块，绝不进入核心。见 [docs/guides/rag-memory.md](../../docs/guides/rag-memory.md)。

## 恢复安全，一张表说清

```go
agent.Safety{ReadOnly: true}          // no side effects → always safe to re-run
agent.Safety{Idempotent: true}        // safe to retry (dedupes downstream)
agent.Safety{}                        // a write → HALT on unknown outcome, don't double-fire
agent.WithApproval(agent.SingleApproval()) // not Safety: a tool option that pauses for human approval before executing
```

在一个非幂等副作用之前，循环记录一个持久化的*尝试标记*，从而恢复能区分"从未运行"（可安全运行）与"运行过并崩溃了"（停机）：是精确地，而非保守地。

这是**被证明的，而非被断言的。** `dst_test.go` 是一个确定性模拟测试：一个故障注入的存储在*每一个*写入点崩溃（并跨越数百个随机化的多次崩溃调度），而该测试工具断言一个非幂等副作用每一次都**至多触发一次**，且运行总是以完成或停机告终，从不双重触发。

该测试工具是导出的（`chaos/`），并在 `benchmarks/` 中被指向其他 SDK。实测结果：**Bide `maxFired=1`（通过）；trpc-agent-go `maxFired=6`；langchaingo `maxFired=64`（两者均失败）。** trpc 的检查点/恢复确实有效（已验证：恢复一次已完成的运行是一个空操作）；它的双重触发是那个有文档记载的 LangGraph"节点必须幂等"窗口。langchaingo 完全没有持久性，所以重试会把一切重新运行。Bide 的尝试标记把那个窗口彻底关闭。

`WithMaxTurns(n)` 为每次运行的模型轮次设上限，使一个不停调用工具的模型无法永远循环下去：触及它会返回 `ErrMaxTurns`（它是 `errors.Is` `ErrBudget` 的）。`WithTokenBudget(n)` 为一次运行可使用的 token 设上限，缓存的输入也计算在内：一旦运行已用掉 `n`，它就不再发起任何模型调用，并返回 `ErrBudgetExceeded`。每次调用的用量都随其轮次记入日志，因此两项限制都从日志中重建，并在崩溃与恢复之后依然成立。

## 人在回路（Human-in-the-loop）

三种风味。**批准/拒绝**：一个标记了 `WithApproval(SingleApproval())` 的工具在运行*之前*暂停；人类的决定是一个布尔：

<!-- docsnip: setup ctx context.Context; a *agent.Agent; store *agent.Journal; runID string; input string -->
```go
_, err := a.Run(ctx, runID, agent.UserText(input))
if pend, ok := errors.AsType[*agent.ApprovalPending](err); ok {
	// ... get a human decision ...
	agent.Approve(ctx, store, pend.RunID, pend.ToolUseID, true)
	res, _ := a.Run(ctx, pend.RootRunID, agent.UserText(input))
	var out agent.Message
	if res != nil {
		out = res.Message
	} // resumes past the pause
}
```

**中断/恢复**：一个工具在*任意点*暂停，并以一个*类型化的*值恢复（推广了那个布尔）。在一个可重试安全的工具内调用 `agent.Interrupt[T]`：

<!-- docsnip: setup ctx context.Context; a *agent.Agent; store *agent.Journal; runID string; input string; type Options struct{}; type Plan struct{}; chosenPlan Plan -->
```go
tool := agent.MustFunc("choose_plan", "pick a plan", func(ctx context.Context, in Options) (Plan, error) {
		pick, err := agent.Interrupt[Plan](ctx, "plan", in) // pauses the run; in is shown to the human
		if err != nil {
			return Plan{}, err // *InterruptPending propagates out of Run
		}
		return pick, nil // on resume, pick is the human's typed answer
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))

_, err := a.Run(ctx, runID, agent.UserText(input))
if intr, ok := errors.AsType[*agent.InterruptPending](err); ok {
	// ... show intr.Prompt, get a typed answer ...
	store.AnswerInterrupt(ctx, intr.RunID, intr.Name, chosenPlan)
	res, _ := a.Run(ctx, intr.RootRunID, agent.UserText(input))
	var out agent.Message
	if res != nil {
		out = res.Message
	} // resumes; Interrupt now returns chosenPlan
}
```

两者都是持久化的：那个决定/值是一个记入日志的步骤，所以它挺得过一次崩溃。Interrupt 必须处于一个可重试安全的工具中（`ReadOnly`/`Idempotent`）：恢复时该工具会一直重新运行直到中断被解决，所以 `Interrupt` 调用之前的一切都必须是可安全重复的。

**m-of-n 批准**：当一次签核不够时，要求来自一个具名的 n 位批准人集合中的 k 份签名决定。每位批准人签署的是确切的那次调用（工具及其参数）；该门在达到 k 份批准时放行，一旦 k 不再可达就拒绝，否则带着当前计票暂停。一份伪造或出错的决定会被忽略，而不会把它的批准人锁在门外：

<!-- docsnip: setup ctx context.Context; model agent.Model; store *agent.Journal; pend *agent.ApprovalPending; type RefundArgs struct{}; doRefund func(context.Context, RefundArgs) (string, error); keysByApprover agent.ApproverVerifierFor; signer audit.Signer -->
```go
refund := agent.MustFunc("refund", "refund the order", doRefund,
	agent.WithApproval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}))
a, err := agent.New(model, store, agent.WithTools(refund), agent.WithApproverVerifiers(keysByApprover))
if err != nil {
	panic(err)
}

// each approver, out of band, signs the paused call they were shown:
sig, _ := signer.Sign(agent.ApprovalDecisionBytes(pend.Subject(), "finance", true))
agent.SubmitDecision(ctx, store, agent.Decision{RunID: pend.RunID, ToolUseID: pend.ToolUseID,
	ApproverID: "finance", Approved: true, Alg: signer.Alg(), Signature: sig})
```

然后，`audit.ApprovalEvidence` 与 `audit.VerifyApprovals`（或 `bide-audit verify-approvals`）离线证明：k 位具名批准人在这次确切的调用运行*之前*、依照预期的策略签核了它，所依据的证据不可能在不被察觉的情况下漏掉任何一份决定。每位批准人都需要自己的密钥：若策略中有两位批准人解析到同一个密钥，该策略会以 `ErrConfig` 被拒绝，因为持有该密钥的人可以替两人签名。见[批准指南](../../docs/guides/hitl-approval.md)；可跨独立进程在 `examples/approval` 中运行。

## 错误

失败以哨兵错误（sentinel error）来分类，由 `errors.Is` 匹配，这是标准库的惯用法，没有自定义的错误框架。两层：一个**类别（category）**（粗粒度的类）和一个**条件（condition）**（一个具体的成因），后者包裹其类别，因此匹配可以在你需要的任一层级上工作：

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string; func backOffAndRetry(); func fixToolWiring(); func alertOps() -->
```go
_, err := a.Run(ctx, runID, agent.UserText(input))
switch {
case errors.Is(err, agent.ErrModel):       // any provider fault (HTTP status, decode, stream)
	backOffAndRetry()
case errors.Is(err, agent.ErrUnknownTool):  // a specific condition (implies agent.ErrTool)
	fixToolWiring()
case errors.Is(err, agent.ErrStorage):      // durable-store I/O
	alertOps()
}
```

类别：`ErrConfig`、`ErrModel`、`ErrTool`、`ErrStorage`、`ErrProtocol`、`ErrBudget`。条件（每一个都包裹一个类别）：`ErrUnknownTool`、`ErrToolArgs`（包裹 `ErrTool`）；`ErrToolReinvoked`、`ErrInvalidApproval`、`ErrAlreadyDecided`（包裹 `ErrConfig`）；`ErrNoRecordedOutput`、`ErrIncompleteResponse`（包裹 `ErrModel`）；`ErrTruncatedToolArgs`（包裹 `ErrProtocol`）；`ErrBudgetExceeded`、`ErrMaxTurns`（包裹 `ErrBudget`）。`ErrToolNotCalled` 不包裹任何类别：它标记一次已知从未到达其工具的工具调用（工具中间件的拒绝会包裹它）。提供商适配器还会返回 `*RateLimited`（HTTP 429，附带一个 `RetryAfter` 提示）和 `*APIError`（其他非 2xx，附带 `StatusCode`），两者都包裹 `ErrModel`。该工具包返回的每一个错误（包括来自模型、MCP、存储和治理适配器的）都带有一个类别，所以 `errors.Is` 在整个表面上都是可靠的。

而**控制流信号**比一个类别更丰富，所以它们保持为具体类型，由 `errors.As` 匹配：`*ApprovalPending`（需要批准）、`*InterruptPending`（等待人类输入）、`*TimerPending`（持久化定时器待触发）、`*SignalPending`（等待一个外部信号）、`*OutcomeUnknown`（恢复不安全）、`*SagaAborted`（已回滚），以及 `*HaltTooYoung`（来自 `ResolveHalt`，当 `WithMinHaltAge` 尚未到期时）。它们都实现了密封接口 `agent.Pause`；用 `agent.IsPause(err)` 判断，用 `agent.AsPause(err)` 读取。一个暂停或停机的运行不是一个"失败"类别；检视那个结构体以获取 `RunID` / `ToolUseID` / 补偿细节。取消以通常的 `context.Canceled` / `context.DeadlineExceeded` 浮现，而一次因其运行租约（`agent.Lease`）丢失而被取消的驱动则以 `ErrLeaseLost` 浮现；与取消一样，它不带任何类别。

## 中间件与可观测性

两条独立的 `func(Handler) Handler` 链，位于两个重要的边界上：模型调用（`WithMiddleware`）和每一次工具调用（`WithToolMiddleware`）。最先添加 = 最外层。两者都是*可变更且可短路的*：改写送进去的东西、变换出来的东西，或者不调用 `next` 就返回。

<!-- docsnip: setup model agent.Model; store *agent.Journal; tools []agent.Tool; import oteltrace "go.opentelemetry.io/otel/trace"; tracer oteltrace.Tracer -->
```go
var cost middleware.CostMeter
a, err := agent.New(model, store,
	agent.WithTools(tools...),
	agent.WithTokenBudget(100_000), // per run, rebuilt from the journal on resume
	agent.WithMiddleware(
		middleware.Retry(3, middleware.WithBackoff(200*time.Millisecond, 10*time.Second)),
		middleware.Cost(&cost, middleware.Rates{InputPer1M: 3, OutputPer1M: 15}),
	),
	agent.WithToolMiddleware(middleware.ToolLog(log.Printf), middleware.ToolCache(), middleware.ToolRetry(3)),
)
if err != nil {
	panic(err)
}

// opt-in OTel gen_ai.* spans (provider and model from agent.ModelInfoOf); the core has no OTel dependency:
a, err = a.With(
	agent.WithMiddleware(trace.Model(tracer)),
	agent.WithToolMiddleware(trace.Tool(tracer)), // execute_tool span per call; nests across the sub-agent boundary
)
// ... after the run: cost.Snapshot() (answer and spend, in tokens and USD)
```

`Retry` 做带抖动的指数退避，并在一个提供商 429 上尊重 `Retry-After`（适配器返回一个类型化的 `*agent.RateLimited`）；`Cost` 把来自 token 用量（含缓存读/写）的 USD 累加进一个你在运行后读取的 `CostMeter`。

因为 `trace.Tool` 在循环内部运行，它的 span 处于交给工具的上下文中，所以当一个工具本身就是一个子智能体时，该子智能体的运行及其自己的 span 会作为子级嵌套。该 trace 自动跨越子智能体边界（这是 ADK / AgenticGoKit / trpc-agent-go 的一个缺口）。

工具中间件在持久化步骤*内部*运行，所以一次短路的结果（一次 `ToolCache` 命中）会像任何工具结果一样被记入日志，包裹了 `agent.ErrToolNotCalled` 的策略拒绝也一样；拒绝必须包裹 `agent.ErrToolNotCalled`：不包裹它的拒绝会让副作用的结果未知，什么都不记录，运行随之停止；恢复会重放它，绝不重新运行中间件或工具。`ToolRetry` 和 `ToolCache` 只作用于 `Safety` 允许的工具（分别是可重试安全的和 `ReadOnly` 的），而无论中间件做什么，智能体对一个不可重试安全的工具每次调用至多运行一次。用 `agent.ToolMiddleware` 签名写你自己的：

<!-- docsnip: setup func authorized(context.Context, string) bool -->
```go
// Deny a tool by policy: the tool never executes; the model sees the error and reacts.
// A denial must wrap agent.ErrToolNotCalled: without it the agent cannot tell the tool did
// not run, so a side effect's outcome is unknown and the run halts.
func RequireTag(tag string) agent.ToolMiddleware {
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, call agent.ToolCall) (json.RawMessage, error) {
			if !authorized(ctx, tag) {
				return nil, fmt.Errorf("tool %q denied: %w", call.Use.Name, agent.ErrToolNotCalled)
			}
			return next(ctx, call) // mutate call.Use.Args before, transform the result after
		}
	}
}
```

## 模块

Bide 是一个多模块仓库：一个依赖精简的**核心**（`github.com/bide-ai/bide`，即循环、schema、中间件、模型适配器、`plan` 流程构建器、`audit`；依赖仅有 `x/sync` + `x/text`），外加每个重型适配器一个模块（`mcptools`、`trace`、`store/sqlite`、`store/postgres`、`govern/redislog`、`govern/sqlitelog`、`govern/postgreslog`、`codec/gcf`），以及承载 gsm 的 `govern` 模块（在 gsm 稳定之前保持 v0.x）。导入一个适配器，你就拉进它的依赖树；只导入核心，你就不会。一个仅用核心的消费者，其外部模块表面是 2，而不是 54。见 [docs/reference/module-structure.md](../../docs/reference/module-structure.md)。

## 架构

由构造即为六边形（hexagonal）：核心定义端口（`Model`、`Store`、`Tool`、`Middleware`）；适配器在边缘处插入。依赖向内指；核心不导入任何适配器、任何基础设施，由 `architecture_test.go` 守护。

```
agent (root)     durable loop · Journal/Store · Message/Part · Tool/Safety · middleware types · RenderMermaid
plan             optional typed flow builder + declarative config; lowers to the loop (Topology · Conform)
model/anthropic  native Claude (thinking + signatures)
model/openai     any OpenAI-compatible endpoint
model/gemini     native Gemini (generativelanguage / Vertex via WithBaseURL)
schema           reflect Go types → inline JSON Schema + OpenAIStrict
middleware       Retry, RateLimit, Cost, Hedge
trace            opt-in OTel gen_ai.* spans
store/sqlite     on-disk durable resume (single binary, no cluster)
store/postgres   HA durable resume (any node resumes any run)
govern           Tier-2: federated governed state + quorum for agents that must agree (gsm-backed; own module)
```

## 联邦化治理：可证明地达成一致的智能体（Tier-2）

持久化核心让*一个*智能体的工作免于崩溃。`govern` 层处理另一个难题：**许多独立运行、却必须达成一致的智能体**，跨进程、跨团队或跨组织边界，没有中央协调者、没有单一写者。它给出两种可验证的一致形式，而在两者中要点都是*核验，不信任*：一方从公开工件核验结果，无需信任任何其他人的智能体。

**在共享状态上达成一致（收敛）。** 把共享状态描述为一个注册表（变量 + 不变量 + 事件）；gsm 在*构建时*检查智能体动作的每一种交错都到达同一个有效状态，否则就拒绝构建并向你展示一个反例（其范围参见[已知限制](../../docs/KNOWN-LIMITATIONS.md#governed-state-gsm)）。运行时是 O(1) 的表查找；状态是事件溯源的、崩溃可恢复的。它从单个共享注册表向上扩展、穿过**联邦（federations）**（跨边界约束：树、带解析器的多源 DAG、单调循环的*网格 mesh*），经由 `Embed` 组合，甚至能替你**合成**补偿（声明规则，得到一个收敛的治理者，或一份证明其不存在的证据）。智能体经由 `FederatedEventTool` 插入，因此一次 LLM 工具调用就成为一个受治理的事件。

**在一个决定上达成一致（法定人数 quorum）。** k-of-n 个具名投票者（每一个是一个模型、提供商或主体）投出一个规范化的决定；每一票都是一个记入日志、至多一次的步骤，记录了谁怎么投的，而 k-of-n 的门是投票计数上的一个 gsm 不变量，因此"k 个达成一致"在每一种可能的计票上都被机器核验。`bide-audit verify-quorum` 从公开工件重新核对计票和每一票，在不信任生产者的情况下复现多数决规则。这个论断是精确的：一个法定人数证明的是*k 个投票者达成了一致*，并降低单模型风险；它并不认证那个决定是正确的（相关的错误不是独立性），且只有规范化的决定才能被法定人数化，自由格式的散文不能。

<!-- docsnip: setup ctx context.Context; machine *gsm.Machine; import "github.com/blackwell-systems/gsm"; log govern.EventLog -->
```go
gov, _ := govern.NewPersistent(ctx, machine, log, "order-42", machine.NewState())
tool := govern.EventTool(gov, govern.EventToolConfig{Name: "pay", Description: "mark the order paid", Event: "pay"})
// hand `tool` to the agent: concurrent agents sharing `gov` converge, durably.
```

> 完整指南、能力阶梯，以及可运行的演示（`examples/govern/mesh`、`examples/govern/compose`、`examples/govern/quorum`）见 **[docs/guides/governance.md](../../docs/guides/governance.md)**。

## 指南

初来乍到？从 **[入门](../../docs/getting-started.md)** 开始，用 **[文档索引](../../docs/README.md)** 获取完整地图，并参见 **[概念](../../docs/CONCEPTS.md)** 了解词汇（journal、at-most-once、lease、Waker、gsm、ProofBundle）。精确的持久性保证陈述于 **[保证](../../docs/GUARANTEE.md)**，其边界见 **[已知限制](../../docs/KNOWN-LIMITATIONS.md)**。

**手写与构建**

- **[流程](../../docs/guides/flows.md)**：`plan` 类型化流程构建器。手写下沉到同一条日志的拓扑（`Step`/`Tool`/`Model`/`Switch`/`Join`/`LoopBack`），然后证明一次运行遵循了它（`Conform`）。可在 `examples/plan` 中运行。
- **[持久化步骤](../../docs/guides/durable-steps.md)**：组合你自己的持久化工作：`Step`、`Parallel`/`Task` 扇入、saga（`WithSaga`）以及持久化定时器（`Sleep`/`WaitUntil`）。可在 `examples/parallel` 中运行。
- **[可靠性](../../docs/guides/reliability.md)**：按尝试计的超时、分类的重试、对冲式模型调用、限流和成本跟踪，以及它们如何组合。可在 `examples/hedge` 中运行。
- **[信号与环境运行](../../docs/guides/signals.md)**：把外部事件接收进一次运行：持久化定时器和 `Waker`、人在回路（`Interrupt`/`AnswerInterrupt`），以及持久化信号（传输进来是至少一次，应用出去是恰好一次）。可在 `examples/signals`、`examples/interrupt` 中运行。
- **[模型](../../docs/guides/models.md)**：Anthropic、OpenAI 兼容和 Gemini 适配器：`WithBaseURL`、采样、提示缓存、类型化错误和多模态图像输入。
- **[MCP](../../docs/guides/mcp.md)**：把一个 MCP 服务器作为运行时工具源接入，并具备副作用安全的恢复；一个受信任服务器的工具注解可以把工具标记为可安全重新运行。
- **[可观测性](../../docs/guides/observability.md)**：一行搞定 OTel gen_ai span（`trace.Instrument`）：span 分类法、子智能体嵌套、token 到成本，以及内容捕获的隐私默认值。可在 `examples/observability` 中运行。
- **[消息](../../docs/guides/messaging.md)**：从一个入站 webhook（Slack、Telegram、SMS、Discord）驱动一个智能体，且重投递安全：一个被重试的 webhook 会重放，而不是重复触发。可在 `examples/webhook` 中运行。
- **[调试与恢复](../../docs/guides/debugging.md)**：确定性重放（`Replay`）、事件重建（`ReplayEvents`）、Mermaid 运行图，以及重新驱动被中断运行的崩溃恢复（`Recover`）。

**问责与治理**

- **[审计](../../docs/guides/audit.md)**：带证明的运行。一次运行随附一份可移植的 `RunCertificate`，可用 `bide-audit verify-run` 离线核验。可在 `examples/govern/proof-carrying-run` 中运行。
- **[委派](../../docs/guides/delegation.md)**：子智能体只能收窄的签名能力授权（`Grant`/`SignGrant`），离线核验（`VerifyDelegationChain`），外加从一份干净轨迹中赢得的权限。可在 `examples/govern/delegation`、`examples/govern/authority` 中运行。
- **[安全模型](../../docs/guides/security-model.md)**：密码学保证的确切范围（完整性、真实性、防篡改性、不可否认性、选择性披露）以及范围之外的内容（机密性）。在依赖审计轨迹之前请读这个。
- **[治理](../../docs/guides/governance.md)**：Tier-2 受治理状态底座（gsm）。把共享状态描述为一个注册表，而 `Build()` 检查每一种交错都收敛，否则返回一个反例。可在 `examples/govern/mesh`、`examples/govern/compose` 中运行。
- **[批准](../../docs/guides/hitl-approval.md)**：工具运行之前的持久化人类签核，从 1-of-1 到签名的 m-of-n（`ApprovalPolicy`、`SubmitDecision`），并离线证明 k 位具名批准人在动作之前批准了它（`audit.ApprovalEvidence`、`audit.VerifyApprovals`）。可在 `examples/approval` 中运行。
- **[法定人数](../../docs/guides/quorum.md)**：受治理的 k-of-n 模型一致（`govern.Quorum`），计票锚定在日志中并可离线重新核对（`bide-audit verify-quorum`）。可在 `examples/govern/quorum` 中运行。

**参考与内部机制**

- **[扩展点](../../docs/reference/extension-points.md)**：端口与适配器（`Model`、`Store`、`Tool`、`Compensator`、`Retriever`、`Anchor`、`EventStore`），附一个"实现你自己的存储"的演练。
- **[bide 如何被验证](../../docs/testing/verification.md)**：没有失败测试就没有修复、变异检查、崩溃与取消扫描、强制交错、一致性测试套件、协调协议的 TLA+ 模型检验，以及 CI 强制执行的内容。
- **[形式化验证](../../docs/formal-verification.md)**：bide 协调协议的 TLA+ 模型（每个 pull request 用 TLC 检验，每晚用 Apalache 检验：一个归纳不变式证明认领协议的“至多一次”规则在任意深度下成立，适用于两个驱动者、有界的尝试次数和认领 ID）、每个模型保证什么并覆盖哪些代码、模型在发布前发现的每个缺陷，以及模型不覆盖的范围。
- **[测试与证据](../../docs/testing/testing.md)**：测试了什么以及如何测试、混沌崩溃注入基准、差分预言机、RFC 6962 一致性，以及 `eval` 包中可证明与统计之间的边界。
- **[日志压紧](../../docs/design/compaction.md)**（设计说明）：在不破坏审计脊柱的包含性和一致性证明的前提下，压紧一条无界的日志。

## 联系方式

问题、反馈，或有意使用 bide：**dayna@blackwell-systems.com**。安全问题请通过 [SECURITY.md](../../SECURITY.md)（私下报告）提交。

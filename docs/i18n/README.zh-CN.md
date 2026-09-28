[English](../../README.md) · **简体中文** · [Русский](README.ru.md) · [हिन्दी](README.hi.md)

<p align="center">
  <img src="../../assets/bide-banner.png" alt="Bide">
</p>

**用 Go 构建持久化 AI 智能体。副作用至多触发一次。**

一条仅追加（append-only）的日志，带来其他任何智能体框架都无法在单个库中同时提供的四项保证：副作用**至多触发一次**；在**单个进程内、无需集群**即可承载成千上万个并发的持久化运行；一条**可加密验证的审计轨迹**（RFC 6962 Merkle 证明，无需信任厂商即可核验）；以及**可证明收敛**的共享状态。这四项都出自同一套机制，而非四套集成起来的系统，作为一个纯 Go 库交付。专为经手资金、触及记录或在审计之下运行的智能体而生。

**为环境智能体（ambient agent）而生。** 环境智能体在无人值守下运行：它休眠，直到某个触发条件（一个计划任务或一个事件）将其唤醒，随后工作数小时乃至数天，仅在需要判断时才暂停以询问人类，全程无人盯着每一步。恰恰在这种场景下，至多一次、高可用恢复以及可验证的轨迹才从"锦上添花"变成刚需；一个不被观察、独自行动的后台智能体，必须做到崩溃安全、重复触发安全、事后可证明。Bide 为此提供了持久化的生命周期：持久化的 `Sleep`/`WaitUntil` 定时器、用于按时间或事件驱动唤醒的可插拔 `Waker`，以及用于类型化人在回路（human-in-the-loop）的持久化 `Interrupt`/`Resume`，全都建立在同一条日志之上。你负责提供触发源和监督 UI；运行时则保证每一次运行在休眠、崩溃和节点交接之间始终正确。

状态：**可用的 v0**，已完成端到端实地验证。需要 **Go 1.27**。

## 一条日志，四项保证

人人都提供一个智能体循环；我们的循环约 40 行。真正重要的是其下的底座：一条持久化、仅追加的日志，四项保证全都*从中推导而来*，因此你从同一套机制获得它们，而不是集成四套系统。

### 1 · 至多一次，而非至少一次（是实测出来的，不是宣称的）

Temporal、DBOS、trpc-agent-go、ADK、eino 全都通过**重新运行**来恢复：活动/步骤必须是幂等的，因此一个非幂等的副作用（一笔扣款、一封邮件、一次发货）可能在崩溃后触发两次。我们构建了一个**公平的**崩溃注入基准测试（[`chaos/`](../../chaos)，跨 SDK 结果见 [`benchmarks/`](../../benchmarks/README.md)），它驱动一笔非幂等的 `charge`（扣款）穿过每一个崩溃点。这个数字*就是*产品本身：

```
Bide      maxFired=1    ✓ at-most-once held
trpc-agent-go  maxFired=5    ✗ double-charged
adk-go         maxFired=4    ✗
langchaingo    maxFired=64   ✗
eino           maxFired=64   ✗
```

`maxFired` 是同一个副作用实际执行的最多次数。**1 是正确的；更高就是一次重复扣款。** 这些竞品适配器都经过验证*不是*稻草人（每个都带有一个公平性测试，证明其恢复机制确实有效）。它们全都没有的那一块：在非幂等写入之前写下的持久化**尝试标记（attempt marker）**，以及恢复时的**结果未知即停机（halt-on-unknown-outcome）**：如果一次写入的结果从未被记入日志，运行就会停下来交由人类决定，而不是靠猜。

### 2 · 作为库、而非集群的持久化执行

Temporal 拥有这些保证，但需要一个服务器加一支 worker 机群才能运转。而这里，它们来自一个**你本就在运行的存储适配器**（本地用 SQLite，生产用 Postgres）。一个 hello-world 只导入**标准库**：不把 Temporal、gRPC、向量数据库拖进你的二进制文件（由 `architecture_test.go` 强制保证）。导入它，而不是运维它。

而且，因为它是一个 Go 库，单个进程可以同时让数量极其庞大的这类持久化运行处于进行中。智能体的工作是 I/O 密集型的（在等待模型和工具调用），而 goroutine 无需集群即可吸收这类等待。[`cmd/bench`](../../cmd/bench/README.md) 测试工具对此做了测量：5,000 次各自在模型上阻塞约 100ms 的运行，重叠成**约 450ms 的墙钟时间**，跑在几千个 goroutine 和数十 MB 之上。它的优势在于吞吐量和运维简洁性，而不是比模型更低的延迟（每次调用的延迟由提供商决定）；在高扇出下，持久化存储的写入吞吐量才是上限，而非 goroutine。每一个并发运行都保有全部四项保证。这种负载下的可靠性是内建的：按尝试计的**超时**、带退避（backoff）且能**区分**瞬时错误与终结性错误的重试、**对冲式（hedged）**模型调用（同时发起一个备份调用，取先到者，用于降低尾延迟并实现提供商故障切换），以及用于模型和工具调用的**限流器（rate limiter）**（[middleware](../../middleware)、[docs/guides/reliability.md](../../docs/guides/reliability.md)）。

为实现高可用，任意节点都能从共享存储恢复任意运行，而相互竞争的驱动方通过一个按运行计的**租约（lease）**（`agent.Lease`）来协调：同一时刻只有一个进程驱动某个运行，崩溃持有者的租约会过期从而由另一个节点接管，任何运行都不会被双重驱动。与保证 1 一样，这是经过验证的，而非断言的：并发 worker 互斥、崩溃接管，以及 Postgres 上的跨进程至多一次（`ha_e2e_test.go`；Postgres 后端用一次 DB 时钟 upsert 实现该租约）。

### 3 · 一条可加密验证的审计脊柱，出自同一条日志

那条让恢复变得安全的日志*就是*审计记录，并且它以**与证书透明度（Certificate Transparency）所用相同的密码学**加以承诺（[RFC 6962](https://datatracker.ietf.org/doc/html/rfc6962)，已对照发布的参考向量核验）。对受监管买家而言真正重要的区别：这是**可验证的，而非仅仅被记录下来的**。第三方核验一个证明，*无需信任你、你的数据库或你的日志*：

- **包含性证明（inclusion proof）**：以 O(log n) 证明某一具体动作发生过（这笔扣款、这次批准），且不泄露其他任何信息。为审计员提供选择性披露。
- **一致性证明（consistency proof）**：证明历史只被追加，从未被重写或重排。
- **签名的树头（signed tree head）＋ 持续锚定**：`AuditedStore` 为每一步签署一份承诺并将其带外发布到一个外部透明度日志；篡改由此变得可证明，而不只是被怀疑。
- **谁在行动，依据什么授权**：同一片叶子可以承诺到行动者身份（谁行动、代表谁、依据哪一份签名授权），并将委派授权作为一项受治理的不变量来强制执行，从而一个证明不仅显示发生了什么，还显示谁为此被授权。自带你自己的 IdP；这让被授权的动作变得可证明，它并不替代身份认证。

**是你自己核验的证明，而非你信任的日志。** 别人提供的都是*可观测性*（你之所以信任那些日志是因为厂商通过了 SOC2）；这里提供的是*一个你亲自核验的密码学证明*。为单个动作生成一份可移植的 `ProofBundle`（`audit.ProveToolCall`），或生成整次运行的 `EvidencePackage`（`audit.Evidence`），将每一个实质性动作的证明打包进一个文件，交给一位审计员，他用 `bide-audit verify` / `verify-evidence`，或用一个从不导入该 SDK 的纯标准库验证器，离线核验它。**其他任何智能体框架都完全没有这个能力。** 同一条脊柱还承载着问责层的其余部分，全部可离线验证：带证明的运行（一份 `RunCertificate` 证明整次运行的策略合规性）、带衰减式委派的签名能力授权、从一份干净审计轨迹中赢得的权限，以及受治理的 k-of-n 法定人数（quorum）。→ [docs/guides/audit.md](../../docs/guides/audit.md)

### 4 · 可证明收敛的共享状态（gsm）

受治理的状态层：多个进程重放同一条持久化日志会**收敛到完全相同的状态**，并有一份**机器核验的证明**作后盾。**gsm** 收敛引擎的规范化重写系统是合流的（confluent），因此各步骤重放的顺序无法改变结果。该证明是无公理的，并在 Coq 8.18 与 8.20 上经 CI 验证（`Print Assumptions` 报告 "Closed under the global context"）：[Coq/Rocq 证明](https://github.com/blackwell-systems/normalization-confluence/tree/main/coq)（[![verify](https://github.com/blackwell-systems/normalization-confluence/actions/workflows/verify.yml/badge.svg)](https://github.com/blackwell-systems/normalization-confluence/actions/workflows/verify.yml)）。而且这份证明并不只是躺在代码旁边：gsm 自身对每台机器的判定会被**从该证明中提取出来的两个独立检查器重新认证**（一个从发出的步骤表重新计算收敛性，另一个直接从规则重新计算），因此 gsm 的 Go 验证器里的 bug 不可能让一台非收敛的机器蒙混过关。规则被表达为**可检视的组合子（combinator）数据**，而非不透明的闭包，正是这一点使它们可序列化、可移植、可重新核验；验证还可以以**局部足迹（footprint-local）**的方式运行（`BuildCompositional`），以认证那些全局状态空间大到无法枚举的机器。这就是独立的智能体在没有单一写者的情况下共享状态的方式。这个论断是精确的：*重放的顺序无关收敛性*，已被证明，而非"智能体总能达成一致"。这一联邦化结果被完整地机械化了，包括异步（混沌）顺序无关性。

在规模上具体化：一个集成测试驱动多达 **1,000 万个并发的受治理智能体**，让它们经历*随机的、违反不变量的*顺序（每一次运行都突破一个设了上限的不变量并被补偿），并断言每一个智能体都收敛到同一个有效的规范形式*且*产出一份可离线核验的审计证明，全在单个进程内、以约 4 MB 的扁平活跃堆完成（约 8.5 分钟，约每秒 2 万个智能体）。这是一个框架级的测试（桩模型、内存存储）：它在规模上考验治理和审计机器，而非一个真实的 LLM 或一个生产数据库。见 [docs/testing/testing.md](../../docs/testing/testing.md)。

### 与持久化执行及智能体运行时的对比

| | **Bide** | Temporal / DBOS | ADK · eino · trpc · langchaingo |
|---|---|---|---|
| 崩溃时的非幂等副作用 | **至多一次（结果未知即停机）** | 至少一次；活动/步骤必须幂等 | 至少一次；重新运行（**实测 4–64×**） |
| 部署 | **一个库 + 一个你本就在运行的数据库** | 服务器 + worker 机群 | 库 |
| 防篡改审计 | **RFC 6962 Merkle 脊柱（同一条日志）** | 非内建 | 无 |
| 收敛的共享状态 | **可证明（gsm）** | 不适用 | 无 |

### 底层的匠心

在四项保证之外，还有那些让在它之上构建变得愉快的细节：

- **默认纯 Go，附带一个可选的类型化流程构建器。** 你写 `if`/`for`/函数，而图是一个*派生出来*的视图（`RenderMermaid`、`Topology`），不是被迫要去手写的东西。当你确实想要手写拓扑时，`plan` 构建器把它交给你，并下沉到同一个运行时。见 [Graphs（图）](#graphs图)。
- **Claude 的推理在往返之间得以保留。** 扩展思考（extended-thinking）签名被保留；大多数 SDK 会丢弃它们，从而悄无声息地破坏"思考 + 工具使用"。
- **感知提供商的工具 schema。** 一份反射得到的 schema，按方言分别发出（OpenAI 严格模式等），而不是一份被严格模式和 Gemini 拒绝的通用 schema。
- **任意模型，一个适配器。** 原生 Claude、原生 Gemini，以及任意 OpenAI 兼容端点（OpenAI、Ollama、DeepSeek、Groq、OpenRouter、vLLM、Azure、xAI……）经由 `WithBaseURL`。
- **多节点故障切换，协调有序。** 任意节点恢复任意运行（Postgres，无单一写者锁）；按运行计的租约让相互竞争的恢复方与在线 worker 不至于双重驱动，而崩溃持有者的运行会在租约过期时被接管。

## Graphs（图）

大多数智能体框架把图当作你要去手写的东西：节点、边、一个状态对象，有时还有一个可视化构建器。Bide 不这样，而其理由是精确的，而非意识形态的。

图不增添任何表达力。图能计算的任何东西，普通控制流都能计算：一个计算图就是一个控制流图，而顺序、选择和迭代足以表达其中任意一个。不存在哪种智能体行为你能用节点-边的图搭出来、却不能用 `if`、`for` 和函数写出来。图增添的不是能力，而是*具象化（reification）*：一份对流程的一等表示，你可以检视它、可视化它、静态校验它，并在代码之外去手写它。那确实有用，但它是一个工具层，而非一个基础，且构建智能体并不需要它。

所以这里的底座是纯 Go，而各项保证（持久性、至多一次、可验证的轨迹）来自日志，而非来自图。图仍然作为一个*派生*视图存在：`RenderMermaid` 从实际运行过的内容中把它重建出来。

如果你想要一个可手写的图，那一层已经存在：**`plan`** 包是一个受约束、经类型检查的流程构建器，它编译下沉到这个运行时，并免费继承至多一次和审计轨迹。你把类型化的节点（`Step`、`Tool`、`Model`、`Switch`、扇入的 `Join`、有界的 `LoopBack`）接入一个 `Flow`，或者把同一份拓扑写成声明式配置（`plan.Load`），供更高的一层（例如一个可视化构建器）来发出。它始终是你选择的一层，而非基座：一个图优先的框架无法提供反向的能力，因为对它来说图是基础而非一个选项。

对一个问责型运行时来说，方向同样重要。一份手写的图是一张你信任的示意图；一份派生的图是从日志中重建的，因此它恰恰是运行过的东西。`plan` 层把两者系在一起：`Topology()` 和 `RenderMermaid()` 暴露所声明的形状，而 `Conform()` 以密码学方式核验一次运行是否遵循了它所声明的拓扑，这与系统其余部分是同一种"核验、不信任"的立场。让任何这样的层都无法分叉运行时的规则是：一个新表面可以增添一种手写方式，绝不增添一种执行方式；每一层都下沉到那一个由日志支撑的运行时。见 [docs/guides/flows.md](../../docs/guides/flows.md)。

## 环境运行：持久化的休眠、唤醒与中断

上文的四项保证是底座；这是它们所使能的生命周期。一次环境运行并不待在一个同步的聊天循环里。它休眠，在某个触发下唤醒，为一个人类暂停，而其中每一次转换都是日志上一个持久化、至多一次的步骤，因此运行能在它们之间幸免于崩溃和节点交接。

- **休眠至某个截止时刻。** `Sleep`/`WaitUntil` 暂停一次运行并把它的唤醒时间记入日志，从而暂停能挺过一次重启。在唤醒时刻重新调用会恰好一次地恢复。
- **按时间或事件唤醒。** 一个可插拔的 `Waker`（默认是进程内的 `MemWaker`）重新调用一个到期的运行；触发源是你的（一个进程内循环、一个 cron、一个队列、一个入站 webhook），因此同一底座既驱动计划型、也驱动事件驱动型的智能体。
- **为一个人类持久化地中断。** `Interrupt[T]`/`Resume` 在任意点暂停一次运行以请求一个类型化的决定，并以人类的回答作为一个记入日志的步骤来恢复（见 [Human-in-the-loop（人在回路）](#human-in-the-loop人在回路)）。批准/拒绝是那个布尔的特例。

你提供触发源和监督表面；运行时保证运行在每一次休眠、唤醒、中断、崩溃和交接之间始终正确。可在 `examples/signals`（把一个事件投递进一个等待中的运行）、`examples/interrupt`（人在回路的暂停/恢复）和 `examples/recover`（持久化恢复）中运行。见[信号与环境指南](../../docs/guides/signals.md)。

## 保证 1，在代码里：它不会重复扣款

```go
// A tool that moves money is a write: not ReadOnly, not Idempotent.
charge := agent.Func("charge_card", "Charge the customer", agent.Safety{},
	func(ctx context.Context, in ChargeArgs) (Receipt, error) { /* ... */ })

// If the process crashes after the charge fires but before its result is journaled,
// resume does NOT run it again: it returns *ResumeHalt so you confirm, not double-charge:
_, err := a.Run(ctx, runID, input)
var halt *agent.ResumeHalt
if errors.As(err, &halt) {
	// halt.ToolName == "charge_card": outcome unknown, a human decides, no double side effect.
}
```

## 快速上手

需要 Go 1.27（核心用到了泛型方法）。如果 `go version` 更旧，请升级或设置 `GOTOOLCHAIN=go1.27.0`。

核心包是 `agent`，从 `github.com/bide-ai/bide/agent` 导入（如下面的代码块所示）。

```go
package main

import (
	"context"
	"fmt"
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
	weather := agent.Func("get_weather", "Current weather for a city",
		agent.Safety{ReadOnly: true},
		func(_ context.Context, in WeatherArgs) (Weather, error) {
			return Weather{TempF: 68, Sky: "sunny"}, nil
		})

	// Durable on-disk store: a crash mid-run resumes from here.
	store, _ := sqlite.Open("agent.db")
	defer store.Close()

	a := agent.New(model, store, weather).
		WithSystemPrompt("You are a concise weather assistant.")
	out, _ := a.Run(context.Background(), "run-1", "Weather in SF? Use the tool.")
	for _, p := range out.Parts {
		if t, ok := p.(agent.Text); ok {
			fmt.Println(t.Text)
		}
	}
}
```

运行实地冒烟示例：`OPENROUTER_API_KEY=sk-... go run ./examples/smoke`

`Run` 只返回最终消息。要获取一份运行摘要（token 用量，跨各轮累加、含缓存；模型轮次计数；墙钟时长），请用 `RunResult`（以及 `RunSagaResult`）：

```go
res, err := a.RunResult(ctx, runID, input)
// res.Message, res.Usage, res.Turns, res.Duration, res.RunID
```

## 流式（Streaming）

`Run` 会阻塞并返回最终答案。要观察智能体工作（token 增量、轮次边界、工具开始/结束），请用 `Stream`。它驱动的是**同一个循环**（`Run` 字面上就是 `Stream(...).Final()`），所以持久性、恢复和副作用安全性是完全一致的：

```go
stream := a.Stream(ctx, runID, input)
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
answer, err := stream.Final() // terminal message + error (incl. *PendingApproval / *ResumeHalt)
```

事件：`TurnStarted`、`ModelEvent`（token 流）、`AssistantTurn`、`ToolStarted` / `ToolCompleted`、`ApprovalRequired`、`Finished`。为 UI 而 range `Events()` 然后调用 `Final()`，或者单独调用 `Final()` 以表现得与 `Run` 完全一样（它会替你把事件排空）。

有两件事值得知道，两者都是持久性的后果：
- **token 增量在中间件链之下抵达**（Retry / TokenBudget 仍然看到整条组装好的消息），而且**只在一次全新的模型调用上**出现。
- **恢复时，记入日志的记录（transcript）会被重新发出**，作为 `AssistantTurn{Replayed: true}` + `ToolCompleted`，在实时进度之前，因此一个全新的 UI 能在一次崩溃之后重建整个故事，而一个被重放的轮次不产生 token 增量（它已经被决定了）。

`StreamSaga` 是 `RunSaga` 的流式对应物。

## 类型化输出

`RunTyped[T]` 返回一个类型化的 `T`，而非一条自由格式的消息。它注入一个合成的 `final_answer` 工具，其 JSON schema 由 `T` 派生而来（经由 `schema` 包），并引导模型在其工作完成后调用它一次，从而一个使用工具的智能体可以先做真正的工作、*然后*给出类型化的回答。与提供商无关（构建在原生工具调用之上，而非某个提供商的 JSON 模式）。

```go
type Weather struct {
	City  string `json:"city"`
	TempF int    `json:"temp_f"`
}

w, err := agent.RunTyped[Weather](ctx, a, runID, "weather in SF?")
// w.City == "SF", w.TempF == 68
```

它是一个包函数，而非一个方法（Go 的方法不能添加类型参数）。该值是从*记入日志的*工具调用中解码出来的，所以它是**恢复安全的**：一次运行中途的崩溃会在恢复时从日志中把类型化的答案找回来。如果模型以纯 JSON 文本回复而没有调用该工具，`RunTyped` 会退回到解析那段文本。`T` 意在是一个结构体。

在支持严格结构化输出的 OpenAI 兼容提供商上，`RunTypedNative[T]` 使用提供商原生的 JSON-schema 响应格式而非工具（schema 在提供商侧强制执行，无需工具往返）；Anthropic 会忽略它，所以在那里请用 `RunTyped` 以获得与提供商无关的输出。

## 采样（Sampling）

生成控制项是与提供商无关的，且一次性设定；每个适配器把它们映射到自己的传输格式（并丢弃它做不到的，例如 Anthropic 没有 `seed`）：

```go
a := agent.New(model, store, tools...).
	WithSampling(agent.Temperature(0), agent.MaxTokens(500), agent.TopP(0.9), agent.Seed(42))
```

字段在设计上是可选的：一个未设置的字段使用提供商默认值，因此一个显式的 `Temperature(0)` 有别于"未指定"。请求级的 `MaxTokens` 会覆盖适配器构造时的默认值。

## 提示缓存（Prompt caching）

一个智能体循环每一轮都重新发送一大段恒定的前缀（系统提示 + 工具 schema）。Anthropic 提示缓存把这些重复部分按缓存读取费率计费：

```go
model := anthropic.New(key, anthropic.WithPromptCache())
```

这会在系统块和工具定义上放置 `cache_control` 断点。OpenAI 自动缓存前缀（无需标志）。无论哪种方式，缓存效果都会体现在 `agent.Usage` 中（`CacheReadTokens`，自缓存供给；`CacheWriteTokens`，写入缓存），因此像 `TokenBudget` 和成本核算这样的中间件看到的是真实数字。

## 会话（多轮）

`Run` 是一轮。一个 `Session` 是一场持久化的多轮对话：每一次 `Send` 都是一次完整的智能体运行（工具、恢复、副作用安全），并以迄今为止的记录为种子，因此智能体记得先前的各轮。

```go
s, _ := a.Session(ctx, "user-42")   // reopens + rebuilds the transcript from the store
a1, _ := s.Send(ctx, "what's the capital of France?")
a2, _ := s.Send(ctx, "and its population?")   // sees turn 1 in context
```

记录按会话 id 逐轮记入日志，因此一个重启的进程 `a.Session(ctx, "user-42")` 会把它重建出来并继续。第 N 轮在 `"<id>/tN"` 之下运行（它自己的持久化日志处理该轮*之内*的崩溃恢复）；对话记忆是问答记录：一轮的中间工具调用留在那一轮里，不会泄漏进后面的轮次。如果一轮暂停了（批准 / `Interrupt`），`Send` 会返回那个错误；解决它，然后用相同的输入再次调用 `Send` 以恢复。

## 可审计性（防篡改日志）

那条持久化日志已经记录了一次运行的每一步。`audit` 包用一条哈希链承诺那段历史，从而一次运行的执行是可验证的：

```go
head, _ := audit.Head(ctx, store, runID)     // SHA-256 chain over the journal (persisted order)
sig := audit.Sign(head, priv)                // anchor it: sign / publish out-of-band
```

对一条记录的任何修改 / 插入 / 删除 / 重排都会改变链头。**安全模型：** 这无条件地给出完整性，并在*你把链头带外锚定时*给出防篡改性（一条与攻击者所控制的数据库处于同一数据库中的链，可以被重写并重新哈希）；见包文档。它是合规/企业级的接缝：可证明的至多一次副作用*外加*一份关于智能体究竟做了什么的可验证记录。

对于**选择性披露**，`audit.Root` / `Prove` / `VerifyInclusion` 构建一棵 **RFC 6962**（证书透明度）Merkle 树，因此你能经由一个 O(log n) 的包含性证明证明某条记录是一次已承诺运行的一部分，*而不泄露其他记录*（例如向审计员表明发生过某一笔扣款，却不暴露任何其他客户或提示）。而 `ProveConsistency` / `VerifyConsistency` 证明一个较早的根是一个较晚的根的**仅追加前缀**：历史只被追加，从未被重写或重排（透明度日志的保证）。该实现已对照发布的 RFC 6962 测试向量核验。

`SignTreeHead` 产出 CT 风格的**签名树头（Signed Tree Head）**，即用 Ed25519 签名的 `{Size, Root, Timestamp}`，就是你要发布的那件工件。完整流程：签署一个 STH，稍后用一个包含性证明披露单条记录，审计员对照签名过的根来核验它，并证明两个 STH 之间的仅追加式增长。关于该模型、API 以及端到端的合规流程，见 [docs/guides/audit.md](../../docs/guides/audit.md)。

## RAG 与记忆（自带）

Bide **不附带任何向量存储、嵌入器或记忆后端**：它给你*接缝*，你插入你本就在运行的存储。针对你自己的基础设施实现一个接口：

```go
type Retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]agent.Doc, error)
}
```

然后用两种方式之一把它接上：

```go
// Agentic RAG: the model searches on demand:
a := agent.New(model, store, agent.RetrievalTool(myStore, 5))

// Classic RAG: top-k auto-injected as context on each user turn:
a.Use(agent.WithRetrieval(myStore, 5))
```

对话记忆已经内建（`Session`）；动态上下文经由 `WithSystemPromptFunc`；这条接缝覆盖语义/长期记忆。具体的存储适配器（如果真有需要的话）会是独立的模块，绝不进入核心。见 [docs/guides/rag-memory.md](../../docs/guides/rag-memory.md)。

## 恢复安全，一张表说清

```go
agent.Safety{ReadOnly: true}          // no side effects → always safe to re-run
agent.Safety{Idempotent: true}        // safe to retry (dedupes downstream)
agent.Safety{}                        // a write → HALT on unknown outcome, don't double-fire
agent.Safety{RequiresApproval: true}  // pause for human approval before executing
```

在一个非幂等副作用之前，循环记录一个持久化的*尝试标记*，从而恢复能区分"从未运行"（可安全运行）与"运行过并崩溃了"（停机）：是精确地，而非保守地。

这是**被证明的，而非被断言的。** `dst_test.go` 是一个确定性模拟测试：一个故障注入的存储在*每一个*写入点崩溃（并跨越数百个随机化的多次崩溃调度），而该测试工具断言一个非幂等副作用每一次都**至多触发一次**，且运行总是以完成或停机告终，从不双重触发。

该测试工具是导出的（`chaos/`），并在 `benchmarks/` 中被指向其他 SDK。实测结果：**Bide `maxFired=1`（通过）；trpc-agent-go `maxFired=5`；langchaingo `maxFired=64`（两者均失败）。** trpc 的检查点/恢复确实有效（已验证：恢复一次已完成的运行是一个空操作）；它的双重触发是那个有文档记载的 LangGraph"节点必须幂等"窗口。langchaingo 完全没有持久性，所以重试会把一切重新运行。Bide 的尝试标记把那个窗口彻底关闭。

`WithMaxTurns(n)` 为每次运行的模型轮次设上限，使一个不停调用工具的模型无法永远循环下去：触及它会返回 `ErrMaxTurns`（它是 `errors.Is` `ErrBudget` 的）。

## 人在回路（Human-in-the-loop）

两种风味。**批准/拒绝**：一个标记了 `RequiresApproval` 的工具在运行*之前*暂停；人类的决定是一个布尔：

```go
_, err := a.Run(ctx, runID, input)
var pend *agent.PendingApproval
if errors.As(err, &pend) {
	// ... get a human decision ...
	agent.Approve(ctx, store, runID, pend.ToolUseID, true)
	out, _ := a.Run(ctx, runID, input) // resumes past the pause
}
```

**中断/恢复**：一个工具在*任意点*暂停，并以一个*类型化的*值恢复（推广了那个布尔）。在一个可重试安全的工具内调用 `agent.Interrupt[T]`：

```go
tool := agent.Func("choose_plan", "pick a plan", agent.Safety{ReadOnly: true},
	func(ctx context.Context, in Options) (Plan, error) {
		pick, err := agent.Interrupt[Plan](ctx, "plan", in) // pauses the run; in is shown to the human
		if err != nil {
			return Plan{}, err // *Interrupted propagates out of Run
		}
		return pick, nil // on resume, pick is the human's typed answer
	})

_, err := a.Run(ctx, runID, input)
var intr *agent.Interrupted
if errors.As(err, &intr) {
	// ... show intr.Prompt, get a typed answer ...
	agent.Resume(ctx, store, runID, intr.Key, chosenPlan)
	out, _ := a.Run(ctx, runID, input) // resumes; Interrupt now returns chosenPlan
}
```

两者都是持久化的：那个决定/值是一个记入日志的步骤，所以它挺得过一次崩溃。Interrupt 必须处于一个可重试安全的工具中（`ReadOnly`/`Idempotent`）：恢复时该工具会一直重新运行直到中断被解决，所以 `Interrupt` 调用之前的一切都必须是可安全重复的。

## 错误

失败以哨兵错误（sentinel error）来分类，由 `errors.Is` 匹配，这是标准库的惯用法，没有自定义的错误框架。两层：一个**类别（category）**（粗粒度的类）和一个**条件（condition）**（一个具体的成因），后者包裹其类别，因此匹配可以在你需要的任一层级上工作：

```go
_, err := a.Run(ctx, runID, input)
switch {
case errors.Is(err, agent.ErrModel):       // any provider fault (HTTP status, decode, stream)
	backOffAndRetry()
case errors.Is(err, agent.ErrUnknownTool):  // a specific condition (implies agent.ErrTool)
	fixToolWiring()
case errors.Is(err, agent.ErrStorage):      // durable-store I/O
	alertOps()
}
```

类别：`ErrConfig`、`ErrModel`、`ErrTool`、`ErrStorage`、`ErrProtocol`、`ErrBudget`。条件（每一个都包裹一个类别）：`ErrUnknownTool`、`ErrToolArgs`、`ErrNoRecordedOutput`、`ErrTruncatedToolArgs`、`ErrBudgetExceeded`、`ErrMaxTurns`（后两者都包裹 `ErrBudget`）。该工具包返回的每一个错误（包括来自模型、MCP、存储和治理适配器的）都带有一个类别，所以 `errors.Is` 在整个表面上都是可靠的。

而**控制流信号**比一个类别更丰富，所以它们保持为具体类型，由 `errors.As` 匹配：`*PendingApproval`（需要批准）、`*ResumeHalt`（恢复不安全）、`*SagaAborted`（已回滚）。一个暂停或停机的运行不是一个"失败"类别；检视那个结构体以获取 `RunID` / `ToolUseID` / 补偿细节。取消以通常的 `context.Canceled` / `context.DeadlineExceeded` 浮现。

## 中间件与可观测性

两条独立的 `func(Handler) Handler` 链，位于两个重要的边界上：模型调用（`Use`）和每一次工具调用（`UseTool`）。最先添加 = 最外层。两者都是*可变更且可短路的*：改写送进去的东西、变换出来的东西，或者不调用 `next` 就返回。

```go
var cost middleware.CostMeter
a := agent.New(model, store, tools...).
	Use(
		middleware.Retry(3, middleware.WithBackoff(200*time.Millisecond, 10*time.Second)),
		middleware.TokenBudget(100_000),
		middleware.Cost(&cost, middleware.Rates{InputPer1M: 3, OutputPer1M: 15}),
	).
	UseTool(middleware.ToolLog(log.Printf), middleware.ToolCache(), middleware.ToolRetry(3))

// opt-in OTel gen_ai.* spans; the core has no OTel dependency:
a.Use(trace.Model(tracer, trace.WithSystem("openai"), trace.WithModel("gpt-4o-mini")))
a.UseTool(trace.Tool(tracer)) // execute_tool span per call; nests across the sub-agent boundary
// ... after the run: cost.Total() (USD), cost.Usage()
```

`Retry` 做带抖动的指数退避，并在一个提供商 429 上尊重 `Retry-After`（适配器返回一个类型化的 `*agent.RateLimited`）；`Cost` 把来自 token 用量（含缓存读/写）的 USD 累加进一个你在运行后读取的 `CostMeter`。

因为 `trace.Tool` 在循环内部运行，它的 span 处于交给工具的上下文中，所以当一个工具本身就是一个子智能体时，该子智能体的运行及其自己的 span 会作为子级嵌套。该 trace 自动跨越子智能体边界（这是 ADK / AgenticGoKit / trpc-agent-go 的一个缺口）。

工具中间件在持久化步骤*内部*运行，所以一次短路（一次 `ToolCache` 命中）或一次策略拒绝会像任何工具结果一样被记入日志；恢复会重放它，绝不重新运行中间件或工具。用 `agent.ToolMiddleware` 签名写你自己的：

```go
// Deny a tool by policy: the tool never executes; the model sees the error and reacts.
func RequireTag(tag string) agent.ToolMiddleware {
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, tu agent.ToolUse) (json.RawMessage, error) {
			if !authorized(ctx, tag) {
				return nil, fmt.Errorf("tool %q denied: %w", tu.Name, agent.ErrTool)
			}
			return next(ctx, tu) // mutate tu.Args before, transform the result after
		}
	}
}
```

## 模块

Bide 是一个多模块仓库：一个依赖精简的**核心**（`github.com/bide-ai/bide`，即循环、schema、中间件、模型适配器、`plan` 流程构建器、`audit`、govern；依赖仅有 gsm + `x/sync`），外加每个重型适配器一个模块（`mcp`、`trace`、`store/sqlite`、`store/postgres`、`govern/redislog`、`govern/sqlitelog`、`govern/postgreslog`）。导入一个适配器，你就拉进它的依赖树；只导入核心，你就不会。一个仅用核心的消费者，其外部模块表面是 2，而不是 54。见 [docs/reference/module-structure.md](../../docs/reference/module-structure.md)。

## 架构

由构造即为六边形（hexagonal）：核心定义端口（`Model`、`Durable`、`Tool`、`Middleware`）；适配器在边缘处插入。依赖向内指；核心不导入任何适配器、任何基础设施，由 `architecture_test.go` 守护。

```
agent (root)     durable loop · Message/Part · Tool/Safety · Durable · middleware types · RenderMermaid
plan             optional typed flow builder + declarative config; lowers to the loop (Topology · Conform)
model/anthropic  native Claude (thinking + signatures)
model/openai     any OpenAI-compatible endpoint
model/gemini     native Gemini (generativelanguage / Vertex via WithBaseURL)
schema           reflect Go types → inline JSON Schema + OpenAIStrict
middleware       Retry, TokenBudget
trace            opt-in OTel gen_ai.* spans
store/sqlite     on-disk durable resume (single binary, no cluster)
store/postgres   HA durable resume (any node resumes any run)
govern           Tier-2: federated governed state + quorum for agents that must agree (gsm-backed)
```

## 联邦化治理：可证明地达成一致的智能体（Tier-2）

持久化核心让*一个*智能体的工作免于崩溃。`govern` 层处理另一个难题：**许多独立运行、却必须达成一致的智能体**，跨进程、跨团队或跨组织边界，没有中央协调者、没有单一写者。它给出两种可验证的一致形式，而在两者中要点都是*核验，不信任*：一方从公开工件核验结果，无需信任任何其他人的智能体。

**在共享状态上达成一致（收敛）。** 把共享状态描述为一个注册表（变量 + 不变量 + 事件）；gsm 在*构建时*证明智能体动作的每一种交错都到达同一个有效状态，否则就拒绝构建并向你展示一个反例。运行时是 O(1) 的表查找；状态是事件溯源的、崩溃可恢复的。它从单个共享注册表向上扩展、穿过**联邦（federations）**（跨边界约束：树、带解析器的多源 DAG、单调循环的*网格 mesh*），经由 `Embed` 组合，甚至能替你**合成**补偿（声明规则，得到一个收敛的治理者，或一份证明其不存在的证据）。智能体经由 `FederatedEventTool` 插入，因此一次 LLM 工具调用就成为一个受治理的事件。

**在一个决定上达成一致（法定人数 quorum）。** k-of-n 个具名投票者（每一个是一个模型、提供商或主体）投出一个规范化的决定；每一票都是一个记入日志、至多一次的步骤，记录了谁怎么投的，而 k-of-n 的门是投票计数上的一个 gsm 不变量，因此"k 个达成一致"在每一种可能的计票上都被机器核验。`bide-audit verify-quorum` 从公开工件重新核对计票和每一票，在不信任生产者的情况下复现多数决规则。这个论断是精确的：一个法定人数证明的是*k 个投票者达成了一致*，并降低单模型风险；它并不认证那个决定是正确的（相关的错误不是独立性），且只有规范化的决定才能被法定人数化，自由格式的散文不能。

```go
gov, _ := govern.NewPersistent(ctx, machine, log, "order-42", machine.NewState())
tool := govern.EventTool(gov, "pay", "mark the order paid", "pay", agent.Safety{})
// hand `tool` to the agent: concurrent agents sharing `gov` converge, durably.
```

> 完整指南、能力阶梯，以及可运行的演示（`examples/mesh`、`examples/compose`、`examples/quorum`）见 **[docs/guides/governance.md](../../docs/guides/governance.md)**。

## 指南

初来乍到？从 **[Getting started（入门）](../../docs/getting-started.md)** 开始，用 **[文档索引](../../docs/README.md)** 获取完整地图，并参见 **[Concepts（概念）](../../docs/CONCEPTS.md)** 了解词汇（journal、at-most-once、lease、Waker、gsm、ProofBundle）。精确的持久性保证陈述于 **[docs/GUARANTEE.md](../../docs/GUARANTEE.md)**，其边界见 **[docs/KNOWN-LIMITATIONS.md](../../docs/KNOWN-LIMITATIONS.md)**。

- **[docs/guides/flows.md](../../docs/guides/flows.md)**：`plan` 类型化流程构建器，用于当你想手写拓扑而非纯 Go 的时候。把类型化节点（`Step` / `Tool` / `Model` / `Switch` / `Join` / 有界的 `LoopBack`）接入一个下沉到同一条日志的 `Flow`（继承至多一次和审计），或从声明式配置加载同一个流程（`plan.Load`）。`Topology` / `RenderMermaid` 暴露形状；`Conform` 证明一次运行遵循了它所声明的拓扑。可在 `examples/plan` 中运行。
- **[docs/guides/reliability.md](../../docs/guides/reliability.md)**：可靠性中间件：按尝试计的超时、分类的重试（`Retry` / `Retryable`）、对冲式模型调用（`Hedge`，同时发起一个备份以降低尾延迟并实现提供商故障切换）、限流和成本跟踪，以及它们如何组合。可在 `examples/hedge` 中运行。
- **[docs/guides/durable-steps.md](../../docs/guides/durable-steps.md)**：在同一底座上组合你自己的持久化工作。`Step`（一个具名的持久化操作）、`Parallel` / `Task`（用于并行检查然后决定的流水线的持久化扇入），以及 saga（`RunSaga` / `CompensatedFunc`，逆序补偿）。持久化定时器（`Sleep` / `WaitUntil`）把一次运行暂停到一个墙钟截止时刻，并经由可插拔的 `Waker`（`MemWaker`）恢复它。可在 `examples/parallel` 中运行。
- **[docs/guides/signals.md](../../docs/guides/signals.md)**：把外部事件接收进一次运行。持久化定时器（`Sleep` / `WaitUntil`）和 `Waker`、人在回路（`Interrupt` / `Resume`），以及持久化信号（`Signal` / `Await` / `AwaitFor`，有序通道 `Send` / `Receive` / `Ack`）：传输进来是至少一次，应用出去是恰好一次。可在 `examples/signals`、`examples/interrupt`、`examples/recover` 中运行。
- **[docs/guides/observability.md](../../docs/guides/observability.md)**：一行搞定 OTel gen_ai span（`trace.Instrument`）：invoke_agent / chat / execute_tool 分类法、子智能体 span 嵌套、span 上的 token 到成本（`WithRates`），以及内容捕获的隐私默认值。可在 `examples/observability` 中运行。
- **[docs/guides/audit.md](../../docs/guides/audit.md#proof-carrying-runs)**：带证明的运行。一次运行随附一份可移植的 `RunCertificate`，就整次运行断言行为属性合规性（only-approved-policies、policies-convergence-certified），由现有审计原语组合而成，并可用 `CertifyRun` / `VerifyRun` 或 `bide-audit verify-run` CLI，对照单个签名树头离线核验。可在 `examples/proof-carrying-run` 中运行。
- **[docs/guides/delegation.md](../../docs/guides/delegation.md)**：签名授权与衰减式委派。一个父级铸造一份子智能体只能收窄的能力授权（`Grant` / `SignGrant` / `AttenuatingSubAgent`），`VerifyDelegationChain` 离线核验整条链，而 `EarnedAuthority` 依据一份干净的审计轨迹拓宽一个主体的范围，并在一个异常出现的那一刻将其撤销，始终受父级授权约束。`agent.Identity` 把行动主体绑定进每一片受治理的叶子。可在 `examples/delegation`、`examples/authority`、`examples/earned-authority` 中运行。
- **[docs/guides/security-model.md](../../docs/guides/security-model.md)**：密码学保证及其确切范围：完整性、真实性、防篡改性、不可否认性和选择性披露，以及什么被明确排除在外（机密性：叶子不加密）。在依赖审计轨迹之前请读这个。
- **[docs/guides/governance.md](../../docs/guides/governance.md)**：Tier-2 受治理状态底座（gsm）。当许多独立运行的智能体必须在没有中央协调者的情况下就共享状态达成一致时：把状态描述为一个注册表（变量 + 不变量 + 事件），而 `Build()` 在构建时证明每一种交错都收敛到同一个有效状态，否则交还一个反例。涵盖 saga 与治理的抉择、prevent/repair/halt、联邦和合成。可在 `examples/mesh`、`examples/compose` 中运行。
- **[docs/guides/quorum.md](../../docs/guides/quorum.md)**：受治理的 k-of-n 模型一致。`govern.Quorum` 在 `agent.Parallel` 之上运行若干模型，并仅在 k 个达成一致时才准入一个答案，计票锚定在日志中并可经由 `bide-audit verify-quorum` 离线重新核对。可在 `examples/quorum` 中运行。
- **[docs/guides/models.md](../../docs/guides/models.md)**：三个模型适配器（Anthropic、OpenAI 兼容、Gemini）：构造器选项与默认值、用于任意 OpenAI 兼容或 Vertex 端点的 `WithBaseURL`、按提供商的采样映射、提示缓存和用量核算、类型化错误浮现（`RateLimited` / `APIError`），以及多模态图像输入（`UserParts` / `Image`）。
- **[docs/guides/mcp.md](../../docs/guides/mcp.md)**：Model Context Protocol 集成。作为一个运行时工具源连接到一个 MCP 服务器，发现它的工具，并从注解到 `Safety` 的映射继承副作用安全的恢复。
- **[docs/guides/debugging.md](../../docs/guides/debugging.md)**：确定性重放（`Replay`）、持久化语义事件重建（`ReplayEvents`），以及用于时间旅行调试、回归和评测的 Mermaid 运行图（`RenderMermaid`）。崩溃恢复在一次重启后重新驱动被中断的运行：`Recover` 枚举一个存储的各次运行（`Lister`），跳过已完成的（`IsComplete`），并恢复其余的，把一次持久化暂停当作成功而非失败。
- **[docs/reference/extension-points.md](../../docs/reference/extension-points.md)**：该框架赖以构建的端口与适配器（`Model`、`Durable`、`Tool`、`Compensator`、`Retriever`、`Anchor`、`EventStore`），附一个"实现你自己的存储"的演练。
- **[docs/design/compaction.md](../../docs/design/compaction.md)**：带证明连续性的日志压紧（一份设计说明）：一条无界的日志如何能在不破坏审计脊柱的包含性和一致性证明的前提下被压紧。
- **[docs/guides/messaging.md](../../docs/guides/messaging.md)**：从一个入站消息 webhook（Slack、Telegram、WhatsApp、SMS、Discord）驱动一个智能体，而不在核心里搭载传输代码：那种重投递安全的幂等模式，其中持久化日志让一个被重试的 webhook 重放，而非双重触发一个副作用。可在 `examples/webhook` 中运行。
- **[docs/testing/testing.md](../../docs/testing/testing.md)**：测试了什么以及如何测试，混沌崩溃注入基准（公平的，非稻草人）、差分预言机检查、规模上的收敛与可追溯性（附实测数字及其边界）、RFC 6962 一致性、运行它的命令，以及统计性的 `eval` 包（Wilson 置信区间、轨迹指标、经显著性检验的回归 `Compare`、`RequiredRuns` 功效定量、分层的 `ByTag`），附那条把一个通过率与一个保证区分开来的可证明与统计边界。

> 治理层的文档见 [docs/guides/governance.md](../../docs/guides/governance.md)。

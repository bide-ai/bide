# Recipes

Six complete flows to copy, run and change. Each one runs with no API key (the model is scripted) and
says how to swap in a real one, what `bide view` shows for its run, and what a crash does to it.

| Recipe | Use it when | Shows |
|--------|-------------|-------|
| [Refund with a 2-of-3 approval](refund-approval.md) | money moves only after several people sign off, and only above a threshold | a signed m-of-n gate on one node, a threshold `switch`, offline proof of the approvals |
| [Invoice approval](invoice-approval.md) | a model reads a document, a person confirms, then a system of record changes | a model node, a 1-of-1 gate, a pause that outlives the process |
| [Support ticket triage](ticket-triage.md) | a model decides where work goes, and the work has side effects | a read-only model call feeding a `switch` to side-effect nodes |
| [Update a customer record safely](customer-record.md) | a write must not run twice, even if the process dies mid-write | the attempt marker, the halt on an unknown outcome, `ResolveHalt`, and an idempotent write |
| [Ambient agent](ambient-agent.md) | an agent should sleep, wake, check something and act | a durable `Sleep` that survives a restart |
| [Get notified when a run needs a human](notify-human.md) | a run halts on an unknown outcome or waits for approval, and someone has to be told | the three places bide reports it, a webhook POST (Slack), and a dedupe so a halt found on every recovery pass posts once |

You need Go 1.27 or later. The recipes other than the refund one are single `main.go` programs in a
fresh module; the refund recipe starts from `bide init`, which writes its project for you
(`brew install bide-ai/tap/bide`, or `go install github.com/bide-ai/bide/cmd/bide@latest`). If something
fails on the way, check `go version` first, then the [getting started](../getting-started.md) page.

Every recipe stores its run in a SQLite file next to the program and uses a fixed run ID, so running
a program again resumes the same run instead of starting a new one. To start over, delete the
`.db` file (and any `-wal` and `-shm` files beside it).

Each Go program here is compiled by CI against the current code. The runnable examples they build on
live in [`examples/`](https://github.com/bide-ai/bide/tree/main/examples): `examples/plan` (flows),
`examples/approval` (m-of-n approval end to end), `examples/signals` and `examples/recover`.

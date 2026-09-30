# API listings

<!-- docsnip: api agent -->

```go
// Step runs fn as a named durable step.
func Step[T any](ctx context.Context, d Durable, runID, name string,
	fn func(context.Context) (T, error), opts ...StepOption) (T, error)

func NewMemStore() *MemStore

func (a *Agent) Run(ctx context.Context, runID, input string) (Message, error)

type Durable interface {
	Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error)
	History(ctx context.Context, runID string) ([]Record, error)
}

// A struct may list a subset of its fields.
type Safety struct {
	ReadOnly bool
	// ...
}
```

<!-- docsnip: api github.com/bide-ai/bide/agent -->

```go
func NewMemStore() MemStore // want "api drift: agent.NewMemStore: the doc has func\(\) MemStore, the code has func\(\) \*MemStore"

func (a *Agent) Run(ctx context.Context, runID string) (Message, error) // want "api drift: agent.Agent.Run"

func (a *Agent) Fly() error // want "api drift: agent.Agent.Fly: no such method"

func NoSuchThing() // want "api drift: agent.NoSuchThing: not in the package"

type Durable interface { // want "api drift: agent.Durable"
	History(ctx context.Context, runID string) ([]Record, error)
}

type Safety struct { // want "field ReadOnly is int in the doc, bool in the code; field Wings is not in the code"
	ReadOnly int
	Wings    bool
}

var ErrConfig error

const ErrModel = 1 // want "api drift: agent.ErrModel: the doc declares a const, the code a var"
```

<!-- docsnip: api agent -->

```go
func Step(x int) {} // want "an api block lists declarations; func Step has a body"
```

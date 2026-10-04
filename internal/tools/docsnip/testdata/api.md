# API listings

<!-- docsnip: api agent -->

```go
// Step runs fn as a named durable step.
func (j *Journal) Step[T any](ctx context.Context, runID, name string,
	fn func(context.Context) (T, error), opts ...StepOption) (T, error)

func NewMemStore() *MemStore

func (a *Agent) Run(ctx context.Context, runID string, input Message, opts ...RunOption) (*Result, error)

type Store interface {
	Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error)
	Get(ctx context.Context, runID, name string) (Entry, bool, error)
	Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error]
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

type Store interface { // want "api drift: agent.Store"
	Get(ctx context.Context, runID, name string) (Entry, bool, error)
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

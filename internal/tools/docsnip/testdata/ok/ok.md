# Blocks that compile

A complete program:

```go
package main

import "fmt"

func main() { fmt.Println("hi") }
```

Declarations, with fmt and agent imported automatically:

```go
type Weather struct{ City string }

func describe(w Weather) string { return fmt.Sprint(w.City, agent.ErrConfig) }
```

Statements, with the free identifiers from a setup (unused ones are fine in an excerpt):

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID, input string -->

```go
res, err := a.Run(ctx, runID, agent.UserText(input))
unused := 1
```

A multi-line setup with a type whose fields are separated by semicolons, a func without a body,
and an import:

<!-- docsnip: setup
type Order struct{ ID string; Total int }
func classify(Order) (bool, error)
import mrand "math/rand/v2"
o Order
-->

```go
rush, err := classify(o)
_ = mrand.IntN(o.Total)
```

A statement block that returns, with a returns item:

<!-- docsnip: setup ctx context.Context; store *agent.Journal; returns (string, error) -->

```go
out, err := store.Step(ctx, "run-1", "fetch",
	func(ctx context.Context) (string, error) { ... })
if err != nil {
	return "", err
}
```

Declarations then statements, and the elisions:

<!-- docsnip: setup returns error -->

```go
import "strings"

type Args struct{ N int }

func handle(a Args) (string, error) { ... }

s, err := handle(Args{...})
if err != nil {
	return err
}
...
_ = strings.ToUpper(s)
```

Values listed as a table:

```go
agent.Safety{ReadOnly: true}   // safe to re-run
agent.Safety{Idempotent: true} // safe to retry
```

A block inside a list item keeps its indentation out of the code:

- item

  <!-- docsnip: setup n int -->
  ```go
  total := n * 2
  ```

Other fences are not checked:

```json
{"not": "go"}
```

```go
// Identical to a block in dup/, compiled once.
var identical = agent.NewMemStore()
```

# A document with Go blocks

A statement block with a setup:

<!-- docsnip: setup ctx context.Context; store agent.Durable; tool agent.Tool -->

```go
a := agent.New(agent.NewScriptedModel(agent.TextTurn("hi")), store, tool).WithMaxTurns(4)
out, err := a.Run(ctx, "run-1", "hello")
if err != nil {
	log.Fatal(err)
}
fmt.Println(out.Text())
```

An indented block in a list, with an elision:

1. Build the tool:

   ```go
   var lookup = agent.Func("lookup", "looks up", agent.Safety{ReadOnly: true},
   	func(ctx context.Context, q struct{ Q string }) (string, error) { ... })
   ```

A skipped block is left alone:

<!-- docsnip: skip pseudo-code -->

```go
a.RunSaga(ctx, "r", "x")
```

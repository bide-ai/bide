# Blocks that do not compile

```go
x := undefinedThing // want "undefined: undefinedThing"
```

```go
a := agent.New(1) // want "not enough arguments in call to agent.New"
```

```go
func (x { // want "syntax error"
```

A function without a body outside an api block:

```go
func Lookup(id string) (string, error) // want "func Lookup has no body"
```

A complete program must compile as written, unused variables included:

```go
package main

func main() {
	x := 1 // want "declared and not used: x"
}
```

A setup that does not compile is reported at its directive:

<!-- docsnip: setup a *agent.Nope -->

```go
a.Run(nil, "", "")
```

A package name that is ambiguous is not imported:

```go
n := rand.Int() // want "the package name rand is ambiguous"
```

A statement block that returns needs a returns item:

```go
return nil // want "too many return values"
```

A returns item on declarations is reported at the directive:

<!-- docsnip: setup returns error -->

```go
type T struct{}
```

A package name used as a value is not imported:

```go
w := log // want "undefined: log"
```

# Skipped blocks

<!-- docsnip: skip pseudo-code: the loop, not the API -->

```go
for each event {
	handle(event)
}
```

A skip on a block that compiles is stale:

<!-- docsnip: skip this compiles -->

```go
var fine = 1
```

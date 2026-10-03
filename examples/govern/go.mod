// The governance examples: each directory is one runnable program that imports govern (and so
// gsm), which the core module does not depend on. Build and run one from this directory:
// GOWORK=off go run ./quorum
module github.com/bide-ai/bide/examples/govern

go 1.27.0

require (
	github.com/bide-ai/bide v0.0.0
	github.com/bide-ai/bide/govern v0.0.0
	github.com/blackwell-systems/gsm v0.12.0
)

require (
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/bide-ai/bide => ../../

replace github.com/bide-ai/bide/govern => ../../govern

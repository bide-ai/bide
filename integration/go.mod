// The integration module is test-only. It holds the tests of the core module that need govern
// (and so gsm), which the core module does not depend on. Run it from this directory:
// GOWORK=off go test ./...
module github.com/bide-ai/bide/integration

go 1.27.0

require (
	github.com/bide-ai/bide v0.0.0
	github.com/bide-ai/bide/govern v0.0.0
	github.com/blackwell-systems/gsm v0.14.0
)

require (
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/bide-ai/bide => ../

replace github.com/bide-ai/bide/govern => ../govern

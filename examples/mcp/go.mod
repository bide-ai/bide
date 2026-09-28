// Self-contained example module: the mcp package is its own module with the MCP go-sdk
// dependency, which the root module does not carry, so this example is its own module too.
// Build and run it from this directory: GOWORK=off go run .
module github.com/blackwell-systems/bide/examples/mcp

go 1.27.0
toolchain go1.27.0

require (
	github.com/blackwell-systems/bide v0.0.0
	github.com/blackwell-systems/bide/mcp v0.0.0
	github.com/modelcontextprotocol/go-sdk v1.8.0
)

require (
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/blackwell-systems/bide => ../../

replace github.com/blackwell-systems/bide/mcp => ../../mcp

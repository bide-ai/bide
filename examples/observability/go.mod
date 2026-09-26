// Self-contained example module: the trace package is its own module carrying the
// OpenTelemetry SDK dependency, which the root module does not, so this example is its own
// module too. Build and run it from this directory: GOWORK=off go run .
module github.com/dayna/go-agents/examples/observability

go 1.27.0

require (
	github.com/dayna/go-agents v0.0.0
	github.com/dayna/go-agents/trace v0.0.0
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/sdk v1.46.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/dayna/go-agents => ../../

replace github.com/dayna/go-agents/trace => ../../trace

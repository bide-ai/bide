// Self-contained example module: the recipe for SSO-issued, short-lived approver certificates (step-ca
// and an OIDC identity provider), with the test that runs it. It builds cmd/bide and cmd/bide/bide-hsm
// and drives containers, so, like the other example modules, it is its own module. Run the test from this
// directory with Docker running: GOWORK=off go test ./...
module github.com/bide-ai/bide/examples/sso-certs

go 1.27.0

require (
	github.com/bide-ai/bide v0.0.0
	github.com/bide-ai/bide/store/sqlite v0.0.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.59.0 // indirect
)

replace github.com/bide-ai/bide => ../../

replace github.com/bide-ai/bide/store/sqlite => ../../store/sqlite

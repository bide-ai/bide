// Self-contained example module: the recipe for SSO-issued, short-lived approver certificates (step-ca
// and an OIDC identity provider), with the test that runs it. It builds cmd/bide and cmd/bide/bide-hsm
// and drives containers, so, like the other example modules, it is its own module. Run the test from this
// directory with Docker running: GOWORK=off go test ./...
module github.com/bide-ai/bide/examples/sso-certs

go 1.27.0

require github.com/bide-ai/bide v0.0.0

replace github.com/bide-ai/bide => ../../

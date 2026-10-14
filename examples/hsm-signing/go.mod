// Self-contained example module: it imports signer/pkcs11, which needs cgo and a PKCS#11
// library, so, like the other example modules, it is its own module. Build and run it from this
// directory: GOWORK=off go run .
module github.com/bide-ai/bide/examples/hsm-signing

go 1.27.0

require (
	github.com/bide-ai/bide v0.0.0
	github.com/bide-ai/bide/signer/pkcs11 v0.0.0
)

require github.com/miekg/pkcs11 v1.1.2 // indirect

replace github.com/bide-ai/bide => ../../

replace github.com/bide-ai/bide/signer/pkcs11 => ../../signer/pkcs11

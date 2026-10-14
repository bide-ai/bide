// Test-only module: it runs the awskms signer against the AWS KMS emulator. It is separate so that
// the emulator and its dependencies are not in the module graph of anyone who imports
// signer/awskms. It is never tagged.
module github.com/bide-ai/bide/signer/awskms/emulatortest

go 1.27.0

require (
	github.com/aws/aws-sdk-go-v2/service/kms v1.61.4
	github.com/bide-ai/bide v0.0.0
	github.com/bide-ai/bide/signer/awskms v0.0.0-00010101000000-000000000000
	github.com/blackwell-systems/aws-kms-emulator v0.1.0
)

require (
	github.com/aws/aws-sdk-go-v2 v1.47.3 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.20.9 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.6 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.6 // indirect
	github.com/aws/smithy-go v1.28.5 // indirect
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.1 // indirect
)

replace github.com/bide-ai/bide => ../../../

replace github.com/bide-ai/bide/signer/awskms => ../

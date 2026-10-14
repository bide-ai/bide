// Package emulatortest is a test-only module: it runs the awskms signer, through the AWS SDK, against
// an in-process AWS KMS emulator (github.com/blackwell-systems/aws-kms-emulator), for both Ed25519 and
// ECDSA P-256. It is separate so that the emulator and its dependencies are not in the module graph of
// anyone who imports signer/awskms. It is never tagged.
//
// An emulator is its author's reading of the KMS documentation, not AWS: it shows that the signer
// speaks KMS's wire protocol and handles KMS's response shapes, not that real KMS behaves the same.
package emulatortest

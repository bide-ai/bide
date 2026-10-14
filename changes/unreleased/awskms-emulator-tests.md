Section: Changed

`signer/awskms` has no behaviour change; its tests now also run, through the AWS SDK, against the in-process aws-kms-emulator for both Ed25519 and ECDSA P-256, in the new test-only module `signer/awskms/emulatortest` (never tagged, so the emulator is not in the signer's module graph), in addition to the fake client.

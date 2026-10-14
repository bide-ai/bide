Section: Changed

`awskms.Open` now bounds its self-test signature by the context passed to `Open` as well as by `Config.Timeout`, so cancelling that context stops a slow or stalled self-test instead of waiting out the timeout (`Sign` is unchanged). The signer's tests now also run, through the AWS SDK, against the in-process aws-kms-emulator for both Ed25519 and ECDSA P-256, in the new test-only module `signer/awskms/emulatortest` (never tagged, so the emulator is not in the signer's module graph), in addition to the fake client.

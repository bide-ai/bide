Section: Fixed

Corrected the docs' statement that an approver decision signs the tool call's arguments when stating AWS KMS's 4096-byte Ed25519 limit: it signs the SHA-256 of the arguments plus the run, call, tool and approver names, so a call with large arguments is decided under a KMS Ed25519 key. `bide approve` also no longer builds a default receipt file name over the 255-byte file name limit for very long run, call or approver ids (each part is cut at 56 characters, was 80), and refuses a key file that is a named pipe instead of blocking on it.

Section: Added

`bide keygen -alg ecdsa-p256` creates an ECDSA P-256 approver key (the file holds `ecdsa-p256:` and the 32-byte private scalar as hex, mode 0600), and `bide approve` signs with it, so an approver can use the scheme their HSM or cloud KMS signs; a key file whose scalar is zero or not below the group order is refused without echoing it.

Section: Fixed

Corrects the roadmap and `docs/KNOWN-LIMITATIONS.md`, which still said gates do not use the certificate-bound approver checks: they have since v0.19.0 (v0.20.0 added the CRL refresher), and the roadmap now lists what is left (the RFC 3161 verifier is not called by any gate yet, `bide identity check`, `bide-hsm csr`, keys issued through SSO).

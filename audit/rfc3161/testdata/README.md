# Test vectors

Every vector stamps the fixed data `bide rfc3161 test vector 1` (SHA-256 `0c25335270...`), with
the request that produced the response kept beside it. Verification runs at the token's `genTime`,
so a vector does not expire when its certificates do.

| Directory | Source | Expected outcome |
| --- | --- | --- |
| `openssl/` | OpenSSL `ts` with a test TSA created by `openssl/gen.sh` (EC P-256, EC P-384, RSA 3072; the signing keys are discarded) | verifies |
| `digicert/` | `http://timestamp.digicert.com`, one request, 2026-10-15 (RSA 4096, SHA-256, signingCertificate and signingCertificateV2) | verifies |
| `freetsa/` | `https://freetsa.org/tsr`, one request, 2026-10-15 (ECDSA P-384, SHA-512, signingCertificate only) | refused: `signing-certificate-v1-only` |

The FreeTSA token is a valid RFC 3161 token that OpenSSL accepts; it is outside the verifier's
profile because it identifies the signer by the SHA-1 `signingCertificate` attribute and has no
`signingCertificateV2` (design note, decision 17: SHA-1 is never accepted). It is kept as the
real-world case of a refusal for being outside the profile.

DigiCert and FreeTSA are public, unauthenticated services. Neither publishes terms that restrict
test use or the retention of a response; each vector is one request for a fixed digest and the token
contains nothing but that digest and the TSA's own certificates. `digicert/root.pem` is DigiCert
Trusted Root G4 and `freetsa/root.pem` is the FreeTSA CA certificate (it names its operator's
contact address, which is the operator's, not the project's).

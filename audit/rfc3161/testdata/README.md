# Test vectors

Every vector stamps the fixed data `bide rfc3161 test vector 1` (SHA-256 `0c25335270...`), with
the request that produced the response kept beside it (`request.tsq`, `response.tsr`, and the
`token.tok` taken out of the response). The public-TSA requests all carry the same nonce. Verification
runs at the token's `genTime`, so a vector does not expire when its certificates do.

| Directory | Source (all fetched 2026-10-15, one request each) | Outcome |
| --- | --- | --- |
| `openssl/` | OpenSSL `ts` with a test TSA created by `openssl/gen.sh` (EC P-256, EC P-384, RSA 3072; one with 1.5 s of accuracy); the signing keys are discarded | verifies |
| `digicert/` | `http://timestamp.digicert.com` (RSA 4096, SHA-256) | verifies |
| `globalsign/` | `http://timestamp.globalsign.com/tsa/r6advanced1` (accuracy 1 s) | verifies |
| `quovadis/` | `http://ts.quovadisglobal.com/eu` (signer `tsaeu2025rsa2.digicert.eu`, accuracy 997 ms) | verifies |
| `sslcom/` | `http://ts.ssl.com` (accuracy 1 s) | verifies |
| `microsoft/` | `http://timestamp.acs.microsoft.com` (accuracy 500 ms) | verifies |
| `swisssign/` | `http://tsa.swisssign.net` (qualified TSA, accuracy 500 ms) | verifies |
| `freetsa/` | `https://freetsa.org/tsr` (ECDSA P-384, SHA-512) | refused: `signing-certificate-v1-only` |
| `sectigo/` | `http://timestamp.sectigo.com` | refused: `signing-certificate-v1-only` |
| `entrust/` | `http://timestamp.entrust.net/TSS/RFC3161sha2TS` | refused: `signing-certificate-v1-only` |
| `apple/` | `http://timestamp.apple.com/ts01` | refused: `weak-hash` (SHA-1 as the signer digest algorithm) |

Not usable: Certum answered without a token, and Harica and Actalis were unreachable.

The refused tokens are valid RFC 3161 tokens (OpenSSL verifies the ones that have a root here). They
are outside the verifier's profile because they identify the signer by the SHA-1 `signingCertificate`
attribute alone, with no `signingCertificateV2`, or use SHA-1 as the digest algorithm (design note,
decision 17: SHA-1 is never accepted). **A bank that picks Sectigo, Entrust, FreeTSA or Apple as its
time authority cannot use the top tier with this verifier.** DigiCert, GlobalSign, QuoVadis, SSL.com,
Microsoft and SwissSign tokens verify. The set is one request per TSA on one day: a TSA can change
its token format, and a different policy or key of the same operator can differ.

Trust anchors. `root.pem` is the self-signed root that anchors the token's path: for DigiCert,
GlobalSign, QuoVadis and SSL.com the root found in the macOS system store; for Microsoft the
"Microsoft Identity Verification Root Certificate Authority 2020" downloaded from Microsoft's PKI
repository; for SwissSign the self-signed root that the token itself carries (so that vector tests
the profile, not the choice of anchor). `freetsa/root.pem` is the FreeTSA CA certificate (it names its
operator's contact address, which is the operator's, not the project's).

These are public, unauthenticated services. None publishes terms that restrict test use or the
retention of a response; each vector is one request for a fixed digest, and the token contains
nothing but that digest and the TSA's own certificates.

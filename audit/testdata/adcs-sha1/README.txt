A refusal fixture, captured by the first run of the AD CS two-tier capture: the issuing CA was installed
straight from a PFX, which puts its key in a legacy provider (see csp.txt: Microsoft Strong Cryptographic
Provider, HashAlgorithm CALG_SHA1), so it signed its certificates and CRLs with SHA-1 while the root's own
CRL (rootcrl.pem) is SHA-256. A bank with an older CA configured this way has the same output. The leaf is an
RSA-2048 key (the profile of that run). The tests expect the verifier to refuse the leaf and the CRLs. The
keys of these CAs are throwaway test anchors that exist only in these files' history; never reuse them.

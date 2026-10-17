Captured from a run of step-ca 0.30.2 (image smallstep/step-ca@sha256:a2b17872915c193259b75a5474c398326f41bd199f0842093e52cf4182bc8270) with its OIDC provisioner and step CLI 0.31.0 (`step ca sign`), on 2026-10-17. The recipe that makes them is docs/guides/sso-certificates.md.

- `root.pem`, `intermediate.pem`: the approver root and the issuing CA (name constraint on URI idp.bank.example, policy OID 1.3.6.1.4.1.99999.1.1, pathlen 0). Test keys, generated for the capture.
- `leaf.pem`: the certificate the OIDC provisioner issued (one URI SAN, the approver EKU, the policy OID, a CRL distribution point `http://localhost:9001/1.0/crl`).
- `crl.der`: the CRL step-ca signed after the leaf was revoked with reason keyCompromise (CRL number 4). Its IDP is critical and holds only the distributionPoint fullName `http://localhost:9001/1.0/crl`.
- `crl-empty.der`: the CRL before the revocation (CRL number 3).

The files hold no private key.

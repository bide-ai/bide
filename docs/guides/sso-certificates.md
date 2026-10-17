# Short-lived approver certificates from single sign-on

A certificate-bound approver needs a certificate from the bank's own CA, and that certificate lives for
hours, not years (`MaxLeafLifetime`, 24 hours by default). A request signed by hand cannot be the daily
path. This guide is a worked, tested recipe for the daily path: the approver logs in to the bank's single
sign-on (OIDC), the CA issues a certificate for the key they hold, and `bide approve -cert` uses it. The
reference CA is [smallstep step-ca](https://smallstep.com/docs/step-ca/); the same pattern fits other CAs
([below](#other-cas)).

bide does not build single sign-on, an identity provider or a CA. It states what the certificate must say
(the [certificate policy](certificate-approvers.md), `audit.IdentityPolicy`), and the CA's template, which
the bank owns, is where that is decided after the login. The pieces here are files you copy and edit:
`examples/sso-certs/` holds the CA configuration, the certificate template, the identity provider
configuration the test uses, `scripts/renew.sh` for the approver, and the end-to-end test that runs them.

What this gives and what it does not. The certificate says what the CA's template says after the login the
identity provider vouched for; bide checks the certificate against the policy and records what it relied on.
bide cannot see how the identity provider authenticated the person, whether the key is in hardware, or that
the template is the one you reviewed. Those are the bank's controls. Nothing here is a compliance
certification.

## What runs, and what was tested

`examples/sso-certs` starts three things in containers, pinned by digest: step-ca 0.30.2, the step CLI
0.31.0 (as the approver's tool) and a mock OIDC identity provider (mock-oauth2-server 2.1.10). It builds
`bide` and `bide-hsm` from the tree, then runs the real login. Run it yourself with Docker running:

```
cd examples/sso-certs && BIDE_TEST_STEPCA=1 go test -v ./...
```

The Integration job runs it with `BIDE_REQUIRE_STEPCA=1`, which turns a missing Docker into a failure (the
job fails on any skip). What the test covers is listed in [What the test covers](#what-the-test-covers); what
it does not cover is stated there too, and the claims in this guide stop where the test stops.

## The CA

**A dedicated instance.** Run a step-ca for approver certificates only, with its own intermediate. A CA that
also issues TLS or workload certificates could issue an approver certificate through a template nobody
reviewed. The intermediate carries what a verifier needs to hold it to its purpose:

- `certificatePolicies` naming the bank's approver policy OID, with `IdentityPolicy.RequirePolicyOnCAs`;
- a critical name constraint on the person URI host (`permittedURIDomains`), with `RequireNameConstraints`;
- a path length of 0.

`step certificate create --template` takes `policyIdentifiers`, `nameConstraints` and `basicConstraints`
members, so the intermediate can be made with the step CLI as well as with the test's Go code. The
intermediate's key belongs in an HSM or a KMS: step-ca reads it through its `kms` option (PKCS#11, AWS KMS,
Google Cloud KMS, Azure Key Vault, YubiKey and others). The test uses a key file and says so; do not.

**The OIDC provisioner and the template.** `ca/ca.json.tmpl` is the CA configuration: one OIDC provisioner
per identity provider client, whose `options.x509.templateFile` is `ca/templates/approver-leaf.tpl`. The
template reads the identity provider's verified ID token (`.Token`) and never the request:

| The policy needs | The template does |
|---|---|
| Exactly one SAN, of the policy's kind, in canonical form | One `uri` SAN `https://<host>/person/<id>`, `<id>` from the token's `person_id` claim, refused unless it is 32 lowercase hex digits. step-ca's default template (an email and an `iss#sub` URI) would be two SANs and is not used |
| The approver EKU and nothing else | `unknownExtKeyUsage` with the bank's OID; no `serverAuth`, no `clientAuth` |
| Key usage digital signature | `keyUsage: ["digitalSignature"]` |
| The policy OID, only after the login with MFA | `policyIdentifiers` with the OID, and `fail` unless the token's `amr` claim holds `mfa`. No other provisioner and no other template of this CA asserts it |
| A leaf the CRL can cover | `crlDistributionPoints` with the URL that the CRL's issuing distribution point names (below) |
| A short life | the provisioner claims, below |

The request supplies only the public key. The subject is empty, because bide reads no subject.

**Lifetime.** step-ca sets `notBefore` to the issue time minus its `backdate` (1 minute by default) and
`notAfter` to the issue time plus the duration, so a certificate lives for the duration plus the backdate.
`MaxLeafLifetime` counts that whole span. Set the claims to `MaxLeafLifetime` minus the backdate, or declare
`IdentityPolicy.NotBeforeBackdate`: the recipe's claims are 7h59m for a `max_leaf_lifetime` of 8h. A
default of 24 hours would make a 24 hour 1 minute certificate that the policy refuses.

**No renewal without a login.** `disableRenewal: true` on the claims. step-ca's `step ca renew` renews with
the certificate itself (mutual TLS), which would let a certificate be extended past the SSO session that
justified it; a short-lived approver certificate is replaced, by a new login, not renewed.

## The approver's flow

```
APPROVER=ops POLICY=policy.json PROVISIONER=sso OUT=~/.bide/ops ISSUING_CA=issuing-ca.pem \
  PARAMS=ca-timings.json EXPECT=gate-policy.json CRL_DIR=./crl-archive scripts/renew.sh
```

`scripts/renew.sh` does the four steps and the check, and prints the `bide approve` command to use:

1. **The request.** `bide keygen -out ops.key -csr ops.csr -cn ops` for a key file (a new key each time), or
   `bide-hsm csr -key "$KEY_REF" -policy policy.json -as ops -cn ops -out ops.csr` for a key in an HSM or a
   KMS, signed where the key is held; set `KEY_REF` for the second. `-cn` sets a placeholder subject:
   `step ca sign` refuses a request whose subject is empty, and the template ignores it.
2. **The login and the certificate.** `step ca sign --provisioner sso ops.csr ops.crt`. step opens the
   identity provider's login in the browser and receives the ID token on its own loopback listener; the
   token never appears in a command line. The OIDC provider URL must be `https`.
3. **The chain.** `cat ops.crt issuing-ca.pem > ops-chain.pem`: the leaf first, then the intermediate, not the
   anchor.
4. **The check.** `bide identity check` with the same policy file and timings the gate uses, and `-cert
   ops-chain.pem`: would the gate count this certificate for `ops`, and would the call run. Pass `SKIP_CHECK=1`
   to try the CA before the gate's files exist.

Then `bide approve -db app.db -run ... -call ... -as ops -key ops.key -cert ops-chain.pem -identity-policy
policy.json` (`bide-hsm approve` for a key in an HSM or a KMS). bide refuses a certificate with less than
`-cert-min-left` (60 seconds) left, so run `renew.sh` again, with a new login, when it is near: once a
working day for the 8 hour certificate.

## The bide side

The policy file (the [format](hitl-approval.md#deciding-from-the-terminal)) names the root as an anchor, pins
the intermediate by its public key hash and gives the approver's identifier; its `crl` member lists the
sources of the CRLs of the intermediate and of the root. The test writes one and reads it back through the
gate's own loader, with `max_leaf_lifetime`, `require_name_constraints` and `require_policy_on_cas` set.
Use `bide identity check` over the files before go-live, as in
[the pre-flight](certificate-approvers.md#pre-flight-bide-identity-check).

## Revocation

Revoke a certificate the day the person leaves, or the key is lost. In step-ca the way that fits an approver is
an operator revoking by serial number with a JWK provisioner (`step ca revoke <serial>` asks for the provisioner
and its password on the terminal). Two facts to design around:

- **Any JWK provisioner can revoke any certificate of the CA.** Give the CA a JWK provisioner for operators
  (`operator` in the recipe, with a template that issues only an ordinary client certificate), keep its key and
  password with the CA operators, in a KMS or an HSM, and do not create other JWK provisioners on this CA.
- **The other ways do not fit.** Revoking with the certificate itself over mutual TLS (`step ca revoke --cert
  --key`) needs the private key as a PEM file, which a `bide keygen` key file is not and an HSM or KMS key never
  is. step-ca documents that a certificate from an OIDC provisioner cannot be revoked by its serial number
  through the OIDC provisioner, so the recipe configures no OIDC administrators.

**No token on a command line, if the operator types the password.** `step ca revoke <serial>` (step CLI 0.31.0) mints
its own token: it asks on the terminal which provisioner to use and for its password, and sends the revocation. That is
the route an operator uses. There is no non-interactive route without a token on a command line: `step ca revoke` has
no `--provisioner` or `--provisioner-password-file` flag (it reads them only inside `step ca token`), and `--token`
takes the token itself, not `-` or a file. A script that must revoke therefore puts a token in the process list for its
lifetime (a token lives for minutes and is for one serial number); the test does this on a throwaway CA, and a
deployment that needs unattended revocation should use step-ca's administration interface or its own tooling rather
than a shell script.

**The reason code decides what the revocation undoes.** bide dates a revocation from its CRL entry: a routine
reason (`affiliationChanged`, `superseded`, `cessationOfOperation`, `privilegeWithdrawn`) is effective from the time
of the revocation, so a decision made before it still counts and a decision made after it does not; a compromise
reason (`keyCompromise`, `caCompromise`, `aACompromise`), or none, is effective from the certificate's `notBefore`
unless the entry carries an invalidity date, so every decision made with the certificate stops counting. Use a
routine reason for a person who left and `keyCompromise` for a lost key. The test revokes with
`affiliationChanged` and shows both halves.

step-ca then publishes a new CRL at once (`generateOnRevoke`), and every 2/3 of `cacheDuration` otherwise. The gate
learns of it when its CRL refresher fetches it ([Keeping the CRLs current](certificate-approvers.md#keeping-the-crls-current)):
the time between the revocation and the refresh is a part of the published latency, and the check before a call
runs (`ExecStatusAge`) is what stops a counted approval that the CRL, once fetched, shows was revoked before the
decision.

### What step-ca's CRLs are, and what bide does with them

step-ca's CRL support is a feature its contributors added and Smallstep says it does not maintain. Its
limits, read in the source of step-ca 0.30.2 and seen in the test's capture (`audit/testdata/stepca`):

- **Full CRLs only.** No delta CRLs. A CRL is regenerated at start-up (the CRL number rises with every
  restart), after each revocation if `generateOnRevoke` is on, and at the `renewPeriod`.
- **`thisUpdate` is the time of generation**, with no back-dating; `nextUpdate` is `thisUpdate` plus
  `cacheDuration` (24 hours by default). Declare `ca_period` from `cacheDuration`.
- **An entry is dropped one hour after the certificate expires.** bide needs no entry for an expired
  certificate; an audit of an old decision uses the CRLs archived at the time.
- **It needs a database-backed CA** (the default badger database, or MySQL or PostgreSQL), and is served on
  `/1.0/crl`, on the plain-HTTP `insecureAddress`. A CRL is signed, so plain HTTP is the normal profile.
- **The CRL carries a critical Issuing Distribution Point** naming one URL, set by `crl.idpURL`. bide
  accepts that shape only as follows, and refuses every other IDP (a flag, a reasons field, a relative
  name, an indirect CRL) as before. The CRL counts for a certificate only if one of the certificate's own
  CRL distribution points holds the same URL, byte for byte, because an IDP narrows what the CRL covers
  (RFC 5280 section 5.2.5): a certificate without that distribution point, or with another, gets no status
  from the CRL and the gate waits. This is why the template sets `crlDistributionPoints`, and why
  `crl.idpURL`, the template's URL and the URL in the policy file's `crl` member are one string. The
  refresher refuses a CRL whose IDP does not name the URL it was fetched from.

The root is offline, so its CRL is a static file that the bank's PKI team publishes (empty, until a
CA is revoked) and the policy file's `crl` member names. Status is needed for every certificate below the
anchor, the intermediate's included.

## What the test covers

| Step | Covered |
|---|---|
| The root and the issuing CA (policy OID, pathlen 0, critical name constraint) are accepted by the gate's policy loader | yes |
| Login through the real `step ca sign` OIDC flow, a CSR from `bide keygen -csr -cn`, ECDSA P-256 and Ed25519 | yes |
| The certificate's shape: one URI SAN, the EKU, key usage, the policy OID, the distribution point, the lifetime inside the policy's | yes |
| The gate's chain rule accepts it for `ops` (`bide identity check -cert`) | yes |
| A login without MFA, and one whose person id is malformed, are refused by the template | yes |
| The CA's CRL (with its issuing distribution point) is fetched by the real `bide identity fetch-crl`, and `bide identity check` is OK for the certificate's chain, status at counting and status before execution | yes |
| A gated call is approved with the real `bide approve -cert` and counted by a real `audit.CertApprover`, and the call runs | yes |
| An operator revokes (`affiliationChanged`), the next CRL is fetched, `bide identity check` fails the status, a decision made afterwards with the certificate is not counted (reason: revoked) and its call does not run, while the decision made before still counts | yes |
| `bide approve` refuses a certificate with less validity left than `-cert-min-left` | yes |
| The check immediately before the call runs refusing a counted decision whose certificate the CRL lists with an effective time at or before the decision, a `keyCompromise` revocation | not run end to end: covered by `audit` unit tests (`TestVerifyCertExec_RevokedAfterCount`, `TestVerifyCertExec_RevokedIntermediate`, `TestVerifyCertExec_FreshStatusPasses`, `TestVerifyCertExec_AgeBoundary`) and by the model's `NoExecAfterKnownRevocation` and, through the gate, by `agent.TestPreExecute_RevokedApprovalHoldsAndReasks` |
| A key in an HSM or a KMS (`bide-hsm csr`), a different identity provider, an operator revocation by OIDC login (step-ca does not offer one) | not run |

The images are pinned, so the test says what step-ca 0.30.2 does; another version may stamp something
else, which is what the pre-flight and a capture of your own CA are for.

## Other CAs

The same pattern, with the CA's own vocabulary. None of these is run by bide's tests; each line says what
to look for, and the [CRL fixtures workflow](certificate-approvers.md#before-go-live-capture-your-own-ca)
captures EJBCA and AD CS CRLs.

- **Microsoft AD CS.** A certificate template whose subject alternative name comes from the directory (never
  "supply in the request"), the bank's EKU and issuance policy OID, issued through an enrolment service
  behind the bank's SSO. Windows auto-enrolment holds the key on the machine, so an HSM-held key needs a
  request-based path. Its CRLs, delta CRLs included, are captured by the fixtures workflow.
- **EJBCA.** A certificate profile (EKU, policy OID) and an end-entity profile that fixes the SAN, enrolled
  over REST, EST or ACME behind an OIDC-authenticated registration authority. CRLs captured by the same workflow.
- **HashiCorp Vault PKI.** A role with `allowed_uri_sans`, `ext_key_usage_oids`, `policy_identifiers`,
  `use_csr_sans=false` and a short `max_ttl`, signing the approver's request at `pki/sign/<role>` after an
  OIDC or JWT login. Whether its CRLs carry an issuing distribution point is for you to check.
- **AWS Private CA.** `IssueCertificate` with the request and a template for the custom extensions, behind
  an OIDC-authenticated service; the CRL goes to S3. Check whether the CRL is a partitioned one (it then
  carries an issuing distribution point) before use.

Whatever the CA, capture its real certificates and CRLs and run `bide identity check` over them before a gate
depends on it.

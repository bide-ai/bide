# Key custody: signing keys in an HSM or a cloud key service

bide's guarantees rest on who holds the signing keys (see [Security model](security-model.md#key-custody)).
A key file on the application host is a key the application, and so anyone who compromises it, can use
freely. This guide puts the key in a PKCS#11 token (a hardware security module, a smart card, or SoftHSM2
for tests), in AWS KMS, in Google Cloud KMS or in Azure Key Vault (including Managed HSM), behind the same `audit.Signer` interface every signing entry point already takes.
Nothing on the verifying side changes: an auditor still holds only the public key, and `bide audit` and
`audit.NewVerifier` check the signatures as they check any Ed25519 signature.

This is key custody, not a compliance certification. It keeps the private key out of the application's
memory and disk (whether the token or KMS can ever export it is their policy; see `RequireNonExtractable`); it does not prove how a given token was provisioned, and it does not by itself separate
the duties of the people who can use the key.

## What is supported

| Signer | Module | Keys (`Config.Scheme`) | Signs | Tested against |
|---|---|---|---|---|
| PKCS#11 | `github.com/bide-ai/bide/signer/pkcs11` (cgo) | Ed25519 (`CKK_EC_EDWARDS`, token lists `CKM_EDDSA`) or ECDSA P-256 (`CKK_EC`, `CKM_ECDSA`) | the audit log (tree heads, evidence seals, grants), approver decisions | SoftHSM2 only |
| AWS KMS | `github.com/bide-ai/bide/signer/awskms` (pure Go, AWS SDK) | Ed25519 (`ECC_NIST_EDWARDS25519`) or ECDSA P-256 (`ECC_NIST_P256`), usage `SIGN_VERIFY` | the same | a fake KMS client and an in-process AWS KMS emulator (`signer/awskms/emulatortest`), never AWS |
| Google Cloud KMS | `github.com/bide-ai/bide/signer/gcpkms` (pure Go, Google Cloud SDK) | Ed25519 (`EC_SIGN_ED25519`, software only) or ECDSA P-256 (`EC_SIGN_P256_SHA256`, software or HSM), purpose `ASYMMETRIC_SIGN` | the same | the Cloud KMS emulator (`signer/gcpkms/emulatortest`) and a fake client, never Google Cloud |
| Azure Key Vault, Managed HSM | `github.com/bide-ai/bide/signer/azurekv` (pure Go, Azure SDK) | ECDSA P-256 only (`EC-HSM`, curve `P-256`, `ES256`) | the same | a fake client and the Azure SDK's in-process fake server, never Azure (Microsoft has no emulator) |

All four are separate Go modules, so the core `github.com/bide-ai/bide` module gains no cgo and no cloud SDK.
`Config.Scheme` is `audit.AlgEd25519` (the default) or `audit.AlgECDSAP256` (for the Google Cloud KMS signer the default is `audit.AlgECDSAP256`, below; the Azure signer has no `Scheme` because Key Vault has no Ed25519 and it signs `audit.AlgECDSAP256` only); the signers never substitute
one for the other. A token or key of the wrong kind is refused with `ErrNoEd25519` or `ErrNoP256` and the
error names what was found. Pick Ed25519 where the HSM or KMS has it; pick ECDSA P-256 where it does not
(many HSMs, and cloud key services such as Azure Key Vault, which has no Ed25519 at all, and any Google Cloud KMS key that must be in an HSM). Both are FIPS-approved signature schemes.
ECDSA signatures are randomized, so signing one message twice gives two different valid signatures.

Not supported, and why:

- **ML-DSA and hybrid in a token.** `audit.HybridSigner` holds a concrete `Ed25519Signer` and an
  `MLDSASigner`, so an HSM signer cannot be one half of a hybrid; and AWS KMS and Google Cloud KMS offer no context parameter
  for ML-DSA, which bide's ML-DSA signatures use for domain separation. Post-quantum anchoring stays with
  software keys; hybrid in a token is deferred (the hybrid scheme stays `ed25519+ml-dsa-65`).
- **Ed25519 in Azure.** Key Vault and Managed HSM offer EC keys on P-256, P-256K, P-384 and P-521 and RSA keys, and no Ed25519, so the Azure signer is P-256 only.
- **Hardware attestation**, except for Google Cloud KMS with `VerifyAttestation` (below). The PKCS#11 and AWS
  KMS signers (and the Azure one, beyond the key type and `hsmPlatform` the service reports) do not check that the key was generated in hardware or that it is non-extractable; that is a
  property of how the token or key was provisioned.

## PKCS#11

### Requirements

For Ed25519 the token must list the `CKM_EDDSA` mechanism (PKCS#11 v3.0) and hold the key as
`CKK_EC_EDWARDS` (`EC_EDWARDS` in `pkcs11-tool`). `pkcs11-tool --module LIB --list-mechanisms` shows it as
`EDDSA`. If it does not, `pkcs11.Open` returns an error wrapping `pkcs11.ErrNoEd25519`.

For ECDSA P-256 (`Config.Scheme: audit.AlgECDSAP256`) the token must list `CKM_ECDSA` and hold a `CKK_EC`
key on prime256v1 (`EC:prime256v1`). bide signs SHA-256 of the message, so the signer hashes on the host
and sends the token the 32-byte digest to `CKM_ECDSA`; the token's raw `r||s` is converted to the canonical
low-S form bide verifies. A missing mechanism or a key of another type returns `pkcs11.ErrNoP256`.

The signer uses `CKM_EDDSA` with no parameters, which is pure Ed25519 (RFC 8032), the scheme
`audit.Ed25519Signer` produces. A token that demands `CK_EDDSA_PARAMS` for the pure variant is not
supported.

Building needs cgo and a C toolchain. `CGO_ENABLED=0` builds compile, and `pkcs11.Open` then returns
`pkcs11.ErrNoCgo`.

### Set up a SoftHSM2 token to try it

```sh
brew install softhsm opensc        # or: apt install softhsm2 opensc
mkdir -p tokens
printf 'directories.tokendir = %s/tokens\nobjectstore.backend = file\n' "$PWD" > softhsm2.conf
export SOFTHSM2_CONF=$PWD/softhsm2.conf
LIB=/opt/homebrew/lib/softhsm/libsofthsm2.so       # /usr/lib/softhsm/libsofthsm2.so on Debian

# Leave out --pin and --so-pin: both programs then prompt, so no PIN is on a command line.
softhsm2-util --init-token --free --label bide
pkcs11-tool --module $LIB --token-label bide --login \
    --keypairgen --key-type EC:edwards25519 --label bide-audit --id 01
```

Then run the example, which anchors an agent's journal with the token and verifies the result with the
public key alone:

```sh
cd examples/hsm-signing
echo -n 'your user PIN' > /path/to/pin && chmod 600 /path/to/pin
BIDE_PKCS11_MODULE=$LIB BIDE_PKCS11_TOKEN=bide BIDE_PKCS11_KEY=bide-audit \
  BIDE_HSM_PIN_FILE=/path/to/pin GOWORK=off go run .
```

A hardware HSM differs in the library path, the token label, and how keys are generated (usually the
vendor's tool or a key ceremony); the signer's configuration is the same.

### Use it

<!-- docsnip: setup inner agent.Store; anchor audit.Anchor -->
```go
signer, err := pkcs11.Open(pkcs11.Config{
	Module:     "/usr/lib/softhsm/libsofthsm2.so", // the vendor's PKCS#11 library
	TokenLabel: "bide",                            // or SlotID
	KeyLabel:   "bide-audit",                      // and/or KeyID
	PIN:        pkcs11.PINFromFile("/run/secrets/hsm-pin"),
})
if err != nil {
	log.Fatal(err)
}
defer signer.Close()

store, err := audit.NewAuditedStore(inner, signer, anchor) // every journal write is anchored and signed in the token
```

`Config` selects the token by label or slot (exactly one) and the private key by label, by `CKA_ID`, or by
both; it must match exactly one key, or `Open` fails with `pkcs11.ErrAmbiguous` or `pkcs11.ErrKeyNotFound`.
The public key is read from the token (the private key object, or the public key object with the same ID).
For a token that does not expose it, set `Config.PublicKey`; when both exist they must be equal.

Behavior to rely on:

- **The PIN** (also in `%q`, JSON and escaped forms a library may print) comes from `PINFromEnv(name)` or `PINFromFile(path)`, never from a command-line argument or a
  string field. It is read each time a session opens and is not kept by the signer. It does not appear in
  an error message, in the error chain (`errors.Unwrap`, `%#v`; the chain keeps only this package's sentinels such as `ErrKeyNotFound`), or in `%v`/`%+v` of the signer. The PKCS#11 return code survives as `pkcs11.CKRError{Code}` (message-free), so a caller can branch on `errors.Is(err, pkcs11.CKRError{Code: pkcs11.CKRPinLocked})`, and on `CKRPinIncorrect`, `CKRTokenNotPresent`, `CKRKeyHandleInvalid`, `CKRSessionClosed`, `CKRUserNotLoggedIn` and the rest of the exported codes. A nil `Config.PIN` skips the login, for a
  token that is already logged in. A token with a PIN pad (`CKF_PROTECTED_AUTHENTICATION_PATH`) is logged in with `Config.ProtectedAuthPath`, which calls `C_Login` with no PIN so the person types it on the device; it is exclusive with `Config.PIN`, and `Open` fails with `ErrNoProtectedAuthPath` if the token has no such path. A key with `CKA_ALWAYS_AUTHENTICATE` is authenticated for each signature with a context-specific login (`CKU_CONTEXT_SPECIFIC`), using the same PIN source (or protected path); `Open` does not sign its self-test with it. A PIN shorter than `MinPINLength` (4) is refused.
- **Open proves the key pair.** It signs one fixed message and checks it against the public key, at open and at every reopen, so a private key that is not the pair of the exposed public key fails there and not at the first anchor. A key of type `CKK_EC_EDWARDS` that is Ed448 returns `ErrNoEd25519`.
- **Non-extractable keys.** The key is not readable by the application through this signer, but whether the token lets it be exported is the token's policy. Set `Config.RequireNonExtractable` to make `Open` fail with `pkcs11.ErrExtractable` unless the token reports the key as `CKA_EXTRACTABLE=false` and `CKA_SENSITIVE=true`, and, where the token reports them, `CKA_NEVER_EXTRACTABLE` and `CKA_ALWAYS_SENSITIVE` as true (a key that was extractable once may have been copied before it was marked otherwise). That checks what the token says; it is not an attestation.
- **Every signature is checked** against the token's public key before `Sign` returns it, so a faulty token
  or a wrong key is an error at signing time, not a head that fails to verify later.
- **A lost session** (the token was reset, removed and returned, or the login lapsed) is reopened once and
  the signature retried. The reopened session must hold a key with the same public key, or `Sign` fails.
- **Concurrency.** Signing calls are serialised, since a PKCS#11 session runs one signing operation at a
  time. Several signers over one library share the loaded module, and the module is released when the last
  one closes.
- **Failure is an error, not a fallback.** A token that is unreachable makes `AuditedStore` writes fail; it
  never signs with another key.

### Tests

`signer/pkcs11/pkcs11test` creates a SoftHSM2 token for a test binary (`pkcs11test.Run` in `TestMain`,
`pkcs11test.Require(t)` in a test, which skips when SoftHSM2 is not installed). Setting
`BIDE_REQUIRE_SOFTHSM=1` turns that skip into a failure; bide's CI sets it. The tests check the token's
signatures with `audit.NewVerifier`, anchor a real run through `AuditedStore`, and verify an evidence
package sealed by the token.

## AWS KMS

### Create the key

```sh
aws kms create-key --key-spec ECC_NIST_EDWARDS25519 --key-usage SIGN_VERIFY \
    --description 'bide audit log signing key'
# For ECDSA P-256 instead: --key-spec ECC_NIST_P256, and Scheme: audit.AlgECDSAP256 below.
aws kms create-alias --alias-name alias/bide-audit --target-key-id <key id>
```

Give the application's role `kms:GetPublicKey` and `kms:Sign` on that key and nothing else; key
administrators should not be able to call `kms:Sign`, which is the separation that matters.

### Use it

<!-- docsnip: setup ctx context.Context; kmsClient awskms.API; inner agent.Store; anchor audit.Anchor -->
```go
signer, err := awskms.Open(ctx, awskms.Config{
	Client: kmsClient, // *kms.Client from kms.NewFromConfig(awsConfig)
	KeyID:  "alias/bide-audit",
})
if err != nil {
	log.Fatal(err)
}

store, err := audit.NewAuditedStore(inner, signer, anchor)
```

With `Scheme: audit.AlgECDSAP256` the key is `ECC_NIST_P256` and KMS signs `ECDSA_SHA_256` over the SHA-256 digest of the message (`MessageType` `DIGEST`).

`Open` resolves the key once and then signs with its ARN, so an alias moved to another key later cannot
change which key signs. It refuses any key that is not of the scheme's spec with usage `SIGN_VERIFY` that offers the scheme's
signing algorithm (`awskms.ErrNoEd25519`, `awskms.ErrNoP256`). For Ed25519 that is `ECC_NIST_EDWARDS25519` and `ED25519_SHA_512`. KMS signs with `ED25519_SHA_512` over the raw
message (`MessageType` `RAW`), which is pure Ed25519 as RFC 8032 defines it. Like the PKCS#11 signer, it
signs once at `Open` (one billed request; it also catches a missing `kms:Sign` permission) and verifies each signature against the public key before returning it. Each KMS call has a timeout
(`Config.Timeout`, default 30 seconds). `Config.GrantTokens` (optional) are passed to `GetPublicKey` and
`Sign`, for a signer whose permission on the key comes from a KMS grant, so that it applies before the grant has propagated.

The AWS signer is tested two ways in bide's CI, and never against AWS. A fake KMS client holds an Ed25519
or P-256 key and enforces the call shape KMS documents (key ARN, algorithm, `RAW` or `DIGEST`, the size
limit); it also produces failures an emulator cannot (a key without the expected signing algorithm or
usage, a replaced key, a bad DER signature). For both schemes the signer is also tested, through the AWS
SDK, against [aws-kms-emulator](https://github.com/blackwell-systems/aws-kms-emulator), run in-process by
the test-only module `signer/awskms/emulatortest`: `Open` and its self-test, signing (KMS's DER P-256
signatures converted to bide's low-S `r||s`, many of them), verification with `audit.NewVerifier`, a full
`AuditedStore` anchor and evidence seal, the refusal of keys of another spec, and disabled and
pending-deletion keys. An emulator is its author's reading of the KMS documentation, so the
interoperability of real KMS's `ED25519_SHA_512` and `ECDSA_SHA_256` output with bide's verifier still rests
on AWS's documentation of those algorithms; check it once against your own key before relying on it (sign a
message, verify it with `audit.VerifierOf`).

### The KMS 4096-byte limit

Under Ed25519 KMS signs at most 4096 bytes, and Ed25519 signs the whole message. Tree heads, grants and
approver decisions are small but carry caller-chosen text: a tree head is about 110 bytes plus the run ID's
length (a run ID of about 4000 bytes or more does not fit), and an approver decision carries the run,
call, tool and approver names and the SHA-256 of the arguments (not the arguments, so a call with large
arguments is fine). `Sign` then returns `awskms.ErrMessageTooLarge` naming the size, without calling KMS. An
evidence package seal signs a fixed 60-byte message (evidence format v6) and always fits. Under ECDSA
P-256, KMS signs a 32-byte digest, so there is no limit.

## Google Cloud KMS

### Create the key

```sh
# ECDSA P-256 in an HSM (the choice when the key must be hardware-protected):
gcloud kms keyrings create bide --location europe-west2
gcloud kms keys create audit --keyring bide --location europe-west2 \
    --purpose asymmetric-signing --default-algorithm ec-sign-p256-sha256 --protection-level hsm
# Ed25519 (Google offers it in software only; the signer then needs AllowSoftwareKey: true):
#   --default-algorithm ec-sign-ed25519 --protection-level software
gcloud kms keys versions list --key audit --keyring bide --location europe-west2
```

bide pins a key **version**, not a key: the version name ends in the numeric version ID, and there is no
"latest" alias, so rotating the key's primary version cannot change which key signs. The application
needs three permissions on that version and nothing else: `cloudkms.cryptoKeyVersions.get` (the signer
checks that the version is enabled and reads its algorithm and protection level),
`cloudkms.cryptoKeyVersions.viewPublicKey` and `cloudkms.cryptoKeyVersions.useToSign`. Neither
predefined `roles/cloudkms.signer` (which has `useToSign`, not `viewPublicKey`) nor `roles/cloudkms.signerVerifier` (which has `useToSign` and `viewPublicKey`) includes the first, and
`roles/cloudkms.viewer` grants far more than needed, so create a custom role with those three
permissions (role contents checked against [Cloud KMS permissions and roles](https://docs.cloud.google.com/kms/docs/reference/permissions-and-roles), 2026-10-14).
Key administrators should not hold `useToSign`, which is the separation that matters.

The custom role, ready to copy (replace the project, and bind the role on the key version or its key, not on the project):

```sh
gcloud iam roles create bideAuditSigner --project my-project \
    --title "bide audit signer" \
    --permissions cloudkms.cryptoKeyVersions.get,cloudkms.cryptoKeyVersions.viewPublicKey,cloudkms.cryptoKeyVersions.useToSign
gcloud kms keys add-iam-policy-binding audit --keyring bide --location europe-west2 \
    --member serviceAccount:bide@my-project.iam.gserviceaccount.com \
    --role projects/my-project/roles/bideAuditSigner
```

or the same role as YAML for `gcloud iam roles create --file`:

```yaml
title: bide audit signer
description: Sign with one Cloud KMS key version for bide's audit log. No key administration.
stage: GA
includedPermissions:
  - cloudkms.cryptoKeyVersions.get
  - cloudkms.cryptoKeyVersions.viewPublicKey
  - cloudkms.cryptoKeyVersions.useToSign
```

### Use it

<!-- docsnip: setup ctx context.Context; kmsClient gcpkms.API; inner agent.Store; anchor audit.Anchor -->
```go
signer, err := gcpkms.Open(ctx, gcpkms.Config{
	Client:     kmsClient, // *kms.KeyManagementClient from kms.NewKeyManagementClient(ctx)
	KeyVersion: "projects/bank/locations/europe-west2/keyRings/bide/cryptoKeys/audit/cryptoKeyVersions/1",
	// Scheme defaults to audit.AlgECDSAP256: an EC_SIGN_P256_SHA256 key version in an HSM. The zero
	// Config requires an HSM key version.
})
if err != nil {
	log.Fatal(err)
}

store, err := audit.NewAuditedStore(inner, signer, anchor)
```

The client authenticates with Application Default Credentials (a workload identity on GKE or Cloud Run,
or `gcloud auth application-default login` for a trial); the signer never sees a credential.

What `Open` checks, in order, each failure a named error:

- **The name** is a full `CryptoKeyVersion` resource name with a numeric version (`agent.ErrConfig`
  otherwise), and every response (version, public key, signature) names that same version, or the call
  fails with `gcpkms.ErrIntegrity`.
- **The algorithm** is `EC_SIGN_ED25519` for the Ed25519 scheme (`gcpkms.ErrNoEd25519` otherwise) and
  `EC_SIGN_P256_SHA256` for `audit.AlgECDSAP256` (`gcpkms.ErrNoP256`). Those algorithms exist only in a key of
  purpose `ASYMMETRIC_SIGN`, so this is also the purpose check. The public key's reported algorithm must agree.
- **The state** is `ENABLED` (`gcpkms.ErrVersionState` otherwise). A version disabled or destroyed after
  `Open` fails at `Sign` with a `gcpkms.CodeError` (`FailedPrecondition`).
- **The protection level** is `HSM` or `HSM_SINGLE_TENANT` unless you opt out (`gcpkms.ErrProtectionLevel`
  otherwise): `Config.AllowSoftwareKey` also accepts `SOFTWARE`, and `Config.AllowExternalKey` also accepts
  `EXTERNAL` and `EXTERNAL_VPC` (Cloud EKM). The level is pinned at `Open`, and a signature response that
  reports another level is `ErrIntegrity`.
- **A self-test signature** (one billed operation) proves the `useToSign` permission and that Cloud KMS's
  output verifies under the public key it reported.

**Protection level.** The zero `Config` is the safe one: only an HSM-protected key version opens. Google offers
`EC_SIGN_ED25519` at protection level `SOFTWARE` only ([algorithms](https://docs.cloud.google.com/kms/docs/algorithms)),
so Cloud KMS has no HSM-backed Ed25519 key. The default `Scheme` is therefore `audit.AlgECDSAP256`, so a default
`Open` works on an HSM key. Ed25519 needs both `Scheme: audit.AlgEd25519` and `AllowSoftwareKey: true`; an Ed25519
key under the default scheme fails with `gcpkms.ErrNoP256` and a message naming both fields, and the Ed25519 scheme
without `AllowSoftwareKey` fails with `gcpkms.ErrProtectionLevel` and a message that says Cloud KMS has no
HSM-backed Ed25519 key and points to P-256. `AllowSoftwareKey` widens what `Open` accepts, not what it trusts: the
level found is still pinned and checked on every signature. A
deployment that must keep the signing key in an HSM, a bank for example, uses the default scheme with an
`EC_SIGN_P256_SHA256` key version at protection level `HSM` and leaves both options off. The level is what Cloud KMS
reports; `Config.VerifyAttestation` (below) checks Cloud KMS's signed HSM attestation, and bide does not look inside an
external key manager when `AllowExternalKey` is set.

**HSM attestation (`Config.VerifyAttestation`, off by default).** Cloud HSM attaches to each `HSM` key version a
statement signed by the HSM hardware (Marvell, formerly Cavium) and countersigned through Google's certificates
([Verify attestations](https://docs.cloud.google.com/kms/docs/attest-key)). With `VerifyAttestation: true`, `Open`
fails with `gcpkms.ErrAttestation` unless that attestation proves the exact key version is an HSM-resident,
non-extractable key whose public key is the one `GetPublicKey` returned. It reads the attestation from
`GetCryptoKeyVersion` (no download, no extra permission), and checks, following Google's reference verifier
([verify_attestation_chains.py](https://github.com/GoogleCloudPlatform/python-docs-samples/blob/main/kms/attestations/verify_attestation_chains.py)):

1. **The chains.** The certificates in the attestation must build the manufacturer chain (manufacturer root, card,
   partition) and the owner chain (Google root, then a card and a partition certificate with the same public keys as
   the manufacturer's), and nothing else may be in them.
2. **The signature.** The attestation (RSA-2048, PKCS#1 v1.5, SHA-256, the last 256 bytes) must verify under both
   partition certificates.
3. **The attested key.** One attested EC object must have the key version's public point, must not be extractable
   (`0x0162` false), must have been generated on the HSM and not imported (`0x0163` true), and its key ID (`0x0102`) must
   carry, as its second SHA-256 digest, the digest of the key version's resource name. Properties are never combined
   across objects, and the object must be a private key (class `0x0000` = 3). Formats `CAVIUM_V1_COMPRESSED` and `CAVIUM_V2_COMPRESSED` are read (the same object layout; version 2 has a five-word response header); any other format is refused. The attested object is the key's private-key object, which carries the public point.

The trust anchors are Google's owner root ("Hawksbill Root v1 prod", SHA-256
`46b5fd35...df0de095`, from Google's sample code and `gstatic.com/cloudhsm/roots`) and Marvell's manufacturer root
("localca.liquidsecurity.cavium.com", SHA-256 `975757f0...bd69cc28`, from Marvell's
`liquid_security_certificate.zip`). They are built in, with their fingerprints pinned in the code and a test, so a
default `Open` works with no setup and a change to either is deliberate. A deployment that has obtained the roots by
its own checked route sets `Config.AttestationRoots` (both certificates; it replaces the pair) and does not depend on
the copies in bide. **Validity periods are not enforced**, as in Google's verifier: the statement records a past fact
(the key was created in that HSM), the HSM certificates are long lived, and Marvell's root already expired on
2025-11-16 (Google's expires 2030-01-01) without the statement becoming false; there is no revocation. Without validity dates or revocation, a leaked partition key from a decommissioned HSM would still verify, so a verifier that needs time bounds or revocation must supply its own roots (`Config.AttestationRoots`) and check dates itself. What
attestation does not prove: that the HSM firmware or Google's operation of it is sound, or that the application's
`useToSign` access is limited (that is IAM). `VerifyAttestation` needs an `HSM` key version, so it never applies to
Ed25519 or software keys, and `Open` with it on fails for them. `examples/hsm-signing` turns it on with `BIDE_GCPKMS_VERIFY_ATTESTATION=1`.

**What is signed.** Ed25519 sends the message in the request's `data` field and Cloud KMS signs it in PureEdDSA mode
(RFC 8032), which is what `audit.Ed25519Signer` produces. ECDSA P-256 sends the SHA-256 digest in the `digest`
field; Cloud KMS returns an ASN.1 DER signature, which the signer converts to the canonical low-S `r||s`. Google
documents no size limit for the Ed25519 `data` field (its [signing guide](https://docs.cloud.google.com/kms/docs/create-validate-signatures)
states one only for raw RSA algorithms, and the [quotas page](https://docs.cloud.google.com/kms/quotas) none),
so the signer enforces none; a message Cloud KMS rejects
comes back as a `CodeError` with code `InvalidArgument`. Cloud KMS cannot sign an empty message under
Ed25519, which `Sign` refuses with `gcpkms.ErrEmptyMessage` (audit never signs an empty message). Under P-256
Cloud KMS sees a 32-byte digest, so there is no limit, and an evidence package of any size seals.

**Integrity checks.** The signer follows Google's
[data integrity guide](https://docs.cloud.google.com/kms/docs/data-integrity-guidelines) for both calls it
makes on the data path. `AsymmetricSign` carries `data_crc32c` or `digest_crc32c`; the response must name the
version, report that it verified the request checksum (`verified_data_crc32c` or `verified_digest_crc32c`), and
carry a `signature_crc32c` that matches the signature. A signature whose checksum does not match is retried, up
to three attempts in all, as the guide advises; every other integrity failure is an immediate
`gcpkms.ErrIntegrity`. `GetPublicKey` is requested as PEM, and its response must name the version and carry a
`pem_crc32c` that matches the PEM. These checks detect corruption in transit; authenticity rests on the check the
signer always makes, that every signature verifies under the public key read at `Open`, so a key replaced behind
the version name is an error at `Sign`. A request Cloud KMS rejects for a checksum mismatch is an
`InvalidArgument` `CodeError`, and is not retried.

**Errors.** A failure from the Cloud KMS client reaches the caller as a `gcpkms.CodeError` that carries the gRPC
status code and nothing else, because the text of such an error can include request detail or the output of a
failed credential exchange. Branch on it with `errors.Is(err, gcpkms.CodeError{Code: codes.PermissionDenied})`; a
cancelled or expired context also matches `context.Canceled` or `context.DeadlineExceeded`. Neither the error
chain, `%v`, `%+v`, `%#v` nor the signer's `String` carries a credential. `Config.Timeout` (default 30 seconds) bounds `Open` as a whole and each `Sign` as a whole, retries included.

**Tests.** The signer's own tests use a fake client. The module `signer/gcpkms/emulatortest` (test-only, never
tagged, so the emulator is not in the module graph of anyone who imports `signer/gcpkms`) runs the signer through
the real Cloud KMS Go client against [gcp-kms-emulator](https://github.com/blackwell-systems/gcp-kms-emulator)
v0.10.0, started in the test process on an in-memory gRPC listener, for both algorithms: no Docker, network or
credentials. The emulator reports protection level `SOFTWARE` for every key, so those tests set `AllowSoftwareKey`; the
`HSM` success path, the response-integrity failures and the other failures the emulator cannot produce are covered by
the fake client. bide's tests never call Google Cloud, so the interoperability of Cloud KMS's signatures with bide's
verifier rests on Google's documentation; check it once against your own key version before relying on it (sign a
message, verify it with `audit.VerifierOf`).

## Azure Key Vault and Managed HSM

Azure has no Ed25519, so this signer is ECDSA P-256 only (`Alg()` is `ecdsa-p256`). Microsoft documents the
key types and algorithms in [Key types, algorithms, and operations](https://learn.microsoft.com/azure/key-vault/keys/about-keys-details)
(Managed HSM: [the same page for Managed HSM](https://learn.microsoft.com/azure/key-vault/managed-hsm/about-keys-details)).

### Create the key

A key is HSM-protected when it is an `EC-HSM` key: in a vault of the Premium tier, or in a Managed HSM (every
Managed HSM key is HSM-protected). A Standard-tier vault can hold only software `EC` and `RSA` keys; HSM-protected key
types need the Premium SKU ([About keys](https://learn.microsoft.com/azure/key-vault/keys/about-keys): "HSM key types in
Premium SKU", "Software-protected keys (Key Vault Standard and Premium)").

```sh
az keyvault key create --vault-name myvault --name bide-audit \
    --kty EC-HSM --curve P-256 --ops sign verify
# Managed HSM: az keyvault key create --hsm-name myhsm --name bide-audit --kty EC-HSM --curve P-256 --ops sign verify
az keyvault key show --vault-name myvault --name bide-audit --query key.kid
```

The `kid` it prints is the key identifier **with its version**, the value for `Config.KeyID`. Use that value
and not the version-less one: the version-less identifier means "the latest version", so a rotation or a new
version created by someone else would silently change which key signs. `Open` refuses an identifier without a
version. Restricting `--ops` to `sign verify` is good practice; the signer needs only `sign` in `key_ops`.

Give the application's identity the **Key Vault Crypto User** role (Managed HSM: **Managed HSM Crypto User**),
scoped to this one key, or on a legacy access policy the `get` and `sign` key permissions. Key administrators
(Crypto Officer) should not hold `sign`.

### Use it

<!-- docsnip: setup ctx context.Context; inner agent.Store; anchor audit.Anchor -->
```go
signer, err := azurekv.Open(ctx, azurekv.Config{
	KeyID: "https://myvault.vault.azure.net/keys/bide-audit/0123456789abcdef0123456789abcdef",
})
if err != nil {
	log.Fatal(err)
}

store, err := audit.NewAuditedStore(inner, signer, anchor)
```

Credentials come from `azidentity` and never from a flag or a field holding a secret: by default
`azidentity.NewDefaultAzureCredential` (workload identity, managed identity, the Azure CLI, the `AZURE_CLIENT_*`
environment); set `Config.Credential` to pin a specific `azidentity` credential, for example a managed identity
credential for one identity.

The vault or Managed HSM host in `KeyID` must end in one of the Azure public cloud, Azure China or Azure US
Government Key Vault or Managed HSM suffixes, and `Open` refuses any other host. `Config.HostSuffixes` replaces
that list with your own DNS suffixes, for a cloud outside it such as Azure Stack Hub.

What `Open` checks, and refuses with a typed error:

- **Key type and curve.** `EC-HSM`, or `EC` only with `Config.AllowSoftwareKey`; curve `P-256`; a public point on
  the curve (`azurekv.ErrNoP256`).
- **HSM protection.** A software key is refused unless `AllowSoftwareKey` is set (`azurekv.ErrNotHSM`). The default is
  strict because the reason to use a key service for an audit log is custody the application cannot override; a
  software key is held in a software cryptographic module of the service (FIPS 140-2 Level 1), and a bank's
  policy decides whether that is acceptable. A key whose `hsmPlatform` is `0` is treated as a software key: Microsoft documents
  `0` as "protected by a FIPS 140-2 Level 1 software cryptographic module" (`1` is FIPS 140-2 Level 2 HSM
  hardware, `2` is FIPS 140-3 Level 3; [Key attributes](https://learn.microsoft.com/azure/key-vault/keys/about-keys-details#key-attributes))
  and lists "software-protected (HSM Platform 0) keys" under FIPS 140-2 Level 1 in its
  [compliance table](https://learn.microsoft.com/azure/key-vault/keys/about-keys#compliance). The check is
  defence in depth on top of the `EC-HSM` key type.
- **Usable now.** Enabled, now not before `nbf` and before `exp`, and `key_ops` includes `sign`
  (`azurekv.ErrKeyUnusable`). The window is checked again on every `Sign`, without calling Azure, so a key that
  expires while the process runs fails with a clear error and not with a 403.
- **Not exportable.** A key marked exportable, or with a release policy, can leave the service under that policy, so
  its private key is not confined to it (`azurekv.ErrExportable`).
- **The pinned key answered.** The identifier in the service's answer (and in each signature response) must name
  the pinned vault, key and version (`azurekv.ErrKeyMismatch`).

`Sign` hashes the message with SHA-256, sends the 32-byte digest with `ES256` (Key Vault signs a hash and
does not hash for you), converts the 64-byte `r||s` answer with `audit.ECDSAP256SignatureFromRaw` (which folds
a high `s` to the low `s` bide verifies), and checks the signature against the public key before returning
it. Like the other signers it signs once at `Open` (one billed operation; it catches a missing `sign`
permission). Each call has a timeout (`Config.Timeout`, default 30 seconds).

**Errors carry no response content.** The Azure SDK's errors print the HTTP response, and a credential error can
print a token endpoint's reply. The signer returns `*azurekv.Error`, holding only the operation, the HTTP status
and Azure's error code (`Forbidden`, `KeyDisabled`, ...), and a timeout still satisfies
`errors.Is(err, context.DeadlineExceeded)`. The signer's `String` and `%+v` show the key identifier and the
public key's ID only.

The Azure signer is tested against a fake client and against the Azure SDK's in-process fake server (so the
real client's wire types are exercised); bide's tests never call Azure, and Microsoft publishes no Key Vault
emulator. Microsoft's documentation points at RFC 7518 for `ES256` (64-byte `r||s`) but does not restate the
encoding, so check the signer once against your own key (sign a message, verify it with `audit.VerifierOf`);
a service answer in any other encoding makes `Sign` fail rather than return a bad signature.

## Approver keys

An approver decision is a signature over `agent.ApprovalDecisionBytes(subject, approverID, approved,
decidedAt)`. The signer is any `audit.Signer`, so an approver's key can live in a token too:

<!-- docsnip: setup pend *agent.ApprovalPending; approver audit.Signer -->
```go
at := time.Now()
sig, err := approver.Sign(agent.ApprovalDecisionBytes(pend.Subject(), "finance", true, at))
```

The verifier the gate uses is `audit.VerifierOf(approver)`, built from the public key alone, as for any
approver (see [Human approval](hitl-approval.md)).

### Deciding from the terminal: `bide-hsm`

`bide approve` loads the approver's key from a key file, and the `bide` binary is pure Go
(`CGO_ENABLED=0`, reproducible builds), so it cannot load a PKCS#11 library. `bide-hsm` is the companion
program for approvers whose keys are in an HSM, on a smart card or token, in AWS KMS, or in Google Cloud KMS or in Azure Key Vault. `bide-hsm
approve` is `bide approve`: one implementation of the display, the confirmation, the signature, the
record and the receipt (`cmd/bide/internal/approvecmd`), with `-key` naming a key by reference instead of
a file. `bide-hsm pubkey` prints the entry that enrols a key, and `bide-hsm csr` writes the certificate
signing request for a certificate-bound approver, signed by the key where it is held (see
[the key and the certificate request](certificate-approvers.md#the-key-and-the-certificate-request)).
`bide-hsm stop` is `bide stop`, the kill switch, with the key held there signing each stop and lift
([kill switch guide](kill-switch.md#audited-mode-from-the-command-line)).

```sh
export BIDE_HSM_PIN=...            # or pin-source=file:/path (mode 0600) in the reference
KEY='pkcs11:token=bide;object=finance?module-path=/usr/lib/softhsm/libsofthsm2.so'

bide-hsm pubkey -as finance -key "$KEY"       # once: prints the approver-keys entry to enrol
bide-hsm approve -db run.db -list             # the calls that wait, each with its command
bide-hsm approve -db run.db -run refund-1049 -call c2 -as finance -key "$KEY" \
    -approver-keys approver-keys.json
```

**Key references.** A key is named by a reference, parsed strictly:

- `pkcs11:token=<label>;object=<label>;id=<%xx>?module-path=<abs path>&pin-source=<source>`, an RFC 7512
  PKCS#11 URI. `token` (or `slot-id`) selects the token; `object` (the key's `CKA_LABEL`), `id` (its
  `CKA_ID`, percent-encoded bytes) or both select the key; `module-path` is the vendor's library, an
  absolute path, which `bide-hsm` loads and runs. `type`, if given, must be `private`.
- `awskms:arn:aws:kms:<region>:<account>:key/<id>` (or `alias/<name>`), an AWS KMS key by its full ARN.
  The ARN's region configures the client, so the reference does not depend on the environment for it;
  credentials come from the AWS SDK's default chain. `signer/awskms` requires KMS to answer with the key
  asked for (the same key ARN for a key ARN; a key in the alias's partition, region and account for an alias
  ARN), so a redirected endpoint or a wrong answer naming another account's key is refused before anything is
  signed; `bide-hsm approve` shows an alias with the key ARN it resolved to. The `awskms:` prefix, not `kms://`, is the form:
  it names the signer (`signer/awskms`) and leaves room for other key services as other schemes.
- `gcpkms:projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>/cryptoKeyVersions/<n>`, a Google Cloud KMS key
  version by its full resource name. The version is the number Cloud KMS assigns, pinned: no alias, no
  `latest`, no `primary`, no query, so signing uses exactly the version the reference names (`signer/gcpkms`
  also checks that Cloud KMS answers with that name). Credentials are Google's application default
  credentials. Only an HSM-backed version is used by default, as in `signer/gcpkms`; Cloud KMS has no HSM-backed
  Ed25519 key, so an Ed25519 key needs `-allow-software-key` (and `bide-hsm approve` then says, beside the
  key, that it is at protection level SOFTWARE). `-verify-attestation` requires the version's Cloud HSM
  attestation to prove the key was generated in the HSM and cannot be extracted (P-256 in an HSM only; it
  excludes `-allow-software-key`). The scheme is detected, P-256 first (the HSM-backed one). `Open` makes one
  billed self-test signature.
- `azurekv:https://<vault>.vault.azure.net/keys/<name>/<32-hex-version>`, an Azure Key Vault or Managed HSM
  key by its full identifier with the version pinned: a base identifier (no version) would follow a rotation
  and is refused, as are a port, credentials, a query, a fragment, an upper-case host, and a host that is not a
  Key Vault or Managed HSM host of the Azure public, China or US Government cloud (`signer/azurekv` applies the
  same rules and also checks that the service answers with that name and version). The key is ECDSA P-256 (Key
  Vault has no Ed25519, so `-alg ed25519` is refused), not exportable, and HSM-protected unless
  `-allow-software-key`, in which case `bide-hsm approve` shows it as software-protected with a notice. The beside-the-key line shows
  `(HSM-protected)` or `(software-protected)`. Credentials are the Azure SDK's default credential chain; the
  principal needs "Key Vault Crypto User" (Managed HSM Crypto User) on the key. `Open` makes one billed
  self-test signature.

**The PIN is never in the reference.** `pin-value` is refused outright, since a PIN in an argument sits
in the process list and the shell history. The PIN comes from `pin-source=file:/absolute/path`, read each
time a session opens, and on Unix refused when another user owns it or group or others can read it (as `bide approve`
refuses a key file, and as `ssh` refuses a private key; the checks are made on the open file, which is opened
without blocking so a named pipe is refused, not waited on); from `pin-source=env:NAME` (an extension of RFC 7512, which defines only URIs for
this); from `pin-source=prompt`, which asks on the controlling terminal with echo off (it opens `/dev/tty`,
or `CONIN$` on Windows, and never reads standard input or a pipe, so a script cannot feed it; with no terminal it
is refused with an error saying so, and detection asks once per run, not once per attempt); or, with no
`pin-source`, from `$BIDE_HSM_PIN`. A token with its own PIN pad
or keypad reader (it reports `CKF_PROTECTED_AUTHENTICATION_PATH`) uses `pin-source=protected`: `C_Login` is
called with no PIN and the person types it on the device. It is refused for a token without the flag (SoftHSM2
has none), a token with the flag is refused a PIN (`pkcs11.Config.ProtectedAuthPath` and `Config.PIN` are
exclusive), and it needs `-alg`, since scheme detection would log in, and ask on the pad, more than once.
Tested against a fake login and SoftHSM2's refusal, never a real PIN pad.

**Per-signature authentication.** A key with `CKA_ALWAYS_AUTHENTICATE` (common for approver keys) needs a
context-specific login for every signature. The signer reads the attribute when it opens the key and, for
such a key, calls `C_Login` as `CKU_CONTEXT_SPECIFIC` between `C_SignInit` and `C_Sign`, with the same PIN
source as the login: a prompt asks again for each signature (which is the point), a file or environment
variable is read again, and a protected authentication path asks on the pad. `Open` does not sign its
self-test with such a key (that would ask for a PIN to show nothing); the first real signature is still
checked against the public key. A wrong PIN at signing is the token's error, and the session is replaced
before the next signature, since the token keeps the failed signature active. Tested against a fake that
signs only after the login and against SoftHSM2; each approval asks for the PIN twice with a prompt (the
login when the key is opened, then the signature).

**PIN length.** A PIN shorter than 4 characters is refused
before the token is touched (SoftHSM2 and most tokens enforce a minimum of their own, and the scrubbing of
error messages replaces the PIN by substring, which a one-character PIN would garble). An error names the
variable or the file, never a PIN. The parser's errors never repeat a value from the reference, and a
stray argument given without `-key` is not echoed, so a PIN typed into `pin-value` or after the command
is not printed back. An error from the token or its library names what it looked up (a label, a path)
as the reference wrote it, so a PIN typed into `object`, `token`, `module-path` or `pin-source` would
be printed back on the approver's own terminal; the PIN was then already in the process list and the shell
history, which is why `bide-hsm` cannot make that safe, only keep the PIN out of the places it controls.
Every attribute that is not listed above (`serial`,
`manufacturer`, `module-name`, and any other) is refused, not ignored: an ignored attribute would let a
reference select a key more loosely than it reads.

**Non-extractable by default.** `RequireNonExtractable` is on: a PKCS#11 key the token does not report as
`CKA_EXTRACTABLE=false` and `CKA_SENSITIVE=true` is refused, because an approver key that can be read out
of the token defeats keeping it there; and when the token also reports `CKA_NEVER_EXTRACTABLE` and
`CKA_ALWAYS_SENSITIVE`, both must be true, since a key that was extractable once may have been copied
before it was marked otherwise. `-allow-extractable` lifts that, and `bide-hsm approve` then prints
a notice beside the key. It is the token's word and not an attestation (see
[known limitations](../KNOWN-LIMITATIONS.md#key-custody)); it does not apply to a KMS key.

**The scheme** is Ed25519 or ECDSA P-256 and is detected (Ed25519 is tried first, and only a key that is
not an Ed25519 key is tried as P-256, so a wrong PIN costs one login, not two); `-alg` names it. Labels are
not unique on a token: when a reference selects both an Ed25519 key and an ECDSA P-256 key, detection
refuses (`pkcs11.ErrAmbiguous`) and asks for `id=` or `-alg` instead of picking one, at the cost of one more
login to look for the second key. Enrol
the key's `<scheme>:<hex>` from `bide-hsm pubkey`, in the application's verifier and in the
`-approver-keys` file; `bide-hsm approve` refuses a key that is not the one enrolled for `-as`, before it
records anything.

**What the approver sees, and what the cloud sees.** `bide-hsm approve` shows the key's scheme and
fingerprint and, beside them, where it is held: the reference without the source of the PIN (the token,
the key's label, the module; or the KMS key ARN). For KMS, each run opens the key with `kms:GetPublicKey`
and one `kms:Sign` self-test (billed, and logged by CloudTrail), then one `kms:Sign` for the decision;
the role needs those two permissions and no others. The AWS SDK's own configuration applies, including
the endpoint overrides it honors (`AWS_ENDPOINT_URL_KMS`, `AWS_ENDPOINT_URL`, the shared config file), which
is how a VPC endpoint is used; a call sent elsewhere by such a setting is a different KMS, and what it
signs only counts if the key is the one enrolled. An environment variable that holds a PIN is visible in
`/proc/<pid>/environ` to the same user (and root) and is inherited by every child process; prefer
`pin-source=file:` on a private file, or an environment variable set for the one command.

**What it does and does not protect.** The key stays in the token or KMS, which signs the bytes
`bide-hsm` hands it. `bide-hsm` shows the call it read from the journal and signs the digest it showed,
but the token does not see the call: on a compromised approver workstation, with the token unlocked,
malware can have it sign a decision other than the one displayed. A token with a trusted display that
confirms each signature is not supported. `module-path` loads native code into the process, so name only
a library you trust. A receipt is written in the working directory (`-receipt` names another file), as
there is no key file to put it beside.

**Building it.** `bide-hsm` is not in the release archives. It is built from source:

```sh
CGO_ENABLED=1 go install github.com/bide-ai/bide/cmd/bide/bide-hsm@latest
```

It needs a C compiler for PKCS#11 keys. Built with `CGO_ENABLED=0` it compiles and still signs with a KMS
key; a `pkcs11:` reference then fails with `pkcs11.ErrNoCgo`, and `bide-hsm version` says which build
you have. It is a module of its own, under `cmd/bide` so that it can use that module's internal approval
code. See [the bide-hsm reference](../reference/bide-hsm.md) for every flag. `bide audit verify-receipts`
(and its `-update`) signs nothing, so the receipts of decisions signed this way are verified with the
`bide` binary as any receipt is.

## Verifying and rotating

Verification is unchanged. Print the public key once (`audit.FormatPublicKey(audit.AlgEd25519,
signer.PublicKey())`, which the example prints) and give auditors that `ed25519:<hex>` string for
`bide audit verify-*` and for `audit.ParsePublicKey`.

A key in a token or KMS is replaced by creating another key and opening the signer on it. Heads signed by
the old key still verify under the old public key, so keep that public key for as long as its heads must
verify.

## See also

- [Retention and archiving](retention.md): keeping public keys, rotating and retiring keys without breaking old verification.
- [Security model: key custody](security-model.md#key-custody): what each key signs and why they are distinct.
- [Audit](audit.md#signature-schemes-and-post-quantum-anchoring): the signature schemes.
- [Known limitations](../KNOWN-LIMITATIONS.md#key-custody).
- `examples/hsm-signing`: the PKCS#11 signer end to end, the Google Cloud KMS signer with `BIDE_GCPKMS_KEY_VERSION` set, or the Azure Key Vault signer with `BIDE_AZURE_KEY_ID` set.

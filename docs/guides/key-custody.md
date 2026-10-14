# Key custody: signing keys in an HSM or a cloud key service

bide's guarantees rest on who holds the signing keys (see [Security model](security-model.md#key-custody)).
A key file on the application host is a key the application, and so anyone who compromises it, can use
freely. This guide puts the key in a PKCS#11 token (a hardware security module, a smart card, or SoftHSM2
for tests) or in AWS KMS, behind the same `audit.Signer` interface every signing entry point already takes.
Nothing on the verifying side changes: an auditor still holds only the public key, and `bide audit` and
`audit.NewVerifier` check the signatures as they check any Ed25519 signature.

This is key custody, not a compliance certification. It keeps the private key out of the application's
memory and disk (whether the token or KMS can ever export it is their policy; see `RequireNonExtractable`); it does not prove how a given token was provisioned, and it does not by itself separate
the duties of the people who can use the key.

## What is supported

| Signer | Module | Keys (`Config.Scheme`) | Signs | Tested against |
|---|---|---|---|---|
| PKCS#11 | `github.com/bide-ai/bide/signer/pkcs11` (cgo) | Ed25519 (`CKK_EC_EDWARDS`, token lists `CKM_EDDSA`) or ECDSA P-256 (`CKK_EC`, `CKM_ECDSA`) | the audit log (tree heads, evidence seals, grants), approver decisions | SoftHSM2 only |
| AWS KMS | `github.com/bide-ai/bide/signer/awskms` (pure Go, AWS SDK) | Ed25519 (`ECC_NIST_EDWARDS25519`) or ECDSA P-256 (`ECC_NIST_P256`), usage `SIGN_VERIFY` | the same | a fake KMS client only |

Both are separate Go modules, so the core `github.com/bide-ai/bide` module gains no cgo and no cloud SDK.
`Config.Scheme` is `audit.AlgEd25519` (the default) or `audit.AlgECDSAP256`; the signers never substitute
one for the other. A token or key of the wrong kind is refused with `ErrNoEd25519` or `ErrNoP256` and the
error names what was found. Pick Ed25519 where the HSM or KMS has it; pick ECDSA P-256 where it does not
(many HSMs, and cloud key services such as Azure Key Vault). Both are FIPS-approved signature schemes.
ECDSA signatures are randomized, so signing one message twice gives two different valid signatures.

Not supported, and why:

- **ML-DSA and hybrid in a token.** `audit.HybridSigner` holds a concrete `Ed25519Signer` and an
  `MLDSASigner`, so an HSM signer cannot be one half of a hybrid; and AWS KMS offers no context parameter
  for ML-DSA, which bide's ML-DSA signatures use for domain separation. Post-quantum anchoring stays with
  software keys; hybrid in a token is deferred (the hybrid scheme stays `ed25519+ml-dsa-65`).
- **Azure Key Vault and Google Cloud KMS.** Planned ([roadmap](../ROADMAP.md#bank-readiness)).
- **Hardware attestation.** A signer does not check that the key was generated in hardware or that it is
  non-extractable; that is a property of how the token was provisioned.

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
  token that is already logged in or authenticates through a protected path (a PIN pad).
- **Open proves the key pair.** It signs one fixed message and checks it against the public key, at open and at every reopen, so a private key that is not the pair of the exposed public key fails there and not at the first anchor. A key of type `CKK_EC_EDWARDS` that is Ed448 returns `ErrNoEd25519`.
- **Non-extractable keys.** The key is not readable by the application through this signer, but whether the token lets it be exported is the token's policy. Set `Config.RequireNonExtractable` to make `Open` fail with `pkcs11.ErrExtractable` unless the token reports the key as `CKA_EXTRACTABLE=false` and `CKA_SENSITIVE=true`. That checks what the token says; it is not an attestation.
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
(`Config.Timeout`, default 30 seconds).

The AWS signer is tested against a fake KMS client in bide's CI, which holds an Ed25519 key and enforces
the call shape KMS documents (key ARN, algorithm, `RAW`, the size limit). bide's tests never call AWS, so
the interoperability of KMS's `ED25519_SHA_512` and `ECDSA_SHA_256` output with bide's verifier rests on
AWS's documentation of those algorithms (and, for ECDSA, on KMS's DER signatures, which the signer
converts to bide's low-S `r||s`); check it once against your own key before relying on it (sign a message, verify it with
`audit.VerifierOf`).

### The KMS 4096-byte limit

Under Ed25519 KMS signs at most 4096 bytes, and Ed25519 signs the whole message. Tree heads, grants and
approver decisions are small but carry caller-chosen text: a tree head is about 110 bytes plus the run ID's
length (a run ID of about 4000 bytes or more does not fit), and an approver decision includes the tool
call's arguments. `Sign` then returns `awskms.ErrMessageTooLarge` naming the size, without calling KMS. An
evidence package seal signs a fixed 60-byte message (evidence format v6) and always fits. Under ECDSA
P-256, KMS signs a 32-byte digest, so there is no limit.

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

Command-line tools that sign a decision with a key loaded from a file do not read keys from a token or a
key service yet. Doing so means accepting a PKCS#11 URI (RFC 7512) where they accept a key file, for example
`pkcs11:token=bide;object=bide-finance?module-path=/usr/lib/softhsm/libsofthsm2.so&pin-source=file:/run/secrets/hsm-pin`.
The `token`, `object` and `id` attributes select the token and key, `module-path` names the library, and
`pin-source` names a PIN file (a URI never carries the PIN). The `bide` binary is pure Go (`CGO_ENABLED=0`,
reproducible builds) and the PKCS#11 signer needs cgo, so that is a build-tagged variant or a companion
binary, not a change to the default build.

## Verifying and rotating

Verification is unchanged. Print the public key once (`audit.FormatPublicKey(audit.AlgEd25519,
signer.PublicKey())`, which the example prints) and give auditors that `ed25519:<hex>` string for
`bide audit verify-*` and for `audit.ParsePublicKey`.

A key in a token or KMS is replaced by creating another key and opening the signer on it. Heads signed by
the old key still verify under the old public key, so keep that public key for as long as its heads must
verify.

## See also

- [Security model: key custody](security-model.md#key-custody): what each key signs and why they are distinct.
- [Audit](audit.md#signature-schemes-and-post-quantum-anchoring): the signature schemes.
- [Known limitations](../KNOWN-LIMITATIONS.md#key-custody).
- `examples/hsm-signing`: the PKCS#11 signer end to end.

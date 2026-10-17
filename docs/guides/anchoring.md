# Anchoring: signed heads in an S3 Object Lock bucket

A journal's Merkle root proves nothing against the operator of the database it is stored in: they can
rewrite the journal, rehash it and re-sign it with the log key. What they cannot do is rewrite a head
that was already written somewhere they cannot change. That is the anchor ([security
model](security-model.md#anchoring-the-condition-on-tamper-evidence)). `audit.Anchor` is the port, and
the only implementation in the core module is the in-memory `audit.MemAnchorLog`, for tests. This guide
covers the first production one: `github.com/bide-ai/bide/anchor/awss3`, which writes every signed head
as an object in an S3 bucket under Object Lock (a write-once store), and the `bide-anchor` command that
verifies what it wrote.

This is tamper evidence, not a compliance certification. It is tested against MinIO (which implements
Object Lock) and a fake, never against AWS ([what it is tested against](#what-it-is-tested-against)).

## What it gives you, and what it does not

An anchored head is an object that S3 will not overwrite or delete until its retention date. In
compliance mode that holds for every principal, the account's root user included. A bank that publishes
its heads to a bucket in an AWS account that the application's operators do not control gets this: the
operator can rewrite the journal and re-sign it, but cannot remove the earlier heads, so a check of the
journal against them (`bide audit verify-receipts -heads`, `verify-decision-coverage -heads`, an evidence
package's head) reports the rewrite.

What an operator who controls the **anchor** account can still do, and what you do about it:

| They can | Because | What to do |
|---|---|---|
| Delete a head before its retention date | governance mode, with `s3:BypassGovernanceRetention` | Use compliance mode (the default). `Config.AllowGovernance` exists for a trial and warns every time it is used. |
| Delete the bucket's objects, or the bucket, after retention ends | Object Lock ends at the retention date | Set a retention at least as long as the journals the heads cover (`Config.Retention`), and copy the heads into your archive before it ends ([retention](retention.md#signed-heads-and-anchor-entries)). |
| Close the AWS account | AWS deletes a closed account's data after a grace period | Keep the anchor account in an organization whose service control policies deny `organizations:CloseAccount`, with break-glass access under MFA. |
| Disable or schedule deletion of the KMS key | the key's policy is theirs | The heads become unreadable, not altered: the reader fails instead of returning a wrong head. Keep the key in the anchor account with a policy that denies `kms:ScheduleKeyDeletion` and `kms:DisableKey` to the application. |
| Add a delete marker over a head | `s3:DeleteObject` creates a marker without touching the locked version | `bide-anchor check` and `Reader.Audit` report every marker; `Reader.Latest` and `Heads` list versions, so a marker does not hide a head. The bucket policy below denies `s3:DeleteObjectVersion`, and the writer's policy has no delete at all. |
| Write a forged head | `s3:PutObject` | The reader verifies each head's signature under the log's public key and refuses an object that does not verify; a forged object is an error, not a head. |
| Stop publishing | nothing stops the application from not calling `Publish` | Heads are only evidence for what they cover: a monitor compares the latest head's size with the journal's. |

**Recommended setup: compliance mode, in a separate AWS account.** The application's account holds
only the writer role (below). The anchor account holds the bucket, its KMS key and the reader roles, and
nobody who administers the application's account administers it. Separate accounts are what make "the
operator cannot rewrite the anchor" true; the same account with compliance mode still lets an account
administrator delete the bucket after the retention date and close the account.

A timestamp in an anchor is AWS's clock (`LastModified`), not an RFC 3161 token from a timestamping
authority. Do not use it where a timestamping authority's token is required.

## Create the bucket

Object Lock is chosen when a bucket is created; it turns versioning on and cannot be turned off.

```sh
aws s3api create-bucket --bucket bank-bide-anchor --region eu-west-1 \
  --create-bucket-configuration LocationConstraint=eu-west-1 --object-lock-enabled-for-bucket
aws s3api put-object-lock-configuration --bucket bank-bide-anchor --object-lock-configuration \
  '{"ObjectLockEnabled":"Enabled","Rule":{"DefaultRetention":{"Mode":"COMPLIANCE","Years":7}}}'
aws s3api put-bucket-encryption --bucket bank-bide-anchor --server-side-encryption-configuration \
  '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"aws:kms","KMSMasterKeyID":"arn:aws:kms:eu-west-1:111122223333:key/..."},"BucketKeyEnabled":true}]}'
```

The default retention is a floor: each head also carries its own retention (`Config.Retention`), and the
default protects any object written without one. `New` refuses a bucket that has no Object Lock, no
versioning, no default retention, a default shorter than `Config.Retention`, or (for compliance mode) a
governance default.

## Use it

<!-- docsnip: setup import "github.com/aws/aws-sdk-go-v2/aws"; import "github.com/aws/aws-sdk-go-v2/service/s3"; ctx context.Context; awsConfig aws.Config; inner agent.Store; signer audit.Signer -->
```go
a, err := awss3.New(ctx, awss3.Config{
	Client:              s3.NewFromConfig(awsConfig), // from config.LoadDefaultConfig
	Bucket:              "bank-bide-anchor",
	ExpectedBucketOwner: "111122223333", // the anchor account
	Retention:           7 * 365 * 24 * time.Hour,
	KMSKeyID:            "arn:aws:kms:eu-west-1:111122223333:key/...",
})
if err != nil {
	log.Fatal(err) // the bucket is not what an anchor needs: nothing was written
}
store, err := audit.NewAuditedStore(inner, signer, a)
```

Credentials come from the AWS SDK's default chain (a role, not keys in configuration). `ExpectedBucketOwner`
is sent with every request, so a bucket of that name in another account is refused, not written to.
`audit.NewAuditedEventStore` takes the same anchor for notification and event trails.

`Publish` returns nil only when the object is stored, locked and encrypted as configured, and equal to the
head: it reads the object back (the version it created) and compares the bytes, the lock mode, the
retain-until date, the legal hold and the KMS key. A weaker object is `awss3.ErrWeak`.

### Failure modes

- **S3 unreachable or throttled.** `Publish` returns an error wrapping `awss3.ErrUnavailable`. It does not
  queue: a queue on the application's disk would be neither tamper-evident nor visible. The journal write
  that triggered it has already succeeded (anchoring is a side channel); the error goes to
  `AuditedStore.OnError`, and the next write or `Reanchor(ctx, runID)` publishes again. The AWS client
  retries a few times first (its retryer is configurable on the client you pass in).
  **The window**: between a failed publish and a successful one, the journal has records no anchored head
  covers. Alert on `OnError` and on a latest head older than the journal.
- **A lost response.** A write that landed but whose answer was lost is retried safely: the key is derived
  from the head, the write is conditional (`If-None-Match: *`), and a retry finds the object, compares it
  and returns nil without writing a second version.
- **Two heads of one size.** Two processes anchoring one run around a lease handoff can each publish a
  head of the same size with different timestamps. Both are kept, as two objects.
- **A bucket changed after start.** `New` checks the bucket once. Each `Publish` still reads the object
  back, so a bucket that lost its lock fails the next publish with `ErrWeak`.

### Run and trail IDs must not hold personal data

The run ID is part of the signed bytes of a head, so a head that verifies necessarily carries it, and an
object cannot be erased before its retention date: **redaction cannot reach the anchor**. Object keys hold
only a hash of the ID, but the objects hold the ID. Do not put a customer's name, email address, account
number or any other personal data in a run ID or a trail ID that is anchored here; use an opaque ID and
keep the mapping in your own store, where it can be erased. `Config.OnWarn` reports an ID that looks like
an email address or holds a run of 12 or more digits, and `Config.RefuseSuspectIDs` makes `Publish`
refuse it. This is a tripwire for the obvious cases, not a proof that an ID is clean.

### Cost and volume

`AuditedStore` anchors after every journal write, so each write is one object that cannot be deleted
before its retention date: at one write a second, a busy run produces about 31 million objects a year per
trail, each kept for the whole retention, and each costs a write, a read-back and a KMS data-key request
(a bucket key, which `Publish` enables, reduces the KMS requests). Estimate requests, storage and KMS for
your write rate and retention before you set one; the figures are AWS's and change. A per-trail
minimum interval, which keeps the window between heads bounded, is planned as a separate change and is
not in this release.

## The key layout

```
<prefix><sha256 of kind and ID, hex>/<inverted size>-<head digest>.json
```

Every head is one object whose key is derived from the head alone: there is no index object to trust or
forget to update. The prefix is `bide-anchor/` unless configured. The partition is a hash, so a bucket
listing names no run. The inverted size (the largest 64-bit integer minus the head's size, 19 digits) makes
the largest head list first, so "the latest head of a trail" is a listing of one key, and "the heads of
size N" is a prefix listing. The digest (16 bytes of the SHA-256 of the body) makes a retry the same key
and two different heads of one size two keys. The body is `{"format":"bide.audit.s3-anchor.v1","sth":{...}}`,
in one canonical encoding; a reader refuses a body that does not re-encode to itself, a key that does not
match its body, and any head whose signature does not verify.

`Reader` gives `Latest`, `Head` (a size), `Heads` (all, in increasing size) and `Audit`. S3 lists a key as
soon as it is written (strong read-after-write consistency, since December 2020), which `Latest` relies on.

## The IAM policies

`awss3.WriterPolicy`, `ReaderPolicy` and `BucketPolicy` are JSON templates (`awss3.PolicyFor` fills in the
bucket, prefix and key ARN), tested to contain no `Delete*` and no `s3:BypassGovernanceRetention`, and to
cover every call the package makes. The writer's, for the application:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "WriteAndReadBackHeads",
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:PutObjectRetention", "s3:GetObject", "s3:GetObjectVersion",
                 "s3:GetObjectRetention", "s3:GetObjectLegalHold"],
      "Resource": "arn:aws:s3:::BUCKET/PREFIX*"
    },
    {
      "Sid": "ListAndCheckTheBucket",
      "Effect": "Allow",
      "Action": ["s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketVersioning",
                 "s3:GetBucketObjectLockConfiguration", "s3:GetEncryptionConfiguration"],
      "Resource": "arn:aws:s3:::BUCKET"
    },
    {
      "Sid": "EncryptHeads",
      "Effect": "Allow",
      "Action": ["kms:GenerateDataKey", "kms:Decrypt"],
      "Resource": "KMS_KEY_ARN"
    }
  ]
}
```

- `s3:PutObjectRetention` is what S3 requires to set a retention date in a write; in compliance mode it can
  only extend a retention, never shorten it.
- With `Config.LegalHold`, also allow `s3:PutObjectLegalHold`. It lets the holder release the hold too, so
  give release (the same action) to a separate identity and keep it off the application's role; the
  application only needs to place it.
- `BucketPolicy` denies everyone `s3:DeleteObjectVersion`, `s3:BypassGovernanceRetention`, and changes to
  the lock configuration, versioning and lifecycle, requires SSE-KMS and an object lock mode on every
  write, and requires TLS. Apply it after the bucket is configured, since it denies the calls that
  configure it. A deny in a bucket policy is overridden by whoever can replace the policy: make
  `s3:PutBucketPolicy` and the organization's service control policies the anchor account's to guard.
- The reader policy (auditors and monitors) is read-only. A reader also needs `kms:Decrypt` on the key.

**CloudTrail.** Turn on S3 data events for the bucket in the anchor account, delivered to a third place
(a log archive account): they record who wrote, read and tried to delete. They cost per event (at the time
of writing about USD 0.10 per 100,000 data events; check AWS's current price), and each head produces at
least two object-level events (the write and the read-back), so at a high write rate they cost more than
the storage. Decide on a sample or a narrower selector for the writer's own traffic if the volume is the
problem, and keep the events for denied deletes and for everything from other principals.

## Verifying: `bide-anchor`

`bide-anchor` is a small command in the same module (`go install
github.com/bide-ai/bide/anchor/awss3/cmd/bide-anchor@latest`), so the AWS SDK is not in `bide`.

```sh
# Offline: a directory of downloaded objects (an auditor's copy; no network, no credentials).
aws s3 sync s3://bank-bide-anchor/bide-anchor/ ./anchor/bide-anchor/
bide-anchor verify -dir ./anchor -pubkey ed25519:<hex>
bide-anchor export -dir ./anchor -pubkey ed25519:<hex> -run run-123 -o heads.jsonl

# Online, with read-only credentials: also reports how the heads are stored.
bide-anchor check  -bucket bank-bide-anchor -owner 111122223333 -pubkey ed25519:<hex> -run run-123
```

`export` writes the verified heads of one run or trail, one per line in increasing size, the file that
`bide audit verify-receipts -heads` and `verify-decision-coverage -heads` read. For a notification trail use
`-kind events` and the trail's name. The public key must come from the log operator out of band, not from
the bucket.

`verify` (offline) checks every object's name, encoding and signature and reports two signed heads of one
size with different roots: that is the log key signing two histories, and no normal operation does it. It
cannot see the objects' lock state, which is S3 metadata and not in the file, and it says so. `check`
(online) reads every version of every object and reports a finding for each delete marker, each object that
is not locked, each retention that has ended (the object can be deleted now), each governance-mode lock,
each key with more than one version, and each object that is not a verified head. Findings exit 1.

Neither command shows that the **journal** matches the heads. That check is `bide audit` against the exported
heads.

## What it is tested against

- A fake bucket (`anchor/awss3/awss3test`) that models versions, conditional writes, lock headers, delete
  markers, lost responses and an expected-owner check, for the unit tests and the end-to-end tests with
  `AuditedStore` and `AuditedEventStore`.
- MinIO, in the Integration job, which implements Object Lock, conditional writes and legal holds and
  refuses a delete or a shortened retention on a compliance-locked version. Run the same tests locally with
  a MinIO and `BIDE_S3_ENDPOINT`, `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` set; with
  `BIDE_REQUIRE_OBJECTLOCK=1` a missing server fails the tests instead of skipping them. MinIO does not
  enforce `x-amz-expected-bucket-owner` and has no KMS in this setup, so those two are tested against the
  fake only.
- **Never AWS.** The IAM actions the package needs, S3's behaviour on the owner check, SSE-KMS, and the
  exact errors AWS returns rest on AWS's documentation. Run the package against a scratch bucket in your own
  account (a short retention in governance mode, `AllowGovernance`) before you rely on it, and test the
  policies above with the writer role.

## See also

- [Retention and archiving](retention.md): keep the heads at least as long as the journals they cover.
- [Security model: anchoring](security-model.md#anchoring-the-condition-on-tamper-evidence).
- [Audit](audit.md): verifying a journal against heads.
- [Known limitations](../KNOWN-LIMITATIONS.md#anchoring).
- [Extension points: Anchor](../reference/extension-points.md#anchor-out-of-band-anchoring-audit).

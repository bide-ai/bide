# Anchoring: signed heads in a Google Cloud Storage bucket with a locked retention policy

The [anchoring guide](anchoring.md) explains why signed heads must be published somewhere the operator
of the journal's database cannot rewrite, and covers the S3 Object Lock anchor. This guide covers the
second production anchor: `github.com/bide-ai/bide/anchor/gcs`, which writes every signed head as an
object in a Google Cloud Storage bucket whose retention policy is **locked** (Bucket Lock), and a Reader
that reads the heads back and verifies them. The object layout, the object body, the Reader's
semantics (equivocation, stray objects, bad signatures) and the run-ID rules are the same as the S3
anchor's; this page covers what is different.

This is tamper evidence, not a compliance certification. It is tested against an in-memory model of
Cloud Storage and was run once against a real bucket ([what it is tested against](#what-it-is-tested-against)).

## What it gives you, and what it does not

A head anchored here is an object that Cloud Storage will not delete before its retention date, for any
principal, a project owner included, because the bucket's retention policy is locked: a locked policy
can be made longer and never shorter, and it applies to noncurrent object versions as well. A bank that
publishes its heads to a bucket in a Google Cloud project that the application's operators do not
control gets this: the operator can rewrite the journal and re-sign it, but cannot remove the earlier
heads, so a check of the journal against them reports the rewrite.

What an operator who controls the **anchor** project can still do, and what you do about it:

| They can | Because | What to do |
|---|---|---|
| Delete a head after its retention date | the policy ends at the date | Set a retention at least as long as the journals the heads cover (`Config.Retention`), and copy the heads into your archive before it ends ([retention](retention.md#signed-heads-and-anchor-entries)). |
| Delete the bucket's objects, or the bucket | after retention only: a bucket with a locked policy cannot be deleted while any object is inside its retention | As above. |
| Delete the project | Cloud Storage puts a lien on project deletion when a bucket is given a retention policy (the real run saw it appear one second after the bucket was created with one), on a best-effort basis, and a project owner can remove a lien. Google's documentation does not say what happens to locked data when a project whose lien was removed is deleted, and this package does not claim anything about it | Keep the anchor project in a folder whose organization policies and IAM deny policies the application's administrators cannot change, with break-glass access under MFA. Treat the lien as a speed bump, not a control. |
| Disable or destroy the Cloud KMS key | the key's IAM is theirs | The heads become unreadable, not altered: the Reader fails instead of returning a wrong head. Keep the key in the anchor project (or a key project of its own) and deny the application's identities any administrative role on it. |
| Make the live object of a name noncurrent | with versioning on, deleting or overwriting the live object keeps the generation; a locked policy lets the live object become noncurrent | `Reader.Latest`, `Heads` and `Audit` list noncurrent generations, so nothing is hidden, and `Audit` reports each noncurrent generation. The writer role has no delete permission, which also means it cannot overwrite (replacing an object needs both `storage.objects.create` and `storage.objects.delete`). |
| Write a forged head | any identity that can create objects | The Reader verifies each head's signature under the log's public key and refuses an object that does not verify. |
| Lengthen the retention | `storage.buckets.update` | This only makes the anchor stronger. |
| Stop publishing | nothing stops the application from not calling `Publish` | Heads are only evidence for what they cover: a monitor compares the latest head's size with the journal's. |

**Recommended setup:** a locked bucket in a Google Cloud project of its own, in a folder that the
application's administrators do not administer, with the application holding only the writer role (below)
on the bucket. Separate projects are what make "the operator cannot rewrite the anchor" true; the same
project with a locked policy still lets a project owner wait out the retention and delete.

A timestamp in an anchor is Google's clock (the object's creation time), not an RFC 3161 token from a
timestamping authority. Do not use it where a timestamping authority's token is required.

## Create the bucket

Run these in the anchor project. **Locking a retention policy cannot be undone, and the bucket then
cannot be deleted until every object in it is past its retention.** Create the bucket, check it, and lock
it only when it is what you want. The flags are written from Google's documentation; check them with
`--help` against your `gcloud` version.

```sh
gcloud storage buckets create gs://bank-bide-anchor --project=ANCHOR_PROJECT --location=europe-west1 \
  --uniform-bucket-level-access --public-access-prevention \
  --default-encryption-key=projects/ANCHOR_PROJECT/locations/europe-west1/keyRings/anchor/cryptoKeys/anchor \
  --retention-period=7y
gcloud storage buckets update gs://bank-bide-anchor --versioning
gcloud storage buckets describe gs://bank-bide-anchor      # check it, then:
gcloud storage buckets update gs://bank-bide-anchor --lock-retention-period
```

The Cloud Storage service agent of the anchor project needs permission to use the key. On a new
project the agent does not exist yet: create it with `gcloud storage service-agent --project=ANCHOR_PROJECT`,
then run the same command with `--authorize-cmek=KEY`.

`New` refuses a bucket that is owned by another project than `Config.ExpectedProjectNumber`, has no
retention policy, has one that is not locked or is shorter than `Config.Retention` (or than
`gcs.MinRetention`, 24 hours), lacks versioning, uniform bucket-level access or enforced public access
prevention, or whose default key is not `Config.KMSKey`. It reads the bucket's configuration and writes
nothing. The project check is the analogue of S3's expected bucket owner: bucket names are global, and a
deleted bucket's name can be created again by anyone, so the number (not the project ID) says it is the
bucket you meant. The Reader makes the same check.

Per-object retention (`Config.ObjectRetention`) is optional. It sets a `Locked` retention on each head,
counted from the write, needs the writer role with `storage.objects.setRetention`, and needs a bucket created with object retention enabled, which cannot be
enabled over gRPC and cannot be disabled once on. It adds a per-head date that can be later than the
bucket policy's; it adds nothing against deletion that the locked policy does not already give. An
`Unlocked` per-object retention can be shortened by an authorised user, so the package refuses to count
one and `Audit` reports one.

## Use it

<!-- docsnip: setup import "cloud.google.com/go/storage"; ctx context.Context; inner agent.Store; signer audit.Signer -->
```go
client, err := storage.NewClient(ctx) // Application Default Credentials
if err != nil {
	log.Fatal(err)
}
a, err := gcs.New(ctx, gcs.Config{
	Store:                 gcs.NewStore(client, "bank-bide-anchor"),
	ExpectedProjectNumber: 123456789012, // the anchor project's number
	Retention:             7 * 365 * 24 * time.Hour,
	KMSKey:                "projects/anchor-project/locations/europe-west1/keyRings/anchor/cryptoKeys/anchor",
})
if err != nil {
	log.Fatal(err) // the bucket is not what an anchor needs: nothing was written
}
store, err := audit.NewAuditedStore(inner, signer, a)
```

Credentials come from Application Default Credentials (a service account attached to the workload, or
workload identity federation), never from keys in configuration or flags. `audit.NewAuditedEventStore`
takes the same anchor for notification and event trails.

`Publish` writes the object create-only (`ifGenerationMatch=0`, with a CRC32C that the server checks) and
returns nil only when the generation it created reads back with the head's bytes, a matching size and
checksum, a retention expiry at least `Config.Retention` after the object's own creation time, a key
version of the configured Cloud KMS key, the temporary hold (with `Config.LegalHold`) and the `Locked`
per-object retention (with `Config.ObjectRetention`). A weaker object is `gcs.ErrWeak`.

### Failure modes

- **Cloud Storage unreachable or throttled.** `Publish` returns an error wrapping `gcs.ErrUnavailable`
  (a transport error, a context that ended, HTTP 408, 429 or 5xx). It does not queue: a queue on the
  application's disk would be neither tamper-evident nor visible. The journal write that triggered it has
  already succeeded; the error goes to `AuditedStore.OnError`, and the next write or `Reanchor` publishes
  again. The Google client retries first. **The window**: between a failed publish and a successful one,
  the journal has records no anchored head covers. Alert on `OnError` and on a latest head older than the
  journal.
- **Permission denied.** `gcs.ErrDenied` wraps a 401 or 403; it is not `ErrUnavailable` and is not retried.
- **A lost response.** The write is create-only and its name is derived from the head, so a retry gets a
  412, finds the object, runs every check above on it and returns nil without writing a second generation.
- **A name that holds different bytes.** `gcs.ErrConflict`. A name is derived from the head's digest, so
  this is never a normal retry: something else wrote there.
- **An existing object stored under a weaker lock.** `ErrWeak`: an object that another writer stored under a
  shorter retention does not count as anchored.
- **A bucket that changed after start.** `New` checks the bucket once. Every `Publish` still reads the object
  back, so a bucket whose retention no longer covers the object fails the next publish with `ErrWeak`.

The run-ID rules, the cost of one object per journal write, and the key layout are those of the
[S3 anchor](anchoring.md#run-and-trail-ids-must-not-hold-personal-data). An object's name is
`<prefix><sha256 of kind and ID>/<inverted size>-<head digest>.json` and its body is
`{"format":"bide.audit.gcs-anchor.v1","sth":{...}}`; the format and partition tags differ from the S3
anchor's, so an object of one anchor can never be taken for the other's. Cloud Storage lists object
names in order, which gives the layout's "the largest head lists first".

## The Reader and generations

`Reader` gives `Latest`, `Head`, `Heads` and `Audit`, with the semantics of the S3 Reader, over
generations instead of versions. Cloud Storage has no delete marker: deleting or overwriting the live
object of a name turns that generation noncurrent and, with versioning on, keeps it. The Reader lists
noncurrent generations (`versions=true`), so a deleted or overwritten newest head does not make an older
head look like the latest. `Audit` reports:

- every noncurrent generation (something deleted or overwrote the live object);
- every generation no retention covers, and every one whose retention has ended;
- every per-object retention that is not `Locked`;
- every object not encrypted with the configured key, when the Reader is given `ReaderConfig.KMSKey`;
- each bucket-level protection that is gone, because the lock lives on the bucket and not on the objects:
  the retention policy absent, not locked, under `gcs.MinRetention` or (with `ReaderConfig.Retention`)
  shorter than the anchor was configured with; versioning, uniform bucket-level access or public access
  prevention off; and, with `ReaderConfig.KMSKey`, another default key. `NewReader` refuses only a bucket
  owned by another project, so that a read-only check still runs on a bucket that has lost a protection;
- every generation deleted between the listing and the read (a partition at the end of its retention is
  reported, not an abort);
- every name with more than one generation;
- every object that is no verified head, and when its name claims a size larger than the largest verified
  head;
- two verified heads of one size with different roots (equivocation).

`Latest`, `Head` and `Heads` return an error wrapping `ErrEquivocation` for two signed heads of one size
that commit to different roots, never picking one; an object that is no head is skipped and reported; a
well-formed head whose signature does not verify is an error wrapping `ErrBadSignature`.

## IAM roles

`gcs.WriterRole`, `ReaderRole` and `HoldReleaserRole` are custom role documents for
`gcloud iam roles create --file`. Bind each at the **bucket**, not the project.

| Role | Permissions |
|---|---|
| writer (the application) | `storage.buckets.get`, `storage.objects.create`, `storage.objects.get`, `storage.objects.list` |
| writer with object retention (only with `Config.ObjectRetention`) | the writer's, and `storage.objects.setRetention` |
| reader (auditors, monitors) | `storage.buckets.get`, `storage.objects.get`, `storage.objects.list` |
| hold releaser | `storage.buckets.get`, `storage.objects.get`, `storage.objects.list`, `storage.objects.update` |

None grants `storage.objects.delete` (so none can overwrite an object either), `overrideUnlockedRetention`,
any `setIamPolicy`, or any bucket-changing permission, and only the writer with object retention grants
`setRetention` (a create that sets a per-object retention is refused without it, as the real run showed). A test checks that
every call the package makes is in the role that uses it (`gcs.MethodPermissions`), and that no role has
a forbidden permission, so a new call cannot be added without the roles being reviewed. Placing a
temporary hold when a head is created is part of the create and needs nothing more than the writer role;
**releasing** it needs
`storage.objects.update`, which the writer does not hold: give it to a separate identity.
`storage.objects.update` also changes other mutable metadata (for example custom time); it cannot shorten
a locked retention.

The Cloud Storage service agent of the anchor project, not the caller, uses the Cloud KMS key to encrypt
and decrypt: in the real run, identities holding only the writer and reader roles and no Cloud KMS role
published, read back and audited CMEK-encrypted heads.

**What roles cannot do.** An IAM role cannot stop a project owner from granting themselves any role. The
controls are outside the project: an organization policy and IAM deny policies on the anchor project's
folder that the application's administrators cannot change. For example, a deny policy on
`storage.googleapis.com/objects.delete` and `storage.googleapis.com/buckets.update` for everyone except one
break-glass group, and organization policies such as `constraints/gcp.restrictNonCmekServices` and
`constraints/iam.disableServiceAccountKeyCreation`. These are examples to adapt, and are not tested here.
**Audit logs:** turn on Data Access audit logs for Cloud Storage in the anchor project and sink them to a
third place; they record who wrote, read and tried to delete, and cost per volume, and each head produces
at least a write and a read-back.

## Verifying the heads

`bide-anchor` (the command of the [S3 anchor](anchoring.md#verifying-bide-anchor)) does not read a Google
Cloud Storage bucket yet: a `-backend gcs` option, with `-project` taking the project number in place of S3's
`-owner`, is planned as a separate change. Until then the library does what the command does:

- `gcs.LoadDir(dir, verifier)` verifies a downloaded copy of the objects offline (for example
  `gcloud storage cp -r gs://bank-bide-anchor/bide-anchor ./anchor/`), as `bide-anchor verify` does for S3. It
  cannot see retention state, and it cannot show that the copy is complete: a stale copy that lacks the
  newest heads verifies without complaint, so compare the largest size with the journal's.
- `gcs.NewReader(...)` then `Heads`, `Latest` and `Audit` read the bucket with a reader identity.
  `Reader.Heads` returns the verified heads in increasing size, the list that `bide audit verify-receipts
  -heads` and `verify-decision-coverage -heads` read once you write them one per line as JSON; `Audit` returns the
  findings listed above, to be run on a schedule and alerted on.

Credentials come from Application Default Credentials, never from flags. Neither shows that the **journal**
matches the heads: that is `bide audit` against the exported heads.

## What it is tested against

- An in-memory model of Cloud Storage (`anchor/gcs/gcstest`) with generations, create-only writes, a
  locked retention policy counted from each object's creation, temporary holds, per-object retention,
  noncurrent generations, fault injection and lost responses. It models Cloud Storage as documented; it is
  not Cloud Storage.
- **No emulator.** Neither of the two public emulators was found to enforce a locked policy, retention on
  delete, versioning or the create-only precondition (read from their source, not run). An opt-in test
  runs the SDK adapter against `STORAGE_EMULATOR_HOST` for the shape of its requests only.
- **One real run.** On 2026-10-17 `TestRealGCS` (opt-in, `BIDE_GCS_BUCKET`, never in CI) ran against a
  real bucket with a locked 24-hour policy, as the shipped roles. It settled the permissions each call
  needs, the form of a key version, how a hold is placed and released, that a retained generation cannot be
  deleted, and that a live delete only makes a generation noncurrent. It did not cover what happens after
  the retention ends, project deletion, multi-region buckets or the gRPC client; the list is in
  [known limitations](../KNOWN-LIMITATIONS.md#anchoring-google-cloud-storage). To repeat it, follow
  [the runbook](../testing/gcs-anchor-real-run.md), which leads with what a locked policy commits you to.

## See also

- [Anchoring (S3 Object Lock)](anchoring.md): the model, the key layout and the run-ID rules.
- [Retention and archiving](retention.md): keep the heads at least as long as the journals they cover.
- [Security model: anchoring](security-model.md#anchoring-the-condition-on-tamper-evidence).
- [Known limitations](../KNOWN-LIMITATIONS.md#anchoring-google-cloud-storage).

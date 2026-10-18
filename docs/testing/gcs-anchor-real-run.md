# Running the GCS anchor against a real bucket (one-time)

This is the runbook for `TestRealGCS` in `anchor/gcs`: the one test that checks the Cloud Storage
behaviour the in-memory fake only models. It is opt-in (skipped unless `BIDE_GCS_BUCKET` is set) and is
never run in CI, which has no credentials.

> **Read this before you create anything. Locking a retention policy cannot be undone.**
>
> - A locked retention policy can be lengthened and never shortened or removed, by anyone, Google
>   support included.
> - The bucket cannot be deleted until every object in it has passed its retention. With a 1-day policy
>   that is one day after the last object is written.
> - Cloud Storage puts a lien on the project's deletion while the bucket is locked, on a best-effort
>   basis. Removing it is a deliberate act (see Teardown), and Google's documentation does not promise
>   that a project whose lien was removed keeps its locked data.
> - Use a scratch project that exists only for this, a 1-day period (the shortest the anchor accepts,
>   `gcs.MinRetention`), and never a bucket that holds anything else.

It was run once, on 2026-10-17 ([what the run settled](#what-the-run-settled)). The `gcloud` flags below
are the ones that run used (gcloud 589.0.0); check them with `--help` against your version.

## What you need

- A scratch Google Cloud project with billing enabled, and its **project number** (not the ID):
  `gcloud projects describe PROJECT_ID --format='value(projectNumber)'`.
- Your identity with, on the project, enough to create the bucket and key (for example Storage Admin and
  Cloud KMS Admin), and application default credentials: `gcloud auth application-default login`.
  Credentials are never passed as flags and the test logs none.
- Cost: a few cents (a handful of objects, one key, requests). The key is billed monthly while it exists.

## 1. Key and bucket

```sh
PROJECT=bide-anchor-scratch   # your scratch project ID
LOC=europe-west1
BUCKET=bide-anchor-scratch-$(date +%s)   # bucket names are global

gcloud services enable storage.googleapis.com cloudkms.googleapis.com --project=$PROJECT

gcloud kms keyrings create anchor --location=$LOC --project=$PROJECT
gcloud kms keys create anchor --keyring=anchor --location=$LOC --purpose=encryption --project=$PROJECT
KEY=projects/$PROJECT/locations/$LOC/keyRings/anchor/cryptoKeys/anchor

# Let the project's Cloud Storage service agent use the key. On a new project the agent does not exist
# yet: create it first (no flag), or --authorize-cmek fails with "Service account ... does not exist".
gcloud storage service-agent --project=$PROJECT
gcloud storage service-agent --project=$PROJECT --authorize-cmek=$KEY

gcloud storage buckets create gs://$BUCKET --project=$PROJECT --location=$LOC \
  --uniform-bucket-level-access --public-access-prevention \
  --default-encryption-key=$KEY --soft-delete-duration=0 \
  --retention-period=1d
gcloud storage buckets update gs://$BUCKET --versioning
```

Add `--enable-per-object-retention` to the create command only if you also want to run with
`BIDE_GCS_OBJECT_RETENTION=1`; it cannot be added through gRPC and, once on, cannot be turned off. The
run left soft delete at its default (7 days); the `--soft-delete-duration=0` above is optional.

Look at it before you lock it:

```sh
gcloud storage buckets describe gs://$BUCKET
```

Check the project number, the retention period (1 day), versioning, the default key, uniform access and
public access prevention. **Then, and only then, lock the policy. This is the irreversible step.**

```sh
gcloud storage buckets update gs://$BUCKET --lock-retention-period   # asks y/N; --quiet answers no
```

## 2. Roles for the run

Your own (operator) credentials need enough to create the bucket and, for the final probe, to attempt a
delete, which is expected to be refused. To run the Anchor and the Reader as the shipped roles, see
"Running as the shipped roles" below.

## 3. Run the test

```sh
cd anchor/gcs
BIDE_GCS_BUCKET=$BUCKET \
BIDE_GCS_PROJECT_NUMBER=<number> \
BIDE_GCS_KMS_KEY=$KEY \
BIDE_GCS_RETENTION_HOURS=24 \
GOWORK=off go test -run TestRealGCS -v -count=1 ./
```

Optional: `BIDE_GCS_OBJECT_RETENTION=1` (needs the bucket created with object retention) and
`BIDE_GCS_LEGAL_HOLD=1`. A hold has no end date and the test cannot release it (the writer role must not
be able to): release it with a separate identity holding the hold-releaser role
(`gcloud storage objects update gs://$BUCKET/NAME --no-temporary-hold`) or the bucket cannot be emptied.

The test writes under a unique `bide-anchor-it/<random>/` prefix, republishes a head (the create-only
412 path), reads the heads back, audits them, tries to delete a retained generation (and fails the test
if that succeeds), deletes the live object, and checks the Reader still returns the head. Every `t.Logf`
line is part of the record: keep the output.

## What the run settled

The run of 2026-10-17 (one scratch project, europe-west1, gcloud 589.0.0, `cloud.google.com/go/storage`
v1.69.0, a locked 24-hour policy, default soft delete of 7 days) answered the items that were unverified.
KNOWN-LIMITATIONS lists what is still not verified.

| Item | Result |
| --- | --- |
| An object's key has the form `<key>/cryptoKeys/.../cryptoKeyVersions/N` | Yes: `.../cryptoKeyVersions/1`. The package matches on `<key>/cryptoKeyVersions/`. |
| A create with a temporary hold needs no permission beyond the writer role | Confirmed: a service account holding only the writer role placed and read back a hold. |
| The callers need no Cloud KMS role (the service agent does the work) | Confirmed: the writer and reader service accounts held no Cloud KMS role and published, read back and audited CMEK-encrypted heads. |
| A create that sets a per-object retention needs only the writer role | **No.** It is refused with a 403 naming `storage.objects.setRetention`. `gcs.WriterObjectRetentionRole` (the writer plus that permission) exists for `Config.ObjectRetention`. |
| A retained generation cannot be deleted | Confirmed: HTTP 403. |
| Deleting the live object only makes it noncurrent | Confirmed, and the audit then lists the noncurrent generation. With a temporary hold on it the delete is refused (403, "cannot be deleted, overwritten or archived until hold is removed"). |
| What soft delete does to a locked object | A live delete produced a noncurrent generation; `gcloud storage ls --soft-deleted` listed nothing. Behaviour at expiry was not observed. |
| The hold releaser role is enough to release a hold, and cannot delete | Confirmed with a service account holding only that role. |
| The lien | Cloud Storage created a lien on `resourcemanager.projects.delete` (origin `storage.googleapis.com`) one second after the bucket was created with a retention policy, not when the policy was locked. Not removed; project deletion not attempted. |

### Evidence for the table

Quoted from the run's output with every account name removed (service accounts of the scratch project are
shown by role name only). Times are UTC, 2026-10-17.

- **Lien** (Cloud Resource Manager `liens` listing for the project, read after the bucket was locked):
  `"restrictions": ["resourcemanager.projects.delete"]`, `"origin": "storage.googleapis.com"`,
  `"reason": "A lien is put on project deletion because one or more Cloud Storage resources were put under retention."`,
  `"createTime": "22:32:58"`. The bucket was created at 22:32:57 with a 1-day policy and locked later, so the
  lien appeared with the policy, not with the lock.
- **Soft delete:** after the live object of a retained head was deleted (default 7-day soft delete on),
  `gcloud storage ls --soft-deleted -r "gs://BUCKET/**"` printed
  `ERROR: (gcloud.storage.ls) One or more URLs matched no objects.`, while `ls --all-versions` still listed the
  generations.
- **Hold releaser:** with a service account holding only the hold-releaser role, `gcloud storage objects update
  --no-temporary-hold` patched both held generations (the object's `temporary_hold` read `True` before and
  empty after), and `gcloud storage rm` of a retained generation as that identity was refused with
  `HTTP 403 ... does not have storage.objects.delete access to the Google Cloud Storage object`.
- **Service-account runs:** `TestRealGCS` passed with the Anchor running as a service account holding only
  the writer role and the Reader as one holding only the reader role, with no Cloud KMS role on either
  (`--- PASS: TestRealGCS`, after the bucket-level binding had propagated; the first attempts failed with
  `iam.serviceAccounts.getAccessToken` denied until the caller had token-creator on the service accounts).
  With per-object retention the same run failed with `403 ... does not have storage.objects.setRetention
  access` until the object-retention writer role was bound.

The transcripts themselves are not committed (they hold project identifiers); if you repeat the run, keep
yours next to the test output.

What differed from the runbook as first written, now corrected above or below:

- `gcloud storage service-agent --authorize-cmek` fails on a new project until the agent exists: run
  `gcloud storage service-agent --project=P` first.
- `gcloud storage buckets update --lock-retention-period` asks "Do you want to continue (y/N)?" and
  `--quiet` answers **no** (it aborts); answer `y` interactively (`echo y | gcloud ...` in a script).
- `gcloud alpha resource-manager liens list` needs the alpha component and the Cloud Resource Manager API
  enabled in the project; the same list is `GET https://cloudresourcemanager.googleapis.com/v1/liens?parent=projects/NUMBER`.
- A new service account and a new custom role take up to a minute to be usable in an IAM binding ("does not exist").
- Impersonation needs `roles/iam.serviceAccountTokenCreator` for the caller on the service account, and
  the Application Default Credentials may belong to a different account than `gcloud`'s.

### Running as the shipped roles

To check that the roles in `anchor/gcs/iam` are enough, create a service account per role, create the
roles (`gcloud iam roles create bideAnchorWriter --project=P --file=anchor/gcs/iam/writer.json`, and the
reader, hold-releaser and writer-object-retention files), bind each at the bucket, grant yourself
`roles/iam.serviceAccountTokenCreator` on them, and set `BIDE_GCS_WRITER_SA` and `BIDE_GCS_READER_SA`:
the Anchor and the Reader then run as those identities while the bucket probes use your own credentials.

## Teardown (after the retention ends)

1. Wait until 24 hours after the last object was written (the test's last `publish`).
2. Delete the objects, then the bucket: `gcloud storage rm --recursive gs://$BUCKET` (this needs
   `storage.objects.delete`, and fails with a retention message before the period ends; that failure is
   expected and means the lock works).
3. Schedule the key version for destruction.
4. The lien: while the bucket exists, Cloud Storage keeps a lien on deleting the project. Once the bucket
   is gone, delete the project. If you must delete the project while the lien is held, list and remove
   it deliberately (`gcloud alpha resource-manager liens list --project=$PROJECT`), knowing what that
   means for the locked data.

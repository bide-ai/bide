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

Nothing here has been run by the authors of this runbook; the `gcloud` flags below are written from the
documentation and must be checked with `--help` against your installed version before you run them. The
test's job is to settle the items under "What the run settles".

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

# Let the project's Cloud Storage service agent use the key.
gcloud storage service-agent --project=$PROJECT --authorize-cmek=$KEY

gcloud storage buckets create gs://$BUCKET --project=$PROJECT --location=$LOC \
  --uniform-bucket-level-access --public-access-prevention \
  --default-encryption-key=$KEY --soft-delete-duration=0 \
  --retention-period=1d
gcloud storage buckets update gs://$BUCKET --versioning
```

Add `--enable-per-object-retention` to the create command only if you also want to run with
`BIDE_GCS_OBJECT_RETENTION=1`; it cannot be added through gRPC and, once on, cannot be turned off.

Look at it before you lock it:

```sh
gcloud storage buckets describe gs://$BUCKET
```

Check the project number, the retention period (1 day), versioning, the default key, uniform access and
public access prevention. **Then, and only then, lock the policy. This is the irreversible step.**

```sh
gcloud storage buckets update gs://$BUCKET --lock-retention-period
```

## 2. Roles for the run

Create the writer and reader roles from the files the package ships (`anchor/gcs/iam/*.json`, also in
the API reference), bind the writer role to your identity on the bucket, and grant `storage.objects.delete`
too (the final probe tries to delete, and expects to be refused):

```sh
gcloud iam roles create bideAnchorWriter --project=$PROJECT --file=anchor/gcs/iam/writer.json
```

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

## What the run settles

These are the details the package could not verify from documentation. KNOWN-LIMITATIONS lists each as
unverified until someone has run this and recorded the answer.

| Item | Where the test shows it |
| --- | --- |
| An object's `KMSKey` has the form `<key>/cryptoKeyVersions/N` (the package matches on that prefix) | `Publish` succeeds against the real bucket; the `object:` log line shows the value |
| Creating with a temporary hold needs no permission beyond the writer role | `BIDE_GCS_LEGAL_HOLD=1` run succeeds |
| What soft delete does to a locked object | the `bucket:` log line shows the policy; the bucket in this runbook turns it off |
| A retained generation cannot be deleted (HTTP 403 with a retention message) | `delete of a retained generation refused` |
| Deleting the live object only makes it noncurrent | the audit after the delete lists a noncurrent generation |
| The writer's callers need no Cloud KMS permission themselves (the service agent does the work) | the run uses an identity with no KMS role |

If any of these differs from what the package assumes, the package or its documentation changes before
release; do not edit the test to match.

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

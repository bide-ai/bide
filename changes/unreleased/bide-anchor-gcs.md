Section: Added

bide-anchor reads the Google Cloud Storage anchor with -backend gcs, taking -project (the project number) in place of -owner: verify and export work offline on a downloaded copy, and check and export read the bucket with read-only credentials, with the same exit codes as for S3 (1 for a finding, including a generation that vanished between the listing and the read, 3 when the audit could not finish, never 0 after an aborted audit). The command module now requires anchor/gcs, and a test checks the S3 and GCS object layouts cannot take each other's objects.

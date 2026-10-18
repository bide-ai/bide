Section: Changed

The Google Cloud Storage real-run runbook and known limitations now record the teardown of the 2026-10-17 run: a delete after the retention ends goes through soft delete (the objects stay listed for the soft delete period), a bucket whose objects are only soft-deleted can be deleted, the Cloud Storage lien on project deletion is not removed when the bucket is deleted and has to be removed by hand before the project can be deleted, and the key version is destroyed after its 30-day delay while the key and key ring remain.

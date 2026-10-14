Section: Changed

The evidence format is `bide.audit.evidence.v6`: its seal signs the tag and the SHA-256 of the package (60 bytes) instead of the whole JSON, so a KMS with a 4096-byte limit can seal a package of any size. A v5 package still verifies under its original seal (`bide audit verify-evidence` prints a NOTE), `Seal` refuses to re-seal one, and v4 and older are refused with `ErrFormat` as before.

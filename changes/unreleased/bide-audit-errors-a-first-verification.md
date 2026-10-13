Section: Changed

`bide audit` errors a first verification meets say what to do next: a missing evidence file, a `-pubkey` that is not a key (it takes `ed25519:<hex>`, 64 hex digits or a file such as `keys/log.pub`), a file that is not the JSON a flow or `bide audit` writes (such as the journal database), and an evidence package that did not verify (check the key came from out of band and the file is unmodified). Exit statuses are unchanged.

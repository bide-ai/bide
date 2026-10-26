Section: Fixed

`bide migrate -h` now lists everything the migration creates: it named the tables, `bide_next_seq_v1` and the lease indexes, but left out the `bide_holds` table and the `bide_redact_v3` function (which the Postgres guide already said it creates), and the `EXECUTE` revokes on older redaction functions. Help text only; the migration is unchanged.

# The Postgres store

`store/postgres` is the journal for agents that run on more than one node: any node can resume any run, a per-run lease keeps two nodes from driving the same run, and every write is one statement that commits before it returns. This guide covers running it with the least privilege a database team will accept: an owner role migrates the schema, and the running service has no DDL rights.

For what the store guarantees under stalls, isolation levels and retries, see the package documentation (`go doc github.com/bide-ai/bide/store/postgres`).

## Open and migrate

<!-- docsnip: skip the store/postgres adapter is a separate module the snippet checker does not load -->
```go
s, err := postgres.Open(ctx, dsn, postgres.WithSchema("bide"))
```

`Open` creates what is missing on every call: the tables `bide_steps`, `bide_leases`, `bide_events` and `bide_schema_version` (the prefix is `WithTablePrefix`), the function `bide_next_seq_v1`, and two lease indexes. It sends `CREATE TABLE IF NOT EXISTS` even when the tables exist, and Postgres checks `CREATE` on the schema for that statement, so a role that opens the store this way needs `CREATE` on the schema. That is convenient for development and for a single-team deployment. It is more than a service that only reads and appends needs.

For production under separation of duties, run the migration separately, as the schema's owner, and open the store without it:

<!-- docsnip: skip the store/postgres adapter is a separate module the snippet checker does not load -->
```go
// at deploy time, as the owner role
res, err := postgres.Migrate(ctx, ownerDSN, postgres.WithSchema("bide"))

// in the service, as the application role
s, err := postgres.Open(ctx, appDSN, postgres.WithSchema("bide"), postgres.WithoutMigration())
```

`postgres.Migrate` (and `MigrateDB` over a pool you own) does the migration `Open` would have done and reports `MigrationResult`: the schema version before (0 for a new database) and after, what it created, and the SQL it ran. `postgres.WithDryRun()` runs it in a transaction and rolls back, so you see the SQL and learn that the role may run it without changing anything.

`WithoutMigration` runs no DDL and no `INSERT`. It verifies what the migration would have created and refuses clearly when it is not there:

| Condition | Error |
|---|---|
| Pinned schema, a table, the `bide_next_seq_v1` function or a lease index is missing | wraps `postgres.ErrSchemaMissing` and `agent.ErrConfig` |
| Schema version other than this package's | wraps `postgres.ErrSchemaVersion` and `agent.ErrConfig` |
| A table lacks a uniqueness the store depends on, or `bide_next_seq_v1` is not the function this version creates | `agent.ErrConfig`, as for `Open` |

The migration never alters or drops an existing table and never replaces a function, and `Migrate` is idempotent: on a current schema it creates nothing. After you upgrade bide, run `Migrate` (or `bide migrate`) as the owner before starting the new version; a service on `WithoutMigration` then finds the schema it expects, or stops with the error above instead of running on a schema it did not check.

## `bide migrate`

```sh
export BIDE_OWNER_DSN='postgres://bide_owner:...@db:5432/app?sslmode=verify-full'
bide migrate -postgres-env BIDE_OWNER_DSN -schema bide -dry-run   # print the SQL, change nothing
bide migrate -postgres-env BIDE_OWNER_DSN -schema bide
```

The DSN comes from the environment variable you name and is never an argument and never printed, including in errors. `-dry-run` prints the statements and rolls back. The command prints the schema version before and after and each object it created, and exits 0 when the schema is current, 1 when the migration failed, and 2 for a usage error. The schema must exist: neither `Open` nor `Migrate` creates it. See the [CLI reference](../reference/cli.md#bide-migrate).

## Roles

Pin the schema with `WithSchema`: the store then trusts the schema's owner and any role that can create objects in it, and the search path plays no part.

| Role | Privileges |
|---|---|
| `bide_owner` | Owns the schema, the tables and `bide_next_seq_v1`. Runs `bide migrate`. Not used by the running service |
| `bide_app` | `USAGE` on the schema. `SELECT`, `INSERT` on `bide_steps` and `bide_events`. `SELECT`, `INSERT`, `UPDATE`, `DELETE` on `bide_leases`. `SELECT` on `bide_schema_version`. `EXECUTE` on `bide_next_seq_v1(text)`. **No `CREATE`** |
| `bide_reader` | `USAGE` on the schema, `SELECT` on the four tables; opens the store with `postgres.OpenReadOnly` |

```sql
CREATE SCHEMA bide AUTHORIZATION bide_owner;
-- after bide migrate has run as bide_owner:
GRANT USAGE ON SCHEMA bide TO bide_app, bide_reader;
GRANT SELECT, INSERT ON bide.bide_steps, bide.bide_events TO bide_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON bide.bide_leases TO bide_app;
GRANT SELECT ON bide.bide_schema_version TO bide_app;
REVOKE ALL ON FUNCTION bide.bide_next_seq_v1(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION bide.bide_next_seq_v1(text) TO bide_app;
GRANT SELECT ON bide.bide_steps, bide.bide_events, bide.bide_leases, bide.bide_schema_version TO bide_reader;
```

`TestWithoutMigration_LeastPrivilege` (`store/postgres/nomigrate_test.go`, run under the Integration job's Postgres) exercises this split: the application role has no `CREATE`, opens before the migration and gets `ErrSchemaMissing`, opens after it and drives a journal write, an attempt claim, a lease (acquire, renew, release), a run listing and the event store, and gets `ErrSchemaVersion` on a version mismatch. The journal tables take inserts only, so the application role has no `UPDATE` or `DELETE` on `bide_steps` or `bide_events`; the lease table is the one it updates and deletes.

Creating the lease indexes on a database from v0.9.0 takes the table's owner, so the first `Migrate` after that upgrade must run as the owner role.

See [Security and retention guidance](../trust/operations.md) for key custody, retention and database audit logging around these roles.

## The governed event log

`govern/postgreslog` has the same split: the owner runs `postgreslog.Migrate(ctx, ownerDSN, postgreslog.WithSchema("bide"))`, and the service opens the log with `postgreslog.WithoutMigration()`. Open then creates nothing and refuses a schema without `governed_events` (and its `append_id` index) or the `governed_events_next_seq_v1` function with `postgreslog.ErrSchemaMissing` (also `agent.ErrConfig`). The log records its schema version (1) in `governed_events_schema_version`, like the store: a different version fails with `postgreslog.ErrSchemaVersion`, and a newer one is refused with the reason (a newer bide wrote it) by `Migrate` and `Open` alike. A log created before that table existed is version 1: the service opens it `WithoutMigration` as it is, and `Migrate` adds the table and records 1. The service role needs `USAGE` on the schema, `SELECT` and `INSERT` on `governed_events`, `SELECT` on `governed_events_schema_version` and `EXECUTE` on the function. `bide migrate` covers the journal store only; migrate the log through `postgreslog.Migrate`.

## What this does not cover

The SQLite store has no equivalent: a SQLite database is a file, so the process that writes the journal needs write access to that file, and there is no separate DDL privilege to withhold. Use `sqlite.OpenReadOnly` for readers.

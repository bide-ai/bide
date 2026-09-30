package postgres

// The next_seq lookup in migrate must see a function another node created while this one waited
// for the migration lock, even on a connection whose catalog cache looked the function up before
// and found it missing: the cache is not refreshed by taking an advisory lock, so the lookup reads
// pg_proc with the statement's snapshot. The test kills the mutant that asks to_regprocedure.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestPostgres_MigrateSeesNextSeqCreatedWhileWaiting(t *testing.T) {
	for _, tablesFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("tablesFirst=", tablesFirst), func(t *testing.T) {
			ctx := context.Background()
			dsn, schema, admin := freshSchema(t)
			def := nextSeqDefinition(t, schema) // before another session holds the migration lock
			if tablesFirst {
				if _, err := admin.ExecContext(ctx, fmt.Sprintf(completeTables, schema)+
					fmt.Sprintf(`CREATE TABLE %[1]s.bide_schema_version (id int PRIMARY KEY CHECK (id = 1), version int NOT NULL); INSERT INTO %[1]s.bide_schema_version VALUES (1, 1);`, schema)); err != nil {
					t.Fatal(err)
				}
			}
			db, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)
			// Warm the one connection's catalog cache with the function missing.
			var missing bool
			if err := db.QueryRowContext(ctx, `SELECT to_regprocedure('bide_next_seq_v1(text)') IS NULL AND to_regprocedure('`+schema+`.bide_next_seq_v1(text)') IS NULL`).Scan(&missing); err != nil || !missing {
				t.Fatalf("warm-up: missing=%v err=%v", missing, err)
			}
			// And with migrate's own lookup, so the connection holds its prepared statement and a
			// warm catalog cache, as a pooled connection that ran an earlier Open does.
			var present bool
			if err := db.QueryRowContext(ctx, nextSeqPresent, "bide_next_seq_v1", schema).Scan(&present); err != nil || present {
				t.Fatalf("warm-up lookup: present=%v err=%v", present, err)
			}
			other, err := admin.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			if err != nil {
				t.Fatal(err)
			}
			defer other.Rollback()
			if _, err := other.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLock); err != nil {
				t.Fatal(err)
			}
			if _, err := other.ExecContext(ctx, def); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				s, err := New(ctx, db)
				if err == nil {
					_, _, err = s.Insert(ctx, "r", "a", []byte("a"))
				}
				done <- err
			}()
			rvWaitLock(t, admin, `$1::bigint`, int64(migrateLock))
			if err := other.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("New after another node created the function while it waited: %v", err)
			}
		})
	}
}

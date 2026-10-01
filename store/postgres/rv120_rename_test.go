package postgres

import (
	"context"
	"testing"
)

// The v1 body names its schema. After ALTER SCHEMA ... RENAME, every insert fails and Open
// refuses the function; the way out is DROP FUNCTION and Open again (the migration recreates it).
func TestRV120_SchemaRenameBreaksNextSeq(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	rv120Schema(t, admin, "rv120ren_old")
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS rv120ren_new CASCADE`) })
	s, err := Open(ctx, rv120DSN(t, base, "", "rv120ren_old"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	rv120Exec(t, admin, `ALTER SCHEMA rv120ren_old RENAME TO rv120ren_new`)
	s, err = Open(ctx, rv120DSN(t, base, "", "rv120ren_new"))
	t.Logf("Open after rename: %v", err)
	if err == nil {
		defer s.Close()
		_, _, err = s.Insert(ctx, "r", "a", nil)
		t.Logf("Insert after rename: %v", err)
	}
	rv120Exec(t, admin, `DROP FUNCTION rv120ren_new.bide_next_seq_v1(text)`)
	s, err = Open(ctx, rv120DSN(t, base, "", "rv120ren_new"))
	if err != nil {
		t.Fatalf("Open after DROP FUNCTION: %v", err)
	}
	defer s.Close()
	if _, _, err := s.Insert(ctx, "r", "a", []byte("a")); err != nil {
		t.Fatalf("Insert after DROP FUNCTION and Open: %v", err)
	}
}

package postgres

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// newSelect accepts only a query that starts with the SELECT keyword, so the statement check can
// trust a selectSQL on the pool.
func TestNewSelect(t *testing.T) {
	for _, q := range []string{"SELECT 1", "  select x FROM t", "SELECT\n1"} {
		if got, err := newSelect(q); err != nil || string(got) != q {
			t.Errorf("newSelect(%q) = %q, %v", q, got, err)
		}
	}
	for _, q := range []string{"UPDATE t SET x = 1", "SELECTED", "", "WITH x AS (DELETE FROM t) SELECT 1", "SELECT"} {
		if _, err := newSelect(q); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("newSelect(%q) = %v, want ErrConfig", q, err)
		}
	}
}

// newWrite accepts only one INSERT, UPDATE or DELETE statement, so the statement check can trust a
// writeSQL on the pool to be a single statement, which Postgres runs as a transaction of its own.
func TestNewWrite(t *testing.T) {
	for _, q := range []string{"INSERT INTO t VALUES (1)", "  update t SET x = 1", "DELETE\nFROM t"} {
		if got, err := newWrite(q); err != nil || string(got) != q {
			t.Errorf("newWrite(%q) = %q, %v", q, got, err)
		}
	}
	for _, q := range []string{"SELECT 1", "BEGIN", "INSERTED", "", "UPDATE", "DELETE FROM t; COMMIT", "UPDATE t SET x = ';'"} {
		if _, err := newWrite(q); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("newWrite(%q) = %v, want ErrConfig", q, err)
		}
	}
}

package postgres

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// newSelect accepts only a query that starts with the SELECT keyword, so the isolation check can
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

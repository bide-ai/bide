package postgres

import (
	"regexp"
	"strings"
	"testing"
)

// The next_seq body calls only pg_catalog-qualified built-ins and reads the schema-qualified steps
// table, independently of the function's SET search_path: each defends against an overload on the
// search path by itself, and each is held here on its own.
func TestNextSeqBodyQualifiesEveryName(t *testing.T) {
	tb, err := newTables("app_", "My\"Schema")
	if err != nil {
		t.Fatal(err)
	}
	body := tb.nextSeqBody(`My"Schema`)
	for _, m := range regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_$.]*)\s*\(`).FindAllStringSubmatch(body, -1) {
		if name := strings.ToLower(m[1]); name != "coalesce" && name != "return" && !strings.HasPrefix(name, "pg_catalog.") {
			t.Errorf("next_seq calls %s, not qualified with pg_catalog:\n%s", m[1], body)
		}
	}
	if !strings.Contains(body, `FROM "My""Schema".app_steps `) {
		t.Errorf("next_seq does not read the schema-qualified steps table:\n%s", body)
	}
	if !strings.Contains(body, "0::pg_catalog.int8") {
		t.Errorf("next_seq's lock key does not pass a bigint 0 to hashtextextended:\n%s", body)
	}
}

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

// The next_seq body writes every operator OPERATOR(pg_catalog.<op>), unary minus included, besides
// running with SET search_path = pg_catalog, pg_temp: the body's text holds no bare operator.
func TestNextSeqBodyHasNoBareOperator(t *testing.T) {
	tb, err := newTables("app_", "app")
	if err != nil {
		t.Fatal(err)
	}
	if bare := bareOperators(t, tb.nextSeqBody("app")); len(bare) > 0 {
		t.Errorf("next_seq's body holds bare operators %q:\n%s", bare, tb.nextSeqBody("app"))
	}
}

// bareOperators returns the operators in sql that are not written OPERATOR(pg_catalog.<op>).
func bareOperators(t *testing.T, sql string) []string {
	t.Helper()
	toks, err := sqlTokens(strings.NewReplacer(";", " ", ":=", " ").Replace(sql)) // plpgsql's statement ends
	if err != nil {
		t.Fatalf("tokenize the body: %v", err)
	}
	var bare []string
	for i, tok := range toks {
		if tok.kind == 'o' && !(isTok(toks, i-1, 'p', ".") && isTok(toks, i-2, 'i', "pg_catalog") && isTok(toks, i-4, 'i', "operator")) {
			bare = append(bare, tok.text)
		}
	}
	return bare
}

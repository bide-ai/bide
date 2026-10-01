package postgreslog

import (
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// WithSchema refuses the system schemas, information_schema and every name starting with pg_
// (pg_catalog, pg_toast, pg_temp and a session's pg_temp_N among them), with ErrConfig.
func TestWithSchemaRefusesSystemSchemas(t *testing.T) {
	for _, name := range []string{"information_schema", "pg_catalog", "pg_toast", "pg_temp", "pg_temp_3", "pg_toast_temp_1", "pg_anything", "pg_"} {
		var c config
		if err := WithSchema(name).apply(&c); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("WithSchema(%q) = %v, want ErrConfig", name, err)
		}
	}
	for _, name := range []string{"app", "Pg_upper", "information_schema2", "my pg_x"} {
		var c config
		if err := WithSchema(name).apply(&c); err != nil || c.schema != name {
			t.Errorf("WithSchema(%q) = %v, schema %q", name, err, c.schema)
		}
	}
}

// The next_seq body writes every operator OPERATOR(pg_catalog.<op>), unary minus included, besides
// running with SET search_path = pg_catalog, pg_temp: the body's text holds no bare operator.
func TestNextSeqBodyHasNoBareOperator(t *testing.T) {
	body := nextSeqBody("app")
	toks, err := sqlTokens(strings.NewReplacer(";", " ", ":=", " ").Replace(body)) // plpgsql's statement ends
	if err != nil {
		t.Fatalf("tokenize the body: %v", err)
	}
	for i, tok := range toks {
		if tok.kind == 'o' && !(isTok(toks, i-1, 'p', ".") && isTok(toks, i-2, 'i', "pg_catalog") && isTok(toks, i-4, 'i', "operator")) {
			t.Errorf("next_seq's body holds the bare operator %q:\n%s", tok.text, body)
		}
	}
}

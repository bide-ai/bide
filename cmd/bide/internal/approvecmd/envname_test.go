package approvecmd

import (
	"io"
	"strings"
	"testing"
)

// TestPostgresEnvIsNotADSN: a DSN given where the variable's name belongs is refused with a usage
// error that does not echo it (it holds a password).
func TestPostgresEnvIsNotADSN(t *testing.T) {
	for _, v := range []string{"postgres://u:S3cr3tPw@127.0.0.1:1/db", "a b", "1x", "x=y"} {
		var errb strings.Builder
		code := Main([]string{"-postgres-env", v, "-list"}, strings.NewReader(""), io.Discard, &errb, Stores{})
		if code != 2 || !strings.Contains(errb.String(), "-postgres-env takes the name of an environment variable, not a DSN") || strings.Contains(errb.String(), "S3cr3tPw") || (strings.Contains(v, "://") && strings.Contains(errb.String(), v)) {
			t.Errorf("%q: exit %d, stderr %q", v, code, errb.String())
		}
	}
}

package runview

import (
	"bytes"
	"strings"
	"testing"
)

// A DSN given where the variable's name belongs must not be echoed (it holds a password).
func TestReviewPostgresEnvDoesNotEchoADSN(t *testing.T) {
	var out, errb bytes.Buffer
	Main([]string{"-postgres-env", "postgres://u:S3cr3tPw@127.0.0.1:1/db"}, &out, &errb, Stores{})
	if strings.Contains(errb.String(), "S3cr3tPw") {
		t.Errorf("stderr echoes the password: %s", errb.String())
	}
}

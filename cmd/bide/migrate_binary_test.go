//go:build !bide_audit_only

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// bide migrate (and the other commands that take -postgres-env) through the real binary: -schema
// is required, and a connection failure never shows
// the password of the DSN in the environment, in the URL or the keyword/value form.
func TestMigrateBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	exe := filepath.Join(t.TempDir(), "bide")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	run := func(env string, args ...string) (code int, stdout, stderr string) {
		t.Helper()
		cmd := exec.Command(exe, append([]string{"migrate"}, args...)...)
		cmd.Env = append(os.Environ(), "BIDE_TEST_DSN="+env)
		var o, e bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &e
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, o.String(), e.String()
	}

	// A DSN given as -postgres-env's value is refused as a usage error without echoing it, by every
	// command that takes the flag.
	for _, args := range [][]string{
		{"migrate", "-schema", "bide"},
		{"approve", "-list"},
		{"doctor", "-offline"},
	} {
		for _, dsn := range []string{"postgres://u:hunter2@127.0.0.1:1/db", "host=h password=hunter2 dbname=db", "pw=hunter2"} {
			cmd := exec.Command(exe, append(args, "-postgres-env", dsn)...)
			var o, e bytes.Buffer
			cmd.Stdout, cmd.Stderr = &o, &e
			err := cmd.Run()
			ee, ok := err.(*exec.ExitError)
			if !ok || ee.ExitCode() != 2 || strings.Contains(o.String()+e.String(), "hunter2") || !strings.Contains(e.String(), "takes the name of an environment variable, not a DSN") {
				t.Errorf("bide %s -postgres-env %q: %v, stdout %q, stderr %q", args[0], dsn, err, o.String(), e.String())
			}
		}
	}

	code, out, errOut := run("postgres://u:hunter2@127.0.0.1:1/db", "-postgres-env", "BIDE_TEST_DSN")
	if code != 2 || out != "" || !strings.Contains(errOut, "-schema is required") || strings.Contains(errOut, "hunter2") {
		t.Fatalf("no -schema: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	for _, dsn := range []string{
		"postgres://u:hunter2@127.0.0.1:1/db?sslmode=disable&connect_timeout=2",
		"host=127.0.0.1 port=1 user=u password=hunter2 dbname=db sslmode=disable connect_timeout=2",
		"host=127.0.0.1 port=1 user=u password='hun ter2' dbname=db sslmode=disable connect_timeout=2",
	} {
		code, out, errOut = run(dsn, "-postgres-env", "BIDE_TEST_DSN", "-schema", "bide")
		if code != 1 || out != "" || errOut == "" || strings.Contains(errOut, "hunter2") || strings.Contains(errOut, "hun ter2") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", dsn, code, out, errOut)
		}
	}
	if code, _, errOut = run("x", "-postgres-env", "BIDE_TEST_DSN", "-schema", "bide", "postgres://u:hunter2@h/db"); code != 2 || strings.Contains(errOut, "hunter2") {
		t.Errorf("a DSN argument: exit %d, stderr %q", code, errOut)
	}
}

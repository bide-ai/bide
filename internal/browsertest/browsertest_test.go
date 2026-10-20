package browsertest

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for Node: with BROWSERTEST_HELPER set it runs the named
// behaviour instead of the tests.
func TestMain(m *testing.M) {
	switch os.Getenv("BROWSERTEST_HELPER") {
	case "":
		os.Exit(m.Run())
	case "ok":
		os.Stdout.WriteString(`{"ok":true}` + "\n")
		os.Exit(0)
	case "fail":
		os.Stderr.WriteString("boom\n")
		os.Exit(3)
	case "stall":
		os.Stdout.WriteString("partial\n")
		os.Stderr.WriteString("waiting for chrome\n")
		stall()
	}
}

func run(t *testing.T, mode string, d time.Duration) ([]byte, error, time.Duration) {
	t.Helper()
	start := time.Now()
	out, err := RunFor(d, os.Args[0], "", append(os.Environ(), "BROWSERTEST_HELPER="+mode))
	return out, err, time.Since(start)
}

func TestRunReturnsStdout(t *testing.T) {
	out, err, _ := run(t, "ok", time.Minute)
	if err != nil || strings.TrimSpace(string(out)) != `{"ok":true}` {
		t.Errorf("out %q, err %v", out, err)
	}
}

func TestRunShowsStderrOfAFailingScript(t *testing.T) {
	_, err, _ := run(t, "fail", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want one that quotes stderr", err)
	}
}

// A script that outlives its deadline is killed, fast, and the error says where it had got to.
func TestRunKillsAStalledScript(t *testing.T) {
	out, err, took := run(t, "stall", 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "was still running after 500ms") || !strings.Contains(err.Error(), "waiting for chrome") || !strings.Contains(err.Error(), "partial") {
		t.Errorf("err = %v, want a deadline error quoting stderr and stdout", err)
	}
	if !strings.Contains(string(out), "partial") {
		t.Errorf("stdout %q lost what the script had printed", out)
	}
	if took > 10*time.Second {
		t.Errorf("the stalled script held the test for %v", took)
	}
}

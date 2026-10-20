package browsertest

import (
	"os"
	"path/filepath"
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
	case "detached":
		detached()
	case "sigterm":
		terminable()
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
	if !TimedOut(err) || !strings.Contains(err.Error(), "was still running after 500ms") || !strings.Contains(err.Error(), "waiting for chrome") || !strings.Contains(err.Error(), "partial") {
		t.Errorf("err = %v, want a deadline error quoting stderr and stdout", err)
	}
	if !strings.Contains(string(out), "partial") {
		t.Errorf("stdout %q lost what the script had printed", out)
	}
	if took > 10*time.Second {
		t.Errorf("the stalled script held the test for %v", took)
	}
}

// A script that handles SIGTERM, as puppeteer's handler does, gets to run it before it is killed.
func TestRunAsksTheScriptToStopFirst(t *testing.T) {
	out, err, took := run(t, "sigterm", 700*time.Millisecond)
	if !TimedOut(err) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if !strings.Contains(string(out), "closing chrome on SIGTERM") || !strings.Contains(err.Error(), "closing chrome on SIGTERM") {
		t.Errorf("the script was not sent SIGTERM before it was killed: stdout %q", out)
	}
	if took > 10*time.Second {
		t.Errorf("held the test for %v", took)
	}
}

// Profile directory names have one length and are never reused, so one run's cannot be a prefix of
// another's.
func TestProfilesAreFixedLengthAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		d, err := newProfile()
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(d)
		if seen[d] {
			t.Fatalf("%s was made twice", d)
		}
		seen[d] = true
		if len(filepath.Base(d)) != len("bide-chrome-")+16 {
			t.Errorf("%s: not a fixed-length name", d)
		}
	}
}

// A name that is already taken is not reused: the second run draws again.
func TestProfileNamesAreNeverReused(t *testing.T) {
	old := randRead
	defer func() { randRead = old }()
	calls := 0
	randRead = func(b []byte) (int, error) {
		calls++
		for i := range b {
			b[i] = 0xab
			if calls > 2 { // the third draw is different
				b[i] = 0xcd
			}
		}
		return len(b), nil
	}
	first, err := newProfile()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(first)
	second, err := newProfile()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(second)
	if first == second {
		t.Errorf("the name %s was handed out twice", first)
	}
	if calls != 3 {
		t.Errorf("%d draws, want 3: the taken name should have been drawn and refused", calls)
	}
}

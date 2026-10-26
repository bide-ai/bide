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

// killSlack is how long past its deadline a run may take before the test calls the kill broken. The
// stalled helper sleeps for an hour, so a kill that does not work holds Run for that hour; any limit
// far below it tells the two apart. It is generous because the elapsed time of a correct kill is
// dominated by the runner, not the code: a macOS CI runner took 18s for a 3s deadline (process starts
// and pgrep/pkill scans stall under load), and a tight bound failed unrelated pull requests.
const killSlack = 90 * time.Second

func run(t *testing.T, mode string, d time.Duration) ([]byte, error, time.Duration) {
	t.Helper()
	type result struct {
		out []byte
		err error
	}
	start := time.Now()
	c := make(chan result, 1)
	go func() {
		out, err := RunFor(d, os.Args[0], "", append(os.Environ(), "BROWSERTEST_HELPER="+mode))
		c <- result{out, err}
	}()
	select {
	case r := <-c:
		return r.out, r.err, time.Since(start)
	case <-time.After(d + killSlack):
		t.Fatalf("RunFor with a %v deadline was still running after %v: the stalled script was not killed", d, d+killSlack)
		return nil, nil, 0
	}
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
	out, err, _ := run(t, "stall", 3*time.Second)
	if !TimedOut(err) || !strings.Contains(err.Error(), "was still running after 3s") || !strings.Contains(err.Error(), "waiting for chrome") || !strings.Contains(err.Error(), "partial") {
		t.Errorf("err = %v, want a deadline error quoting stderr and stdout", err)
	}
	if !strings.Contains(string(out), "partial") {
		t.Errorf("stdout %q lost what the script had printed", out)
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

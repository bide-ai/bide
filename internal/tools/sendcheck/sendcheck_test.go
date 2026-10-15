package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

const bad = `package p

import "sync/atomic"

var inFlight atomic.Int32

func start(out chan int) {
	inFlight.Add(1)
	go func() {
		defer inFlight.Add(-1)
		out <- 1
	}()
}
`

func TestFlagsASendBeforeADeferredDecrement(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", bad)
	var stderr bytes.Buffer
	if code := check(dir, "", &stderr); code != 1 || !strings.Contains(stderr.String(), "inFlight.Add(-1)") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestFlagsANamedGoroutineAndATestFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a_test.go", `package p

func work(out chan int, release func()) {
	defer release()
	out <- 1
}

func start(out chan int) { go work(out, func() {}) }
`)
	var stderr bytes.Buffer
	if code := check(dir, "", &stderr); code != 1 || !strings.Contains(stderr.String(), "release()") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestSignalsAreNotFindings(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", `package p

import (
	"context"
	"sync"
)

func start(out chan int, done chan struct{}) {
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	_ = ctx
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		defer close(done)
		out <- 1
	}()
	wg.Wait()
}

func ordered(out chan int, release func()) {
	go func() {
		release() // bookkeeping first, then the answer
		out <- 1
	}()
}
`)
	var stderr bytes.Buffer
	if code := check(dir, "", &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestTheAllowlistNeedsAReasonAndMustMatch(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", bad)
	key := "a.go|start|defer inFlight.Add(-1)"
	allow := filepath.Join(dir, "allow")
	for name, tc := range map[string]struct {
		content string
		code    int
		want    string
	}{
		"allowed with a reason":   {key + " # the receiver never reads the counter\n", 0, ""},
		"no reason":               {key + "\n", 1, "no reason"},
		"empty reason":            {key + " #  \n", 1, "no reason"},
		"stale entry":             {key + " # r\nb.go|f|x # r\n", 1, "matches no goroutine"},
		"comments and blank line": {"# a comment\n\n" + key + " # fine\n", 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			write(t, dir, "allow", tc.content)
			var stderr bytes.Buffer
			if code := check(dir, allow, &stderr); code != tc.code || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, want %d: %s", code, tc.code, stderr.String())
			}
		})
	}
}

func findings(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "a.go", src)
	var stderr bytes.Buffer
	code := check(dir, "", &stderr)
	if code == 0 {
		return ""
	}
	return stderr.String()
}

func TestFollowsASendIntoAHelper(t *testing.T) {
	got := findings(t, `package p

import "sync"

var mu sync.Mutex
var n int

func send(ch chan int) { ch <- 1 }

func helper(ch chan int) {
	go func() {
		defer func() { mu.Lock(); n--; mu.Unlock() }()
		send(ch)
	}()
}
`)
	if !strings.Contains(got, "call send") {
		t.Fatalf("a send through a helper was not found: %q", got)
	}
}

func TestHelpersThatDoNotTakeAChannelAreNotSends(t *testing.T) {
	// close, Done and the like are named in many packages; only a function handed a channel counts.
	if got := findings(t, `package p

type g struct{ ch chan int }

func (x *g) signal() { x.ch <- 1 }

func start(x *g, release func()) {
	go func() {
		defer release()
		x.signal()
	}()
}
`); got != "" {
		t.Fatalf("flagged: %q", got)
	}
}

func TestFindsBookkeepingAfterASendThatIsNotDeferred(t *testing.T) {
	got := findings(t, `package p

import "sync"

var mu sync.Mutex
var n int

func afterSend(ch chan int) {
	go func() {
		ch <- 1
		mu.Lock()
		n--
		mu.Unlock()
	}()
}
`)
	for _, want := range []string{"after send: decrement", "after send: Unlock"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q not found: %q", want, got)
		}
	}
}

func TestOnlyBookkeepingOnTheSamePathAfterTheSendCounts(t *testing.T) {
	if got := findings(t, `package p

var n int

func branches(ch chan int, fail bool) {
	go func() {
		if fail {
			ch <- 1
			return // the send path ends here
		}
		n--
		ch <- 2
	}()
}

func before(ch chan int) {
	go func() {
		n--
		ch <- 1 // bookkeeping first: fine
	}()
}
`); got != "" {
		t.Fatalf("flagged: %q", got)
	}
	if got := findings(t, `package p

var n int

func inBranch(ch chan int, ok bool) {
	go func() {
		if ok {
			ch <- 1
		}
		n-- // reached after the send when ok
	}()
}
`); !strings.Contains(got, "decrement") {
		t.Fatalf("a send in a branch that falls through was not found: %q", got)
	}
}

func TestCountsFunctionLiteralsNestedInTheGoroutine(t *testing.T) {
	got := findings(t, `package p

var n int

func nested(ch chan int) {
	go func() {
		defer func() { n-- }()
		func() { ch <- 1 }()
	}()
}
`)
	if !strings.Contains(got, "send ch") {
		t.Fatalf("a send in a nested literal was not found: %q", got)
	}
}

func TestAnAmbiguousNameIsCheckedAgainstEveryDeclaration(t *testing.T) {
	// Two functions called work: go work(...) could run either, so either being unsafe counts.
	got := findings(t, `package p

type a struct{}
type b struct{}

func (b) work() {}
func (a) work(out chan int, release func()) { defer release(); out <- 1 }

func start(x a, out chan int) { go x.work(out, func() {}) }
`)
	if !strings.Contains(got, "defer release()") {
		t.Fatalf("an ambiguous name was skipped: %q", got)
	}
}

func TestAGoroutineStartedFromALocalFunctionValueIsFollowed(t *testing.T) {
	got := findings(t, `package p

var n int

func start(out chan int) {
	run := func() {
		defer func() { n-- }()
		out <- 1
	}
	go run()
}
`)
	if !strings.Contains(got, "defer func() { n-- }()") {
		t.Fatalf("go run() was not followed: %q", got)
	}
}

func TestAGoroutineItCannotResolveIsReported(t *testing.T) {
	got := findings(t, `package p

func start(fn func()) { go fn() }
`)
	if !strings.Contains(got, "cannot be resolved") {
		t.Fatalf("an unresolvable go statement was skipped: %q", got)
	}
}

func TestAnAllowlistEntryDoesNotAbsorbANewDeferredCall(t *testing.T) {
	dir := t.TempDir()
	allow := filepath.Join(dir, "allow")
	write(t, dir, "allow", "a.go|start|defer inFlight.Add(-1) # reviewed\n")
	write(t, dir, "a.go", bad)
	var stderr bytes.Buffer
	if code := check(dir, allow, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	// The goroutine gains a second bookkeeping defer: the entry no longer matches, and it is reviewed again.
	write(t, dir, "a.go", strings.Replace(bad, "defer inFlight.Add(-1)", "defer inFlight.Add(-1)\n\t\tdefer other.Add(-1)", 1)+"\nvar other atomic.Int32\n")
	stderr.Reset()
	if code := check(dir, allow, &stderr); code != 1 || !strings.Contains(stderr.String(), "other.Add(-1)") || !strings.Contains(stderr.String(), "matches no goroutine") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

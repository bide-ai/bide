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
	key := "a.go|start|inFlight.Add(-1)"
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

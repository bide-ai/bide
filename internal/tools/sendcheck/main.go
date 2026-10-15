// Command sendcheck fails when a goroutine hands a result to another goroutine (a channel send or
// a close) and only afterwards runs a deferred call that is more than a signal: a decrement, a
// slot release, an unlock, a flag reset. The receiver can act on the result before that bookkeeping
// runs, and finds the goroutine's state still claimed. That pattern was a real bug three times in
// bide (a notification call counted in flight after it had answered, a trail slot held after the
// head was published, a recovery slot held while the user's error handler ran), and each one showed
// up as a flaky test, not as a failure at the cause.
//
// It reads every .go file below the root, test files included, and looks at each goroutine: a
// function literal started with go, a function value bound to a name in the same function, or a
// declared function (every declaration of an ambiguous name is checked; one it cannot find is
// reported, not skipped). A goroutine hands a result over when it sends on a channel, closes one, or
// calls a function that is handed a channel and sends on it (followed through the call graph, by
// name); function literals nested in the goroutine count as part of it. It is flagged when it then
// has bookkeeping: a deferred call, or a statement after the send on the same path (a decrement, an
// unlock, a release, a slot given back, a flag cleared, an entry deleted). Deferred WaitGroup.Done,
// cancel functions, closing a channel and Stop are signals and do not count. A goroutine whose order
// is safe goes in the allowlist (.sendcheck-allow) as
//
//	path|enclosing function|every deferred call and bookkeeping statement flagged, joined by " ; " # why
//
// so a new bookkeeping call in an allowlisted goroutine is a new finding, and an allowlist entry that
// matches no goroutine is an error, so the list cannot rot. What it cannot see: a result handed over
// by a callback or a method on a channel held in a field, and anything started by a function it
// cannot name.
//
// Usage, from the repository root:
//
//	go run ./internal/tools/sendcheck [-root dir] [-allow file]
//
// The exit status is 0 when clean, 1 on findings, and 2 when the check could not run.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	root := flag.String("root", ".", "repository root")
	allow := flag.String("allow", ".sendcheck-allow", "the allowlist file ('' for none)")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "sendcheck: unexpected arguments %q\n", flag.Args())
		os.Exit(2)
	}
	os.Exit(check(*root, *allow, os.Stderr))
}

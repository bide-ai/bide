package main

import (
	"bytes"
	"strings"
	"testing"
)

// The example's claims, line by line: the requester's decision is recorded and not counted, the
// two others pass the gate, a token cannot decide for another approver, the refund runs once, and
// the receipt is covered by a signed head.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{
		"no token: HTTP 401",
		"ops approves: HTTP 200, counted=false, gate 0 of 2 (excluded: [ops])",
		"finance approves: HTTP 200, counted=true, gate 1 of 2",
		"risk approves: HTTP 200, counted=true, gate 2 of 2",
		"risk's token posting finance's decision: HTTP 403",
		"refund executed 1 time",
		"finance's receipt: anchored",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

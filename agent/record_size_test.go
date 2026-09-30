package agent

import (
	"testing"
	"unsafe"
)

// Record is copied by value throughout the engine. Growing it from 368 to 384 bytes (the Safety
// and Approval fields, before its flags were packed together) cost up to 70% of cmd/bench's
// contended overhead scenario on a 4-vCPU runner, so its size is pinned: a change that grows it
// must say why, and be measured with the bench workflow in A/B mode.
func TestRecord_Size(t *testing.T) {
	if n := unsafe.Sizeof(Record{}); n > 368 {
		t.Fatalf("Record is %d bytes, over the 368 it is held to; see the comment", n)
	}
}

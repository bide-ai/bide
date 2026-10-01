package agent

import (
	"testing"
	"unsafe"
)

// Record is copied by value throughout the engine, and cmd/bench's contended overhead scenario is
// sensitive to its size: padding it by 16 bytes, all the Safety and Approval fields first added,
// shifted the run loop's scheduling under contention (p50 down, p99 up two to three times, and
// wall-clock up by tens of percent on a 4-vCPU runner). Its size is pinned, so a change that grows
// it says why and is measured with the bench workflow in A/B mode.
func TestRecord_Size(t *testing.T) {
	if n := unsafe.Sizeof(Record{}); n > 368 {
		t.Fatalf("Record is %d bytes, over the 368 it is held to; see the comment", n)
	}
}

package agent

import (
	"testing"
	"unsafe"
)

// Record is copied by value throughout the engine, and every heap copy of one (a decode, an
// encode, a store's put) is allocated in the Go size class that holds it. cmd/bench's contended
// overhead scenario is sensitive to its size: padding it by 16 bytes, all the Safety and Approval
// fields first added, shifted the run loop's scheduling under contention (p50 down, p99 up two to
// three times, and wall-clock up by tens of percent on a 4-vCPU runner), and growing it from the
// 288-byte size class to the 384-byte one cost 3 to 8% throughput. The model-turn metadata
// (ModelTurn) and a signed decision's fields (ApproverSignature) sit behind pointers, which brings
// it to 256 bytes, exactly the 256-byte size class. Its size is pinned, so a change that grows it
// says why and is measured with the bench workflow in A/B mode.
func TestRecord_Size(t *testing.T) {
	if n := unsafe.Sizeof(Record{}); n > 256 {
		t.Fatalf("Record is %d bytes, over the 256 it is held to; see the comment", n)
	}
}

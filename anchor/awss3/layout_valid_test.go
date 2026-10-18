package awss3

import (
	"testing"

	"github.com/bide-ai/bide/audit/anchorobj"
)

func TestLayoutIsValidAndCollidesWithNoOtherBackend(t *testing.T) {
	if err := layout.Validate(); err != nil {
		t.Fatal(err)
	}
	// The other backends' layouts, spelled out: only a test that sees both can check that an object of
	// one is never an object of the other. The bide-anchor module, which imports every backend, repeats
	// this check against the real values.
	for name, other := range map[string]anchorobj.Layout{
		"gcs": {Format: "bide.audit.gcs-anchor.v1", PartitionTag: "bide.audit.gcs-anchor.partition.v1\x00"},
	} {
		if layout.Collides(other) {
			t.Errorf("the awss3 layout collides with the %s layout", name)
		}
	}
}

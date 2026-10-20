//go:build unix

package runview

import (
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// ReadDescription is read by the viewer's server and takes a regular file only.
func TestReadDescription_RefusesADescriptor(t *testing.T) {
	_, err := ReadDescription(safefiletest.Descriptor(t, "x"))
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("ReadDescription(descriptor) = %v", err)
	}
}

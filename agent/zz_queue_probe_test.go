package agent

import (
	"os"
	"testing"
)

// TestQueueProbe is a throwaway: it fails only in the merge queue, so a pull request that carries it
// can never merge.
func TestQueueProbe(t *testing.T) {
	if os.Getenv("GITHUB_EVENT_NAME") == "merge_group" {
		t.Fatal("throwaway probe: must never merge")
	}
}

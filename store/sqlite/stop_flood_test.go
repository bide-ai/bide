package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/internal/stopflood"
)

// A writer with store access and no key can fill the control run with junk under a stop's generation names.
// The stop still lands, and SetStop and State() cost time linear in the junk (see docs/KNOWN-LIMITATIONS.md);
// this guards the bound: 10k entries must not push either past stopflood.Bound (measured: about 0.1 s here).
func TestStopFlood_StopStillLandsInBoundedTime(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stopflood.Run(context.Background(), t, s, stopflood.Junk)
}

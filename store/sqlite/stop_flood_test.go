package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/stopflood"
)

// A writer with store access and no key can fill the control run with junk under a stop's generation names.
// The stop still lands, and SetStop, State and the stop switch's reads cost time linear in the junk (see
// docs/KNOWN-LIMITATIONS.md); this guards that: at 10k entries each call stays under stopflood.Bound and
// grows no faster than linearly from 2.5k (measured: under 0.1 s).
func TestStopFlood_StopStillLandsInBoundedTime(t *testing.T) {
	stopflood.Guard(context.Background(), t, func(t *testing.T) agent.Store {
		s, err := Open(filepath.Join(t.TempDir(), "j.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

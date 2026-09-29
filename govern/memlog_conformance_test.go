package govern_test

import (
	"testing"

	"github.com/bide-ai/bide/govern"
	"github.com/bide-ai/bide/govern/eventlogtest"
)

// MemEventLog meets the EventLog contract. Its handles share one in-memory log, as separate
// processes would share a durable one.
func TestMemEventLog_Conformance(t *testing.T) {
	shared := govern.NewMemEventLog()
	eventlogtest.Run(t, func(*testing.T) govern.EventLog { return shared })
}

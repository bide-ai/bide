package eventstoretest_test

import (
	"testing"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/eventstoretest"
)

func TestMemEventStore(t *testing.T) {
	s := audit.NewMemEventStore()
	eventstoretest.Run(t, func(*testing.T) audit.EventStore { return s })
}

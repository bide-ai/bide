package audit

import (
	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journalhook"
)

// withSalt returns r with its salt replaced by salt: a record no journal writes.
func withSalt(r agent.Record, salt []byte) agent.Record {
	return journalhook.WithSalt(r, salt).(agent.Record)
}

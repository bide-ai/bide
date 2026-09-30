package audit

import (
	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journalhook"
)

// withSalt returns r with its salt replaced by salt: a record no journal writes.
func withSalt(r agent.Record, salt []byte) agent.Record {
	return journalhook.WithSalt(r, salt).(agent.Record)
}

// stored returns r as a journal would hand it back: its Raw set to its journal encoding, whatever
// its salt, so a test can commit a record no store would have written.
func stored(r agent.Record) agent.Record {
	b, err := agent.EncodeRecord(r)
	if err != nil {
		panic(err)
	}
	return journalhook.WithRaw(r, b).(agent.Record)
}

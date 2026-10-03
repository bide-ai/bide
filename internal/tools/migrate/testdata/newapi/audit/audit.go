// Package audit is a stub of bide's pre-1.0 audit API for the migrate tool's tests.
package audit

import (
	"context"
	"iter"

	"github.com/bide-ai/bide/agent"
)

type EventLog struct{}
type AuditedStore struct{}

func NewAuditedStore(inner agent.Store) (*AuditedStore, error) { panic("stub") }
func (a *AuditedStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	panic("stub")
}
func (a *AuditedStore) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	panic("stub")
}
func (a *AuditedStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	panic("stub")
}

func RecordStream(log *EventLog, stream *agent.RunStream, onEvent func(agent.RunEvent)) (*agent.Result, error) {
	panic("stub")
}

// Package audit is a stub of bide v0.10's audit API for the migrate tool's tests.
package audit

import "github.com/bide-ai/bide/agent"

type EventLog struct{}
type AuditedStore struct{}

func NewAuditedStore(inner agent.Durable) (*AuditedStore, error) { panic("stub") }
func (a *AuditedStore) Do(ctx any, runID, name string, fn any) (agent.Record, error) {
	panic("stub")
}

func Record(log *EventLog, stream *agent.AgentStream, onEvent func(agent.AgentEvent)) (agent.Message, error) {
	panic("stub")
}

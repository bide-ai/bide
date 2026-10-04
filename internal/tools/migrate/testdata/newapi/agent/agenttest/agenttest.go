// Package agenttest is a stub of bide's agenttest helpers for the migrate tool's tests.
package agenttest

import (
	"context"

	"github.com/bide-ai/bide/agent"
)

func MemJournal() *agent.Journal                                                     { panic("stub") }
func MustJournal(s agent.Store) *agent.Journal                                       { panic("stub") }
func MustNew(model agent.Model, j *agent.Journal, opts ...agent.Option) *agent.Agent { panic("stub") }
func Must[T any](v T, err error) T                                                   { panic("stub") }

type ScriptedTurn struct{}
type ScriptedModel struct{}

func (m *ScriptedModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	panic("stub")
}

func NewScriptedModel(turns ...ScriptedTurn) *ScriptedModel { panic("stub") }
func ToolTurn(id, name, args string) ScriptedTurn           { panic("stub") }
func TextTurn(text string) ScriptedTurn                     { panic("stub") }

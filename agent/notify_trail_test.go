package agent_test

import (
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

func TestMemNotifyTrail_Contract(t *testing.T) {
	agenttest.NotifyTrailContract(t, func() func() agent.NotifyTrail {
		m := agent.NewMemNotifyTrail()
		return func() agent.NotifyTrail { return m }
	})
}

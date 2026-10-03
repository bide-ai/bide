package manual

import "github.com/bide-ai/bide/agent"

// Constructors that now return an error, in positions a hoisted call would change: each is
// reported and left.

func ready() bool { return false }

func shortCircuit(m agent.Model) bool {
	return ready() && agent.New(m, agent.NewMemStore()) != nil // only when ready
}

func elseIf(m agent.Model, a bool) {
	if a {
		return
	} else if agent.New(m, agent.NewMemStore()) != nil { // only when !a
		return
	}
}

func inClosure(m agent.Model) func() *agent.Agent {
	return func() *agent.Agent { return agent.New(m, agent.NewMemStore()) }
}

// noErrorResult has no error result: the rewrite handles the error with panic(err) and says so.
func noErrorResult(m agent.Model) *agent.Agent {
	a := agent.New(m, agent.NewMemStore())
	return a
}

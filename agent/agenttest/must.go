package agenttest

import "github.com/bide-ai/bide/agent"

// MemJournal returns a Journal over a new, empty agent.MemStore.
func MemJournal() *agent.Journal { return MustJournal(agent.NewMemStore()) }

// MustJournal returns agent.NewJournal(s), and panics if that fails (a nil store).
func MustJournal(s agent.Store) *agent.Journal { return Must(agent.NewJournal(s)) }

// Must returns v, and panics with err if it is not nil: Must(agent.NewJournal(s)), say, in a test
// whose setup cannot fail.
func Must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// MustNew returns agent.New(model, j, opts...), and panics if it fails.
func MustNew(model agent.Model, j *agent.Journal, opts ...agent.Option) *agent.Agent {
	return Must(agent.New(model, j, opts...))
}

// Answer returns the answer of a run's Result and the run's error, as the string entry points
// returned them: Answer(a.Run(ctx, runID, agent.UserText(input))). A nil Result (a run ID that
// fails agent.ValidateRunID, or a session turn refused before it starts) has the zero Message.
func Answer(res *agent.Result, err error) (agent.Message, error) {
	if res == nil {
		return agent.Message{}, err
	}
	return res.Message, err
}

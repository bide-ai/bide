package agent

import "context"

// The test helpers of package agenttest, which this package's own tests cannot import (agenttest
// imports agent).

// memJournal returns a Journal over a new, empty MemStore.
func memJournal() *Journal { return mustJournal(NewMemStore()) }

// mustJournal returns NewJournal(s), and panics if that fails.
func mustJournal(s Store) *Journal { return must(NewJournal(s)) }

// must returns v, and panics with err if it is not nil.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// mustNew returns New(model, j, opts...), and panics if it fails.
func mustNew(model Model, j *Journal, opts ...Option) *Agent { return must(New(model, j, opts...)) }

// hasValueStep reports whether runID's journal holds a StepValue record named name.
func hasValueStep(ctx context.Context, j *Journal, runID, name string) (bool, error) {
	r, ok, err := j.Get(ctx, runID, name)
	return ok && r.Kind == StepValue, err
}

// answerOf returns res's Message and err, as agenttest.Answer does.
func answerOf(res *Result, err error) (Message, error) {
	if res == nil {
		return Message{}, err
	}
	return res.Message, err
}

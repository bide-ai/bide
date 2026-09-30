// Package journalhook gives the packages of this module that write raw journal records (audit's
// leaves, plan's flow records) the journal's step functions without them being part of package
// agent's API. Package agent assigns the variables in its init; they take and return only any,
// string and json.RawMessage values, so this package imports nothing of agent's and no import
// cycle arises. A package that uses a hook imports package agent, which guarantees the hook is set.
package journalhook

import (
	"context"
	"encoding/json"
)

// Do runs fn as the memoized step name of runID in j, which is an *agent.Journal or an
// agent.Durable, and returns the agent.Record the journal holds for it (see agent.Journal.Do).
// fn returns the agent.Record to record.
var Do func(ctx context.Context, j any, runID, name string, fn func(context.Context) (any, error)) (any, error)

// Step runs fn as the Step named name of runID in j, which is an *agent.Journal or an
// agent.Durable, with agent.Step's semantics (an attempt claim before fn unless safety is
// retry-safe, the not-started record when fn never ran, numbered re-attempts, a halt as
// *agent.OutcomeUnknown, the pause guard), and returns the JSON value it records. safety is the
// step's agent.Safety. name must be a plan node key (agent's planNodeKey: "node:<name>" or
// "node:iter:<n>:<name>"); any other name is ErrConfig, so the hook runs no other reserved key. A
// Step that fn runs for the same run is recorded under name (see agent.Step), so each node, and
// each loop iteration of one, records its own.
var Step func(ctx context.Context, j any, runID, name string, safety any, fn func(context.Context) (json.RawMessage, error)) (json.RawMessage, error)

// Begin is a flow drive's first step. It holds the run to start, an agent.RunStart: start is
// recorded as the run's run:start if it has none, and a drive whose kind, flow, saga flag or input
// differs from the recorded one is ErrConfig (see agent.RunStart). A run whose completion marker
// (run:complete) is recorded then returns its Result and done=true, since a finished run is final. A new run costs its header and run:start
// Inserts (no read of the completion, which a new run cannot hold); an existing run the run:start
// Insert that finds its recorded start, and one point read of its completion.
var Begin func(ctx context.Context, j any, runID string, start any) (completed json.RawMessage, done bool, err error)

// CheckRunID refuses (ErrConfig) a run ID agent.Run refuses: an empty one, or one of the form the
// engine reserves for the runs of sub-agents and session turns.
var CheckRunID func(ctx context.Context, runID string) error

// Complete records runID's completion marker in j with result as its Result, and returns the Result
// the journal holds (another driver's, if it recorded one first).
var Complete func(ctx context.Context, j any, runID string, result json.RawMessage) (json.RawMessage, error)

// WithSalt returns rec, an agent.Record, with its salt replaced by salt: a record no journal
// writes, for tests of code that must refuse one.
var WithSalt func(rec any, salt []byte) any

// WithClaim returns rec, an agent.Record, with its claim id replaced by claim: a marker another
// driver wrote, for tests.
var WithClaim func(rec any, claim string) any

// WithRaw returns rec, an agent.Record, with the bytes Raw returns replaced by raw, for tests of
// code that reads a record's stored bytes.
var WithRaw func(rec any, raw json.RawMessage) any

// Package journalhook gives the packages of this module that write raw journal records (audit's
// leaves, plan's flow records) the journal's step function without it being part of package
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

// WithSalt returns rec, an agent.Record, with its salt replaced by salt: a record no journal
// writes, for tests of code that must refuse one.
var WithSalt func(rec any, salt []byte) any

// WithClaim returns rec, an agent.Record, with its claim id replaced by claim: a marker another
// driver wrote, for tests.
var WithClaim func(rec any, claim string) any

// WithRaw returns rec, an agent.Record, with the bytes Raw returns replaced by raw, for tests of
// code that reads a record's stored bytes.
var WithRaw func(rec any, raw json.RawMessage) any

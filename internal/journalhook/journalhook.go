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

// Do runs fn as the memoized step name of runID in j, an *agent.Journal, and returns the
// agent.Record the journal holds for it.
// fn returns the agent.Record to record.
var Do func(ctx context.Context, j any, runID, name string, fn func(context.Context) (any, error)) (any, error)

// DoFresh is Do for a step the caller knows is not recorded (the journal's doFresh: the engine's
// live model turns and tool results): one Insert and no read, which keeps the record another
// writer stored first. Tests use it to hold that path to the journal's rules, and plan to record a
// flow's topology (flow:topology), a pure value, without a read.
var DoFresh func(ctx context.Context, j any, runID, name string, fn func(context.Context) (any, error)) (any, error)

// Step runs fn as the Step named name of runID in j, an *agent.Journal, with
// agent.Journal.Step's semantics (an attempt claim before fn unless safety is
// retry-safe, the not-started record when fn never ran, numbered re-attempts, a halt as
// *agent.OutcomeUnknown, the pause guard), and returns the JSON value it records. safety is the
// step's agent.Safety. name must be a plan node key (agent's planNodeKey: "node:<name>" or
// "node:iter:<n>:<name>"); any other name is ErrConfig, so the hook runs no other reserved key. A
// Step that fn runs for the same run is recorded under name (see agent.Journal.Step), so each node, and
// each loop iteration of one, records its own.
var Step func(ctx context.Context, j any, runID, name string, safety any, fn func(context.Context) (json.RawMessage, error)) (json.RawMessage, error)

// Gate evaluates the approval gate policy (an *agent.ApprovalPolicy) of the plan flow node named
// node, under its node key key, in run runID of j (an *agent.Journal), for the node's input args
// (JSON). verifiers is the flow's agent.ApproverVerifierFor (nil when it has none). It reports
// approved=true when the node may run and false when its gate denied it; a gate still waiting
// returns an *agent.ApprovalPending. It counts with the records and the rule an agent's gated tool
// call does.
var Gate func(ctx context.Context, j any, runID, key, node string, args json.RawMessage, policy any, verifiers any) (approved bool, err error)

// PreExec is the pre-execution status check of a gated plan flow node, with Gate's arguments. It runs
// immediately before the node's body would execute (not when its recorded result replays), and returns nil
// when the node may run; a gate with certificate-bound approvers whose counted approvals no longer hold
// returns an *agent.ApprovalPending (or an error) and the node does not run.
var PreExec func(ctx context.Context, j any, runID, key string, policy any, verifiers any) error

// Begin is a flow drive's first step. It holds the run to start, an agent.RunStart: start is
// recorded as the run's run:start if it has none, and a drive whose kind, flow, saga flag or input
// differs from the recorded one is ErrConfig (see agent.RunStart). A run whose completion marker
// (run:complete) is recorded then returns its Result and done=true, since a finished run is final. A new run costs its header and run:start
// Inserts (no read of the completion, which a new run cannot hold); an existing run the run:start
// Insert that finds its recorded start, and one point read of its completion. A run with a
// principal (the identity ctx carries) or ancestors first reads whether run:start exists, and a new
// one records its run:principal (agent.RunPrincipal) before its run:start. runCtx is ctx with the
// run's principal chain bound, for the runs the flow's nodes start.
var Begin func(ctx context.Context, j any, runID string, start any) (runCtx context.Context, completed json.RawMessage, done bool, err error)

// RunIdentity reads a plan flow run's options (each an agent.RunOption): the identity
// agent.WithIdentity gives, as its Actor, OnBehalfOf and AuthorityRef, and set=false when none does.
// An option a flow run does not take (every one but WithIdentity), a nil one, or one that is not an
// agent.RunOption is ErrConfig.
var RunIdentity func(opts []any) (actor, onBehalfOf, authorityRef string, set bool, err error)

// CheckRunID refuses (ErrConfig) a run ID agent.Run refuses: an empty one, or one of the form the
// engine reserves for the runs of sub-agents and session turns.
var CheckRunID func(ctx context.Context, runID string) error

// Complete records runID's completion marker in j with result as its Result, and returns the Result
// the journal holds (another driver's, if it recorded one first).
var Complete func(ctx context.Context, j any, runID string, result json.RawMessage) (json.RawMessage, error)

// Marshal encodes v as the journal encodes values: JSON with no HTML escapes (agent's
// marshalJournal). Package plan encodes every value it records with it.
var Marshal func(v any) ([]byte, error)

// SameJSON reports whether a and b are the same JSON value under the engine's one comparison rule
// (agent's canonical JSON: blind to whitespace, key order, number spelling and string escaping); a
// text that rule refuses (a repeated key, a lone surrogate, invalid UTF-8) equals only itself.
var SameJSON func(a, b []byte) bool

// WithSalt returns rec, an agent.Record, with its salt replaced by salt: a record no journal
// writes, for tests of code that must refuse one.
var WithSalt func(rec any, salt []byte) any

// WithClaim returns rec, an agent.Record, with its claim id replaced by claim: a marker another
// driver wrote, for tests.
var WithClaim func(rec any, claim string) any

// WithRaw returns rec, an agent.Record, with the bytes Raw returns replaced by raw, for tests of
// code that reads a record's stored bytes.
var WithRaw func(rec any, raw json.RawMessage) any

// WithStop returns ctx carrying the plan flow named flow and its kill switch sw (an
// *agent.StopSwitch), which the flow's nodes (Step) are checked against: a node is held, as a pause
// (*agent.Stopped), where a stop covers the flow, before it begins.
var WithStop func(ctx context.Context, flow string, sw any) context.Context

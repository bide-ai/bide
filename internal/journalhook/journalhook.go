// Package journalhook gives the packages of this module that write raw journal records (audit's
// leaves, plan's flow records) the journal's step functions without them being part of package
// agent's API. Package agent assigns the variables in its init; they take and return only any,
// string and json.RawMessage values, so this package imports nothing of agent's and no import
// cycle arises. A package that uses a hook imports package agent, which guarantees the hook is set.
package journalhook

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/bide-ai/bide/internal/capledger"
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

// CapLedger returns a cap ledger (internal/capledger) over a *agent.Journal, reading now from clock;
// bound is how many windows later than the newest recorded one a first intent may be (0 for one). It
// is agent's newCapLedger, for the tests of the store modules (internal/capledger/capledgertest), which
// run the ledger across real processes on real stores; the engine does not call it.
var CapLedger func(j any, clock func() time.Time, bound int) *capledger.Ledger

// ErrTaintRefused is what FlowTaintNode wraps when a plan flow's taint policy refuses a node: the run has
// read untrusted content, the policy is Refuse, and the node can have a side effect. Package plan wraps it in
// its own sentinel.
var ErrTaintRefused = errors.New("refused: the run has read untrusted content")

// FlowTaintNode is what a plan flow node declares for taint (docs/design/flow-taint.md): the sources of
// untrusted content its results carry, the reason they are the host's own, and the reason the node needs no
// escalation gate.
type FlowTaintNode struct {
	Untrusted []string
	Trusted   string
	Exempt    string
}

// FlowTaint is a plan flow's taint declarations: its policy (an escalation Gate, an *agent.ApprovalPolicy,
// or Refuse, and RequireProvenance) and, by node name, what each node declares. A flow with no taint
// policy passes none (nil).
type FlowTaint struct {
	Gate              any
	Refuse            bool
	RequireProvenance bool
	Nodes             map[string]FlowTaintNode
}

// What a drive of a plan flow stands at against its run's taint declarations (FlowTaintEnter).
const (
	// FlowTaintNone: the run started with no declarations; it is never gated or refused for taint.
	FlowTaintNone = iota + 1
	// FlowTaintUnder: the run started under the declarations this flow has.
	FlowTaintUnder
	// FlowTaintPolicyless: the run started under declarations and this flow has no policy to enforce them.
	FlowTaintPolicyless
)

// FlowTaintDecision is what taint requires of a node about to run: nothing (the zero value), the
// escalation Gate (an *agent.ApprovalPolicy) on the node's instance, or, for a refused node, an error.
type FlowTaintDecision struct {
	Gate any
	// Ctx is the context the node's body runs under, for a run under taint: ctx bound with the run's
	// under-taint status, the node's read set as the marks the runs the body starts inherit, and the node's
	// sink, where the runs the body starts send the marks they learn. nil for a run
	// that is not under taint, whose body runs under the context the drive has.
	Ctx context.Context
}

// FlowTaintPrepare returns start (an agent.RunStart) recording d, the flow's declarations, for a drive
// that may start the run; it returns start unchanged for d == nil and a run that is not under taint by
// inheritance. With d.RequireProvenance it refuses (ErrConfig) a new run that states neither where its input
// came from nor started inside a tainted run, before the run's journal is written. A run started inside a
// tainted run (ctx carries its status) records the marks it inherited; a flow with no policy records the
// parent's status with no declarations, and with a node that can have a side effect (tracked names them) it is
// refused (ErrConfig) when the run is new, before anything is written. A run that ctx's node started under taint
// is recorded as a child of that node in the node's journal (ErrConfig when it uses another journal), so a
// ResolveHalt of the node finds it. The agent package sets it in init.
var FlowTaintPrepare func(ctx context.Context, j any, runID string, start any, d *FlowTaint, tracked []string) (any, error)

// FlowTaintEnter compares a drive of the flow, whose declarations are d (nil for a flow with no policy),
// with the declarations the run's recorded start holds, and returns one of the FlowTaint standings. Any
// difference is ErrConfig, naming both digests, before a node runs. A run that started inside a tainted run under a
// flow with no policy is refused (ErrConfig) on every drive by a flow with a node that can have a side effect
// (tracked names them), as it was at Begin, before any node runs. The agent package sets it in init.
var FlowTaintEnter func(ctx context.Context, j any, runID string, d *FlowTaint, tracked []string) (standing int, err error)

// FlowTaintNodeHook decides what taint requires of the node with key nodeKey (and safety, an agent.Safety),
// about to run in a run at standing: it computes the node's read set from the journal, records the
// gate-reason record (insert-once), and returns the escalation Gate the node's instance needs, nothing, or an
// error that wraps ErrTaintRefused for a refused node (recorded, and no node record written). The agent
// package sets it in init.
var FlowTaintNodeHook func(ctx context.Context, j any, runID, nodeKey string, safety any, standing int) (FlowTaintDecision, error)

// FlowTaintSink sends the marks the run runID has learned to the node that started it, when ctx carries that
// node's sink: the sources the node nodeKey declares once its result is recorded and those its own children
// brought up, or those of every node for nodeKey "". It is insert-once per mark, called after the node's result
// is recorded and again on every drive that replays it, and for a run that already finished, so a mark is not
// lost to a crash, a ResolveHalt of the child's node, or a drive without the parent. It sends the run's own
// marks, never the ones it inherited. Nothing happens when ctx carries no sink or the run is not under taint.
// The agent package sets it in init.
var FlowTaintSink func(ctx context.Context, j any, runID, nodeKey string) error

// FlowTaintDriven registers the plan flow run runID of journal j with the call that drives it, when ctx carries
// the result mark of a tool call: the call's result then carries "subflow:<tool>" if the run ended with a mark
// (one it inherited or one of its own), as a sub-agent's answer carries "subagent:<tool>". The agent package
// sets it in init.
var FlowTaintDriven func(ctx context.Context, j any, runID string)

// FlowTaintBodyDone is called when the body of node nodeKey of run runID returns without an error, before the
// node's result is recorded: the agent runs the body drove hand the marks they read up as taint-mark records for
// the node (a crash before the node's record leaves them, never fewer). Nothing happens when ctx carries no
// result mark (the run is not under taint). The agent package sets it in init.
var FlowTaintBodyDone func(ctx context.Context, j any, runID, nodeKey string) error

// RunProvenance reads a plan flow run's options (each an agent.RunOption) for where the run's input came
// from: the source WithUntrustedInput names, or the reason WithTrustedInput gives (at most one is set; both
// is ErrConfig). The agent package sets it in init.
var RunProvenance func(opts []any) (untrusted, trusted *string, err error)

// FlowTaintConform checks the taint records of a plan flow run against the flow (Flow.Conform): recs is the
// run's journal (a []agent.Record), cur the flow's declarations (nil for a flow with no policy), and safeties
// maps each declared node name to its agent.Safety. It returns the divergences, each naming the record: a
// taint record in a run that did not start under taint, declarations that are not the flow's, a node that ran
// though its requirement was not met (a refused node, or an escalation gate that was not passed before its
// record), and a gate-reason or taint-mark record that does not read or names an undeclared node. The agent
// package sets it in init.
var FlowTaintConform func(recs any, cur *FlowTaint, safeties map[string]any) []string

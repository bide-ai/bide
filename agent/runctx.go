package agent

import (
	"context"
	"strconv"
	"strings"
)

type ctxKey int

const (
	runContextKey ctxKey = 3
	onceScopeKey  ctxKey = 4
)

// runCtx carries the run a tool call belongs to into the tool's context, so Interrupt, Sleep and
// the other durable waits can journal and read their values without the tool holding the store,
// and so RunInfoFrom can describe the call.
type runCtx struct {
	store     Durable
	runID     string
	root      string // the top-level run; a sub-agent's runs inherit it
	toolUseID string // the call being executed; "" outside one
	saga      bool   // the run is a saga
}

// withRunContext returns ctx carrying run runID of store, and the call toolUseID of it being
// executed ("" for none), in a saga run or not.
func withRunContext(ctx context.Context, store Durable, runID, toolUseID string, saga bool) context.Context {
	return context.WithValue(ctx, runContextKey, runCtx{store: store, runID: runID, root: rootRunID(ctx, runID), toolUseID: toolUseID, saga: saga})
}

// rootRunID is the top-level run for a run with this ID reached through ctx: the root recorded
// by an enclosing run (a sub-agent is called from its parent's tool context), or runID itself.
func rootRunID(ctx context.Context, runID string) string {
	if rc, ok := ctx.Value(runContextKey).(runCtx); ok && rc.root != "" {
		return rc.root
	}
	return runID
}

func runContext(ctx context.Context) (Durable, string, bool) {
	rc, ok := ctx.Value(runContextKey).(runCtx)
	return rc.store, rc.runID, ok
}

// inSaga reports whether ctx is a tool call's context in a saga run.
func inSaga(ctx context.Context) bool {
	rc, _ := ctx.Value(runContextKey).(runCtx)
	return rc.saga
}

// RunInfo describes the run some code runs in: a tool call's (RunInfoFrom), or the run a
// WithSystemPromptFunc function is computing the prompt for.
type RunInfo struct {
	// RunID is the run's ID: the run whose tool call this is.
	RunID string
	// RootRunID is the top-level run of the agent tree the run belongs to: RunID itself for a run
	// started directly, and its root's for a sub-agent's run.
	RootRunID string
	// ToolUseID is the ID of the tool call being executed, as the model issued it; "" outside a
	// tool call (in a WithSystemPromptFunc function, for one).
	ToolUseID string
	// Saga reports whether the run is a saga, whose failure rolls back its completed calls. A
	// sub-agent called from one runs as a saga too, so the whole tree rolls back together.
	Saga bool
}

// RunInfoFrom returns the RunInfo of the tool call whose context ctx is (or derives from), and
// false outside one. The agent loop sets it for every call, and for a saga's rollback re-runs.
func RunInfoFrom(ctx context.Context) (RunInfo, bool) {
	rc, ok := ctx.Value(runContextKey).(runCtx)
	if !ok {
		return RunInfo{}, false
	}
	return RunInfo{RunID: rc.runID, RootRunID: rc.root, ToolUseID: rc.toolUseID, Saga: rc.saga}, true
}

// stepRunMark starts the segment SubRunFor appends: "step:" and an encoded name. encodeID escapes
// ':', so no encoded tool-use ID contains it, and a programmatic sub-run never shares an ID with
// a sub-agent's (SubRunID) or a session's.
const stepRunMark = "step:"

// callScope is the run scope of the code r describes: the call's sub-run ID (SubRunID) inside a
// tool call, and the run's ID outside one.
func (r RunInfo) callScope() string {
	if r.ToolUseID == "" {
		return r.RunID
	}
	return SubRunID(r.RunID, r.ToolUseID)
}

// SubRunFor returns the run ID of the programmatic sub-run name, for code that starts a run of
// another agent itself (where SubAgent would start it for a tool call): "<scope>>step:<name>",
// the name encoded as SubRunID encodes a tool-use ID, where scope is the tool call's sub-run ID
// (SubRunID(RunID, ToolUseID)) inside a tool call, and RunID outside one. The ID is stable, so a
// resumed call that starts the sub-run again resumes it, and distinct, so two calls (or two names
// in one call) never share a journal. The sub-run is part of the run's tree like a sub-agent's:
// Recover leaves it to its root (IsSubRun), and a run given this ID from the tool call's context
// is accepted although it contains '>'. name must not be empty: Run refuses the ID otherwise.
func (r RunInfo) SubRunFor(name string) string {
	return r.callScope() + subRunSep + stepRunMark + encodeID(name)
}

// derivedRunID reports whether runID is a run ID the run described by ctx may start: the
// sub-agent run of the tool call ctx belongs to (SubRunID), or a programmatic sub-run it names
// with SubRunFor.
func derivedRunID(ctx context.Context, runID string) bool {
	info, ok := RunInfoFrom(ctx)
	if !ok {
		return false
	}
	if info.ToolUseID != "" && runID == SubRunID(info.RunID, info.ToolUseID) {
		return true
	}
	rest, ok := strings.CutPrefix(runID, info.callScope()+subRunSep+stepRunMark)
	return ok && rest != "" && isEncodedID(rest)
}

// isEncodedID reports whether s is a string encodeID returns for some ID: the escaped form
// (decoded and encoded again, it is s itself), or '~' and a lowercase hex SHA-256.
func isEncodedID(s string) bool {
	if sum, ok := strings.CutPrefix(s, "~"); ok {
		return len(sum) == 64 && strings.Trim(sum, "0123456789abcdef") == ""
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b = append(b, s[i])
			continue
		}
		if i+2 >= len(s) {
			return false
		}
		v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		if err != nil {
			return false
		}
		b = append(b, byte(v))
		i += 2
	}
	return encodeID(string(b)) == s
}

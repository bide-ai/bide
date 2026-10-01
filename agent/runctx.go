package agent

import (
	"context"
	"fmt"
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

// stepRunName reports whether runID is a programmatic sub-run the tool call ctx belongs to names
// with SubRunFor, and returns its name, or "" for a name too long to be decoded (its encoding is a
// digest; see encodeID).
func stepRunName(ctx context.Context, runID string) (name string, ok bool) {
	info, ok := RunInfoFrom(ctx)
	if !ok || info.ToolUseID == "" {
		return "", false
	}
	rest, ok := strings.CutPrefix(runID, info.callScope()+subRunSep+stepRunMark)
	if !ok || rest == "" || !isEncodedID(rest) {
		return "", false
	}
	name, _ = decodeID(rest)
	return name, true
}

// linkSubRun admits the programmatic sub-run runID, if it is one: it is refused (ErrConfig) once
// the tool call that names it has returned, since a sub-run started from a goroutine that
// outlived its call would belong to no call (no rollback, and no Recover, would reach it). In a
// saga, the parent run's journal records that the call started it (subRunLinkStep), before the
// sub-run records anything, so the saga's rollback walks it (see WithSubRuns). The link is written
// while the call is held open, so a call that has returned has recorded every link it will have.
func linkSubRun(ctx context.Context, runID string) error {
	name, ok := stepRunName(ctx, runID)
	if !ok {
		return nil
	}
	rc, _ := ctx.Value(runContextKey).(runCtx)
	cu, _ := ctx.Value(callUsageKey).(*callUsage)
	if cu == nil {
		return fmt.Errorf("run: programmatic sub-run %q started outside its tool call's context: %w", runID, ErrConfig)
	}
	cu.mu.Lock()
	defer cu.mu.Unlock()
	if cu.returned {
		return fmt.Errorf("run: programmatic sub-run %q started after its tool call (%s of run %s) returned; start it within the call: %w",
			runID, rc.toolUseID, rc.runID, ErrConfig)
	}
	if !rc.saga {
		return nil
	}
	if name == "" {
		return fmt.Errorf("run: programmatic sub-run %q of a saga: its name is too long for the saga's link to it (at most %d bytes once escaped): %w",
			runID, maxEncodedID, ErrConfig)
	}
	if _, err := rc.store.Do(ctx, rc.runID, subRunLinkStep(rc.toolUseID, name), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: mustJSON(name)}, nil
	}); err != nil {
		return fmt.Errorf("run: record the programmatic sub-run %q in run %s: %w (%w)", runID, rc.runID, err, ErrStorage)
	}
	return nil
}

// isEncodedID reports whether s is a string encodeID returns for some ID: the escaped form
// (decoded and encoded again, it is s itself), or '~' and a lowercase hex SHA-256.
func isEncodedID(s string) bool {
	if sum, ok := strings.CutPrefix(s, "~"); ok {
		return len(sum) == 64 && strings.Trim(sum, "0123456789abcdef") == ""
	}
	_, ok := decodeID(s)
	return ok
}

// decodeID returns the ID whose escaped form (see encodeID) s is, and false if s is not one: a
// digest form ('~' and a SHA-256) cannot be decoded.
func decodeID(s string) (string, bool) {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b = append(b, s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", false
		}
		v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		if err != nil {
			return "", false
		}
		b = append(b, byte(v))
		i += 2
	}
	if id := string(b); encodeID(id) == s {
		return id, true
	}
	return "", false
}

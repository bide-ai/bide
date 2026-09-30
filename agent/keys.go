// keys.go holds the rules that keep a journal's keys apart. Every key the engine writes starts
// with a reserved prefix (reservedPrefixes), and a developer-chosen step name may not (Step
// refuses one), so the two never meet. A model-chosen tool-use ID only ever appears encoded
// (encodeID) after a prefix of its own, so no ID can name an engine key, another call's key, or
// another call's sub-run.

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// reservedPrefixes are the prefixes of every journal key the engine writes, in a run's journal
// or a session's. The functions and constants named *Step build those keys; a test asserts each
// one starts with a prefix listed here.
var reservedPrefixes = []string{
	"@",               // the journal header @journal, and engine-internal steps: @llm/<n>, @saga/compensate/<call>, @saga/args/<call>, @retrieval/<layer>, @spend/<n>
	"run:",            // run:start, run:complete, run:aborted, run:cancelled, run:limits:<n>
	"tool:",           // a tool call's result: tool:<call>
	"attempt:",        // attempt markers: attempt:tool:<call>, attempt:step:<name>, attempt:retry:<n>:..., attempt:not-started:<claim>:<marker>
	"approval:",       // approval decisions: approval:<call>[:<approver>:<digest>]
	"approval-tally:", // an m-of-n gate's tally
	"signal:",         // Signal / Await
	"await-timeout:",  // AwaitFor's deadline
	"await-resolved:", // AwaitFor's outcome
	"timer:",          // Sleep / WaitUntil
	"interrupt:",      // Interrupt / Resume
	"chan:",           // Send
	"chanack:",        // Ack
	"turn/",           // a session's completed turns
	"start/",          // a session's started turns
	"from/",           // a session turn's starting transcript
	"audit:",          // the audit package's leaves
}

// IsReservedStepName reports whether name starts with a prefix the engine reserves for its own
// journal keys. Step, Parallel and ResolveStepHalt refuse such a name.
func IsReservedStepName(name string) bool {
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// checkStepName refuses a developer-chosen step name the engine reserves.
func checkStepName(op, name string) error {
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(name, p) {
			return fmt.Errorf("%s: step name %q starts with %q, which the engine reserves for its own journal keys: %w", op, name, p, ErrConfig)
		}
	}
	return nil
}

// subRunSep separates a derived run ID from the root it hangs off. A sub-agent's run is
// "<parent run>><encoded call>" (SubRunID), and a session's journal and turn runs are
// "<session id>>@<what>" (sessionJournalID, sessionTurnRunID, sessionEventRunID). A root run ID
// and a session ID may not contain it (checkRunID, Session), so no ID a caller passes to Run names
// a derived one, and an encoded call ID never starts with sessionMark, so a sub-run and a session
// run never meet either.
const subRunSep = ">"

// sessionMark starts the segment after subRunSep in a session's run IDs. encodeID escapes '@', so
// no encoded call ID starts with it.
const sessionMark = "@"

// SubRunID returns the run ID of the sub-agent run that the call toolUseID of run parent
// starts: parent, '>', then toolUseID encoded (see encodeID). It is RunScope inside that call.
func SubRunID(parent, toolUseID string) string { return parent + subRunSep + encodeID(toolUseID) }

// IsSubRun reports whether runID is a sub-agent's run (SubRunID), which its root run drives: its
// last '>' is followed by an encoded call ID, not by a session's mark.
func IsSubRun(runID string) bool {
	i := strings.LastIndex(runID, subRunSep)
	return i >= 0 && !strings.HasPrefix(runID[i+len(subRunSep):], sessionMark)
}

// IsSessionRun reports whether runID is a session's journal or one of its turn runs, which the
// session drives (see Session). A sub-agent called in a turn is IsSubRun instead.
func IsSessionRun(runID string) bool { return strings.Contains(runID, subRunSep) && !IsSubRun(runID) }

// sessionJournalID is the run ID of the journal of session id: which message started each Send
// turn and each completed turn's record.
func sessionJournalID(id string) string { return id + subRunSep + sessionMark + "session" }

// sessionTurnRunID is the run ID of the n-th Send turn of session id.
func sessionTurnRunID(id string, n int) string {
	return id + subRunSep + sessionMark + "turn/" + strconv.Itoa(n)
}

// sessionEventRunID is the run ID of the SendOnce turn of session id for the message key, which
// is encoded (see encodeID) so that no key names another turn's run.
func sessionEventRunID(id, key string) string {
	return id + subRunSep + sessionMark + "event/" + encodeID(key)
}

// sessionRunKey carries the session run ID a Session is driving, the one such ID run accepts.
type sessionRunKey struct{}

func withSessionRun(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, sessionRunKey{}, runID)
}

// checkRunID refuses a run ID that is empty or contains subRunSep, which only the engine's own
// derived run IDs carry: a sub-agent's call passes its run ID in the run scope the loop gave it,
// and a session its turn's in the context it drives the turn with.
func checkRunID(ctx context.Context, runID string) error {
	if runID == "" {
		// An empty runID would key every run to the same journal, silently cross-contaminating
		// their memoized steps. Reject it rather than corrupt the log.
		return fmt.Errorf("run: empty runID: %w", ErrConfig)
	}
	if !strings.Contains(runID, subRunSep) || runID == RunScope(ctx) {
		return nil
	}
	if sr, _ := ctx.Value(sessionRunKey{}).(string); sr == runID && IsSessionRun(runID) {
		return nil
	}
	return fmt.Errorf("run: run ID %q contains %q, which the engine reserves for the run IDs of sub-agents and session turns: %w", runID, subRunSep, ErrConfig)
}

// maxEncodedID bounds encodeID's output. An ID whose escaped form is longer is replaced by a
// digest, so a key stays within any store's key limit (Postgres refuses an index row over
// 2704 bytes) however long the ID a provider sends.
const maxEncodedID = 96

// encodeID maps a tool-use ID to a key segment that no other ID maps to and that carries none
// of the characters keys and run IDs are built with (':', '/', '>', '@', '~'). Bytes outside
// [A-Za-z0-9._-] are escaped as %XX, which is one-to-one and leaves the IDs providers issue
// ("toolu_01...", "call_...") unchanged. An escaped form longer than maxEncodedID becomes '~'
// and the hex SHA-256 of the ID; '~' is always escaped otherwise, so the two forms never meet,
// and two long IDs share a segment only if they collide under SHA-256.
func encodeID(id string) string {
	var b strings.Builder
	for i := 0; i < len(id); i++ {
		c := id[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '.' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
		if b.Len() > maxEncodedID {
			sum := sha256.Sum256([]byte(id))
			return "~" + hex.EncodeToString(sum[:])
		}
	}
	return b.String()
}

// retrievalStep is the key of the documents retrieval layer layer fetched for the run.
func retrievalStep(layer int) string { return "@retrieval/" + strconv.Itoa(layer) }

// spendStep is the key of the run's n-th spend record (a failed model call's usage).
func spendStep(n int) string { return spendStepPrefix + strconv.Itoa(n) }

// modelStep is the key of the run's n-th model turn.
func modelStep(n int) string { return "@llm/" + strconv.Itoa(n) }

// ToolResultStep is the key of the result of the call toolUseID: the record the loop, a saga
// rollback, or ResolveHalt writes when the call's outcome is known.
func ToolResultStep(toolUseID string) string { return "tool:" + encodeID(toolUseID) }

// toolAttemptStep is the key of the attempt marker of the call toolUseID.
func toolAttemptStep(toolUseID string) string { return "attempt:tool:" + encodeID(toolUseID) }

// stepAttemptStep is the key of the attempt marker of the Step named name.
func stepAttemptStep(name string) string { return "attempt:step:" + name }

// Prefixes of the keys of a re-attempt's marker and of the record that an attempt never started
// (see attempt.go).
const (
	retryAttemptPrefix = "attempt:retry:"
	notStartedPrefix   = "attempt:not-started:"
)

// retryAttemptStep is the key of the marker of attempt number gen of the effect whose first
// attempt's marker key is base (toolAttemptStep or stepAttemptStep): base itself for gen 0, and
// otherwise "attempt:retry:<gen>:" followed by base without its "attempt:" prefix, so
// "attempt:retry:<gen>:tool:<call>" or "attempt:retry:<gen>:step:<name>". The digits end at the
// first ':', and base's own third segment is "tool" or "step", never "retry" or "not-started", so
// no two (base, gen) pairs share a key and none meets a first attempt's key.
func retryAttemptStep(base string, gen int) string {
	if gen == 0 {
		return base
	}
	return retryAttemptPrefix + strconv.Itoa(gen) + ":" + strings.TrimPrefix(base, "attempt:")
}

// attemptBase is the first attempt's marker key of the effect whose attempt marker is key:
// the inverse of retryAttemptStep over gen.
func attemptBase(key string) string {
	rest, ok := strings.CutPrefix(key, retryAttemptPrefix)
	if !ok {
		return key
	}
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		return "attempt:" + rest[i+1:]
	}
	return key
}

// notStartedStep is the key of the record that the attempt whose marker key is marker, claimed
// under claim, never started its effect: "attempt:not-started:<claim>:<marker>". A claim id is hex,
// so the claim ends at the first ':'. Keying it by the claim keeps apart the records of two drivers
// that each claimed the same marker key, one of whose claims may never have committed.
func notStartedStep(marker, claim string) string { return notStartedPrefix + claim + ":" + marker }

// isToolAttempt reports whether r is an attempt marker of the call r.ToolUseID (its first attempt
// or a re-attempt), not of a Step that happens to share its string.
func isToolAttempt(r Record) bool {
	return r.Kind == StepAttempt && attemptBase(r.Name) == toolAttemptStep(r.ToolUseID)
}

// approvalStep is the key of the single approve/deny decision (Approve) on the call toolUseID.
func approvalStep(toolUseID string) string { return "approval:" + encodeID(toolUseID) }

// sagaCompensateStep is the key of the compensation of the call toolUseID.
func sagaCompensateStep(toolUseID string) string { return "@saga/compensate/" + encodeID(toolUseID) }

// sagaArgsStep is the key of the arguments the compensable call toolUseID accepted in a saga, as
// the tool received them after tool middleware. It is journaled only when a middleware changed
// the model's arguments (see toolHandler); compensation reads it (see rollbackRun).
func sagaArgsStep(toolUseID string) string { return "@saga/args/" + encodeID(toolUseID) }

// awaitTimeoutStep is the key of AwaitFor's deadline for name.
func awaitTimeoutStep(name string) string { return "await-timeout:" + name }

// stepKey is the in-process key of step name of runID: the run ID's length in bytes, ':', the run
// ID, then the name. The length makes the split exact whatever bytes the two hold, so two
// different steps never share a key (joining them with a separator would not: ("a\x00b", "c") and
// ("a", "b\x00c") both join to "a\x00b\x00c").
func stepKey(runID, name string) string { return strconv.Itoa(len(runID)) + ":" + runID + name }

// runCancelledStep is the key of the record that a run was cancelled: a terminal marker, like
// run:complete, that Recover excludes. The key is reserved now; the engine does not write it yet.
const runCancelledStep = "run:cancelled"

// runLimitsStep is the key of the n-th amendment of a run's limits (its turn cap or token budget)
// by a later drive. The key is reserved now; the engine does not write it yet.
func runLimitsStep(n int) string { return "run:limits:" + strconv.Itoa(n) }

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
	"@",               // engine-internal steps: @llm/<n>, @saga/compensate/<call>, @retrieval/<layer>, @spend/<n>
	"run:",            // run:complete, run:aborted
	"tool:",           // a tool call's result: tool:<call>
	"attempt:",        // attempt markers: attempt:tool:<call>, attempt:step:<name>
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

// subRunSep separates a sub-agent's run ID from its parent's: "<parent run>><encoded call>". An
// encoded ID never contains it, and a top-level run ID or session ID may not (checkRunID).
const subRunSep = ">"

// SubRunID returns the run ID of the sub-agent run that the call toolUseID of run parent
// starts: parent, '>', then toolUseID encoded (see encodeID). It is RunScope inside that call.
func SubRunID(parent, toolUseID string) string { return parent + subRunSep + encodeID(toolUseID) }

// IsSubRun reports whether runID is a sub-agent's run (SubRunID), which its root run drives.
func IsSubRun(runID string) bool { return strings.Contains(runID, subRunSep) }

// checkRunID refuses a run ID that is empty or names a sub-agent's run. A sub-agent's own call
// passes its run ID in the run scope the loop gave it, and only that ID may carry subRunSep.
func checkRunID(ctx context.Context, runID string) error {
	if runID == "" {
		// An empty runID would key every run to the same journal, silently cross-contaminating
		// their memoized steps. Reject it rather than corrupt the log.
		return fmt.Errorf("run: empty runID: %w", ErrConfig)
	}
	if IsSubRun(runID) && runID != RunScope(ctx) {
		return fmt.Errorf("run: run ID %q contains %q, which separates a sub-agent's run from its parent's: %w", runID, subRunSep, ErrConfig)
	}
	return nil
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

// isToolAttempt reports whether r is the attempt marker of the call r.ToolUseID, not of a Step
// that happens to share its string.
func isToolAttempt(r Record) bool {
	return r.Kind == StepAttempt && r.Name == toolAttemptStep(r.ToolUseID)
}

// approvalStep is the key of the single approve/deny decision (Approve) on the call toolUseID.
func approvalStep(toolUseID string) string { return "approval:" + encodeID(toolUseID) }

// sagaCompensateStep is the key of the compensation of the call toolUseID.
func sagaCompensateStep(toolUseID string) string { return "@saga/compensate/" + encodeID(toolUseID) }

// awaitTimeoutStep is the key of AwaitFor's deadline for name.
func awaitTimeoutStep(name string) string { return "await-timeout:" + name }

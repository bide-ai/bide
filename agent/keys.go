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
	"@",               // the journal header @journal, and engine-internal steps: @llm/<n>, @saga/compensate/<call>, @saga/args/<call>, @retrieval/<layer>, @spend/<id>, @spend-late/<id>, @subrun/<call>/<name>
	"run:",            // run:start, run:complete, run:aborted, run:cancelled, run:cancel-requested, run:limits:<n>
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
	"node:",           // a plan flow's nodes, run as Steps: node:<name>, node:iter:<n>:<name> (see planNodeStep)
	"switch:",         // a plan flow's branch choices: switch:<over>, switch:iter:<n>:<over>
	"flow:",           // a plan flow's topology digest: flow:digest
}

// planNodePrefix starts the key of every plan flow node, which package plan runs as a Step through
// the engine step hook (internal/journalhook.Step).
const planNodePrefix = "node:"

// planNodeKey reports whether name is the key of a plan flow node: "node:<name>" or
// "node:iter:<n>:<name>", where <n> is a decimal number with no leading zero (the form strconv.Itoa
// writes) and <name> is not empty and holds no ':' (package plan refuses a node name with one).
// These are the reserved names a Step may run under through the step hook.
func planNodeKey(name string) bool {
	_, step, ok := parsePlanKey(name)
	return ok && step == ""
}

// planNodeStep reports whether name is the key of a plan flow node (planNodeKey) or of a Step a
// node's body runs (planScopedStep): the reserved step names ResolveHaltRef accepts, since each
// halts as a Step does.
func planNodeStep(name string) bool {
	_, _, ok := parsePlanKey(name)
	return ok
}

// planStepSep joins a node's key and the name of a Step its body runs (see planScopedStep).
const planStepSep = ":step:"

// parsePlanKey splits a plan key into the node key and, for a Step a node's body runs, that
// Step's name: "node:[iter:<n>:]<node>" or "node:[iter:<n>:]<node>:step:<step>". A node name holds
// no ':', so the node ends at the first ':' after the iteration scope, and "iter:" followed by
// anything but a number is the node named "iter".
func parsePlanKey(name string) (node, step string, ok bool) {
	rest, ok := strings.CutPrefix(name, planNodePrefix)
	if !ok {
		return "", "", false
	}
	body := rest
	if it, ok := strings.CutPrefix(rest, "iter:"); ok {
		if digits, after, ok := strings.Cut(it, ":"); ok && isIterNumber(digits) {
			body = after
		}
	}
	n, tail, scoped := strings.Cut(body, ":")
	if n == "" {
		return "", "", false
	}
	nodeKey := name[:len(name)-len(body)] + n
	if !scoped {
		return nodeKey, "", true
	}
	step, ok = strings.CutPrefix(":"+tail, planStepSep)
	if !ok || step == "" {
		return "", "", false
	}
	return nodeKey, step, true
}

// isIterNumber reports whether s is a loop iteration as package plan writes it: decimal digits
// with no leading zero, or "0".
func isIterNumber(s string) bool {
	if s == "" || len(s) > 1 && s[0] == '0' {
		return false
	}
	return strings.Trim(s, "0123456789") == ""
}

// planScopeKey carries, in the context of a plan node's body, the run and key of the node.
type planScopeKey struct{}

type planScope struct{ runID, node string }

// protocol:flows begin NNested

// planScopedStep returns the journal key of the Step named name of runID when it runs in the body
// of a plan flow node of that run: the node's key, ":step:", then name, so a Step a node's body
// runs is recorded once per node and per loop iteration (a loop body's Step runs again in each
// iteration, under that iteration's key), and never meets a Step outside the flow. Outside a node's
// body, or for another run, name is returned unchanged.
func planScopedStep(ctx context.Context, runID, name string) string {
	if s, ok := ctx.Value(planScopeKey{}).(planScope); ok && s.runID == runID {
		return s.node + planStepSep + name
	}
	return name
}

// protocol:flows end

// IsReservedStepName reports whether name starts with a prefix the engine reserves for its own
// journal keys. Step and Parallel refuse such a name, and so do ResolveHaltRef and ResolveStepHalt
// for a step, except a plan flow node's key ("node:<name>"), which halts as a Step does.
func IsReservedStepName(name string) bool {
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// checkStepName refuses an empty developer-chosen step name, and one the engine reserves.
func checkStepName(op, name string) error {
	if name == "" {
		return fmt.Errorf("%s: empty step name: %w", op, ErrConfig)
	}
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

// treeRootID is the root run of the agent tree runID belongs to, read from the ID alone: the run
// a driver drives, and leases, to drive runID. It is a session's turn run ("<id>>@turn/<n>" or
// "<id>>@event/<key>") for the turn and for every sub-run inside it, and otherwise everything up
// to the first '>' (a root run's ID for its sub-agents' and programmatic sub-runs'). rootRunID
// gives the same run from a run's context, and the live-driver check of a halt resolution
// leases it.
func treeRootID(runID string) string {
	id, rest, ok := strings.Cut(runID, subRunSep)
	if !ok || !strings.HasPrefix(rest, sessionMark) {
		return id
	}
	seg, _, _ := strings.Cut(rest, subRunSep)
	return id + subRunSep + seg
}

// protocol:sessions begin TurnId EventId

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

// protocol:delegation begin SStart

// checkRunID refuses a run ID that is empty or contains subRunSep, which only derived run IDs
// carry: a tool call may start the sub-agent run of its own call (SubRunID) or a programmatic
// sub-run it names (RunInfo.SubRunFor), from the context the loop gave it, and a session drives
// its turn's run with that run's ID in the context.
func checkRunID(ctx context.Context, runID string) error {
	if runID == "" {
		// An empty runID would key every run to the same journal, silently cross-contaminating
		// their memoized steps. Reject it rather than corrupt the log.
		return fmt.Errorf("run: empty runID: %w", ErrConfig)
	}
	if !strings.Contains(runID, subRunSep) || derivedRunID(ctx, runID) {
		return nil
	}
	if sr, _ := ctx.Value(sessionRunKey{}).(string); sr == runID && IsSessionRun(runID) {
		return nil
	}
	return fmt.Errorf("run: run ID %q contains %q, which the engine reserves for the run IDs of sub-agents and session turns: %w", runID, subRunSep, ErrConfig)
}

// protocol:delegation end

// protocol:sessions end

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

// spendStep is the key of a spend record (a failed model call's usage). id is fresh for each record
// (newSpendID), so two drivers of one run never write their spend under one key.
func spendStep(id string) string { return spendStepPrefix + id }

// modelStep is the key of the run's n-th model turn.
func modelStep(n int) string { return "@llm/" + strconv.Itoa(n) }

// protocol:claims begin ClaimInsert NotStarted Record

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

// nextAttemptStep is the marker key of the attempt after the one whose marker key is key.
func nextAttemptStep(key string) string {
	base := attemptBase(key)
	if key == base {
		return retryAttemptStep(base, 1)
	}
	rest := strings.TrimPrefix(key, retryAttemptPrefix)
	gen, err := strconv.Atoi(rest[:strings.IndexByte(rest, ':')])
	if err != nil {
		return retryAttemptStep(base, 1) // not a key retryAttemptStep builds; never reached
	}
	return retryAttemptStep(base, gen+1)
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

// protocol:claims end

// approvalStep is the key of the single approve/deny decision (Approve) on the call toolUseID.
func approvalStep(toolUseID string) string { return "approval:" + encodeID(toolUseID) }

// sagaCompensateStep is the key of the compensation of the call toolUseID.
func sagaCompensateStep(toolUseID string) string { return "@saga/compensate/" + encodeID(toolUseID) }

// sagaArgsStep is the key of the arguments the compensable call toolUseID accepted in a saga, as
// the tool received them after tool middleware. It is journaled only when a middleware changed
// the model's arguments (see toolHandler); compensation reads it (see rollbackRun).
func sagaArgsStep(toolUseID string) string { return "@saga/args/" + encodeID(toolUseID) }

// subRunLinkPrefix starts the keys of the call toolUseID's programmatic sub-run links.
func subRunLinkPrefix(toolUseID string) string { return subRunLinkPrefixEnc(encodeID(toolUseID)) }

// subRunLinkPrefixEnc is subRunLinkPrefix for a tool-use ID already encoded (encodeID).
func subRunLinkPrefixEnc(encToolUseID string) string { return "@subrun/" + encToolUseID + "/" }

// subRunLinkStep is the key of the record, in a saga's journal, that its call toolUseID started the
// programmatic sub-run name (RunInfo.SubRunFor), so its rollback walks that sub-run. It holds name.
func subRunLinkStep(toolUseID, name string) string {
	return subRunLinkPrefix(toolUseID) + encodeID(name)
}

// awaitTimeoutStep is the key of AwaitFor's deadline for name.
func awaitTimeoutStep(name string) string { return "await-timeout:" + name }

// stepKey is the in-process key of step name of runID: the run ID's length in bytes, ':', the run
// ID, then the name. The length makes the split exact whatever bytes the two hold, so two
// different steps never share a key (joining them with a separator would not: ("a\x00b", "c") and
// ("a", "b\x00c") both join to "a\x00b\x00c").
func stepKey(runID, name string) string { return strconv.Itoa(len(runID)) + ":" + runID + name }

// runCancelledStep is the key of the record that a run was cancelled, {"reason"}: an end marker,
// like run:complete, that Recover excludes. Cancel writes it for a run that is not a saga, and a
// saga's rollback writes it once a cancellation's rollback has finished.
const runCancelledStep = "run:cancelled"

// runCancelRequestedStep is the key of a saga's rollback request, {"reason"}: Cancel on a saga
// writes it rather than run:cancelled. It is not an end marker, so recovery still lists the run;
// the drive that sees it rolls the run back and writes run:cancelled.
const runCancelRequestedStep = "run:cancel-requested"

// runLimitsStep is the key of the n-th amendment of a run's limits (its turn cap or token budget)
// by a later drive (see RunStart).
func runLimitsStep(n int) string { return "run:limits:" + strconv.Itoa(n) }

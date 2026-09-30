package agent

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
)

// StepKind records what a journal entry represents.
type StepKind string

const (
	StepModel      StepKind = "model"       // an assistant message from the model
	StepToolResult StepKind = "tool_result" // a completed tool call + its result
	StepValue      StepKind = "value"       // a user-authored durable step (see Step[T])
	StepSignal     StepKind = "signal"      // an external event delivered into a run (see Signal/Await)
	StepApproval   StepKind = "approval"    // a durable human approve/deny decision (HITL)
	StepAttempt    StepKind = "attempt"     // "about to run a non-retriable side effect" marker
	StepSagaFail   StepKind = "saga_fail"   // a saga step failed → durable abort trigger
	// StepNotStarted records that the attempt whose marker it names never called its side
	// effect, written by the driver that claimed it (see attempt.go). That attempt then no
	// longer halts a resume, and the effect is re-attempted under a new marker.
	StepNotStarted StepKind = "not_started"
)

// Record is one durably-recorded, named event. The ordered sequence of Records for a
// run is its full, replayable history; resume rebuilds state by replaying them. Name
// is the stable identity used for memoization (see Durable.Do).
type Record struct {
	Name    string   `json:"name"`
	Kind    StepKind `json:"kind"`
	Message *Message `json:"message,omitempty"` // StepModel
	// Usage is token usage. On a StepModel record, the call's. On a StepToolResult or StepSagaFail
	// record, the Usage of the runs the tool call started (sub-agents): their recorded responses.
	Usage *Usage `json:"usage,omitempty"`
	// DiscardedUsage is billed usage no recorded response carries. On a StepModel record, the
	// turn's other model requests: failed attempts a middleware retried, losing hedge targets. On
	// a StepValue record named "@spend/<n>", a model call that failed for good. On a
	// StepToolResult or StepSagaFail record, the rest of the Spend of the runs the tool call
	// started. Nil when there was none. WithTokenBudget counts it.
	DiscardedUsage *Usage          `json:"discarded_usage,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"` // StepToolResult
	Result         json.RawMessage `json:"result,omitempty"`      // StepToolResult / StepValue
	IsError        bool            `json:"is_error,omitempty"`    // StepToolResult
	Approved       bool            `json:"approved,omitempty"`    // StepApproval
	Approver       string          `json:"approver,omitempty"`    // StepApproval written by ApproveAs
	Signature      []byte          `json:"signature,omitempty"`   // StepApproval written by ApproveAs
	// AttemptedAt is the Unix-millis wall-clock time an attempt marker (StepAttempt) was
	// written, i.e. just before a non-retriable side effect fired. It is set once and read
	// back verbatim on replay, so it stays deterministic. Zero (and omitted) on every
	// other kind; a reconciler uses it to honor a grace period before resolving a halt.
	AttemptedAt int64 `json:"attempted_at,omitempty"`
	// Reconciled marks a StepToolResult that ResolveHalt injected from verified evidence
	// rather than one the tool produced by running. It lets a later reader (and bide-audit)
	// tell a reconciled outcome from a clean one at a glance.
	Reconciled bool `json:"reconciled,omitempty"`
	// ReadOnly marks a StepToolResult for a call whose tool was declared ReadOnly when the call
	// ran. A saga rollback skips such a call (it changed nothing) and treats any other completed
	// call as a write, whatever the tool is declared as by the time the rollback runs. A result
	// that ResolveHalt injected is never ReadOnly: only a call that was not retry-safe halts.
	ReadOnly bool `json:"read_only,omitempty"`
	// Evidence is what a reconciler read to decide the outcome (a queried provider record,
	// a message id, a log line). It is carried on the reconciled result and signed with it,
	// so the verdict and its basis live in the journal beside the outcome.
	Evidence json.RawMessage `json:"evidence,omitempty"`
	// Claim is the random id of the driver that wrote an attempt marker (see ClaimAttempt).
	// A driver runs the side effect only if the marker it gets back carries its own claim, so
	// two drivers of the same run can never both run it, whatever their leases say.
	Claim string `json:"claim,omitempty"`
	// Salt is SaltSize random bytes a store sets when it first journals the record (see
	// JournalEntry), replacing any salt the step returned. It is persisted with the record and
	// read back verbatim, so it is stable across replay and across stores. It has no meaning to
	// the run; the audit trail needs it. An audit leaf commits to the record's journal encoding,
	// salt included, and an inclusion proof for one record carries its neighbours' leaf hashes, so
	// without the salt anyone holding a proof could confirm a guessed neighbour (an approval, a
	// small tool result) by hashing it. The salt is disclosed only with its own record.
	Salt []byte `json:"salt,omitempty"`
}

// markerTime is the time an attempt marker's AttemptedAt records, or the zero time when it records
// none. Zero means a marker written before the field existed. A negative value is not a time the
// engine writes (it stamps time.Now().UnixMilli()), so it is a hand-written or tampered row; read as
// a time it would place the attempt in 1969 and make any grace period look long elapsed, so it
// counts as no timestamp too. A marker in the future (clock skew between nodes, or a tampered row)
// is kept: it is younger than any grace period, so WithMinHaltAge waits on it.
func markerTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// SaltSize is the length of Record.Salt: 32 bytes (256 bits) from crypto/rand.
const SaltSize = 32

// JournalEntry returns the bytes a store persists when it records rec as the step named name:
// rec with Name set to name and a fresh random Salt (see Record.Salt), in its journal encoding
// (EncodeRecord). Every Durable implementation must record a new step through it, so every
// record carries a salt; the audit package refuses to commit a record without one. It errors
// only if the system's random source fails or rec cannot be encoded.
func JournalEntry(name string, rec Record) ([]byte, error) {
	var back Record
	b, _, err := journalEntry(name, rec, &back)
	return b, err
}

// journalEntry is JournalEntry that also sets *back and reports stable as encodeRecord does.
func journalEntry(name string, rec Record, back *Record) (b []byte, stable bool, err error) {
	salt := make([]byte, SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, false, fmt.Errorf("salt step %q: %w (%w)", name, err, ErrStorage)
	}
	rec.Name = name
	rec.Salt = salt
	return encodeRecord(rec, back)
}

// EncodeRecord returns the journal encoding of r: the bytes a store persists for it and the
// bytes an audit leaf commits to. Every store writes exactly these bytes, so a record's journal
// form does not depend on the backend, and every store hands back DecodeRecord of them, on the
// live path as on replay, so a resumed run rebuilds exactly the conversation the live run had.
//
// The encoding is JSON with two properties that make it canonical:
//
//   - It is a fixed point: decoding it with DecodeRecord and encoding the result again yields
//     the same bytes. So a store holds exactly EncodeRecord(rec) for the record rec it hands
//     back, and an audit leaf computed from a record read back from any store is the bytes that
//     store persisted.
//   - It keeps the content of every json.RawMessage (a tool's arguments and result, a step's
//     value, a reconciler's evidence) byte for byte, except whitespace between JSON tokens,
//     which is removed. Nothing is HTML-escaped: a tool result that says "<b>" is journaled as
//     "<b>", not with the escapes json.Marshal writes for < and >, so an adapter that forwards
//     the JSON text to a model sends what the tool returned. Key order, number spelling ("1.50",
//     "1e20"), escapes already in the text, and even invalid UTF-8 inside a JSON string are kept
//     as they are.
//
// Insignificant whitespace is dropped because encoding/json compacts every raw value it embeds,
// and one spelling per JSON value is what an audit leaf should commit to. For the same reason
// the line and paragraph separators U+2028 and U+2029 are always written as their JSON escapes,
// which encoding/json emits in some builds but not others. In a Go string field
// (message text, a reasoning signature), invalid UTF-8 becomes U+FFFD, as encoding/json writes
// it; since the stores hand out only the decoded form, the live and replayed conversations agree.
func EncodeRecord(r Record) ([]byte, error) {
	var back Record
	b, _, err := encodeRecord(r, &back)
	return b, err
}

// encodeRecord is EncodeRecord that also sets *back to the record its first pass decoded and
// reports whether that is DecodeRecord of the bytes returned (stable). It is when both passes
// wrote the same bytes: DecodeRecord depends on nothing but its input, so decoding the returned
// bytes again would build exactly *back, and a caller that needs that record (MemStore.Do) can
// skip the decode. When the passes differ, *back must not stand in for the bytes' decoding.
func encodeRecord(r Record, back *Record) (b []byte, stable bool, err error) {
	first, err := marshalJournal(r)
	if err != nil {
		return nil, false, err
	}
	// One decode and re-encode reaches the fixed point. The first pass may write invalid UTF-8 in
	// a Go string field as the JSON escape for U+FFFD (a GOEXPERIMENT=nojsonv2 build does), which
	// decodes to a valid U+FFFD that a later pass writes verbatim; every other part of the
	// encoding is already stable.
	*back, err = DecodeRecord(first)
	if err != nil {
		return nil, false, err
	}
	b, err = marshalJournal(*back)
	if err != nil {
		return nil, false, err
	}
	return b, bytes.Equal(first, b), nil
}

// DecodeRecord decodes a record from its journal encoding (see EncodeRecord) into an independent
// copy that shares no memory with b.
func DecodeRecord(b []byte) (Record, error) {
	if h := decodeHook.Load(); h != nil {
		(*h)(b)
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, fmt.Errorf("decode stored record: %w (%w)", err, ErrStorage)
	}
	return r, nil
}

// decodeHook, when set (by tests only), is called with the bytes of every DecodeRecord, so a test
// can count the decodes a code path makes.
var decodeHook atomic.Pointer[func([]byte)]

// DecodeStoredRecord decodes the record a store holds for the step name of run runID (see
// DecodeRecord) and checks that the record carries that name. Every record is journaled with the
// name it is stored under (JournalEntry), and the engine finds records by the name inside them
// (IsComplete, the approval tally, a signal, an attempt marker), so a row whose record names
// another step (a row edited or copied in the backing store) would be read as that step: a row
// under "x" holding a record named run:complete would mark an unfinished run complete. Such a row
// is ErrStorage, naming the run and the key, like any other stored record that does not decode:
// the store's contents are wrong, whatever wrote them. Fields this version does not know still
// decode, so a journal a newer version wrote stays readable.
//
// Every Durable implementation must read a record back through it, in Do and in History, or hand
// back a record known to be what it returns (MemStore.Do keeps the decoding it made while
// encoding the record it writes, when that is the stored bytes' decoding, and checks the name).
func DecodeStoredRecord(runID, name string, b []byte) (Record, error) {
	r, err := DecodeRecord(b)
	if err != nil {
		return Record{}, fmt.Errorf("run %s, step %q: %w", runID, name, err)
	}
	if r.Name != name {
		return Record{}, fmt.Errorf("run %s: the row stored as step %q holds a record named %q: %w", runID, name, r.Name, ErrStorage)
	}
	return r, nil
}

// marshalJournal is json.Marshal without HTML escaping and without the newline an Encoder
// appends.
//
// U+2028 and U+2029 are always written as their six-character JSON escapes. encoding/json escapes
// them inside a raw value when built on its v2 implementation (the Go 1.27 default) but not when
// built with GOEXPERIMENT=nojsonv2; escaping them here gives the journal one encoding in every
// build. Both byte sequences can occur only inside a JSON string (everything outside one is
// ASCII), where the escape denotes the same character.
func marshalJournal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	// bytes.ReplaceAll copies b even when there is nothing to replace. Both separators start with
	// the byte 0xE2, so an encoding that holds neither, as most do, skips both copies.
	if bytes.IndexByte(b, lineSep[0]) >= 0 {
		b = bytes.ReplaceAll(b, []byte(lineSep), []byte(jsonEscape+"2028"))
		b = bytes.ReplaceAll(b, []byte(paraSep), []byte(jsonEscape+"2029"))
	}
	return b, nil
}

const (
	lineSep    = "\xe2\x80\xa8" // U+2028 LINE SEPARATOR, UTF-8 encoded
	paraSep    = "\xe2\x80\xa9" // U+2029 PARAGRAPH SEPARATOR, UTF-8 encoded
	jsonEscape = "\x5cu"        // a backslash and u: the prefix of a JSON \uXXXX escape
)

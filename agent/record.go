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
	// StepHeader is the journal header: the first record of every run's journal, named "@journal",
	// whose Format names the journal format the run is written in (see JournalFormat). The
	// journal writes it before a run's first record and checks it before reading any other.
	StepHeader StepKind = "header"
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
	// a StepValue record named "@spend/<id>", a model call that failed for good; on one named
	// "@spend-late/<id>", requests that ended after their turn was recorded, or whose turn another
	// driver recorded. On a
	// StepToolResult or StepSagaFail record, the rest of the Spend of the runs the tool call
	// started. Nil when there was none. WithTokenBudget counts it.
	DiscardedUsage *Usage `json:"discarded_usage,omitempty"`
	// ModelTurn is the model-turn metadata of a StepModel record: why the turn ended, the model that
	// answered it, and digests of the prompt and tools it was sent. Nil on every other kind, and on
	// a StepModel record that carries none. Read it through the nil-safe accessors (Record.Finish,
	// RawFinish, Model, PromptDigest, ToolsDigest). It sits behind a pointer to keep Record small
	// (see TestRecord_Size); its fields are journaled in place, as members of the record.
	*ModelTurn
	ToolUseID string          `json:"tool_use_id,omitempty"` // StepToolResult
	Result    json.RawMessage `json:"result,omitempty"`      // StepToolResult / StepValue
	IsError   bool            `json:"is_error,omitempty"`    // StepToolResult
	Approved  bool            `json:"approved,omitempty"`    // StepApproval
	// ApproverSignature is the signed decision of a StepApproval record written by SubmitDecision:
	// who decided and their signature. Nil on every other record, including an approval that Approve
	// or Deny wrote. Read it through the nil-safe accessors (Record.Approver, ApproverAlg,
	// Signature). It sits behind a pointer to keep Record small (see TestRecord_Size); its fields
	// are journaled in place, as members of the record.
	*ApproverSignature
	// AttemptedAt is the Unix-millis wall-clock time an attempt marker (StepAttempt) was
	// written, i.e. just before a non-retriable side effect fired. It is set once and read
	// back verbatim on replay, so it stays deterministic. Zero (and omitted) on every
	// other kind; a reconciler uses it to honor a grace period before resolving a halt.
	AttemptedAt int64 `json:"attempted_at,omitempty"`
	// Reconciled marks a StepToolResult that ResolveHalt injected from verified evidence
	// rather than one the tool produced by running. It lets a later reader (and bide-audit)
	// tell a reconciled outcome from a clean one at a glance.
	Reconciled bool `json:"reconciled,omitempty"`
	// OutcomeUnknown marks a StepSagaFail record for a step whose outcome is unknown: a
	// retry-safe step that failed with ErrToolOutcomeUnknown, or returned an error after its
	// deadline. It may have committed, so a rollback reports it (SagaAborted.UnknownOutcome)
	// rather than take it for a step that changed nothing.
	OutcomeUnknown bool `json:"outcome_unknown,omitempty"`
	// Redacted marks a record whose stored bytes a redaction replaced with a tombstone, the
	// reserved form {"redacted":{"leaf_hash":"<hex>","at_ms":<ms>}}: the hex audit leaf hash of
	// the bytes it replaced and the Unix-millis time of the redaction. Only Name is meaningful on
	// such a record, and Raw returns the tombstone. It is never journaled: the tombstone is what the
	// store holds. A run holding a redacted record is over and cannot be driven again.
	Redacted bool `json:"-"`
	// stamped marks a salt the engine drew for a record it is about to record (stampSalt), which
	// JournalEntry keeps instead of drawing another, so the engine can tell its own record from
	// another writer's by the salt the journal holds. It and Redacted sit beside Reconciled so the
	// three flags share one word, which is why it is not in ModelTurn: there it would save no space
	// and would make every stamped record allocate one.
	stamped bool
	// Safety is the Safety of the tool a StepToolResult or StepSagaFail record's call ran under
	// (or, for a denied call, would have run under), and Approval its approval gate, nil for an
	// ungated tool. A saga rollback reads Safety: it skips a call that ran ReadOnly (it changed
	// nothing) and treats any other completed call as a write, whatever the tool is declared as
	// by the time the rollback runs. Safety is nil on a result that ResolveHalt injected, which
	// is a write (only a call that was not retry-safe halts), and on every other kind.
	Safety   *Safety         `json:"safety,omitempty"`
	Approval *ApprovalPolicy `json:"approval,omitempty"`
	// Evidence is what a reconciler read to decide the outcome (a queried provider record,
	// a message id, a log line). It is carried on the reconciled result and signed with it,
	// so the verdict and its basis live in the journal beside the outcome.
	Evidence json.RawMessage `json:"evidence,omitempty"`
	// Format is the journal format a StepHeader record names (see JournalFormat). Empty on every
	// other kind.
	Format string `json:"format,omitempty"`

	// claim is the random id of the driver that wrote an attempt marker (see ClaimAttempt and
	// ClaimID). A driver runs the side effect only if the marker it gets back carries its own
	// claim, so two drivers of the same run can never both run it, whatever their leases say.
	claim string
	// salt is SaltSize random bytes the journal sets when it first records the record (see Salt
	// and JournalEntry).
	salt []byte
	// raw is the bytes the record was decoded from (see Raw).
	raw []byte
}

// ModelTurn is a StepModel record's model-turn metadata (see Record.ModelTurn). A Record copy
// shares it, so replace it rather than change a shared one.
type ModelTurn struct {
	// Finish is why the model turn ended, and RawFinish the provider's own reason as it sent it
	// (see FinishReason). Empty on a record written before they were journaled.
	Finish    FinishReason `json:"finish,omitempty"`
	RawFinish string       `json:"raw_finish,omitempty"`
	// Model identifies the model that answered the turn (see ModelInfoOf), for audit and for a
	// run whose turns a middleware sent to different providers. Nil when the model does not
	// describe itself or no request produced the response (a middleware built it).
	Model *ModelInfo `json:"model,omitempty"`
	// PromptDigest and ToolsDigest are digests of the system prompt and the tool set the turn was
	// sent (see the functions of the same names), so an auditor can tell which instructions and
	// which tools each answer was given, although agent-level defaults stay live across a
	// redeploy. Empty when the turn was sent no system prompt, or no tools.
	PromptDigest string `json:"prompt_digest,omitempty"`
	ToolsDigest  string `json:"tools_digest,omitempty"`
}

// ApproverSignature is the signed decision a StepApproval record written by SubmitDecision carries
// (see Record.ApproverSignature). A Record copy shares it, so replace it rather than change a
// shared one.
type ApproverSignature struct {
	Approver    string `json:"approver,omitempty"`     // the approver's id (Decision.ApproverID)
	ApproverAlg Alg    `json:"approver_alg,omitempty"` // the scheme Signature is under
	Signature   []byte `json:"signature,omitempty"`    // the approver's signature (Decision.Signature)
}

// Finish returns why the model turn ended (ModelTurn.Finish), or "" when the record carries no
// model-turn metadata.
func (r Record) Finish() FinishReason {
	if r.ModelTurn == nil {
		return ""
	}
	return r.ModelTurn.Finish
}

// RawFinish returns the provider's own finish reason (ModelTurn.RawFinish), or "" when the record
// carries no model-turn metadata.
func (r Record) RawFinish() string {
	if r.ModelTurn == nil {
		return ""
	}
	return r.ModelTurn.RawFinish
}

// Model returns the model that answered the turn (ModelTurn.Model), or nil when the record
// carries no model-turn metadata.
func (r Record) Model() *ModelInfo {
	if r.ModelTurn == nil {
		return nil
	}
	return r.ModelTurn.Model
}

// PromptDigest returns the digest of the system prompt the turn was sent (ModelTurn.PromptDigest),
// or "" when the record carries no model-turn metadata.
func (r Record) PromptDigest() string {
	if r.ModelTurn == nil {
		return ""
	}
	return r.ModelTurn.PromptDigest
}

// ToolsDigest returns the digest of the tool set the turn was sent (ModelTurn.ToolsDigest), or ""
// when the record carries no model-turn metadata.
func (r Record) ToolsDigest() string {
	if r.ModelTurn == nil {
		return ""
	}
	return r.ModelTurn.ToolsDigest
}

// Approver returns the approver of a signed decision (ApproverSignature.Approver), or "" when the
// record carries none.
func (r Record) Approver() string {
	if r.ApproverSignature == nil {
		return ""
	}
	return r.ApproverSignature.Approver
}

// ApproverAlg returns the scheme a signed decision is under (ApproverSignature.ApproverAlg), or ""
// when the record carries none.
func (r Record) ApproverAlg() Alg {
	if r.ApproverSignature == nil {
		return ""
	}
	return r.ApproverSignature.ApproverAlg
}

// Signature returns the signature of a signed decision (ApproverSignature.Signature), or nil when
// the record carries none. The slice is the record's own, as the field was.
func (r Record) Signature() []byte {
	if r.ApproverSignature == nil {
		return nil
	}
	return r.ApproverSignature.Signature
}

// modelTurn returns the ModelTurn of its arguments, or nil when they are all empty, so a record
// built in memory carries metadata exactly when its journal encoding does.
func modelTurn(t ModelTurn) *ModelTurn {
	if t == (ModelTurn{}) {
		return nil
	}
	return &t
}

// ClaimID returns the random id of the driver that wrote this attempt marker or not-started record
// (see ClaimAttempt), or "" for any other record. The journal sets it when it records the marker,
// and it is read back verbatim.
func (r Record) ClaimID() string { return r.claim }

// Salt returns the record's salt: SaltSize random bytes the journal sets when it first records the
// record (see JournalEntry), replacing any salt the step returned (the engine draws the salt of its
// own model records itself, just as randomly, to tell its record from another writer's). It is persisted with the record
// and read back verbatim, so it is stable across replay and across stores. It has no meaning to
// the run; the audit trail needs it. An audit leaf commits to the record's journal encoding, salt
// included, and an inclusion proof for one record carries its neighbours' leaf hashes, so without
// the salt anyone holding a proof could confirm a guessed neighbour (an approval, a small tool
// result) by hashing it. The salt is disclosed only with its own record. The returned slice is a
// copy, nil for a record built in memory.
func (r Record) Salt() []byte { return bytes.Clone(r.salt) }

// Raw returns the bytes the store holds for the record, verbatim, for a record read back from a
// journal (Journal.Get, Journal.History, Journal.Records, or a Durable's Do and History). It is
// nil for a record built in memory. The returned slice is a copy.
func (r Record) Raw() []byte { return bytes.Clone(r.raw) }

// recordFields is Record without its methods, so the JSON codec's default struct encoding applies
// to it (see recordWire).
type recordFields Record

// recordWire is a record's journal encoding: its exported fields, then its claim and salt. Those
// two are unexported so no caller can change them, and are written last, where they have always
// been.
type recordWire struct {
	*recordFields
	Claim string `json:"claim,omitempty"`
	Salt  []byte `json:"salt,omitempty"`
}

// MarshalJSON returns the record's JSON encoding, claim and salt included, without HTML escaping
// (see EncodeRecord for the canonical form a store persists).
func (r Record) MarshalJSON() ([]byte, error) { return marshalRecord(r) }

// UnmarshalJSON decodes a record from its JSON encoding, claim and salt included.
func (r *Record) UnmarshalJSON(b []byte) error {
	back, err := unmarshalRecord(b)
	if err != nil {
		return err
	}
	*r = back
	return nil
}

// marshalRecord is the journal encoding of r, written through its wire form so that no Marshaler
// method runs inside another.
func marshalRecord(r Record) ([]byte, error) {
	return marshalJournal(recordWire{recordFields: (*recordFields)(&r), Claim: r.claim, Salt: r.salt})
}

// unmarshalRecord decodes a record from its journal encoding. Its raw bytes are not set.
func unmarshalRecord(b []byte) (Record, error) {
	if h := decodeHook.Load(); h != nil {
		(*h)(b)
	}
	var f recordFields
	w := recordReadWire{recordWire: recordWire{recordFields: &f}}
	if err := json.Unmarshal(b, &w); err != nil {
		return Record{}, err
	}
	f.Approval = w.Approval.policy()
	f.claim, f.salt, f.raw = w.Claim, w.Salt, nil
	return Record(f), nil
}

// recordReadWire is the wire form a record is read through. Its approval member shadows
// Record.Approval's (a shallower field wins), so a stored record's approval is read leniently
// (storedApproval): the strict ApprovalPolicy decoding is for a policy being configured or
// received, and a stored record keeps DecodeStoredRecord's promise that a journal a newer version
// wrote stays readable.
type recordReadWire struct {
	recordWire
	Approval *storedApproval `json:"approval,omitempty"`
}

// storedApproval is a journaled approval policy, read leniently: members this version does not
// know are ignored, {"single":true} is SingleApproval whatever else a newer version wrote beside
// it, and anything else is the m-of-n policy its need and approvers give. The policy is a record of
// the gate the call ran under; the gate a run enforces is always the registered tool's.
type storedApproval struct {
	Single    json.RawMessage `json:"single,omitempty"`
	Need      int             `json:"need"`
	Approvers []string        `json:"approvers,omitempty"`
}

// policy returns the ApprovalPolicy a stored approval records, or nil for none.
func (a *storedApproval) policy() *ApprovalPolicy {
	switch {
	case a == nil:
		return nil
	case string(bytes.TrimSpace(a.Single)) == "true":
		return SingleApproval()
	}
	return &ApprovalPolicy{Need: a.Need, Approvers: a.Approvers}
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
// rec with Name set to name and a fresh random Salt (see Record.Salt; a salt the engine drew for
// the record is kept), in its journal encoding
// (EncodeRecord). Every Durable implementation must record a new step through it, so every
// record carries a salt; the audit package refuses to commit a record without one. It errors
// only if the system's random source fails or rec cannot be encoded.
func JournalEntry(name string, rec Record) ([]byte, error) {
	if !rec.stamped || len(rec.salt) != SaltSize {
		salt, err := newSalt()
		if err != nil {
			return nil, fmt.Errorf("salt step %q: %w (%w)", name, err, ErrStorage)
		}
		rec.salt = salt
	}
	rec.Name = name
	return EncodeRecord(rec)
}

// newSalt returns SaltSize random bytes from crypto/rand.
func newSalt() ([]byte, error) {
	salt := make([]byte, SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return salt, nil
}

// stampSalt gives r, a record the engine is about to record, a fresh salt that JournalEntry keeps,
// so the engine can tell its own record by it (ownRecord). The salt is as random as the one
// JournalEntry would draw.
func stampSalt(r *Record) error {
	salt, err := newSalt()
	if err != nil {
		return fmt.Errorf("salt: %w (%w)", err, ErrStorage)
	}
	r.salt, r.stamped = salt, true
	return nil
}

// ownRecord reports whether held, a record read from the journal, is built, a record the engine
// stamped (stampSalt) and tried to record: the journal holds the salt it was given. A store that
// breaks the contract and journals no salt is compared by the journal encoding instead, both
// records put through EncodeRecord with no salt, so its canonical form (compact JSON, U+FFFD for invalid
// UTF-8) does not make the engine's own record look like another writer's.
func ownRecord(held, built Record) bool {
	if s := held.Salt(); s != nil {
		return bytes.Equal(s, built.salt)
	}
	enc := func(r Record) []byte {
		r.salt, r.stamped, r.raw, r.claim = nil, false, nil, ""
		b, err := EncodeRecord(r) // the fixed point a store holds (see EncodeRecord)
		if err != nil {
			return nil
		}
		return b
	}
	built.Name = held.Name
	a, b := enc(held), enc(built)
	return a != nil && bytes.Equal(a, b)
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
	b, err := marshalRecord(r)
	if err != nil {
		return nil, err
	}
	// One decode and re-encode reaches the fixed point. The first pass writes invalid UTF-8 in a
	// Go string field as U+FFFD: as its JSON escape when built with GOEXPERIMENT=nojsonv2, which
	// decodes to a valid U+FFFD that a later pass writes verbatim. Every other part of the encoding
	// is already stable, so an encoding with no such escape is the fixed point.
	if !bytes.Contains(b, []byte(jsonEscape+"fffd")) {
		return b, nil
	}
	back, err := unmarshalRecord(b)
	if err != nil {
		return nil, fmt.Errorf("decode stored record: %w (%w)", err, ErrStorage)
	}
	return marshalRecord(back)
}

// DecodeRecord decodes a record from its journal encoding (see EncodeRecord) into an independent
// copy that shares no memory with b. The record's Raw is a copy of b.
func DecodeRecord(b []byte) (Record, error) { return decodeRecord(bytes.Clone(b)) }

// decodeRecord is DecodeRecord for bytes the record may keep as its Raw: bytes nothing modifies.
func decodeRecord(b []byte) (Record, error) {
	r, err := unmarshalRecord(b)
	if err != nil {
		return Record{}, fmt.Errorf("decode stored record: %w (%w)", err, ErrStorage)
	}
	r.raw = b
	return r, nil
}

// DecodeStoredRecord decodes the record a store holds for the step name of run runID (see
// DecodeRecord) and checks that the record carries that name. Every record is journaled with the
// name it is stored under (JournalEntry), and the engine finds records by the name inside them
// (IsComplete, the approval tally, a signal, an attempt marker), so a row whose record names
// another step (a row edited or copied in the backing store) would be read as that step: a row
// under "x" holding a record named run:complete would mark an unfinished run complete. Such a row
// is ErrStorage, naming the run and the key, like any other stored record that does not decode:
// the store's contents are wrong, whatever wrote them. Fields this version does not know still
// decode, so a journal a newer version wrote stays readable; that includes members of the
// record's approval policy, which a stored record reads leniently (ApprovalPolicy's own
// UnmarshalJSON is strict, for a policy being configured or received).
//
// A redaction tombstone (see Record.Redacted) decodes as a record that carries only its name.
//
// Every Durable implementation must read a record back through it, in Do and in History.
func DecodeStoredRecord(runID, name string, b []byte) (Record, error) {
	return decodeStored(runID, name, bytes.Clone(b))
}

// decodeStored is DecodeStoredRecord for bytes the record may keep as its Raw: bytes nothing
// modifies, such as an entry a store handed the journal.
func decodeStored(runID, name string, b []byte) (Record, error) {
	if isTombstone(b) {
		return Record{Name: name, Redacted: true, raw: b}, nil
	}
	r, err := decodeRecord(b)
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
	if bytes.Contains(b, []byte(lineSep)) {
		b = bytes.ReplaceAll(b, []byte(lineSep), []byte(jsonEscape+"2028"))
	}
	if bytes.Contains(b, []byte(paraSep)) {
		b = bytes.ReplaceAll(b, []byte(paraSep), []byte(jsonEscape+"2029"))
	}
	return b, nil
}

const (
	lineSep    = "\xe2\x80\xa8" // U+2028 LINE SEPARATOR, UTF-8 encoded
	paraSep    = "\xe2\x80\xa9" // U+2029 PARAGRAPH SEPARATOR, UTF-8 encoded
	jsonEscape = "\x5cu"        // a backslash and u: the prefix of a JSON \uXXXX escape
)

// decodeHook, when set (by tests only), is called with the bytes of every record decoded from its
// journal encoding, so a test can count the decodes a code path makes.
var decodeHook atomic.Pointer[func([]byte)]

// tombstone is the reserved form of a redaction tombstone (see Record.Redacted).
type tombstone struct {
	Redacted *struct {
		LeafHash string `json:"leaf_hash"`
		AtMs     int64  `json:"at_ms"`
	} `json:"redacted"`
}

// isTombstone reports whether b is a redaction tombstone: a JSON object whose only member is
// "redacted", holding a leaf hash. A record's encoding always starts with its "name" member, so
// no record is ever read as a tombstone.
func isTombstone(b []byte) bool {
	if !bytes.HasPrefix(b, []byte(`{"redacted":`)) {
		return false
	}
	var t tombstone
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil || dec.More() {
		return false
	}
	return t.Redacted != nil && t.Redacted.LeafHash != ""
}

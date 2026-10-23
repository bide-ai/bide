package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Redactor is the optional store capability that replaces one entry's bytes with a redaction
// tombstone (see Record.Redacted). It is not part of Store, so the engine, which never obtains it,
// keeps requirement A6: a run is only ever appended to. A store opts in by implementing Redact, and a
// caller finds it with Capability, so a wrapper that implements Unwrap keeps its inner store's
// Redactor. SQLite (store/sqlite), Postgres (store/postgres) and MemStore implement it; Redact (the
// function) is the way to use it.
type Redactor interface {
	// Redact replaces runID's entry name with tombstone, atomically, only if all of these hold:
	//
	//   - the entry's bytes are exactly current (a compare and swap, so a row another writer changed
	//     is refused, never overwritten);
	//   - CheckRedaction accepts the call: tombstone is a well-formed tombstone, its leaf hash is
	//     the leaf hash of current (the store recomputes it itself, so a caller cannot write a
	//     tombstone that breaks every root over the run), and name is not a record a verifier or the
	//     engine needs (RedactionProtected);
	//   - the run holds an end marker (run:complete, run:aborted or run:cancelled), read in the
	//     same transaction as the replacement, so the run is over; and
	//   - no lease on the run is live (a Leaser store; a store without leases skips this check).
	//
	// It reports whether it replaced the entry. An entry that already holds a tombstone with the
	// leaf hash of current is not an error and replaces nothing (false, nil), so a pass that
	// stopped partway can be run again. Every other refusal is an error that wraps one of the
	// ErrRedact sentinels and ErrConfig; a store failure wraps ErrStorage. The entry keeps its
	// Seq, name and run, so positions and every proof of another record are unchanged (A2, A4).
	Redact(ctx context.Context, runID, name string, current, tombstone []byte) (bool, error)
}

// The refusals of Redactor.Redact. Each wraps ErrConfig: the call asked for something the journal
// does not allow, and nothing was written.
var (
	// ErrRedactNotTerminal is a redaction in a run with no end marker.
	ErrRedactNotTerminal = fmt.Errorf("redaction refused: the run has no end marker (run:complete, run:aborted or run:cancelled), so it is not over: %w", ErrConfig)
	// ErrRedactLeased is a redaction in a run whose lease is live: a driver may be running it.
	ErrRedactLeased = fmt.Errorf("redaction refused: a lease on the run is live, so a driver may still be running it: %w", ErrConfig)
	// ErrRedactProtected is a redaction of a record a verifier or the engine needs (see
	// RedactionProtected).
	ErrRedactProtected = fmt.Errorf("redaction refused: the record is one a verifier or the engine needs: %w", ErrConfig)
	// ErrRedactMismatch is a tombstone whose leaf hash is not the leaf hash of the bytes it
	// replaces, or one that is not a well-formed tombstone.
	ErrRedactMismatch = fmt.Errorf("redaction refused: the tombstone does not keep the leaf hash of the bytes it replaces: %w", ErrConfig)
	// ErrRedactSession is a redaction in a session's journal or turn run: the session reads its
	// turns' runs and never ends, so a redaction there is refused.
	ErrRedactSession = fmt.Errorf("redaction refused: the run belongs to a session, which does not end: %w", ErrConfig)
	// ErrRedactParentLive is a redaction in a sub-agent's run while a run above it is not over: the
	// parent reads the child's records when it replays the call.
	ErrRedactParentLive = fmt.Errorf("redaction refused: a run above this one (the sub-agent's parent) is not over: %w", ErrConfig)
	// ErrRedactChanged is a redaction of an entry whose bytes are not the bytes the caller read:
	// another writer changed it, or the caller's copy is not what is stored.
	ErrRedactChanged = fmt.Errorf("redaction refused: the stored entry is not the one the caller read: %w", ErrConfig)
	// ErrRedactNoEntry is a redaction of an entry the run does not hold.
	ErrRedactNoEntry = fmt.Errorf("redaction refused: the run holds no such entry: %w", ErrConfig)
)

// journalLeafTag is the tag audit.JournalLeafHash puts before a record's stored bytes.
const journalLeafTag = "bide.audit.journal-leaf.v1\x00"

// LeafHash returns the audit leaf hash of a journal record from the bytes the journal stores for
// it: SHA-256(0x00 || "bide.audit.journal-leaf.v1\x00" || recordBytes), the RFC 6962 leaf hash of
// the journal leaf. It is audit.JournalLeafHash; a redaction records it in the tombstone that
// replaces the bytes, and a test pins that the two agree.
func LeafHash(recordBytes []byte) []byte {
	h := sha256.New()
	h.Write([]byte{0})
	h.Write([]byte(journalLeafTag))
	h.Write(recordBytes)
	return h.Sum(nil)
}

// NewTombstone returns the redaction tombstone that replaces the stored bytes recordBytes of the
// record named name:
// {"redacted":{"leaf_hash":"<hex of LeafHash(recordBytes)>","at_ms":<at in Unix millis>,"name":"<name>"}}.
// The name is the row's key, which the row keeps; the tombstone repeats it so that an export of
// the journal, which holds bytes and no keys, still says which record it replaced. It refuses
// bytes that are already a tombstone, an empty name, and an at before the Unix epoch.
func NewTombstone(name string, recordBytes []byte, at time.Time) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("NewTombstone: no record name: %w", ErrConfig)
	}
	if isTombstone(recordBytes) {
		return nil, fmt.Errorf("NewTombstone: the bytes are already a tombstone: %w", ErrRedactMismatch)
	}
	ms := at.UnixMilli()
	if ms < 0 {
		return nil, fmt.Errorf("NewTombstone: the time %s is before the Unix epoch: %w", at.UTC().Format(time.RFC3339), ErrConfig)
	}
	nameJSON, err := json.Marshal(name)
	if err != nil {
		return nil, fmt.Errorf("NewTombstone: %w", ErrConfig)
	}
	return fmt.Appendf(nil, `{"redacted":{"leaf_hash":"%s","at_ms":%d,"name":%s}}`, hex.EncodeToString(LeafHash(recordBytes)), ms, nameJSON), nil
}

// TombstoneLeafHash returns the leaf hash a tombstone keeps, and false if b is not a tombstone
// in the exact form NewTombstone writes (see isTombstone).
func TombstoneLeafHash(b []byte) ([]byte, bool) {
	leaf, _, _, ok := parseTombstone(b)
	if !ok {
		return nil, false
	}
	h, err := hex.DecodeString(leaf)
	if err != nil {
		return nil, false
	}
	return h, true
}

// TombstoneMillis returns the time a tombstone records, in Unix millis, and false if b is not a
// tombstone in the exact form NewTombstone writes.
func TombstoneMillis(b []byte) (int64, bool) {
	_, ms, _, ok := parseTombstone(b)
	return ms, ok
}

// TombstoneName returns the record name a tombstone carries, and false if b is not a tombstone or
// names none (the reserved form allows a tombstone without one).
func TombstoneName(b []byte) (string, bool) {
	_, _, name, ok := parseTombstone(b)
	return name, ok && name != ""
}

// journalHeaderName is the name of a journal's header entry.
const journalHeaderName = "@journal"

// redactionClass says what a redaction does with a record by its key.
type redactionClass int

const (
	// classPayload: content a redaction may erase, and the Payloads preset selects.
	classPayload redactionClass = iota
	// classProtected: a record a verifier or the engine reads the content or the structure of.
	classProtected
	// classBookkeeping: engine bookkeeping with no personal content (usage, branch choices, timers):
	// not protected, but not chosen by the Payloads preset; a selector may name it.
	classBookkeeping
)

// redactionClasses classifies every key family the engine writes (reservedPrefixes, and the
// engine-internal "@" keys), longest prefix first. TestRedactionClasses_CoverEveryReservedKey fails
// when a key family is added without a line here, so a new record is classified on purpose.
// A key that matches none (a developer-chosen step name) is payload.
var redactionClasses = []struct {
	prefix string
	class  redactionClass
	why    string
}{
	{journalHeaderName, classProtected, "the journal header"},
	{"@saga/", classProtected, "a saga record (compensation memo or arguments): a re-drive of the run reads what was compensated"},
	{"@subrun/", classProtected, "a saga's link to a sub-run it started: a rollback reads it"},
	{"@spend-late/", classBookkeeping, ""},
	{"@spend/", classBookkeeping, ""},
	{"@llm/", classPayload, ""},
	{"@retrieval/", classPayload, ""},
	{runStartStep, classPayload, ""},
	{"run:", classProtected, "a run state record (cancellation, limits); the end markers are erasable when named (RedactionProtected), and the Payloads preset never selects them"},
	{"tool:", classPayload, ""},
	{"attempt:", classProtected, "an attempt marker: the at-most-once evidence"},
	{"approval-tally:", classProtected, "a record an approval count rests on"},
	{"approval-identity:", classProtected, "a record an approval count rests on"},
	{"approval-status:", classProtected, "the revocation basis an approval count rests on"},
	{"approval-exec-hold:", classProtected, "a record the pre-execution check of an approval rests on"},
	{"approval-deadline:", classProtected, "a gate's deadline"},
	{"approval-pending:", classBookkeeping, ""},
	{"approval-pause:", classBookkeeping, ""},
	{"approval-request:", classPayload, ""},
	{"approval:", classPayload, ""}, // a decision: kept unless the selector asks for approvals
	{"signal:", classPayload, ""},
	{"await-timeout:", classBookkeeping, ""},
	{"await-resolved:", classBookkeeping, ""},
	{"timer:", classBookkeeping, ""},
	{"interrupt:", classPayload, ""},
	{"chanack:", classBookkeeping, ""},
	{"chan:", classPayload, ""},
	{"turn/", classProtected, "a session record"},
	{"start/", classProtected, "a session record"},
	{"from/", classProtected, "a session record"},
	{"audit:", classProtected, "an audit leaf a verifier relies on"},
	{"node:", classPayload, ""},
	{"switch:", classBookkeeping, ""},
	{"flow:", classProtected, "a flow's topology record"},
	{"slot:", classProtected, "a cap ledger slot: a window's total is counted from it"},
	{"cap-intent:", classProtected, "a cap reservation's intent: a re-drive reads its window and amount"},
	{"cap-reserve:", classProtected, "the slot a call reserved: an audit recomputes a window's total against it"},
	{"gate-reason:", classProtected, "why a call was gated: evidence for the approvers' decision"},
}

// classOf returns the class of the record key name, and whether a line above names its family.
func classOf(name string) (class redactionClass, why string, explicit bool) {
	for _, c := range redactionClasses {
		if strings.HasPrefix(name, c.prefix) {
			return c.class, c.why, true
		}
	}
	return classPayload, "", false
}

// RedactionProtected reports why a record named name, of kind kind, may never be redacted, or ""
// if it may. The protected records are the ones a verifier or the engine reads the content or the
// structure of: the journal header, the run: records except run:start and the end markers (an end
// marker's tombstone keeps its name, which is how every reader finds the end), attempt
// and not-started markers and saga failures (the at-most-once and rollback evidence), the saga
// compensation and sub-run link records, the records an approval count and its revocation basis
// rest on, and the audit and flow records. The payload records (run:start's input and principal,
// model turns, tool results, user steps, signals, approval decisions) are not protected; Redact's
// selector keeps approval decisions unless asked.
func RedactionProtected(name string, kind StepKind) string {
	switch kind {
	case StepHeader:
		return "the journal header"
	case StepAttempt, StepNotStarted:
		return "an attempt marker: the at-most-once evidence"
	case StepSagaFail:
		return "a saga failure: the rollback evidence"
	}
	if slices.Contains(endOfRunMarkers, name) {
		// An end marker's content (the final answer, the cancellation or failure text) is erasable when
		// it is named: its tombstone keeps its place, and every reader finds the run's end by it, though
		// not how the run ended (a tombstone's name is not attested; see endMarker).
		return ""
	}
	if class, why, _ := classOf(name); class == classProtected {
		return why
	}
	return ""
}

// RedactionEndMarkers returns the names of the end markers: a run that holds one of them is over,
// and only such a run may be redacted.
func RedactionEndMarkers() []string { return slices.Clone(endOfRunMarkers) }

// CheckRedaction checks the part of a redaction that needs no store: that tombstone is a well-formed
// tombstone (TombstoneLeafHash), that the leaf hash it keeps is the leaf hash of current (LeafHash),
// and that the record current holds, named name in run runID, is not protected (RedactionProtected).
// Every Redactor calls it before it writes, so a caller cannot make a store write a tombstone that
// does not keep the hash its proofs rest on. It wraps ErrRedactMismatch or ErrRedactProtected.
func CheckRedaction(runID, name string, current, tombstone []byte) error {
	if RedactionSession(runID) {
		return fmt.Errorf("run %s: %w", runID, ErrRedactSession)
	}
	want, ok := TombstoneLeafHash(tombstone)
	if !ok {
		return fmt.Errorf("run %s, entry %q: the replacement is not a well-formed tombstone: %w", runID, name, ErrRedactMismatch)
	}
	if isTombstone(current) {
		// A redaction replaces content, once. Replacing a tombstone with one that keeps the hash of
		// the first would change the leaf, and with it every root over the run.
		return fmt.Errorf("run %s, entry %q: the stored bytes are already a tombstone: %w", runID, name, ErrRedactMismatch)
	}
	if n, ok := TombstoneName(tombstone); !ok || n != name {
		return fmt.Errorf("run %s, entry %q: the tombstone does not name the entry it replaces (%q): %w", runID, name, n, ErrRedactMismatch)
	}
	if !bytes.Equal(want, LeafHash(current)) {
		return fmt.Errorf("run %s, entry %q: the tombstone's leaf hash %x is not the leaf hash of the stored bytes: %w", runID, name, want, ErrRedactMismatch)
	}
	rec, err := decodeStored(runID, name, current)
	if err != nil {
		return fmt.Errorf("run %s, entry %q: the stored bytes do not read as a record, so it cannot be classified: %w (%w)", runID, name, err, ErrRedactProtected)
	}
	if why := RedactionProtected(name, rec.Kind); why != "" {
		return fmt.Errorf("run %s, entry %q is %s: %w", runID, name, why, ErrRedactProtected)
	}
	return nil
}

// RedactState is what a Redactor read after its conditional replacement changed nothing, to say why.
// Row is the entry now stored (Found false if there is none), Ended whether the run holds an end
// marker and Leased whether a lease on it is live.
type RedactState struct {
	Row    Entry
	Found  bool
	Ended  bool
	Leased bool
	// ParentLive is whether a run above this one (RedactionAncestors) holds records and no end marker.
	ParentLive bool
	// Held is whether an active legal hold covers the run (see Holder).
	Held bool
}

// Explain says what a replacement that changed nothing meant: it returns nil if the entry already
// holds the tombstone's redaction (nothing to do), and otherwise an error that wraps the ErrRedact
// sentinel that names the refusal. A replacement refused for a reason that is gone by the time the
// state is read (a lease that lapsed) is ErrRedactChanged, to be run again.
func (s RedactState) Explain(runID, name string, current, tombstone []byte) error {
	switch {
	case !s.Found:
		return fmt.Errorf("run %s, entry %q: %w", runID, name, ErrRedactNoEntry)
	case !bytes.Equal(s.Row.Data, current):
		if got, ok := TombstoneLeafHash(s.Row.Data); ok {
			if want, wok := TombstoneLeafHash(tombstone); wok && bytes.Equal(got, want) {
				return nil
			}
		}
		return fmt.Errorf("run %s, entry %q: %w", runID, name, ErrRedactChanged)
	case !s.Ended:
		return fmt.Errorf("run %s: %w", runID, ErrRedactNotTerminal)
	case s.Leased:
		return fmt.Errorf("run %s: %w", runID, ErrRedactLeased)
	case s.ParentLive:
		return fmt.Errorf("run %s: %w", runID, ErrRedactParentLive)
	case s.Held:
		return fmt.Errorf("run %s: %w", runID, ErrRedactHeld)
	}
	return fmt.Errorf("run %s, entry %q: the run changed while it was redacted: %w", runID, name, ErrRedactChanged)
}

// RedactionAncestors returns the runs above runID, nearest first, whose records runID's are read
// with: for a sub-agent's run "a>b>c" they are "a>b" and "a". A run that is not a sub-agent's has
// none. A Redactor refuses a sub-run unless each ancestor that holds any record holds an end marker.
// It is empty for a session's run, which RedactionSession reports.
func RedactionAncestors(runID string) []string {
	if !IsSubRun(runID) || IsSessionRun(runID) {
		return nil
	}
	var out []string
	for {
		i := strings.LastIndex(runID, subRunSep)
		if i < 0 {
			return out
		}
		runID = runID[:i]
		out = append(out, runID)
	}
}

// RedactionSession reports whether runID is a session's journal or turn run (agent.IsSessionRun),
// or a sub-agent run inside one: they are never redacted.
func RedactionSession(runID string) bool {
	for id := runID; ; {
		if IsSessionRun(id) {
			return true
		}
		i := strings.LastIndex(id, subRunSep)
		if i < 0 {
			return false
		}
		id = id[:i]
	}
}

// Compactor is the optional store capability that makes an erasure complete at the storage layer: a
// store whose engine keeps the bytes a redaction replaced (SQLite's write-ahead log and freed pages)
// overwrites them. A store opts in by implementing Compact; the command calls it after a pass. Postgres
// does not: its dead tuples go with VACUUM, which the operator runs.
type Compactor interface {
	// Compact makes the replaced bytes unrecoverable from the store's own files, as far as the engine
	// allows, and reports whether it could. It does not claim success it did not get: a reader that
	// holds the database open can keep the engine from finishing, which Complete false reports.
	Compact(ctx context.Context) (CompactionReport, error)
}

// CompactionReport says what Compact did.
type CompactionReport struct {
	// Complete is whether the engine finished (for SQLite, the WAL checkpoint ran to the end and the
	// log was truncated). False means old bytes may remain: close the readers and run it again.
	Complete bool `json:"complete"`
	// Detail says what the engine reported, for the operator.
	Detail string `json:"detail"`
}

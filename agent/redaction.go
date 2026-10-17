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

// TombstoneName returns the record name a tombstone carries, and false if b is not a tombstone or
// names none (a tombstone is read without a name, as the reserved form allows).
func TombstoneName(b []byte) (string, bool) {
	if !isTombstone(b) {
		return "", false
	}
	var t tombstone
	if err := json.Unmarshal(b, &t); err != nil || t.Redacted == nil || t.Redacted.Name == "" {
		return "", false
	}
	return t.Redacted.Name, true
}

// TombstoneLeafHash returns the leaf hash a tombstone keeps, and false if b is not a well-formed
// tombstone (the reserved form of Record.Redacted with a leaf hash of 32 bytes in lower-case hex).
func TombstoneLeafHash(b []byte) ([]byte, bool) {
	if !isTombstone(b) {
		return nil, false
	}
	var t tombstone
	if err := json.Unmarshal(b, &t); err != nil || t.Redacted == nil || t.Redacted.AtMs < 0 {
		return nil, false
	}
	h, err := hex.DecodeString(t.Redacted.LeafHash)
	if err != nil || len(h) != sha256.Size || hex.EncodeToString(h) != t.Redacted.LeafHash {
		return nil, false
	}
	return h, true
}

// RedactionProtected reports why a record named name, of kind kind, may never be redacted, or ""
// if it may. The protected records are the ones a verifier or the engine reads the content or the
// structure of: the journal header, the end markers (a tombstone has no kind, so a run whose end
// marker was replaced would read as running), the other run: records except run:start, the attempt
// and not-started markers and saga failures (the at-most-once and rollback evidence), the
// records an approval count and its revocation basis rest on, and the audit and flow records. The
// payload records (run:start's input and principal, model turns, tool results, user steps,
// signals, approval decisions) are not protected; Redact's selector keeps approval decisions
// unless asked.
func RedactionProtected(name string, kind StepKind) string {
	switch kind {
	case StepHeader:
		return "the journal header"
	case StepAttempt, StepNotStarted:
		return "an attempt marker: the at-most-once evidence"
	case StepSagaFail:
		return "a saga failure: the rollback evidence"
	}
	switch {
	case name == journalHeaderName:
		return "the journal header"
	case slices.Contains(endOfRunMarkers, name):
		return "an end marker: it says the run is over, and Status and Recover read it by kind and name"
	case strings.HasPrefix(name, "run:") && name != runStartStep:
		return "a run state record (cancellation, limits)"
	case strings.HasPrefix(name, "attempt:"):
		return "an attempt marker: the at-most-once evidence"
	}
	for _, p := range protectedPrefixes {
		if strings.HasPrefix(name, p) {
			return "a record an approval count, its revocation basis or the audit leaves rest on"
		}
	}
	return ""
}

// journalHeaderName is the name of a journal's header entry.
const journalHeaderName = "@journal"

// protectedPrefixes are the key prefixes of records whose content is not payload (RedactionProtected).
var protectedPrefixes = []string{
	"approval-tally:", "approval-identity:", "approval-status:", "approval-exec-hold:", "approval-deadline:",
	"audit:", "flow:",
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
	want, ok := TombstoneLeafHash(tombstone)
	if !ok {
		return fmt.Errorf("run %s, entry %q: the replacement is not a well-formed tombstone: %w", runID, name, ErrRedactMismatch)
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
	}
	return fmt.Errorf("run %s, entry %q: the run changed while it was redacted: %w", runID, name, ErrRedactChanged)
}

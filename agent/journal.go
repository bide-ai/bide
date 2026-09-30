// journal.go holds the storage port (Store) and the journal the engine writes through (Journal).
//
// A Store is a dumb, append-only map of named entries per run: Insert if absent, Get by name, and
// Load in commit order. Everything with meaning lives in the Journal above it: memoization, the
// record encoding and its salt, the journal format header, attempt claims and the records that an
// attempt never started, and recording a step's outcome whatever its caller's context does. So a
// new backend implements three methods and the storetest suite, and gets the engine's guarantees
// without reimplementing any of them.

package agent

import (
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"runtime"
	"slices"
	"sync"
)

// JournalFormat is the journal format this version writes: the format every run's header names
// (see StepHeader). Before 1.0 it is one dev tag, "bide.journal.v1-dev", the same in every
// pre-release and never bumped per change: a pre-release journal is not promised to be readable
// by a later pre-release. It becomes "bide.journal.v1" at the 1.0 tag, so a 1.0 binary refuses
// every pre-release journal and none is read under rules it was not written for.
const JournalFormat = "bide.journal.v1-dev"

// supportedFormats are the journal formats this version reads and writes. A run's format is pinned
// by its first write, and a Journal refuses a run in any other format (see JournalVersionError).
var supportedFormats = []string{JournalFormat}

// headerStep is the name of the journal header, the first entry of every run's journal.
const headerStep = "@journal"

// Store is the storage port: an append-only set of named entries per run. It stores bytes and
// knows nothing of what they mean; the Journal above it encodes records, memoizes steps, writes
// the format header and runs attempt claims. A backend implements Store and passes the storetest
// suite (agent/storetest), which checks every requirement below.
//
// Requirements (A1 to A8):
//
//   - A1, unique names and a linearizable insert: a run holds at most one entry per name.
//     Concurrent Inserts of one name, from any number of processes, have exactly one winner, and
//     every caller and every later reader sees the winner's bytes. Which entry a (runID, name)
//     names does not depend on the context a method is called with: a store that scopes keys by
//     a tenant carried in the context breaks the journal (the Journal remembers runs by run ID),
//     so a tenant belongs in the run ID (see RunFilter.Prefix).
//   - A2, commit-ordered, prefix-closed visibility: Seq is an opaque int64, strictly increasing
//     in commit order within a run. Every read of a run returns a prefix of the run's final
//     order: no entry ever becomes visible with a lower Seq than an entry already visible. Gaps in
//     Seq are allowed. Nothing reads Seq as a count; positions are computed when a run is read,
//     in Load order.
//   - A3, durable before return: Insert reports inserted only after the entry is committed. On an
//     error the entry is either absent or complete, and the same bytes may be inserted again.
//   - A4, read-your-writes and monotone visibility: once visible, an entry stays visible with the
//     same bytes and position (the one exception is a redaction, which keeps the position).
//   - A5, byte fidelity: Data comes back exactly as inserted, and the caller may keep or modify
//     the slices it passes and receives. That includes every record's salt: the engine tells its
//     own model record from another driver's by the salt it drew (see JournalEntry), so a store
//     that rewrites or drops salt bytes makes it take its own record for another's.
//   - A6, immutable: the port has no update or delete. Only a redaction may replace an entry's
//     Data, with a tombstone (see Record.Redacted), and only in a run that is over.
//   - A7, context: every method honors ctx. Recording a step's outcome after ctx is cancelled is
//     the Journal's job, not the store's.
//   - A8, iterator hygiene: breaking out of Load's iterator releases every resource it holds, and
//     a store holds no connection, transaction or lock across a yield, so a caller may Insert
//     into the run inside its Load loop without deadlocking.
//
// A store that wraps another store may implement
//
//	Unwrap() Store
//
// so Capability finds the capabilities of the store it wraps (Lister, Leaser), but only if it
// passes run IDs and names through unchanged. A wrapper that rewrites keys (a tenant prefix, say)
// must not implement Unwrap, and implements each capability itself; storetest.CheckWrapper checks
// this.
type Store interface {
	// Insert stores data under (runID, name) if no entry has that name, and returns the stored
	// entry (the caller's, or the one already there) and whether this call stored it.
	Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error)
	// Get returns runID's entry named name, if there is one.
	Get(ctx context.Context, runID, name string) (Entry, bool, error)
	// Load yields runID's entries whose Seq is greater than after (-1 for all of them), in
	// ascending Seq. It yields an error at most once, as its last element.
	Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error]
}

// Entry is one stored entry of a run: its position, name and bytes.
type Entry struct {
	// Seq is the entry's position: opaque, strictly increasing in commit order within the run
	// (see Store). It is not a count.
	Seq  int64
	Name string
	Data []byte
}

// Lister is the optional store capability a crash-recovery supervisor needs: it enumerates the
// runs a store holds, so Recover can find the ones to re-drive after a restart. A store opts in by
// implementing Runs; Recover finds it with Capability, so a wrapper that implements Unwrap keeps
// its inner store's Lister.
type Lister interface {
	// Runs yields the IDs of the runs f admits, in ascending byte order. It yields an error at
	// most once, as its last element, and holds no connection or lock across a yield.
	Runs(ctx context.Context, f RunFilter) iter.Seq2[string, error]
}

// RunFilter selects the runs a Lister yields. A SQL store evaluates it in its query, so a
// recovery pass over many finished runs reads none of them.
type RunFilter struct {
	// After is a cursor: only run IDs greater than it (in byte order) are yielded.
	After string
	// Prefix admits only run IDs that start with it: a tenant or namespace, by the convention
	// that a deployment prefixes its run IDs.
	Prefix string
	// ExcludeHolding drops every run that holds an entry with any of these names. Recover passes
	// the terminal markers (run:complete, run:aborted, run:cancelled).
	ExcludeHolding []string
}

// Admits reports whether f admits runID, given holds, which reports whether the run holds an
// entry with a given name. It is the filter's meaning, for a store that evaluates it in Go.
func (f RunFilter) Admits(runID string, holds func(name string) bool) bool {
	if runID <= f.After || len(runID) < len(f.Prefix) || runID[:len(f.Prefix)] != f.Prefix {
		return false
	}
	for _, n := range f.ExcludeHolding {
		if holds(n) {
			return false
		}
	}
	return true
}

// Capability returns store's implementation of the optional capability T (Lister, Leaser, or any
// other interface a store may implement beyond Store), looking through wrappers.
//
// A store that wraps another store exposes the store it wraps by implementing
//
//	Unwrap() Store
//
// Capability checks store itself first, then follows Unwrap until a store implements T, and
// reports false once a store has no Unwrap method or Unwrap returns nil. A wrapper therefore
// exposes exactly the capabilities of the store it wraps, with no forwarding methods to keep in
// step with new capabilities. A wrapper that must change what a capability means implements that
// capability itself, which takes precedence over the wrapped store's, as with errors.As. Only a
// wrapper that passes run IDs and names through unchanged may implement Unwrap (see Store).
func Capability[T any](store Store) (T, bool) {
	for store != nil {
		if c, ok := store.(T); ok {
			return c, true
		}
		u, ok := store.(interface{ Unwrap() Store })
		if !ok {
			break
		}
		store = u.Unwrap()
	}
	var zero T
	return zero, false
}

// capabilityOf is Capability for the store behind a Durable: the Durable itself, then the store a
// Journal writes to, then a Durable wrapper's Unwrap (the transitional form), then the Durable as
// a Store.
func capabilityOf[T any](d Durable) (T, bool) {
	var zero T
	if d == nil {
		return zero, false
	}
	if c, ok := d.(T); ok {
		return c, true
	}
	if j := journalOf(d); j != nil {
		return Capability[T](j.store)
	}
	if u, ok := d.(interface{ Unwrap() Durable }); ok {
		if inner := u.Unwrap(); inner != nil {
			return capabilityOf[T](inner)
		}
		return zero, false
	}
	if s, ok := d.(Store); ok {
		return Capability[T](s)
	}
	return zero, false
}

// Durable is the step-memoization interface the engine and its callers use during the transition
// to Journal: Do runs a step at most once per (runID, name), and History reads a run back. *Journal
// implements it, and so do MemStore and the SQL stores, whose Do and History go through a Journal
// over themselves.
//
// Deprecated: transitional; renamed by the 1.0 rewrite. Use *Journal.
//
// A Durable that is not a Journal must keep the Journal's guarantees, which the engine's
// accounting relies on as much as its at-most-once does: it records a step's result at most once
// under one name (a second record under the name is never written, and Do returns the one the
// journal holds); it calls fn at most once per record it writes, never again for a name already
// recorded; and it keeps each record's bytes, the salt among them, exactly as JournalEntry built
// them (a salt a step's record carries from the engine is the one the journal must hold).
type Durable interface {
	// Do returns the recorded Record for (runID, name) without running fn if present; otherwise
	// runs fn, records the returned Record (with Name set and a fresh salt), and returns the
	// record the journal holds. If fn errors, nothing is recorded and the step runs again on the
	// next attempt. Once fn has returned a record, Do records it even if ctx was cancelled
	// meanwhile: fn may have fired a side effect, and its outcome must not be lost.
	Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error)
	// History returns all recorded steps for a run, in order, the journal header first.
	History(ctx context.Context, runID string) ([]Record, error)
}

// JournalOption configures NewJournal.
type JournalOption interface{ applyJournal(*journalConfig) error }

type journalConfig struct{}

// Journal is the durable journal the engine writes through: it owns everything a run's records
// mean on top of a Store. It encodes each record (EncodeRecord) with a fresh salt, memoizes named
// steps, writes and checks the journal format header, runs the exclusive attempt claims that keep
// side effects at most once, and records a step's outcome even when its caller's context is
// cancelled. A Journal is safe for concurrent use, and any number of Journals may share a store,
// in one process or many: the store's single-winner Insert (A1) decides every race, and Journals
// in one process over the same store also share in-flight steps.
//
// The header. Before its first write to a run a Journal inserts the header, a StepHeader record
// named "@journal" that names JournalFormat, and requires it to be the run's first entry. Every
// read checks that the first entry is a header naming a supported format; a run in another
// format, or with no header first, is refused with a *JournalVersionError before anything is
// read from it or written to it. A Journal remembers the runs it has checked (the 65536 most recently used), so a run is checked once per process, not on every step. Run IDs must never be reused
// after a run is deleted from its store: a Journal that remembers the deleted run writes into the
// new one with no header, which every reader refuses.
type Journal struct {
	store Store
	id    any    // the store's identity for sharing in-flight steps and claims (see storeIdentity)
	good  runSet // runs whose header this Journal has checked
}

var _ Durable = (*Journal)(nil)

// NewJournal returns a Journal over s. A nil s is ErrConfig.
func NewJournal(s Store, opts ...JournalOption) (*Journal, error) {
	if isNil(s) {
		return nil, fmt.Errorf("NewJournal: nil store: %w", ErrConfig)
	}
	var cfg journalConfig
	for _, o := range opts {
		if o == nil {
			return nil, fmt.Errorf("NewJournal: nil option: %w", ErrConfig)
		}
		if err := o.applyJournal(&cfg); err != nil {
			return nil, err
		}
	}
	return newJournal(s), nil
}

// newJournal is NewJournal for a store known to be non-nil, with no options.
func newJournal(s Store) *Journal {
	j := &Journal{store: s}
	j.id = storeIdentity(s, j)
	return j
}

// isNil reports whether v is nil or a nil pointer.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// storeIdentity is the key under which Journals in this process share in-flight steps and
// remembered claims for s: s itself when it is a pointer (every store in this module is), and
// otherwise j, so a store whose identity cannot be compared safely shares only within one Journal.
// Claims keep that safe; it only loses the in-process deduplication across Journals.
func storeIdentity(s Store, j *Journal) any {
	if reflect.ValueOf(s).Kind() == reflect.Pointer {
		return s
	}
	return j
}

// Store returns the store the journal writes to.
func (j *Journal) Store() Store { return j.store }

// Get returns the record named name in runID's journal, if there is one. It reads the record
// first and then checks the run's header, so a read that races the run's first write never
// takes a record for one without a header: the header commits before any record. A run this
// version cannot read is a *JournalVersionError whether or not it holds the name; a run with no
// entries holds nothing.
func (j *Journal) Get(ctx context.Context, runID, name string) (Record, bool, error) {
	e, ok, err := j.getEntry(ctx, runID, name)
	if err != nil || !ok {
		return Record{}, false, err
	}
	rec, err := decodeStored(runID, name, e.Data)
	return rec, err == nil, err
}

// getEntry is Get's store read and header check, without decoding.
func (j *Journal) getEntry(ctx context.Context, runID, name string) (Entry, bool, error) {
	e, ok, err := j.store.Get(ctx, runID, name)
	if err != nil {
		return Entry{}, false, storageErr(fmt.Sprintf("read step %q of run %s", name, runID), err)
	}
	if !ok {
		// Not recorded, in a run this version can read, or in a run with no entries at all. A
		// run in another format is refused, not reported empty.
		if j.good.has(runID) {
			return Entry{}, false, nil
		}
		// The run's first entry tells whether it is empty, in a format this version reads, or not.
		first, any, err := j.firstEntry(ctx, runID)
		if err != nil || !any {
			return Entry{}, false, err // an empty run holds nothing
		}
		return Entry{}, false, j.checkFirst(runID, first)
	}
	if err := j.readable(ctx, runID); err != nil {
		return Entry{}, false, err
	}
	return e, true, nil
}

// Records yields runID's records in journal order, the header first. It checks the header as it
// streams: a run whose first entry is not a header naming a supported format yields a
// *JournalVersionError and nothing else.
func (j *Journal) Records(ctx context.Context, runID string) iter.Seq2[Record, error] {
	return func(yield func(Record, error) bool) {
		first := true
		for e, err := range j.store.Load(ctx, runID, -1) {
			if err != nil {
				yield(Record{}, storageErr("load run "+runID, err))
				return
			}
			if first {
				first = false
				if err := j.checkFirst(runID, e); err != nil {
					yield(Record{}, err)
					return
				}
			}
			rec, err := decodeStored(runID, e.Name, e.Data)
			if err != nil {
				yield(Record{}, err)
				return
			}
			if !yield(rec, nil) {
				return
			}
		}
	}
}

// History returns runID's records in journal order, the header first (see Records). A run with no
// entries has an empty history.
func (j *Journal) History(ctx context.Context, runID string) ([]Record, error) {
	var out []Record
	for r, err := range j.Records(ctx, runID) {
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Format returns the journal format runID's header names, "" for a run with no entries, and a
// *JournalVersionError for a run whose first entry is not a header. It reports a format this
// version does not support rather than refusing it.
func (j *Journal) Format(ctx context.Context, runID string) (string, error) {
	e, ok, err := j.firstEntry(ctx, runID)
	if err != nil || !ok {
		return "", err
	}
	if e.Name != headerStep {
		return "", &JournalVersionError{RunID: runID, Supported: slices.Clone(supportedFormats)}
	}
	return headerFormat(runID, e)
}

// Do runs fn as the named step name of runID, at most once: if the step is recorded, Do returns
// the recorded record without calling fn; otherwise it calls fn and records the record fn returns
// (with Name set and a fresh salt), and returns the record the journal holds, which is another
// caller's if one recorded the step first. If fn errors, nothing is recorded. Once fn has
// returned a record, Do records it even if ctx was cancelled meanwhile, since fn may have fired a
// side effect whose outcome must not be lost. Callers of the same step in this process, through
// any Journal over the same store, share one call of fn.
//
// Deprecated: transitional; the 1.0 rewrite unexports it. Engine code writes through the journal's
// own paths; audit and plan reach it through internal/journalhook.
func (j *Journal) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	b, err := shareFlight(flightKey{j.id, runID, name}, func() ([]byte, error) {
		e, ok, err := j.getEntry(ctx, runID, name)
		if err != nil {
			return nil, err
		}
		if ok {
			return e.Data, nil // recorded: do not run fn
		}
		// The run's format is settled before fn runs, so fn never fires a side effect for a run
		// this version would then refuse to record it in.
		if err := j.ensureHeader(ctx, runID); err != nil {
			return nil, err
		}
		rec, err := fn(ctx)
		if err != nil {
			return nil, err
		}
		return j.insert(context.WithoutCancel(ctx), runID, name, rec)
	})
	if err != nil {
		return Record{}, err
	}
	return decodeStored(runID, name, b)
}

// doFresh is Do for a step its caller knows was not recorded when it last read the run (the
// engine's live model turns and tool calls): it calls fn without reading the step first. If
// another driver recorded the step meanwhile, the Insert loses and doFresh returns that driver's
// record, as Do would have; fn has then run for nothing, which is why only a step whose fn is safe
// to run again (a model call, a retry-safe tool, or a side effect under a won claim) uses it.
func (j *Journal) doFresh(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	b, err := shareFlight(flightKey{j.id, runID, name}, func() ([]byte, error) {
		if err := j.ensureHeader(ctx, runID); err != nil {
			return nil, err
		}
		rec, err := fn(ctx)
		if err != nil {
			return nil, err
		}
		return j.insert(context.WithoutCancel(ctx), runID, name, rec)
	})
	if err != nil {
		return Record{}, err
	}
	return decodeStored(runID, name, b)
}

// put records rec as the step name of runID unless the step is recorded, and returns the record
// the journal holds: one Insert, for a record that is a pure value (a marker, a decision).
func (j *Journal) put(ctx context.Context, runID, name string, rec Record) (Record, error) {
	if err := j.ensureHeader(ctx, runID); err != nil {
		return Record{}, err
	}
	b, err := j.insert(ctx, runID, name, rec)
	if err != nil {
		return Record{}, err
	}
	return decodeStored(runID, name, b)
}

// putNew is put that also reports whether this call stored rec (rather than finding a record
// another writer stored first).
func (j *Journal) putNew(ctx context.Context, runID, name string, rec Record) (Record, bool, error) {
	if err := j.ensureHeader(ctx, runID); err != nil {
		return Record{}, false, err
	}
	data, err := JournalEntry(name, rec)
	if err != nil {
		return Record{}, false, fmt.Errorf("encode step %q: %w (%w)", name, err, ErrStorage)
	}
	e, inserted, err := j.store.Insert(ctx, runID, name, data)
	if err != nil {
		return Record{}, false, storageErr(fmt.Sprintf("record step %q of run %s", name, runID), err)
	}
	got, err := decodeStored(runID, name, e.Data)
	return got, inserted, err
}

// insert records rec under name with a fresh salt and returns the bytes the store holds for the
// name: rec's, or those of the record another writer stored first.
func (j *Journal) insert(ctx context.Context, runID, name string, rec Record) ([]byte, error) {
	data, err := JournalEntry(name, rec)
	if err != nil {
		return nil, fmt.Errorf("encode step %q: %w (%w)", name, err, ErrStorage)
	}
	e, _, err := j.store.Insert(ctx, runID, name, data)
	if err != nil {
		return nil, storageErr(fmt.Sprintf("record step %q of run %s", name, runID), err)
	}
	return e.Data, nil
}

// open reads runID's journal for a drive that will write to it: one Load, which also checks the
// header, and, for a run with no entries, the header's Insert. A run in another format, or with
// no header first, is refused before anything is written to it. A run holding a redacted record
// is over and cannot be driven.
func (j *Journal) open(ctx context.Context, runID string) ([]Record, error) {
	recs, err := j.History(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		// A new run (or one deleted since this Journal checked it, which must start again with a
		// header rather than take records a reader would refuse as headerless).
		j.good.remove(runID)
		h, err := j.insertHeader(ctx, runID)
		if err != nil {
			return nil, err
		}
		return []Record{h}, nil
	}
	for _, r := range recs {
		if r.Redacted {
			return nil, fmt.Errorf("run %s holds a redacted record (%q), so it is over and cannot be driven again: %w", runID, r.Name, ErrConfig)
		}
	}
	return recs, nil
}

// ensureHeader makes sure runID's journal starts with a header this version writes, before the
// journal's first write to the run: it reads the run's first entry and inserts the header only if
// the run has none. A run in another format, or with no header first, is refused without a write.
func (j *Journal) ensureHeader(ctx context.Context, runID string) error {
	if j.good.has(runID) {
		return nil
	}
	e, ok, err := j.firstEntry(ctx, runID)
	if err != nil {
		return err
	}
	if ok {
		return j.checkFirst(runID, e)
	}
	_, err = j.insertHeader(ctx, runID)
	return err
}

// insertHeader inserts the header naming JournalFormat into runID, which the caller found empty,
// and returns the header the run holds: this one, or a concurrent first writer's. A run whose
// header names a format this version does not write is refused.
//
// Entries of a writer that is not a Journal could land between the caller's read and this Insert,
// ahead of the header; the next reader of the run refuses it then, since the header is not first.
func (j *Journal) insertHeader(ctx context.Context, runID string) (Record, error) {
	e, _, err := j.store.Insert(ctx, runID, headerStep, headerEntry())
	if err != nil {
		return Record{}, storageErr("write the journal header of run "+runID, err)
	}
	f, err := headerFormat(runID, e)
	if err != nil {
		return Record{}, err
	}
	if !slices.Contains(supportedFormats, f) {
		return Record{}, &JournalVersionError{RunID: runID, Found: f, Supported: slices.Clone(supportedFormats)}
	}
	h, err := decodeStored(runID, headerStep, e.Data)
	if err != nil {
		return Record{}, err
	}
	j.good.add(runID)
	return h, nil
}

// readable checks that runID's first entry is a header naming a supported format, unless this
// Journal has checked the run already.
func (j *Journal) readable(ctx context.Context, runID string) error {
	if j.good.has(runID) {
		return nil
	}
	return j.checkHeader(ctx, runID)
}

// checkHeader checks that runID's first entry is a header naming a supported format, and remembers
// the run as checked if it is. It is called for a run known to hold an entry. It reads the first
// entry, not the header by name: a run whose header is not first (entries of a writer that is not
// a Journal landed ahead of it) is refused, as History refuses it.
func (j *Journal) checkHeader(ctx context.Context, runID string) error {
	first, ok, err := j.firstEntry(ctx, runID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("run %s reads back empty after an entry of it was read: %w", runID, ErrStorage)
	}
	return j.checkFirst(runID, first)
}

// checkFirst checks that e, the first entry of runID in Load order, is a header naming a
// supported format, and remembers the run as checked if it is.
func (j *Journal) checkFirst(runID string, e Entry) error {
	if e.Name != headerStep {
		return &JournalVersionError{RunID: runID, Supported: slices.Clone(supportedFormats)}
	}
	f, err := headerFormat(runID, e)
	if err != nil {
		return err
	}
	if !slices.Contains(supportedFormats, f) {
		return &JournalVersionError{RunID: runID, Found: f, Supported: slices.Clone(supportedFormats)}
	}
	j.good.add(runID)
	return nil
}

// firstEntry returns runID's first entry in Load order, if it has any.
func (j *Journal) firstEntry(ctx context.Context, runID string) (Entry, bool, error) {
	for e, err := range j.store.Load(ctx, runID, -1) {
		if err != nil {
			return Entry{}, false, storageErr("load run "+runID, err)
		}
		return e, true, nil
	}
	return Entry{}, false, nil
}

// headerEntry returns the journal encoding of a new journal header: JournalEntry of a StepHeader
// record naming JournalFormat, written directly, since every run's first write makes one.
func headerEntry() []byte {
	var salt [SaltSize]byte
	_, _ = rand.Read(salt[:]) // crypto/rand.Read never returns an error
	b := make([]byte, 0, len(headerPrefix)+base64.StdEncoding.EncodedLen(SaltSize)+2)
	b = append(b, headerPrefix...)
	b = base64.StdEncoding.AppendEncode(b, salt[:])
	return append(b, `"}`...)
}

// headerPrefix is the journal encoding of a new journal header up to its salt.
const headerPrefix = `{"name":"` + headerStep + `","kind":"` + string(StepHeader) + `","format":"` + JournalFormat + `","salt":"`

// headerFormat returns the format the header entry e names. It reads only the members every
// journal format keeps (name, kind, format), so a header written by any version is readable.
func headerFormat(runID string, e Entry) (string, error) {
	if e.Name == headerStep && bytes.HasPrefix(e.Data, []byte(headerPrefix)) {
		return JournalFormat, nil // a header this version writes
	}
	var h struct {
		Name   string   `json:"name"`
		Kind   StepKind `json:"kind"`
		Format string   `json:"format"`
	}
	if err := json.Unmarshal(e.Data, &h); err != nil || h.Name != headerStep || h.Kind != StepHeader {
		return "", fmt.Errorf("run %s: the entry stored as %q is not a journal header: %w", runID, headerStep, ErrStorage)
	}
	return h.Format, nil
}

// storageErr wraps a store's error with what the journal was doing, as ErrStorage.
func storageErr(what string, err error) error {
	var v *JournalVersionError
	if errors.As(err, &v) || errors.Is(err, ErrStorage) {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: %w (%w)", what, err, ErrStorage)
}

// ===========================================================================
// Attempt claims (see attempt.go for the protocol)
// ===========================================================================

// newClaimID returns a fresh random claim id.
func newClaimID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}

// claim inserts rec as the attempt marker key, stamped with a fresh claim id, and reports whether
// this caller won it: whether the marker the journal holds carries that id. One Insert decides it.
//
// The invariant every claim keeps: every won claim has a fresh id, so its not-started key
// (notStartedStep(key, id)) is empty, and nothing but its own holder can void the attempt; an
// effect runs only under a marker no not-started record voids.
//
// If the marker's Insert fails, it may still have committed (a connection lost after the commit, a
// timeout). The effect was not called either way, so the claim records that its attempt did not
// start (see notStarted), and the next claim of the effect re-attempts it instead of halting over
// an effect that never ran. If that record cannot be written (it may have committed too), the
// claim id is remembered, and the next claim of the same key in this process writes it again
// before it claims with a fresh id of its own: the marker it may have left is then void, and the
// fresh claim loses to it and moves to the next attempt (see claimNext). A single-key claim
// (ClaimAttempt) halts instead, which is safe.
//
// The process remembers every such id of a key, not the last one: two claims of one key can each
// fail to record that they did not start (one that won the marker and was cancelled, one whose
// marker Insert failed), and forgetting either could leave a marker no record ever voids, so a
// never-started effect would halt forever. The claim writes the not-started record of each
// remembered id, one at a time, and remembers again any it cannot write; then it claims with a
// fresh id. An id whose record is still missing leaves at worst a live marker, which the fresh
// claim loses to, so the effect halts rather than runs.
func (j *Journal) claim(ctx context.Context, runID, key string, rec Record) (bool, Record, error) {
	if err := j.ensureHeader(ctx, runID); err != nil {
		return false, Record{}, err
	}
	for _, old := range pendingClaims.takeAll(flightKey{j.id, runID, key}) {
		m := rec
		m.claim = old
		_ = j.notStarted(ctx, runID, key, m) // on failure it remembers old again
	}
	id := newClaimID()
	rec.claim = id
	b, err := j.insert(ctx, runID, key, rec)
	if err != nil {
		// The marker may have committed all the same. This driver knows it never called the
		// effect, so it records that under its own claim: whoever claims next, in any process,
		// re-attempts instead of halting.
		if nerr := j.notStarted(ctx, runID, key, rec); nerr != nil {
			return false, Record{}, fmt.Errorf("claim %s: %w (%w)", key, err, nerr)
		}
		return false, Record{}, fmt.Errorf("claim %s: %w", key, err)
	}
	got, err := decodeStored(runID, key, b)
	if err != nil {
		return false, Record{}, err
	}
	return got.claim == id, got, nil
}

// retryNotStarted writes again the not-started record of the attempt with marker key key, whose
// marker is marker, when this process remembers that marker's own claim id (its earlier
// not-started write failed), and reports whether the attempt is now recorded as not started. A
// resume uses it before it halts on the attempt. It takes only the marker's id from the ids the
// process remembers for the key; the others stay for the next claim of the key.
func (j *Journal) retryNotStarted(ctx context.Context, runID, key string, marker Record) bool {
	if marker.claim == "" || !pendingClaims.takeID(flightKey{j.id, runID, key}, marker.claim) {
		return false
	}
	return j.notStarted(ctx, runID, key, marker) == nil
}

// claimNext claims the next attempt of the effect whose first marker key is base (see
// claimNextAttempt).
func (j *Journal) claimNext(ctx context.Context, runID, base string, rec Record) (bool, Record, string, error) {
	for gen := 0; ; gen++ {
		key := retryAttemptStep(base, gen)
		won, got, err := j.claim(ctx, runID, key, rec)
		if err != nil || won {
			return won, got, key, err
		}
		if ok, err := j.voided(ctx, runID, key, got); err != nil || !ok {
			return false, got, key, err
		}
	}
}

// voided reports whether the attempt with marker key key, whose marker is marker, is recorded as
// never started by the driver that claimed it.
func (j *Journal) voided(ctx context.Context, runID, key string, marker Record) (bool, error) {
	rec, ok, err := j.Get(ctx, runID, notStartedStep(key, marker.claim))
	if err != nil || !ok {
		return false, err
	}
	return rec.Kind == StepNotStarted && marker.claim != "" && rec.claim == marker.claim, nil
}

// liveAttempt returns the marker of the effect whose first marker key is base that is not
// recorded as not started, if there is one.
func (j *Journal) liveAttempt(ctx context.Context, runID, base string) (Record, bool, error) {
	for gen := 0; ; gen++ {
		key := retryAttemptStep(base, gen)
		marker, ok, err := j.Get(ctx, runID, key)
		if err != nil || !ok {
			return Record{}, false, err
		}
		if v, err := j.voided(ctx, runID, key, marker); err != nil {
			return Record{}, false, err
		} else if !v {
			return marker, true, nil
		}
	}
}

// notStarted records that the attempt with marker key key, which this driver claimed with marker,
// never called its effect. It is written whatever ctx's state. If it cannot be written, the claim
// id is remembered, so the next claim of the key in this process (or a resume that meets the
// attempt) writes it again.
func (j *Journal) notStarted(ctx context.Context, runID, key string, marker Record) error {
	_, err := j.put(context.WithoutCancel(ctx), runID, notStartedStep(key, marker.claim),
		Record{Kind: StepNotStarted, ToolUseID: marker.ToolUseID, claim: marker.claim})
	if err != nil {
		pendingClaims.remember(flightKey{j.id, runID, key}, marker.claim)
		return fmt.Errorf("record that %s did not start: %w", key, err)
	}
	return nil
}

// ===========================================================================
// The engine's view of a Durable
// ===========================================================================

// journalOf returns the Journal that d writes through: d itself, or the Journal a store's Do and
// History shims delegate to. It returns nil for any other Durable (a test's wrapper that
// intercepts Do, say), which the engine then drives through its Do and History only. A wrapper
// that embeds a store is not the store: its Journal writes to the embedded store, not to the
// wrapper, so the wrapper's own Do is kept.
func journalOf(d Durable) *Journal {
	switch v := d.(type) {
	case *Journal:
		return v
	case interface{ Journal() *Journal }:
		if j := v.Journal(); j != nil && sameValue(j.store, d) {
			return j
		}
	}
	return nil
}

// checkDurable refuses a Durable whose Do and History would write past a Store method it has: a
// wrapper that gets its Do from a store with the transitional Do and History (MemStore, the SQL
// stores), or from an embedded Durable interface, while its Insert, Get or Load comes from
// somewhere else (the wrapper itself, or another embedded field). Those Do and History write
// through a Journal over the store they come from, so every write would bypass the wrapper's
// Insert. Such a wrapper is used through a Journal over it (NewJournal(wrapper)). A wrapper that
// declares Do itself, at any depth, is driven through its Do, as any Durable is.
func checkDurable(d Durable) error {
	if d == nil {
		return fmt.Errorf("agent: nil store: %w", ErrConfig)
	}
	if journalOf(d) != nil {
		return nil
	}
	if _, ok := d.(Store); !ok {
		return nil
	}
	t := reflect.TypeOf(d)
	do := methodOrigin(t, "Do")
	if do.Kind() != reflect.Interface && !isShim(do) {
		return nil // the wrapper's own Do
	}
	for _, name := range []string{"Insert", "Get", "Load"} {
		if o := methodOrigin(t, name); o != do {
			return fmt.Errorf("agent: %T takes %s from %v but Do and History from %v, which write past it; use it through agent.NewJournal(wrapper): %w", d, name, o, do, ErrConfig)
		}
	}
	return nil
}

// isShim reports whether t, the type that declares a Do, is a store whose Do is the transitional
// shim over a Journal of its own: t declares Journal() *Journal too. A wrapper that declares Do
// and inherits Journal from a store it embeds is not one.
func isShim(t reflect.Type) bool {
	m, ok := t.MethodByName("Journal")
	return ok && m.Type.NumOut() == 1 && m.Type.Out(0) == reflect.TypeFor[*Journal]() && methodOrigin(t, "Journal") == t
}

// methodOrigin returns the type that provides method name to type t: t itself if it declares the
// method (on a value or pointer receiver), otherwise the origin in the embedded field that
// provides it, or the embedded interface type whose method it is.
func methodOrigin(t reflect.Type, name string) reflect.Type {
	if declares(t, name) {
		return t
	}
	st := t
	if st.Kind() == reflect.Pointer {
		st = st.Elem()
	}
	if st.Kind() != reflect.Struct {
		return t
	}
	for i := range st.NumField() {
		f := st.Field(i)
		if !f.Anonymous {
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Interface {
			if _, ok := ft.MethodByName(name); ok {
				return ft
			}
			continue
		}
		if ft.Kind() != reflect.Pointer {
			ft = reflect.PointerTo(ft)
		}
		if _, ok := ft.MethodByName(name); ok {
			return methodOrigin(ft, name)
		}
	}
	return t
}

// declares reports whether type t declares its method name itself, on a pointer or a value
// receiver, rather than inheriting it from an embedded field: an inherited method, and a pointer
// method standing for a value method, are wrappers the compiler generates.
func declares(t reflect.Type, name string) bool {
	for _, c := range []reflect.Type{t, elemOf(t)} {
		if c == nil {
			continue
		}
		m, ok := c.MethodByName(name)
		if !ok {
			continue
		}
		f := runtime.FuncForPC(m.Func.Pointer())
		if f == nil {
			return true
		}
		if file, _ := f.FileLine(f.Entry()); file != "<autogenerated>" {
			return true
		}
	}
	return false
}

// elemOf returns the element type of a pointer type, and nil for any other type.
func elemOf(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Pointer {
		return t.Elem()
	}
	return nil
}

// sameValue reports whether a and b are the same pointer.
func sameValue(a, b any) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	return va.Kind() == reflect.Pointer && va.Type() == vb.Type() && va.Pointer() == vb.Pointer()
}

// lookup returns the record named name in runID's journal, if any: one Get through d's Journal,
// or, for a Durable without one, a scan of its History.
func lookup(ctx context.Context, d Durable, runID, name string) (Record, bool, error) {
	if j := journalOf(d); j != nil {
		return j.Get(ctx, runID, name)
	}
	recs, err := d.History(ctx, runID)
	if err != nil {
		return Record{}, false, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	for _, r := range recs {
		if r.Name == name {
			return r, true, nil
		}
	}
	return Record{}, false, nil
}

// putRecord records rec, a pure value, as the step name of runID unless it is recorded, and
// returns the record the journal holds.
func putRecord(ctx context.Context, d Durable, runID, name string, rec Record) (Record, error) {
	if j := journalOf(d); j != nil {
		return j.put(ctx, runID, name, rec)
	}
	return d.Do(ctx, runID, name, func(context.Context) (Record, error) { return rec, nil })
}

// recordFresh runs fn as the step name of runID, which the caller found unrecorded when it last
// read the run (see Journal.doFresh).
func recordFresh(ctx context.Context, d Durable, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	if j := journalOf(d); j != nil {
		return j.doFresh(ctx, runID, name, fn)
	}
	return d.Do(ctx, runID, name, fn)
}

// openRun reads runID's journal at the start of a drive (see Journal.open).
func openRun(ctx context.Context, d Durable, runID string) ([]Record, error) {
	if j := journalOf(d); j != nil {
		return j.open(ctx, runID)
	}
	recs, err := d.History(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	return recs, nil
}

// ===========================================================================
// Process-wide state shared by Journals over one store
// ===========================================================================

// flightKey names one step of one run in one store (see storeIdentity).
type flightKey struct {
	store       any
	runID, name string
}

// flight is one in-flight step call whose outcome its concurrent callers share.
type flight struct {
	done sync.WaitGroup // done when the call has returned
	val  []byte
	err  error
}

// flights holds the in-flight step calls of this process. An entry lives only while its call
// runs, so nothing accumulates.
var flights = struct {
	mu sync.Mutex
	m  map[flightKey]*flight
}{m: map[flightKey]*flight{}}

// errFlightPanicked is what the callers sharing a step call get when the call panicked.
var errFlightPanicked = errors.New("agent: a shared journal step panicked")

// shareFlight calls fn once for concurrent callers with the same key, and hands each of them its
// outcome. The bytes are shared: a caller decodes its own copy and never modifies them.
func shareFlight(k flightKey, fn func() ([]byte, error)) ([]byte, error) {
	flights.mu.Lock()
	if f, ok := flights.m[k]; ok {
		flights.mu.Unlock()
		f.done.Wait()
		return f.val, f.err
	}
	f := &flight{}
	f.done.Add(1)
	flights.m[k] = f
	flights.mu.Unlock()
	returned := false
	defer func() {
		if !returned {
			f.val, f.err = nil, errFlightPanicked
		}
		flights.mu.Lock()
		delete(flights.m, k)
		flights.mu.Unlock()
		f.done.Done()
	}()
	f.val, f.err = fn()
	returned = true
	return f.val, f.err
}

// joinFlight waits for the call in flight with key k, if there is one, and returns its outcome
// and true; it returns false at once when no call is in flight. It never starts a call.
func joinFlight(k flightKey) ([]byte, bool, error) {
	flights.mu.Lock()
	f, ok := flights.m[k]
	flights.mu.Unlock()
	if !ok {
		return nil, false, nil
	}
	f.done.Wait()
	return f.val, true, f.err
}

// pendingClaims holds, by store, run and marker key, the claim ids whose not-started record could
// not be written, so their markers may be live though their effect never ran (see Journal.claim).
// A key holds a set of ids: every claim of the key that failed that way.
//
// It is bounded: past maxPendingClaims ids, whole keys are dropped, oldest first (a key taken and
// remembered again may be dropped by its earlier position). Dropping is safe: a forgotten id's
// marker stays unvoided, which only costs a halt that remembering would have avoided; it never
// voids a marker, so it cannot let an effect run twice.
var pendingClaims = &claimMemo{m: map[flightKey]map[string]struct{}{}}

const maxPendingClaims = 4096

type claimMemo struct {
	mu    sync.Mutex
	m     map[flightKey]map[string]struct{}
	n     int         // ids held, over every key
	order []flightKey // keys in the order first remembered, oldest first; may hold keys already taken
}

// remember adds id to the ids remembered for k.
func (c *claimMemo) remember(k flightKey, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids, ok := c.m[k]
	if !ok {
		ids = map[string]struct{}{}
		c.m[k] = ids
		c.order = append(c.order, k)
	}
	if _, ok := ids[id]; !ok {
		ids[id] = struct{}{}
		c.n++
	}
	for c.n > maxPendingClaims && len(c.order) > 0 {
		old := c.order[0]
		c.order = c.order[1:]
		c.n -= len(c.m[old])
		delete(c.m, old)
	}
	if len(c.order) > 2*maxPendingClaims { // drop keys already taken
		live := c.order[:0]
		for _, o := range c.order {
			if _, ok := c.m[o]; ok {
				live = append(live, o)
			}
		}
		c.order = live
	}
}

// takeAll removes and returns every id remembered for k, in sorted order.
func (c *claimMemo) takeAll(k flightKey) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := c.m[k]
	delete(c.m, k)
	c.n -= len(ids)
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// takeID removes id from the ids remembered for k, and reports whether it was there.
func (c *claimMemo) takeID(k flightKey, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := c.m[k]
	if _, ok := ids[id]; !ok {
		return false
	}
	delete(ids, id)
	c.n--
	if len(ids) == 0 {
		delete(c.m, k)
	}
	return true
}

// runSet is a bounded set of run IDs: past maxKnownRuns, the least recently used is forgotten.
type runSet struct {
	mu  sync.Mutex
	m   map[string]*list.Element
	lru list.List // most recently used first; each Value is a run ID
}

const maxKnownRuns = 1 << 16

func (s *runSet) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if ok {
		s.lru.MoveToFront(e)
	}
	return ok
}

func (s *runSet) add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*list.Element{}
	}
	if e, ok := s.m[id]; ok {
		s.lru.MoveToFront(e)
		return
	}
	s.m[id] = s.lru.PushFront(id)
	if s.lru.Len() > maxKnownRuns {
		old := s.lru.Back()
		s.lru.Remove(old)
		delete(s.m, old.Value.(string))
	}
}

func (s *runSet) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[id]; ok {
		s.lru.Remove(e)
		delete(s.m, id)
	}
}

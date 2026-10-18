package audit

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// The redaction trail is the record of every erasure of journal content: who erased which records
// of which run, when, and why, written before the erasure and again after it. It is a separate
// append-only trail in an EventStore (never in the redacted run's journal, which may be over and
// whose leaves a redaction must not change), so AuditedEventStore signs and anchors it as it does a
// notification trail, and VerifyRedactions checks it against the journals.

const (
	// RedactionTrailName is the name of the deployment's redaction trail in an EventStore.
	RedactionTrailName = "redact:@log"
	// RedactionRecordFormat is the format of a RedactionRecord.
	RedactionRecordFormat = "bide.audit.redaction-record.v1"

	// Event leaf kinds of the trail.
	redactionBeginKind = "redaction_begin"
	redactionEndKind   = "redaction_end"
	redactionAckKind   = "redaction_ack"
)

// Phases of a RedactionRecord.
const (
	RedactionBegin = "begin"
	RedactionEnd   = "end"
	// RedactionAcknowledge is the phase of a record in which an operator acknowledges a planted
	// leaf (see AcknowledgeRecord).
	RedactionAcknowledge = "acknowledge"
)

// A RedactedRef names one record a redaction replaces: its position in the run's journal, its
// name, kind and stored length, and the hex leaf hash its tombstone keeps. It holds no content.
type RedactedRef struct {
	Position int    `json:"position"`
	Name     string `json:"name"`
	Kind     string `json:"kind,omitempty"`
	Length   int    `json:"length,omitempty"`
	LeafHash string `json:"leaf_hash"`
	// Already is set when the record held a tombstone when the redaction was planned.
	Already bool `json:"already,omitempty"`
}

// A RedactionRecord is one leaf of the redaction trail. A redaction writes a begin record, then
// redacts, then an end record; a begin with no end is an unfinished pass, finished by running the
// same plan again.
type RedactionRecord struct {
	Format      string        `json:"format"`
	Phase       string        `json:"phase"`
	PlanDigest  string        `json:"plan_digest"`
	RunID       string        `json:"run_id"`
	Records     []RedactedRef `json:"records,omitempty"` // begin
	JournalSize int           `json:"journal_size,omitempty"`
	Reason      string        `json:"reason"`
	Ticket      string        `json:"ticket"`
	// Executor is who ran it, as the tool reports: the key ID of the trail's signer and the
	// operating-system user, informational.
	Executor string `json:"executor"`
	AtMs     int64  `json:"at_ms"`
	Version  string `json:"version,omitempty"`
	// HeadSize is the size of the signed journal head that covered the run when it was erased, 0
	// when the redaction ran without one (Uncovered).
	HeadSize  int  `json:"head_size,omitempty"`
	Uncovered bool `json:"uncovered,omitempty"`
	// End only: how many records this pass replaced, and how many already held a tombstone, and the
	// error that stopped it, if any.
	Redacted int    `json:"redacted,omitempty"`
	Already  int    `json:"already,omitempty"`
	Error    string `json:"error,omitempty"`
	// EventCopiesKept is the number of events of the run the event store holds, which the erasure
	// left (the operator chose to erase the journal anyway); 0 when it held none or was not asked.
	EventCopiesKept int `json:"event_copies_kept,omitempty"`
	// Position and Prev bind the record to its place in the trail: its index, and the leaf hash of
	// the record before it (empty for the first). They are signed, so a signed record copied to
	// another position, or after other records, or into another trail, no longer verifies.
	Position int    `json:"position"`
	Prev     []byte `json:"prev,omitempty"`
	// Deployment names the deployment the trail belongs to (RedactionOptions.Deployment), signed, so a
	// record cannot be moved into another deployment's trail that holds the same key.
	Deployment string `json:"deployment,omitempty"`
	// AckPosition and AckLeafHash, on an acknowledge record only, name the planted leaf the operator
	// acknowledges: its position in the trail and the hex leaf hash of its bytes.
	AckPosition int    `json:"ack_position,omitempty"`
	AckLeafHash string `json:"ack_leaf_hash,omitempty"`
	// Alg and Signature sign the record's other members under the redaction key: a record without a
	// signature that verifies is not the tool's, whatever signed head covers it.
	Alg       string `json:"alg"`
	Signature []byte `json:"signature"`
}

// redactionRecordTag domain-separates the signature of a trail record from every other message the
// package signs.
const redactionRecordTag = RedactionRecordFormat + "\x00"

// signedBytes is what a record's signature covers: the tag and the record's encoding with no
// signature.
func (r RedactionRecord) signedBytes() ([]byte, error) {
	r.Signature = nil
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("audit: encode redaction record: %w", err)
	}
	return append([]byte(redactionRecordTag), b...), nil
}

func (r *RedactionRecord) sign(s Signer) error {
	r.Alg = string(s.Alg())
	m, err := r.signedBytes()
	if err != nil {
		return err
	}
	if r.Signature, err = s.Sign(m); err != nil {
		return fmt.Errorf("audit: sign redaction record: %w", err)
	}
	return nil
}

// verifySignature reports whether the record's signature verifies under v.
func (r RedactionRecord) verifySignature(v Verifier) bool {
	if v == nil || len(r.Signature) == 0 || r.Alg != string(v.Alg()) {
		return false
	}
	m, err := r.signedBytes()
	return err == nil && v.Verify(m, r.Signature)
}

func (r RedactionRecord) kind() string {
	switch r.Phase {
	case RedactionEnd:
		return redactionEndKind
	case RedactionAcknowledge:
		return redactionAckKind
	}
	return redactionBeginKind
}

func redactionLeaf(r RedactionRecord) ([]byte, error) {
	salt, err := newEventSalt()
	if err != nil {
		return nil, err
	}
	inner, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("audit: encode redaction record: %w", err)
	}
	b, err := json.Marshal(eventLeaf{Kind: r.kind(), Event: inner, Salt: salt})
	if err != nil {
		return nil, fmt.Errorf("audit: encode redaction record: %w", err)
	}
	return tagged(eventLeafTag, b), nil
}

// DecodeRedactionLeaf reads a redaction record from a stored event leaf. The leaf's kind must be the
// record's phase, the format must be this one, and an unknown member is an error: a record of
// another version is not skipped.
func DecodeRedactionLeaf(leaf []byte) (RedactionRecord, error) {
	body, ok := bytes.CutPrefix(leaf, []byte(eventLeafTag))
	if !ok {
		return RedactionRecord{}, errors.New("not an event leaf")
	}
	var el eventLeaf
	if err := json.Unmarshal(body, &el); err != nil {
		return RedactionRecord{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(el.Event))
	dec.DisallowUnknownFields()
	var r RedactionRecord
	if err := dec.Decode(&r); err != nil {
		return RedactionRecord{}, err
	}
	switch {
	case r.Format != RedactionRecordFormat:
		return RedactionRecord{}, fmt.Errorf("format %q is not %s", r.Format, RedactionRecordFormat)
	case r.Phase != RedactionBegin && r.Phase != RedactionEnd && r.Phase != RedactionAcknowledge:
		return RedactionRecord{}, fmt.Errorf("phase %q", r.Phase)
	case el.Kind != r.kind():
		return RedactionRecord{}, fmt.Errorf("leaf kind %q holds a %q record", el.Kind, r.Phase)
	}
	if r.Phase == RedactionAcknowledge {
		if h, err := hex.DecodeString(r.AckLeafHash); err != nil || len(h) != 32 || r.AckPosition < 0 {
			return RedactionRecord{}, errors.New("an acknowledge record names no leaf")
		}
	}
	return r, nil
}

// AppendRedactionRecord signs r with signer (the redaction key) and appends it to the redaction trail
// in store, at the next position. A lost
// race for the position is retried; any other failure is returned.
func AppendRedactionRecord(ctx context.Context, store EventStore, r RedactionRecord, signer Signer) error {
	if store == nil {
		return fmt.Errorf("audit: AppendRedactionRecord: no store: %w", agent.ErrConfig)
	}
	if err := checkSigner(signer); err != nil {
		return err
	}
	r.Format = RedactionRecordFormat
	for range 8 {
		leaves, err := store.Load(ctx, RedactionTrailName)
		if err != nil {
			return fmt.Errorf("audit: load the redaction trail: %w", err)
		}
		// The record is bound to the position it is appended at and to the leaf before it, so every
		// retry after a lost race signs it again at the new position.
		r.Position, r.Prev = len(leaves), prevHash(leaves, len(leaves))
		if err := r.sign(signer); err != nil {
			return err
		}
		leaf, err := redactionLeaf(r)
		if err != nil {
			return err
		}
		err = store.Append(ctx, RedactionTrailName, len(leaves), leaf)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrSeqTaken) {
			return fmt.Errorf("audit: append to the redaction trail: %w", err)
		}
	}
	return fmt.Errorf("audit: append to the redaction trail: position taken again and again: %w", ErrSeqTaken)
}

// LoadRedactionTrail returns the records of the redaction trail in store, in order.
func LoadRedactionTrail(ctx context.Context, store EventStore) ([]RedactionRecord, error) {
	if store == nil {
		return nil, fmt.Errorf("audit: LoadRedactionTrail: no store: %w", agent.ErrConfig)
	}
	leaves, err := store.Load(ctx, RedactionTrailName)
	if err != nil {
		return nil, fmt.Errorf("audit: load the redaction trail: %w", err)
	}
	return decodeRedactionLeaves(leaves)
}

func decodeRedactionLeaves(leaves [][]byte) ([]RedactionRecord, error) {
	out := make([]RedactionRecord, len(leaves))
	for i, l := range leaves {
		r, err := DecodeRedactionLeaf(l)
		if err != nil {
			return nil, fmt.Errorf("audit: redaction trail record %d: %w", i, err)
		}
		out[i] = r
	}
	return out, nil
}

// RedactionOptions are what an accountable redaction needs besides the plan.
type RedactionOptions struct {
	// Reason and Ticket say why, and under what authority: both are required and are recorded.
	Reason, Ticket string
	// Signer signs every record the redaction writes (required): the redaction key, which
	// VerifyRedactions verifies under the same log key that signs the trail's heads.
	Signer Signer
	// EventCopiesKept is the number of events of the run the event store holds that the erasure
	// leaves; it is recorded and VerifyRedactions reports it.
	EventCopiesKept int
	// Deployment names the deployment the trail belongs to (required); it is signed into every record,
	// and VerifyRedactions checks it, so two deployments that share a key cannot copy records between
	// their trails.
	Deployment string
	// Executor says who is erasing (a key ID, a user name); required.
	Executor string
	// Now is the time stamped into the tombstones and the trail; the zero value is an error.
	Now time.Time
	// Version is the tool's version, recorded.
	Version string
	// Heads are signed journal heads of the plan's run, and Verifier the log key that verifies them:
	// the redaction needs one that covers the whole journal as it stands (its size is at least the
	// journal's), so the evidence of the run is anchored before its content is erased. Without
	// such a head the redaction is refused, unless AllowUncovered is set, which is recorded.
	Heads    []SignedTreeHead
	Verifier Verifier
	// Redactor, when set, replaces the records instead of the journal's own store (see agent.RedactUsing).
	Redactor agent.Redactor
	// TrailHeads are the signed heads of the redaction trail so far (the operator's anchor, or the
	// -head-out file): RedactJournal checks the trail against them before it appends, so a trail
	// that is not the one they attest (a typo in the trail's database starts a new one) is refused.
	TrailHeads []SignedTreeHead
	// AllowUncovered lets a redaction go ahead with no covering head. It is recorded in the trail.
	AllowUncovered bool
}

// Refusals of RedactJournal, each wrapping agent.ErrConfig.
var (
	// ErrRedactNoHead is a redaction of a run no verified signed head covers.
	ErrRedactNoHead = fmt.Errorf("redaction refused: no signed journal head that verifies and matches the journal covers the whole run, so its evidence is not anchored (make it first, or allow an uncovered redaction, which is recorded): %w", agent.ErrConfig)
	// ErrRedactTrailUntrusted is a redaction whose trail does not match the signed heads given: it is
	// not the trail they attest (records deleted or changed, or another trail altogether). A record
	// planted in the trail does not stop an erasure: it is reported and accounts for nothing.
	ErrRedactTrailUntrusted = fmt.Errorf("redaction refused: the redaction trail does not match its signed heads, so it is not the trail they attest (records were deleted or changed, or this is another trail): %w", agent.ErrConfig)
	// ErrRedactNoTrail is a redaction whose begin record could not be written.
	ErrRedactNoTrail = fmt.Errorf("redaction refused: the redaction trail cannot be written, so nothing was erased: %w", agent.ErrConfig)
)

// RedactJournal erases the records plan names, accountably: it checks the plan can run and that a
// signed head covers the run, appends a begin record to the redaction trail in trail, runs
// agent.Redact, and appends an end record, even when the redaction failed partway (its error is in
// the record). If the begin record cannot be written, nothing is erased (ErrRedactNoTrail). If the
// end record cannot be written, the erasure stands, the begin has no end (VerifyRedactions reports it
// as unfinished) and the error says so; run the same plan again to finish and close it.
func RedactJournal(ctx context.Context, j *agent.Journal, trail EventStore, plan agent.RedactionPlan, opt RedactionOptions) (agent.RedactionResult, error) {
	var res agent.RedactionResult
	opt.Deployment = strings.TrimSpace(opt.Deployment)
	switch {
	case j == nil || trail == nil:
		return res, fmt.Errorf("audit: RedactJournal: a nil journal or trail: %w", agent.ErrConfig)
	case strings.TrimSpace(opt.Reason) == "" || strings.TrimSpace(opt.Ticket) == "" || strings.TrimSpace(opt.Executor) == "":
		return res, fmt.Errorf("audit: RedactJournal: a reason, a ticket and an executor are required, and are recorded: %w", agent.ErrConfig)
	case strings.TrimSpace(opt.Deployment) == "":
		return res, fmt.Errorf("audit: RedactJournal: a deployment name is required: it is signed into every trail record, so records cannot be copied between deployments that share a key: %w", agent.ErrConfig)
	case opt.Signer == nil:
		return res, fmt.Errorf("audit: RedactJournal: no signer for the trail's records: %w", agent.ErrConfig)
	case opt.Now.IsZero():
		return res, fmt.Errorf("audit: RedactJournal: no time: %w", agent.ErrConfig)
	case plan.Refusal != "":
		return res, fmt.Errorf("audit: RedactJournal: the plan is refused: %s: %w", plan.Refusal, agent.ErrConfig)
	case len(plan.Records) == 0:
		return res, nil
	}
	rec := RedactionRecord{
		Format: RedactionRecordFormat, Phase: RedactionBegin, PlanDigest: plan.Digest, RunID: plan.RunID,
		JournalSize: plan.JournalSize, Reason: opt.Reason, Ticket: opt.Ticket, Executor: opt.Executor,
		AtMs: opt.Now.UnixMilli(), Version: opt.Version, EventCopiesKept: opt.EventCopiesKept, Deployment: opt.Deployment,
	}
	for _, p := range plan.Records {
		rec.Records = append(rec.Records, RedactedRef{Position: p.Position, Name: p.Name, Kind: string(p.Kind), Length: p.Length, LeafHash: p.LeafHash, Already: p.AlreadyRedacted})
	}
	if err := checkTrailBeforeAppend(ctx, trail, opt); err != nil {
		return res, err
	}
	size, err := coveringHeadSize(ctx, j, plan, opt)
	if err != nil {
		return res, err
	}
	rec.HeadSize, rec.Uncovered = size, size == 0
	if err := AppendRedactionRecord(ctx, trail, rec, opt.Signer); err != nil {
		return res, fmt.Errorf("%w: %w", ErrRedactNoTrail, err)
	}
	var rerr error
	if opt.Redactor != nil {
		res, rerr = agent.RedactUsing(ctx, j, opt.Redactor, plan, opt.Now)
	} else {
		res, rerr = agent.Redact(ctx, j, plan, opt.Now)
	}
	end := rec
	end.Phase, end.Records = RedactionEnd, nil
	end.Redacted, end.Already = len(res.Redacted), len(res.Already)
	if rerr != nil {
		end.Error = rerr.Error()
	}
	if eerr := AppendRedactionRecord(ctx, trail, end, opt.Signer); eerr != nil {
		return res, errors.Join(rerr, fmt.Errorf("audit: the erasure ran but its end record was not written (the trail shows it unfinished; run the same plan again to close it): %w", eerr))
	}
	return res, rerr
}

// coveringHeadSize returns the size of a verified head that matches the run's journal and covers all
// of it, 0 if AllowUncovered and none does, and ErrRedactNoHead otherwise.
func coveringHeadSize(ctx context.Context, j *agent.Journal, plan agent.RedactionPlan, opt RedactionOptions) (int, error) {
	recs, err := j.History(ctx, plan.RunID)
	if err != nil {
		return 0, err
	}
	best := 0
	if opt.Verifier != nil {
		for _, h := range opt.Heads {
			if class, _, _ := classifyHead(h, plan.RunID, opt.Verifier, recs); class == headMatches && h.Size >= len(recs) && h.Size > best {
				best = h.Size
			}
		}
	}
	if best == 0 && !opt.AllowUncovered {
		return 0, ErrRedactNoHead
	}
	return best, nil
}

// RedactionFindingKind names what VerifyRedactions found wrong.
type RedactionFindingKind string

// The findings of VerifyRedactions.
const (
	// FindingUnanchored: the trail has no signed head, so nothing shows it is complete.
	FindingUnanchored RedactionFindingKind = "UNANCHORED"
	// FindingTrailAltered: a head contradicts the trail (records deleted or changed).
	FindingTrailAltered RedactionFindingKind = "TRAIL_ALTERED"
	// FindingUnattestedTail: records after the newest signed head.
	FindingUnattestedTail RedactionFindingKind = "UNATTESTED_TAIL"
	// FindingIncomplete: a begin with no end: an unfinished pass.
	FindingIncomplete RedactionFindingKind = "INCOMPLETE"
	// FindingUnaccounted: a tombstone no trail record names (a redaction made by hand).
	FindingUnaccounted RedactionFindingKind = "UNACCOUNTED"
	// FindingNotErased: a record the trail lists as erased still holds content.
	FindingNotErased RedactionFindingKind = "NOT_ERASED"
	// FindingNameMismatch: a tombstone's name is not the name the trail recorded at that position.
	FindingNameMismatch RedactionFindingKind = "NAME_MISMATCH"
	// FindingHashMismatch: a tombstone's leaf hash is not the one the trail recorded.
	FindingHashMismatch RedactionFindingKind = "HASH_MISMATCH"
	// FindingUncovered: a redaction that ran with no covering signed head.
	FindingUncovered RedactionFindingKind = "UNCOVERED"
	// FindingReordered: a trail record that is not at the position, or after the record, it was signed for: a replayed, copied or reordered record.
	FindingReordered RedactionFindingKind = "REORDERED"
	// FindingBadSignature: a trail record with no signature, or one that does not verify under the key: not the tool's, whatever head covers it.
	FindingBadSignature RedactionFindingKind = "BAD_SIGNATURE"
	// FindingEventCopies: an erasure that left the run's events in the event store.
	FindingEventCopies RedactionFindingKind = "EVENT_COPIES_KEPT"
	// FindingTimeMismatch: a tombstone stamped at a time no pass of the trail that listed it carries.
	FindingTimeMismatch RedactionFindingKind = "TIME_MISMATCH"
	// FindingUnchecked: the trail names a run no journal was given for.
	FindingUnchecked RedactionFindingKind = "UNCHECKED"
	// FindingMalformed: a trail record that does not read.
	FindingMalformed RedactionFindingKind = "MALFORMED"
)

// A RedactionFinding is one problem VerifyRedactions found.
type RedactionFinding struct {
	Kind   RedactionFindingKind `json:"kind"`
	RunID  string               `json:"run_id,omitempty"`
	Detail string               `json:"detail"`
}

// RedactionVerifyInput is what VerifyRedactions checks.
type RedactionVerifyInput struct {
	// Trail is the redaction trail's leaves, in order (an ExportNotifyTrail export of RedactionTrailName).
	Trail [][]byte
	// Heads are signed heads over the trail (TreeEvents, RunID RedactionTrailName) and Verifier their key.
	Heads    []SignedTreeHead
	Verifier Verifier
	// Unanchored accepts a trail with no head, reported but not failed.
	Unanchored bool
	// Deployment is the deployment every record must be signed for (RedactionOptions.Deployment); required.
	Deployment string
	// Journals are the journals to check against, by run ID: the records as read (a store's History,
	// or JournalExport.Journal). A run the trail names that has no journal here is not checked.
	Journals map[string][]agent.Record
}

// A RedactionReport is the result of VerifyRedactions.
type RedactionReport struct {
	Records    int                `json:"records"`
	Redactions int                `json:"redactions"` // begin records
	Anchored   bool               `json:"anchored"`
	HeadSize   int                `json:"head_size,omitempty"`
	Findings   []RedactionFinding `json:"findings,omitempty"`
	// Notes are things that are not findings: an earlier failed pass that a later pass finished.
	Notes []string `json:"notes,omitempty"`
}

// OK reports whether nothing was found.
func (r RedactionReport) OK() bool { return len(r.Findings) == 0 }

// VerifyRedactions checks the redaction trail against its signed heads and the journals. A tombstone's
// name is outside the journal's leaf hashes, so it is attested only here: it is checked against the
// name the signed trail recorded at that position, so names are attested exactly when the trail
// verifies. It returns ErrNotVerified wrapping the first finding when any is found.
func VerifyRedactions(in RedactionVerifyInput) (RedactionReport, error) {
	rep := RedactionReport{Records: len(in.Trail)}
	if strings.TrimSpace(in.Deployment) == "" {
		return rep, fmt.Errorf("audit: VerifyRedactions: a deployment name is required: every record is signed for one: %w", agent.ErrConfig)
	}
	in.Deployment = strings.TrimSpace(in.Deployment)
	add := func(k RedactionFindingKind, run, format string, a ...any) {
		rep.Findings = append(rep.Findings, RedactionFinding{Kind: k, RunID: run, Detail: fmt.Sprintf(format, a...)})
	}
	// Anchoring: every head verifies, is of this trail, and commits to a prefix of it.
	newest := -1
	for _, h := range in.Heads {
		switch {
		case in.Verifier == nil || h.Verify(in.Verifier) != nil:
			add(FindingTrailAltered, "", "a signed head of size %d does not verify under the log key", h.Size)
			continue
		case h.Kind != TreeEvents || h.RunID != RedactionTrailName:
			add(FindingTrailAltered, "", "a signed head is of the %q tree of %q, not of the redaction trail", h.Kind, h.RunID)
			continue
		case h.Size > len(in.Trail):
			add(FindingTrailAltered, "", "the trail has %d record(s) and a signed head commits to %d: records were deleted", len(in.Trail), h.Size)
			continue
		case !bytes.Equal(merkleRoot(in.Trail[:h.Size]), h.Root):
			add(FindingTrailAltered, "", "the first %d record(s) are not the ones a signed head commits to: a record was changed, reordered or replaced", h.Size)
			continue
		}
		if h.Size > newest {
			newest = h.Size
		}
	}
	switch {
	case len(in.Heads) == 0 && !in.Unanchored:
		add(FindingUnanchored, "", "no signed head: nothing attests that the trail is complete or unaltered")
	case newest >= 0:
		rep.Anchored, rep.HeadSize = true, newest
		if newest < len(in.Trail) {
			add(FindingUnattestedTail, "", "%d record(s) after the newest signed head (size %d) are not attested", len(in.Trail)-newest, newest)
		}
	}
	recs, trusted, pend := classifyTrail(in)
	// A planted, replayed or unreadable leaf is a finding, unless the redaction key signed an
	// acknowledgement of exactly that leaf (its position and hash) after it.
	acked := map[[2]string]bool{}
	for i, r := range recs {
		if trusted[i] && r.Phase == RedactionAcknowledge && r.AckPosition < i {
			acked[[2]string{fmt.Sprint(r.AckPosition), r.AckLeafHash}] = true
		}
	}
	for _, f := range pend {
		if acked[[2]string{fmt.Sprint(f.pos), hex.EncodeToString(leafHash(in.Trail[f.pos]))}] {
			rep.Notes = append(rep.Notes, fmt.Sprintf("trail record %d (%s) is acknowledged by a signed record of the redaction key: %s", f.pos, f.kind, f.detail))
			continue
		}
		add(f.kind, f.run, "%s", f.detail)
	}
	type key struct {
		run string
		pos int
	}
	// A tombstone is accounted for by the begin record that first lists the record as erased by
	// that pass (Already false), never by one that lists it as already redacted: the plan of a later
	// erasure of a run reads a tombstone made by hand as "already redacted", and that must not
	// launder it into the trail.
	listed := map[key]RedactedRef{}
	times := map[key][]int64{} // the time of every pass that listed the record as erased
	type pass struct{ phase, err string }
	history := map[string][]pass{} // run + plan digest -> its records, in order
	var order []string
	for i, r := range recs {
		if !trusted[i] || r.Phase == RedactionAcknowledge {
			continue
		}
		id := r.RunID + "\x00" + r.PlanDigest
		if _, ok := history[id]; !ok {
			order = append(order, id)
		}
		history[id] = append(history[id], pass{r.Phase, r.Error})
		if r.Phase != RedactionBegin {
			continue
		}
		rep.Redactions++
		if r.EventCopiesKept > 0 {
			add(FindingEventCopies, r.RunID, "the redaction of plan %.12s left %d event(s) of the run in the event store (ticket %s)", r.PlanDigest, r.EventCopiesKept, r.Ticket)
		}
		if r.Uncovered {
			add(FindingUncovered, r.RunID, "the redaction of plan %.12s ran with no covering signed journal head (ticket %s)", r.PlanDigest, r.Ticket)
		}
		for _, ref := range r.Records {
			if ref.Already {
				continue
			}
			k := key{r.RunID, ref.Position}
			if _, ok := listed[k]; !ok {
				listed[k] = ref
			}
			times[k] = append(times[k], r.AtMs)
		}
	}
	// A pass is judged by its last record: a begin with no end after it is unfinished, an end with
	// an error is a failed pass, and a later end without one (the same plan run again) closes it.
	for _, id := range order {
		h := history[id]
		run, digest, _ := strings.Cut(id, "\x00")
		last := h[len(h)-1]
		switch {
		case last.phase == RedactionBegin:
			add(FindingIncomplete, run, "the redaction of plan %.12s has a begin record and no end after it: an unfinished pass (run the same plan again)", digest)
		case last.err != "":
			add(FindingIncomplete, run, "the redaction of plan %.12s stopped with an error: %s", digest, last.err)
		default:
			for _, p := range h[:len(h)-1] {
				if p.err != "" {
					rep.Notes = append(rep.Notes, fmt.Sprintf("run %s: an earlier pass of plan %.12s stopped with an error (%s) and a later pass finished it", run, digest, p.err))
				}
			}
		}
	}
	checked := map[string]bool{}
	for run, journal := range in.Journals {
		checked[run] = true
		for i, rec := range journal {
			ref, isListed := listed[key{run, i}]
			if rec.Redacted {
				leaf, okLeaf := agent.TombstoneLeafHash(rec.Raw())
				at, okAt := agent.TombstoneMillis(rec.Raw())
				if !okLeaf || !okAt {
					add(FindingMalformed, run, "record %d (%q): its tombstone does not read", i, rec.Name)
					continue
				}
				switch {
				case !isListed:
					add(FindingUnaccounted, run, "record %d (%q) is a tombstone and no trail record names it as erased by a pass: a redaction made outside the tool (a later plan listing it as already redacted does not account for it)", i, rec.Name)
				case ref.Name != rec.Name:
					add(FindingNameMismatch, run, "record %d: the tombstone says %q and the trail recorded %q", i, rec.Name, ref.Name)
				case hex.EncodeToString(leaf) != ref.LeafHash:
					add(FindingHashMismatch, run, "record %d (%q): the tombstone keeps leaf hash %x and the trail recorded %s", i, rec.Name, leaf, ref.LeafHash)
				case !slices.Contains(times[key{run, i}], at):
					add(FindingTimeMismatch, run, "record %d (%q): the tombstone is stamped %d and no pass of the trail that listed it was stamped then", i, rec.Name, at)
				}
				continue
			}
			if isListed {
				add(FindingNotErased, run, "record %d (%q) is listed as erased in the trail and still holds content", i, rec.Name)
			}
		}
	}
	// A run the trail names that no journal was given for is not checked, and that is not a pass.
	runs := map[string]bool{}
	for k := range listed {
		runs[k.run] = true
	}
	for _, run := range slices.Sorted(maps.Keys(runs)) {
		if !checked[run] {
			add(FindingUnchecked, run, "the trail names this run and no journal of it was given, so its tombstones and listed records were not checked")
		}
	}
	if len(rep.Findings) > 0 {
		return rep, notVerified("audit: redactions: %d finding(s), first: %s: %s", len(rep.Findings), rep.Findings[0].Kind, rep.Findings[0].Detail)
	}
	return rep, nil
}

// prevHash is the leaf hash of the trail's leaf before position i, nil for the first.
func prevHash(leaves [][]byte, i int) []byte {
	if i == 0 {
		return nil
	}
	return leafHash(leaves[i-1])
}

// SignRedactionTrail signs a tree head over the redaction trail in store, after checking the trail is
// the one the earlier heads attest: every head in prior must verify under v, be a head of the trail,
// and commit to a prefix of it, and every record in the trail must carry a signature that verifies
// under v. A record that is not the tool's (unsigned, signed for another deployment or position, or
// not a record) does not stop the signing: the head attests that the leaves exist, VerifyRedactions
// reports them, and they account for nothing. The head is over exactly the leaves it checked, never a
// fresh load, so a record appended meanwhile is not attested. It returns an error wrapping
// ErrNotVerified when a prior head does not match the trail.
func SignRedactionTrail(ctx context.Context, store EventStore, signer Signer, v Verifier, deployment string, prior []SignedTreeHead, now time.Time) (SignedTreeHead, error) {
	if store == nil || signer == nil || strings.TrimSpace(deployment) == "" || v == nil {
		return SignedTreeHead{}, fmt.Errorf("audit: SignRedactionTrail: a store, a signer, a verifier and a deployment name are required: %w", agent.ErrConfig)
	}
	deployment = strings.TrimSpace(deployment)
	leaves, err := store.Load(ctx, RedactionTrailName)
	if err != nil {
		return SignedTreeHead{}, fmt.Errorf("audit: load the redaction trail: %w", err)
	}
	rep, verr := VerifyRedactions(RedactionVerifyInput{Trail: leaves, Heads: prior, Verifier: v, Deployment: deployment, Unanchored: len(prior) == 0})
	if verr != nil && !errors.Is(verr, ErrNotVerified) {
		return SignedTreeHead{}, verr // not a finding: the check could not run (no deployment, say), so nothing is signed
	}
	for _, f := range rep.Findings {
		switch f.Kind {
		case FindingTrailAltered:
			return SignedTreeHead{}, notVerified("audit: refusing to sign the redaction trail: %s: %s", f.Kind, f.Detail)
		}
	}
	return SignTreeHead(TreeHead{Kind: TreeEvents, RunID: RedactionTrailName, Size: len(leaves), Root: merkleRoot(leaves), TimestampNanos: now.UnixNano()}, signer)
}

// CheckRedactionTrail checks, as RedactJournal does before it appends, that the redaction trail in store
// is the one the earlier heads attest, and returns ErrRedactTrailUntrusted if it is not. A planted
// record does not fail it.
func CheckRedactionTrail(ctx context.Context, store EventStore, signer Signer, deployment string, prior []SignedTreeHead) error {
	if store == nil || signer == nil || strings.TrimSpace(deployment) == "" {
		return fmt.Errorf("audit: CheckRedactionTrail: a store, a signer and a deployment name are required: %w", agent.ErrConfig)
	}
	return checkTrailBeforeAppend(ctx, store, RedactionOptions{Signer: signer, Deployment: strings.TrimSpace(deployment), TrailHeads: prior})
}

// checkTrailBeforeAppend refuses to append to a trail that holds a record the redaction key did not
// match the heads given, before anything is erased or written. A planted record does not stop an erasure.
func checkTrailBeforeAppend(ctx context.Context, trail EventStore, opt RedactionOptions) error {
	v, err := NewVerifier(opt.Signer.Alg(), opt.Signer.PublicKey())
	if err != nil {
		return fmt.Errorf("audit: RedactJournal: the signer's public key: %w", err)
	}
	leaves, err := trail.Load(ctx, RedactionTrailName)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRedactNoTrail, err)
	}
	rep, verr := VerifyRedactions(RedactionVerifyInput{Trail: leaves, Heads: opt.TrailHeads, Verifier: v, Deployment: opt.Deployment, Unanchored: true})
	if verr != nil && !errors.Is(verr, ErrNotVerified) {
		return fmt.Errorf("audit: RedactJournal: the trail could not be checked: %w", verr)
	}
	for _, f := range rep.Findings {
		switch f.Kind {
		case FindingTrailAltered:
			return fmt.Errorf("%w (%s: %s)", ErrRedactTrailUntrusted, f.Kind, f.Detail)
		}
	}
	return nil
}

// A pendingFinding is a problem with one trail leaf, held until the acknowledgements are read.
type pendingFinding struct {
	pos    int
	kind   RedactionFindingKind
	run    string
	detail string
}

// classifyTrail reads each leaf of in.Trail on its own and decides which are the tool's: a record
// counts only if it reads, is signed under the key, is signed for this deployment, and is at the
// position, after the record, it was signed for. Any other is returned as a pending finding, and
// accounts for nothing. in.Deployment must be trimmed.
func classifyTrail(in RedactionVerifyInput) (recs []RedactionRecord, trusted []bool, pend []pendingFinding) {
	recs = make([]RedactionRecord, len(in.Trail))
	trusted = make([]bool, len(in.Trail))
	for i, l := range in.Trail {
		r, err := DecodeRedactionLeaf(l)
		if err != nil {
			pend = append(pend, pendingFinding{i, FindingMalformed, "", fmt.Sprintf("trail record %d does not read: %v", i, err)})
			continue
		}
		recs[i], trusted[i] = r, true
	}
	for i, r := range recs {
		if !trusted[i] {
			continue
		}
		switch {
		case !r.verifySignature(in.Verifier):
			pend = append(pend, pendingFinding{i, FindingBadSignature, r.RunID, fmt.Sprintf("trail record %d has no signature that verifies under the key: it was not written by the redaction tool (a planted record), whatever signed head covers it", i)})
		case r.Deployment != in.Deployment:
			pend = append(pend, pendingFinding{i, FindingBadSignature, r.RunID, fmt.Sprintf("trail record %d is signed for deployment %q, not %q: it was copied from another trail", i, r.Deployment, in.Deployment)})
		case r.Position != i || !bytes.Equal(r.Prev, prevHash(in.Trail, i)):
			pend = append(pend, pendingFinding{i, FindingReordered, r.RunID, fmt.Sprintf("trail record %d was signed for position %d after another record: a replayed, copied or reordered record (it hides the pass that follows it)", i, r.Position)})
		default:
			continue
		}
		trusted[i] = false
	}
	slices.SortStableFunc(pend, func(a, b pendingFinding) int { return a.pos - b.pos })
	return recs, trusted, pend
}

// AcknowledgeRecord appends to the redaction trail a record, signed with signer (the redaction key),
// in which the operator acknowledges the leaf at position: a planted, replayed or unreadable one that
// VerifyRedactions reports and that the operator has investigated. The record names the leaf's
// position and the hash of its bytes, with the reason, the ticket and who, so VerifyRedactions then
// reports that leaf as acknowledged and no longer fails on it. It acknowledges nothing else: not a
// leaf the tool wrote, not a tombstone no pass accounts for, and not a different leaf at that
// position later. It refuses a position that holds no finding.
func AcknowledgeRecord(ctx context.Context, store EventStore, signer Signer, deployment string, position int, reason, ticket, executor string, now time.Time) error {
	deployment = strings.TrimSpace(deployment)
	switch {
	case store == nil || signer == nil:
		return fmt.Errorf("audit: AcknowledgeRecord: a store and a signer are required: %w", agent.ErrConfig)
	case deployment == "":
		return fmt.Errorf("audit: AcknowledgeRecord: a deployment name is required: %w", agent.ErrConfig)
	case strings.TrimSpace(reason) == "" || strings.TrimSpace(ticket) == "" || strings.TrimSpace(executor) == "":
		return fmt.Errorf("audit: AcknowledgeRecord: a reason, a ticket and an executor are required, and are recorded: %w", agent.ErrConfig)
	case now.IsZero():
		return fmt.Errorf("audit: AcknowledgeRecord: no time: %w", agent.ErrConfig)
	}
	v, err := NewVerifier(signer.Alg(), signer.PublicKey())
	if err != nil {
		return fmt.Errorf("audit: AcknowledgeRecord: the signer's public key: %w", err)
	}
	leaves, err := store.Load(ctx, RedactionTrailName)
	if err != nil {
		return fmt.Errorf("audit: load the redaction trail: %w", err)
	}
	if position < 0 || position >= len(leaves) {
		return fmt.Errorf("audit: AcknowledgeRecord: position %d is outside the trail of %d record(s): %w", position, len(leaves), agent.ErrConfig)
	}
	_, _, pend := classifyTrail(RedactionVerifyInput{Trail: leaves, Verifier: v, Deployment: deployment})
	if !slices.ContainsFunc(pend, func(f pendingFinding) bool { return f.pos == position }) {
		return fmt.Errorf("audit: AcknowledgeRecord: the record at position %d is not a finding: only a record the tool did not write (BAD_SIGNATURE, REORDERED, MALFORMED) can be acknowledged: %w", position, agent.ErrConfig)
	}
	return AppendRedactionRecord(ctx, store, RedactionRecord{
		Phase: RedactionAcknowledge, RunID: "", Reason: reason, Ticket: ticket, Executor: executor, AtMs: now.UnixMilli(),
		Deployment: deployment, AckPosition: position, AckLeafHash: hex.EncodeToString(leafHash(leaves[position])),
	}, signer)
}

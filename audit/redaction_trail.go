package audit

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
)

// Phases of a RedactionRecord.
const (
	RedactionBegin = "begin"
	RedactionEnd   = "end"
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
}

func (r RedactionRecord) kind() string {
	if r.Phase == RedactionEnd {
		return redactionEndKind
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
	case r.Phase != RedactionBegin && r.Phase != RedactionEnd:
		return RedactionRecord{}, fmt.Errorf("phase %q", r.Phase)
	case el.Kind != r.kind():
		return RedactionRecord{}, fmt.Errorf("leaf kind %q holds a %q record", el.Kind, r.Phase)
	}
	return r, nil
}

// AppendRedactionRecord appends r to the redaction trail in store, at the next position. A lost
// race for the position is retried; any other failure is returned.
func AppendRedactionRecord(ctx context.Context, store EventStore, r RedactionRecord) error {
	r.Format = RedactionRecordFormat
	leaf, err := redactionLeaf(r)
	if err != nil {
		return err
	}
	for range 8 {
		leaves, err := store.Load(ctx, RedactionTrailName)
		if err != nil {
			return fmt.Errorf("audit: load the redaction trail: %w", err)
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
	// AllowUncovered lets a redaction go ahead with no covering head. It is recorded in the trail.
	AllowUncovered bool
}

// Refusals of RedactJournal, each wrapping agent.ErrConfig.
var (
	// ErrRedactNoHead is a redaction of a run no verified signed head covers.
	ErrRedactNoHead = fmt.Errorf("redaction refused: no signed journal head that verifies and matches the journal covers the whole run, so its evidence is not anchored (make it first, or allow an uncovered redaction, which is recorded): %w", agent.ErrConfig)
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
	switch {
	case j == nil || trail == nil:
		return res, fmt.Errorf("audit: RedactJournal: a nil journal or trail: %w", agent.ErrConfig)
	case strings.TrimSpace(opt.Reason) == "" || strings.TrimSpace(opt.Ticket) == "" || strings.TrimSpace(opt.Executor) == "":
		return res, fmt.Errorf("audit: RedactJournal: a reason, a ticket and an executor are required, and are recorded: %w", agent.ErrConfig)
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
		AtMs: opt.Now.UnixMilli(), Version: opt.Version,
	}
	for _, p := range plan.Records {
		rec.Records = append(rec.Records, RedactedRef{Position: p.Position, Name: p.Name, Kind: string(p.Kind), Length: p.Length, LeafHash: p.LeafHash, Already: p.AlreadyRedacted})
	}
	size, err := coveringHeadSize(ctx, j, plan, opt)
	if err != nil {
		return res, err
	}
	rec.HeadSize, rec.Uncovered = size, size == 0
	if err := AppendRedactionRecord(ctx, trail, rec); err != nil {
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
	if eerr := AppendRedactionRecord(ctx, trail, end); eerr != nil {
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
}

// OK reports whether nothing was found.
func (r RedactionReport) OK() bool { return len(r.Findings) == 0 }

// VerifyRedactions checks the redaction trail against its signed heads and the journals. A tombstone's
// name is outside the journal's leaf hashes, so it is attested only here: it is checked against the
// name the signed trail recorded at that position, so names are attested exactly when the trail
// verifies. It returns ErrNotVerified wrapping the first finding when any is found.
func VerifyRedactions(in RedactionVerifyInput) (RedactionReport, error) {
	rep := RedactionReport{Records: len(in.Trail)}
	add := func(k RedactionFindingKind, run, format string, a ...any) {
		rep.Findings = append(rep.Findings, RedactionFinding{Kind: k, RunID: run, Detail: fmt.Sprintf(format, a...)})
	}
	recs, derr := decodeRedactionLeaves(in.Trail)
	if derr != nil {
		add(FindingMalformed, "", "%v", derr)
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
	if derr != nil {
		return rep, notVerified("audit: redactions: %s: %s", rep.Findings[0].Kind, rep.Findings[0].Detail)
	}

	type key struct {
		run string
		pos int
	}
	listed := map[key]RedactedRef{}
	open := map[string]int{} // run+plan digest -> begins not yet ended
	for _, r := range recs {
		id := r.RunID + "\x00" + r.PlanDigest
		switch r.Phase {
		case RedactionBegin:
			rep.Redactions++
			open[id]++
			if r.Uncovered {
				add(FindingUncovered, r.RunID, "the redaction of plan %.12s ran with no covering signed journal head (ticket %s)", r.PlanDigest, r.Ticket)
			}
			for _, ref := range r.Records {
				listed[key{r.RunID, ref.Position}] = ref
			}
		case RedactionEnd:
			if open[id] > 0 {
				open[id]--
			}
			if r.Error != "" {
				add(FindingIncomplete, r.RunID, "the redaction of plan %.12s stopped with an error: %s", r.PlanDigest, r.Error)
			}
		}
	}
	for _, r := range recs {
		id := r.RunID + "\x00" + r.PlanDigest
		if r.Phase == RedactionBegin && open[id] > 0 {
			add(FindingIncomplete, r.RunID, "the redaction of plan %.12s has a begin record and no end: an unfinished pass (run the same plan again)", r.PlanDigest)
			open[id] = 0
		}
	}
	for run, journal := range in.Journals {
		for i, rec := range journal {
			ref, isListed := listed[key{run, i}]
			if rec.Redacted {
				leaf, _ := agent.TombstoneLeafHash(rec.Raw())
				switch {
				case !isListed:
					add(FindingUnaccounted, run, "record %d (%q) is a tombstone and no trail record names it: a redaction made outside the tool", i, rec.Name)
				case ref.Name != rec.Name:
					add(FindingNameMismatch, run, "record %d: the tombstone says %q and the trail recorded %q", i, rec.Name, ref.Name)
				case hex.EncodeToString(leaf) != ref.LeafHash:
					add(FindingHashMismatch, run, "record %d (%q): the tombstone keeps leaf hash %x and the trail recorded %s", i, rec.Name, leaf, ref.LeafHash)
				}
				continue
			}
			if isListed {
				add(FindingNotErased, run, "record %d (%q) is listed as erased in the trail and still holds content", i, rec.Name)
			}
		}
	}
	if len(rep.Findings) > 0 {
		return rep, notVerified("audit: redactions: %d finding(s), first: %s: %s", len(rep.Findings), rep.Findings[0].Kind, rep.Findings[0].Detail)
	}
	return rep, nil
}

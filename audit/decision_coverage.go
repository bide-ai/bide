package audit

import (
	"fmt"
	"slices"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Coverage statuses of a recorded decision (DecisionCoverage.Status).
const (
	// CoverageCovered: a verified signed head over this journal includes the decision's record.
	CoverageCovered = "covered"
	// CoveragePending: no such head includes it yet, and neither bound has passed.
	CoveragePending = "pending"
	// CoverageOverdue: no such head includes it and a bound has passed (see Basis).
	CoverageOverdue = "overdue"
)

// The bases of an overdue decision (DecisionCoverage.Basis).
const (
	// BasisRecords: the journal holds at least CoverageBound.MaxRecords records after the decision
	// and none of the heads covers it. It compares journal positions with head sizes only, so no
	// clock is in it.
	BasisRecords = "records"
	// BasisAge: the decision was recorded more than CoverageBound.MaxAge (plus Tolerance) before
	// CoverageBound.Now. It compares the recording process's clock (the decision's unsigned
	// recorded_at) with the checker's, so it is the weaker basis: clocks across processes skew.
	BasisAge = "age"
)

// CoverageBound is how long a recorded decision may stay outside every signed tree head before
// CheckDecisionCoverage reports it overdue. A zero field is a bound not set; with none set a
// decision is pending or covered, never overdue.
//
// MaxRecords is the journal's growth past the decision, in records, that its signer is allowed
// before a head must cover the decision. It is the bound that needs no clock, and it is only
// reached when the journal grows. A decision on a run that nobody writes to again, a rejection,
// say, never grows the journal, so MaxAge is what finds it.
type CoverageBound struct {
	MaxRecords int
	// MaxAge, with Now, flags a decision still uncovered MaxAge+Tolerance after its recorded_at.
	// Without a Now it is not applied.
	MaxAge    time.Duration
	Now       time.Time
	Tolerance time.Duration // clock skew allowed between the recorder and the checker
}

// DecisionCoverage is one recorded decision and the signed head, if any, that covers it.
type DecisionCoverage struct {
	Step       string `json:"step"`
	Approver   string `json:"approver,omitempty"` // empty for a decision recorded without a signature (Approve)
	Index      int    `json:"index"`              // the record's 0-based position in the journal
	RecordedAt string `json:"recorded_at,omitempty"`
	Status     string `json:"status"`
	// Basis says what made an overdue decision overdue: BasisRecords, or BasisAge. Empty otherwise.
	Basis string `json:"basis,omitempty"`
	// CoveredBySize is the size of the smallest verified head that includes the record, 0 if none.
	CoveredBySize int `json:"covered_by_size,omitempty"`
	// Behind is how many records the journal holds after this one.
	Behind int `json:"behind"`
	// Note says what limits the finding: "no recorded time" for a record with no usable
	// recorded_at (an unsigned agent.Approve decision records none), which an age bound treats as
	// over the bound.
	Note string `json:"note,omitempty"`
}

// RejectedHead is a head CheckDecisionCoverage ignored, and why: it does not verify under the log
// key, or it is not a journal head of the run. It says nothing about the journal.
type RejectedHead struct {
	Size   int    `json:"size"`
	Reason string `json:"reason"`
}

// DivergentHead is a head the log key signed for this run that does not match the journal given:
// the journal is shorter than the head (a stale or truncated export), or its first Size records are
// not what the head signed (an export, or a journal, that differs from what was signed). It is
// evidence against the export, not noise: a decision deleted from the export looks exactly like
// this, so any such head fails the check.
type DivergentHead struct {
	Size   int    `json:"size"`
	Reason string `json:"reason"`
}

// CoverageReport is CheckDecisionCoverage's result.
type CoverageReport struct {
	RunID          string             `json:"run_id"`
	Records        int                `json:"records"`
	Heads          int                `json:"heads"`        // heads that verified and belong to this journal
	LargestHead    int                `json:"largest_head"` // the largest such head's size, 0 if none
	Decisions      []DecisionCoverage `json:"decisions"`
	RejectedHeads  []RejectedHead     `json:"rejected_heads,omitempty"`  // ignored: invalid, or not this run's
	DivergentHeads []DivergentHead    `json:"divergent_heads,omitempty"` // signed for this run, and not this journal's
}

// OK reports whether no decision is overdue and no signed head diverges from the journal given.
func (r CoverageReport) OK() bool {
	return len(r.DivergentHeads) == 0 &&
		!slices.ContainsFunc(r.Decisions, func(d DecisionCoverage) bool { return d.Status == CoverageOverdue })
}

// CheckDecisionCoverage reports, for each approval decision in recs (runID's journal, in order),
// whether a signed tree head covers it, and flags the ones no head has covered for longer than
// bound. It finds the gap that opens when a decision is written by a process other than the one
// that signs the journal (a CLI or an approval service writing the database while the application
// holds the log key): the decision is in the journal, and under no signed head until the signer
// next writes to the run, which for a rejection may never happen.
//
// A head counts only if it verifies under v, is a journal head of runID, and its root is the root
// of recs' own prefix of its size. A head that is not a journal head of runID, or does not verify,
// is ignored (RejectedHeads). A head the log key signed for runID that does not match recs, because
// recs is shorter than the head or differs from what it signed, is a DivergentHead and fails the
// check: the export is stale, truncated, or has had a record removed (re-export the journal; if it
// still diverges, the journal is not what was signed). A head of size n covers the records at
// positions 0 to n-1.
//
// "Covered" means a signed head includes the record, not that the record is authentic: the check
// ratifies what the journal holds, so verify-approvals is the check that a decision is genuine. The
// bounds are only as strong as the export's completeness, which divergent heads test: an export
// cut short shrinks every decision's Behind, and recorded_at is not signed. Heads are used by size, never by the order
// they are given in or by their timestamps: two processes anchoring one run can deliver a smaller
// head after a larger one, and both commit to prefixes of one journal.
//
// Overdue is decided by positions first (BasisRecords) and, when bound.MaxAge and bound.Now are
// set, by age (BasisAge), which compares two machines' clocks and is labeled as the weaker. A
// decision a head covers is covered however old it is. Halt resolutions are not decisions and are
// not checked.
//
// It returns an error wrapping agent.ErrConfig for a nil verifier, and no error for a journal with
// no decisions.
func CheckDecisionCoverage(runID string, recs []agent.Record, heads []SignedTreeHead, v Verifier, bound CoverageBound) (CoverageReport, error) {
	if v == nil {
		return CoverageReport{}, fmt.Errorf("audit: CheckDecisionCoverage: nil verifier: %w", agent.ErrConfig)
	}
	rep := CoverageReport{RunID: runID, Records: len(recs)}
	var sizes []int // sizes of the heads that count, ascending
	for _, h := range heads {
		switch {
		case h.Kind != TreeJournal:
			rep.RejectedHeads = append(rep.RejectedHeads, RejectedHead{Size: h.Size, Reason: fmt.Sprintf("tree head is a %q tree, not a journal", h.Kind)})
			continue
		case h.RunID != runID:
			rep.RejectedHeads = append(rep.RejectedHeads, RejectedHead{Size: h.Size, Reason: fmt.Sprintf("tree head is for run %q, not %q", h.RunID, runID)})
			continue
		}
		if err := h.Verify(v); err != nil {
			rep.RejectedHeads = append(rep.RejectedHeads, RejectedHead{Size: h.Size, Reason: fmt.Sprintf("signature: %v", err)})
			continue
		}
		// From here the head is the log's own, for this run: it must match the journal given.
		if _, err := journalPrefix(runID, recs, h.TreeHead); err != nil {
			rep.DivergentHeads = append(rep.DivergentHeads, DivergentHead{Size: h.Size, Reason: err.Error()})
			continue
		}
		sizes = append(sizes, h.Size)
	}
	slices.Sort(sizes)
	rep.Heads = len(sizes)
	if len(sizes) > 0 {
		rep.LargestHead = sizes[len(sizes)-1]
	}
	for i, r := range recs {
		if r.Kind != agent.StepApproval {
			continue
		}
		d := DecisionCoverage{Step: r.Name, Approver: r.Approver(), Index: i, RecordedAt: r.RecordedAt(), Behind: len(recs) - 1 - i}
		if _, err := agent.ParseTimestamp(d.RecordedAt); err != nil {
			d.Note = NoteNoRecordedTime
		}
		if k, _ := slices.BinarySearch(sizes, i+1); k < len(sizes) {
			d.CoveredBySize = sizes[k] // the smallest head of size at least i+1
		}
		switch {
		case d.CoveredBySize > 0:
			d.Status = CoverageCovered
		case bound.MaxRecords > 0 && d.Behind >= bound.MaxRecords:
			d.Status, d.Basis = CoverageOverdue, BasisRecords
		case overAge(d.RecordedAt, bound):
			d.Status, d.Basis = CoverageOverdue, BasisAge
		default:
			d.Status = CoveragePending
		}
		rep.Decisions = append(rep.Decisions, d)
	}
	return rep, nil
}

// NoteNoRecordedTime is DecisionCoverage.Note for a decision whose record has no usable recorded_at.
const NoteNoRecordedTime = "no recorded time"

// overAge reports whether recordedAt is more than bound.MaxAge+bound.Tolerance before bound.Now.
// Without an age or a Now there is no age bound, and nothing is over it. Under one, a decision with
// no usable recorded time (an unsigned agent.Approve decision records none, and recorded_at is not
// signed) cannot be shown to be inside it, so it is over.
func overAge(recordedAt string, b CoverageBound) bool {
	if b.MaxAge <= 0 || b.Now.IsZero() {
		return false
	}
	at, err := agent.ParseTimestamp(recordedAt)
	if err != nil {
		return true
	}
	return b.Now.Sub(at) > b.MaxAge+max(b.Tolerance, 0)
}

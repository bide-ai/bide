package audit

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bide-ai/bide/agent"
)

// A decision receipt is what an approver keeps of one decision they signed: the decision, as
// signed, and where the journal recorded it. It lets the approver (or an auditor they hand it to)
// show, without trusting the journal or the application that runs on it, that the decision was
// made and recorded, and notice when a later signed tree head of the journal leaves it out.
//
// A receipt holds no key material: the signature is a public value. It holds the call's arguments,
// as signed, so the receipt verifies on its own (Verify); an approver who does not want the
// arguments on disk keeps the receipt where they keep the journal.

// ReceiptFormat identifies a decision receipt.
const ReceiptFormat = "bide.decision-receipt.v1"

// DecisionReceipt is one signed approver decision, with the journal record that holds it.
type DecisionReceipt struct {
	Format    string          `json:"format"` // ReceiptFormat
	RunID     string          `json:"run_id"`
	ToolUseID string          `json:"tool_use_id"`
	ToolName  string          `json:"tool_name"`
	Args      json.RawMessage `json:"args"`        // the call's arguments, as the approver signed them
	ArgsSHA   string          `json:"args_sha256"` // SHA-256 of the canonical arguments, lowercase hex: what the signature binds
	Approver  string          `json:"approver"`
	Approved  bool            `json:"approved"`
	DecidedAt string          `json:"decided_at"` // the approver's clock, signed (agent.FormatTimestamp form)
	Alg       Alg             `json:"alg"`
	Signature []byte          `json:"signature"`
	// Record is the journal record's name, Index its position in the run's journal (0 is the
	// journal header), and RecordedAt when it was journaled, by the clock of the process that wrote
	// it (not signed by the approver).
	Record     string `json:"record"`
	Index      int    `json:"index"`
	RecordedAt string `json:"recorded_at"`
}

func (DecisionReceipt) artifactFormat() (string, string) { return "decision receipt", ReceiptFormat }

// ApprovalArgsDigest returns the SHA-256 of the canonical arguments of s, in lowercase hex: the
// digest a signature over agent.ApprovalDecisionBytes(s, ...) binds. It is read out of the decision
// bytes themselves, at the offset their layout fixes (after the domain tag and the three
// length-prefixed ids), so it is the digest signed, not a second computation of it.
func ApprovalArgsDigest(s agent.ApprovalSubject) string {
	b := agent.ApprovalDecisionBytes(s, "x", true, time.Unix(0, 0))
	off := len(agent.DecisionFormat) + 1 + 3*4 + len(s.RunID) + len(s.ToolUseID) + len(s.ToolName)
	return hex.EncodeToString(b[off : off+32])
}

// NewDecisionReceipt returns the receipt of decision d on call s, held by the journal record rec
// at position index of the run's journal. It refuses a record that is not that decision.
func NewDecisionReceipt(s agent.ApprovalSubject, d agent.Decision, rec agent.Record, index int) (DecisionReceipt, error) {
	r := DecisionReceipt{
		Format: ReceiptFormat, RunID: s.RunID, ToolUseID: s.ToolUseID, ToolName: s.ToolName, Args: bytes.Clone(s.Args),
		ArgsSHA: ApprovalArgsDigest(s), Approver: d.ApproverID, Approved: d.Approved, DecidedAt: agent.FormatTimestamp(d.DecidedAt),
		Alg: d.Alg, Signature: bytes.Clone(d.Signature), Record: rec.Name, Index: index, RecordedAt: rec.RecordedAt(),
	}
	if index < 0 || !r.isRecord(rec) {
		return DecisionReceipt{}, fmt.Errorf("audit: the journal record %q is not this decision: %w", rec.Name, agent.ErrConfig)
	}
	return r, nil
}

// isRecord reports whether rec is the journal record of the receipt's decision.
func (r DecisionReceipt) isRecord(rec agent.Record) bool {
	return rec.Kind == agent.StepApproval && rec.Name == r.Record && rec.ToolUseID == r.ToolUseID &&
		rec.Approver() == r.Approver && rec.Approved == r.Approved && rec.DecidedAt() == r.DecidedAt &&
		rec.ApproverAlg() == r.Alg && bytes.Equal(rec.Signature(), r.Signature)
}

// Verify checks the receipt on its own: it is of ReceiptFormat, its arguments hash to ArgsSHA, and
// Signature is a valid signature, by the approver's key v, over the decision bytes
// (agent.ApprovalDecisionBytes) of exactly this run, call, tool, arguments, approver, decision and
// time. It does not look at the journal. An error wraps ErrNotVerified, or ErrFormat.
func (r DecisionReceipt) Verify(v Verifier) error {
	if err := formatOf(r, r.Format); err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("audit: receipt: no approver key: %w", agent.ErrConfig)
	}
	at, err := agent.ParseTimestamp(r.DecidedAt)
	if err != nil {
		return fmt.Errorf("audit: receipt decided_at: %v: %w", err, ErrNotVerified)
	}
	s := agent.ApprovalSubject{RunID: r.RunID, ToolUseID: r.ToolUseID, ToolName: r.ToolName, Args: r.Args}
	if got := ApprovalArgsDigest(s); got != r.ArgsSHA {
		return fmt.Errorf("audit: receipt: the arguments hash to %s, not the recorded %s: %w", got, r.ArgsSHA, ErrNotVerified)
	}
	if r.Alg != v.Alg() {
		return fmt.Errorf("audit: receipt: signed under %q, the approver's key is %q: %w", r.Alg, v.Alg(), ErrNotVerified)
	}
	if !v.Verify(agent.ApprovalDecisionBytes(s, r.Approver, r.Approved, at), r.Signature) {
		return fmt.Errorf("audit: receipt: the signature does not verify for approver %q on this exact call: %w", r.Approver, ErrNotVerified)
	}
	return nil
}

// ReceiptVerdict is what CheckReceipt finds about one receipt.
type ReceiptVerdict string

// The verdicts. Only ReceiptDeleted and ReceiptNeverAnchored are failures.
const (
	// ReceiptAnchored: a signed tree head of the journal commits to the decision at its position.
	ReceiptAnchored ReceiptVerdict = "anchored"
	// ReceiptPending: the journal holds the decision and no signed head covers its position yet. The
	// application's next write to the run signs it into a head; until then this is expected.
	ReceiptPending ReceiptVerdict = "pending"
	// ReceiptDeleted: a signed head commits to a journal prefix that reaches the receipt's
	// position, and that prefix does not hold the decision there: the decision was removed or
	// replaced after it was recorded, and the head was signed over the journal without it.
	ReceiptDeleted ReceiptVerdict = "decision deleted after signing"
	// ReceiptNeverAnchored: the decision was recorded, yet no signed head commits to it although
	// one was signed after it (a head that stops short of its position, signed later than the skew
	// allows), or the journal does not hold it and no head covers its position.
	ReceiptNeverAnchored ReceiptVerdict = "decision never anchored"
	// ReceiptUnchecked: a head reaches the position but does not match the journal given, so
	// nothing can be said of the decision from it. A journal that does not match a signed head is
	// itself a failure, which the journal's own verification reports.
	ReceiptUnchecked ReceiptVerdict = "unchecked"
)

// Failed reports whether the verdict is a failure.
func (v ReceiptVerdict) Failed() bool { return v == ReceiptDeleted || v == ReceiptNeverAnchored }

// ReceiptFinding is the verdict on one receipt, with the reason.
type ReceiptFinding struct {
	Verdict ReceiptVerdict
	Detail  string
}

// DefaultReceiptSkew is how far a head's signing time may fall short of a receipt's recorded_at
// before CheckReceipt takes a head that omits the decision as signed after it. The two times come
// from different clocks (the process that journaled the decision, and the one that signed the head),
// and a head signs what the journal held when it was read, a moment before it is stamped.
const DefaultReceiptSkew = time.Minute

// CheckReceipt decides whether the signed tree heads of the run commit to the receipt's decision.
// journal is the run's journal (JournalExport.Journal), heads are heads of it signed by the log key
// logKey (a head that does not verify, or is of another run or tree, is ignored), and skew is
// DefaultReceiptSkew unless set.
//
// "Should contain" is positional. A journal only grows, and the decision is at Index. So a head
// whose size is above Index commits to a journal prefix that includes position Index, and that
// prefix must hold the decision there. CheckReceipt rebuilds the prefix's root from journal and
// compares it with the head's, so what it says about a head it says about a head that matches this
// journal. A head at or below Index says nothing of the decision, except through time: one signed
// at least skew after recorded_at, which stops short of it, was signed over a journal that lacked a
// decision that was already recorded.
func CheckReceipt(r DecisionReceipt, journal []agent.Record, heads []SignedTreeHead, logKey Verifier, skew time.Duration) ReceiptFinding {
	if skew <= 0 {
		skew = DefaultReceiptSkew
	}
	recordedAt, err := agent.ParseTimestamp(r.RecordedAt)
	if err != nil {
		return ReceiptFinding{ReceiptUnchecked, fmt.Sprintf("the receipt's recorded_at is not a timestamp: %v", err)}
	}
	if r.Index < 0 {
		return ReceiptFinding{ReceiptUnchecked, "the receipt's index is negative"}
	}
	holds := r.Index < len(journal) && r.isRecord(journal[r.Index])
	var late *SignedTreeHead
	unmatched := ""
	for i := range heads {
		h := heads[i]
		if h.Kind != TreeJournal || h.RunID != r.RunID || logKey == nil || h.Verify(logKey) != nil {
			continue
		}
		if h.Size <= r.Index {
			if h.TimestampNanos >= recordedAt.Add(skew).UnixNano() && late == nil {
				late = &heads[i]
			}
			continue
		}
		// The head reaches the decision's position: it must match this journal to say anything.
		if h.Size > len(journal) {
			unmatched = fmt.Sprintf("a head of size %d is longer than the journal given (%d records)", h.Size, len(journal))
			continue
		}
		th, err := journalHead(r.RunID, journal[:h.Size], 0)
		if err != nil || !bytes.Equal(th.Root, h.Root) {
			unmatched = fmt.Sprintf("a head of size %d does not match the journal's first %d records", h.Size, h.Size)
			continue
		}
		if holds {
			return ReceiptFinding{ReceiptAnchored, fmt.Sprintf("committed to by the head of size %d signed at %s", h.Size, time.Unix(0, h.TimestampNanos).UTC().Format(time.RFC3339))}
		}
		return ReceiptFinding{ReceiptDeleted, fmt.Sprintf("the head of size %d signed at %s commits to a journal whose position %d is not this decision (%q): it was removed or replaced after it was recorded at %s",
			h.Size, time.Unix(0, h.TimestampNanos).UTC().Format(time.RFC3339), r.Index, r.Record, r.RecordedAt)}
	}
	switch {
	case unmatched != "":
		return ReceiptFinding{ReceiptUnchecked, unmatched + ", so it says nothing of the decision"}
	case late != nil:
		return ReceiptFinding{ReceiptNeverAnchored, fmt.Sprintf("the head of size %d signed at %s, after the decision was recorded at %s, stops short of its position %d",
			late.Size, time.Unix(0, late.TimestampNanos).UTC().Format(time.RFC3339), r.RecordedAt, r.Index)}
	case !holds:
		return ReceiptFinding{ReceiptNeverAnchored, fmt.Sprintf("the journal does not hold the decision at position %d (%d records) and no signed head covers that position", r.Index, len(journal))}
	}
	return ReceiptFinding{ReceiptPending, "the journal holds the decision and no signed head covers its position yet; the application's next write to the run signs it into a head"}
}

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
const ReceiptFormat = "bide.audit.decision-receipt.v1"

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
	// LeafHash is the journal record's leaf hash (JournalLeafHash), lowercase hex: what a head's
	// tree holds at Index, and what a redaction tombstone keeps of the record.
	LeafHash string `json:"leaf_hash"`
	// Anchor is the proof that a signed head of the journal commits to the decision at Index: the
	// record's inclusion under that head. It is empty on a receipt just written and added by
	// AnchorReceipt (bide audit verify-receipts -update) once a signed head covers the decision.
	// Everything above it is the approver's claim: Record, Index, RecordedAt and LeafHash are not
	// signed, and nothing in a receipt without an Anchor shows the application ever journaled the
	// decision. With an Anchor it does, and only then can CheckReceipt say the decision was deleted.
	Anchor *ProofBundle `json:"anchor,omitempty"`
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
	lh, err := journalLeafHashes([]agent.Record{rec})
	if err != nil {
		return DecisionReceipt{}, err
	}
	r.LeafHash = hex.EncodeToString(lh[0])
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

// The verdicts. Each says only what the inputs prove. Without an Anchor in the receipt, nothing but
// the approver's word says the application ever journaled the decision, so a receipt the journal
// does not match is ReceiptUnmatched, an accusation of no one. ReceiptDeleted needs the proof.
const (
	// ReceiptAnchored: a signed tree head of the journal commits to the decision at its position.
	ReceiptAnchored ReceiptVerdict = "anchored"
	// ReceiptRedacted: as anchored, and a redaction has since replaced the record's bytes with a
	// tombstone that keeps its leaf hash. A legitimate redaction, not a deletion.
	ReceiptRedacted ReceiptVerdict = "redacted (anchored)"
	// ReceiptPending: the journal holds the decision and no signed head covers its position yet. The
	// application's next write to the run signs it into a head; until then this is expected.
	ReceiptPending ReceiptVerdict = "pending"
	// ReceiptDeleted: the receipt carries an anchoring proof (the decision's record under a signed
	// head A), and a signed head B that matches the journal given reaches the position and commits
	// to something else there. The log's signer signed A, with the decision, and B, without it. Only
	// a receipt with a valid Anchor gets this verdict.
	ReceiptDeleted ReceiptVerdict = "decision deleted after signing"
	// ReceiptUnmatched: the journal does not hold the decision at the receipt's position (or a signed
	// head that matches the journal commits to something else there), and the receipt carries no
	// anchoring proof. That is the approver's claim: it proves deletion only if the receipt is
	// trusted, and a receipt for a decision never submitted looks the same.
	ReceiptUnmatched ReceiptVerdict = "receipt not matched by the journal (the approver's claim)"
	// ReceiptUnchecked: a head reaches the position but does not match the journal given, or the
	// receipt's anchoring proof does not verify, so nothing can be said of the decision from it. A
	// journal that does not match a signed head is itself a failure, which the journal's own
	// verification reports.
	ReceiptUnchecked ReceiptVerdict = "unchecked"
)

// Failed reports whether the verdict is a failure: the journal does not show a decision that the
// receipt says is there (ReceiptDeleted, proven; ReceiptUnmatched, claimed), or nothing could be
// checked (ReceiptUnchecked).
func (v ReceiptVerdict) Failed() bool {
	return v == ReceiptDeleted || v == ReceiptUnmatched || v == ReceiptUnchecked
}

// ReceiptFinding is the verdict on one receipt, with the reason.
type ReceiptFinding struct {
	Verdict ReceiptVerdict
	Detail  string
}

// checkAnchor verifies the receipt's anchoring proof under the log key: the bundle verifies, is of
// this run, proves position Index, and its record is this decision. It returns the decision's leaf
// hash as the proof fixes it. ok is false, with the reason, when the receipt has no proof or it does
// not hold; present tells those apart.
func (r DecisionReceipt) checkAnchor(logKey Verifier) (leaf []byte, present bool, err error) {
	if r.Anchor == nil {
		return nil, false, nil
	}
	b := *r.Anchor
	if err := b.Verify(logKey); err != nil {
		return nil, true, err
	}
	if b.RunID != r.RunID || b.Inclusion.Index != r.Index {
		return nil, true, fmt.Errorf("the anchoring proof is for run %q position %d, not run %q position %d", b.RunID, b.Inclusion.Index, r.RunID, r.Index)
	}
	rec, err := b.Record()
	if err != nil {
		return nil, true, err
	}
	if !r.isRecord(rec) {
		return nil, true, fmt.Errorf("the anchoring proof's record is not the receipt's decision")
	}
	return JournalLeafHash(b.RecordBytes), true, nil
}

// AnchorReceipt returns r with its anchoring proof: the inclusion of the decision's record under the
// smallest signed head of heads that reaches its position and matches journal. It fails, with
// ErrNotVerified, if the journal does not hold the decision at the position under such a head (a
// proof is made only of what holds), and if the record was redacted (its bytes are gone).
func AnchorReceipt(r DecisionReceipt, journal []agent.Record, heads []SignedTreeHead, logKey Verifier) (DecisionReceipt, error) {
	var best *SignedTreeHead
	for i := range heads {
		h := heads[i]
		if h.Kind != TreeJournal || h.RunID != r.RunID || logKey == nil || h.Verify(logKey) != nil || h.Size <= r.Index || h.Size > len(journal) {
			continue
		}
		if _, err := journalPrefix(r.RunID, journal, h.TreeHead); err != nil {
			continue
		}
		if best == nil || h.Size < best.Size {
			best = &heads[i]
		}
	}
	if best == nil {
		return r, fmt.Errorf("audit: no signed head of the journal reaches position %d: %w", r.Index, ErrNotVerified)
	}
	b, err := proveIn(r.RunID, journal, r.Index, *best)
	if err != nil {
		return r, fmt.Errorf("audit: %v: %w", err, ErrNotVerified)
	}
	out := r
	out.Anchor = &b
	if _, _, err := out.checkAnchor(logKey); err != nil {
		return r, fmt.Errorf("audit: the journal's record at position %d is not this decision: %v: %w", r.Index, err, ErrNotVerified)
	}
	return out, nil
}

// CheckReceipt decides whether the signed tree heads of the run commit to the receipt's decision.
// journal is the run's journal (JournalExport.Journal) and heads are heads of it signed by the log
// key logKey (a head that does not verify, or is of another run or tree, is ignored).
//
// "Should contain" is positional, and never by time: the clocks of the approver's machine and the
// application differ, and heads reach an anchor out of order. A journal only grows, and the
// decision is at the receipt's Index. So a head whose size is above Index commits to a journal
// prefix that includes position Index, and that prefix must hold the decision there. CheckReceipt
// rebuilds the prefix's leaves and root from journal and compares the root with the head's, so what
// it says about a head it says about a head that matches this journal; the decision is held when the
// leaf at Index is the decision's leaf hash (so a redacted record, whose tombstone keeps it, still
// holds). A head at or below Index says nothing of the decision. recorded_at is display only.
//
// What a mismatch means depends on the receipt. Its Record, Index and LeafHash are the approver's
// claim, and without an Anchor nothing shows the application ever journaled the decision, so a
// mismatch is ReceiptUnmatched. With a valid Anchor (the decision under a signed head A) a head B that
// matches the journal and commits to something else at Index contradicts A: the log's signer signed
// both, and that is ReceiptDeleted. B need not be later than A in time or arrival: B is checked
// against the journal, not against A.
func CheckReceipt(r DecisionReceipt, journal []agent.Record, heads []SignedTreeHead, logKey Verifier) ReceiptFinding {
	if r.Index < 0 {
		return ReceiptFinding{ReceiptUnchecked, "the receipt's index is negative"}
	}
	want, proven, err := r.checkAnchor(logKey)
	if err != nil {
		return ReceiptFinding{ReceiptUnchecked, "the receipt's anchoring proof does not hold: " + err.Error()}
	}
	if !proven {
		if want, err = hex.DecodeString(r.LeafHash); err != nil || len(want) != 32 {
			want = nil // no usable claim: fall back to the decision's own fields
		}
	}
	// holdsAt reports whether the record at journal[i], whose leaf hash is leaf, is the decision.
	holdsAt := func(leaf []byte, i int) bool {
		if want != nil {
			return bytes.Equal(leaf, want)
		}
		return r.isRecord(journal[i])
	}
	redacted := func(i int) bool { return i < len(journal) && journal[i].Redacted }
	unmatched := ""
	for _, h := range heads {
		if h.Kind != TreeJournal || h.RunID != r.RunID || logKey == nil || h.Verify(logKey) != nil {
			continue
		}
		if h.Size <= r.Index {
			continue
		}
		// The head reaches the decision's position: it must match this journal to say anything.
		if h.Size > len(journal) {
			unmatched = fmt.Sprintf("a head of size %d is longer than the journal given (%d records)", h.Size, len(journal))
			continue
		}
		leaves, err := journalLeafHashes(journal[:h.Size])
		if err != nil || !bytes.Equal(hashRoot(leaves), h.Root) {
			unmatched = fmt.Sprintf("a head of size %d does not match the journal's first %d records", h.Size, h.Size)
			continue
		}
		at := time.Unix(0, h.TimestampNanos).UTC().Format(time.RFC3339)
		switch {
		case holdsAt(leaves[r.Index], r.Index) && redacted(r.Index):
			return ReceiptFinding{ReceiptRedacted, fmt.Sprintf("committed to by the head of size %d signed at %s; a redaction has since replaced the record with a tombstone that keeps its leaf hash", h.Size, at)}
		case holdsAt(leaves[r.Index], r.Index):
			return ReceiptFinding{ReceiptAnchored, fmt.Sprintf("committed to by the head of size %d signed at %s", h.Size, at)}
		case proven:
			return ReceiptFinding{ReceiptDeleted, fmt.Sprintf("the receipt's anchoring proof puts the decision at position %d under the signed head of size %d; the signed head of size %d (at %s) commits to a journal whose position %d is something else: it was removed or replaced after it was recorded",
				r.Index, r.Anchor.STH.Size, h.Size, at, r.Index)}
		default:
			return ReceiptFinding{ReceiptUnmatched, fmt.Sprintf("the signed head of size %d (at %s) commits to a journal whose position %d is not this decision (%q). The receipt carries no anchoring proof, so this proves a deletion only if the approver's receipt is trusted; nothing shows the application journaled the decision. Add a proof when the decision is anchored (bide audit verify-receipts -update)",
				h.Size, at, r.Index, r.Record)}
		}
	}
	held := false
	if r.Index < len(journal) {
		if one, err := journalLeafHashes(journal[r.Index : r.Index+1]); err == nil {
			held = holdsAt(one[0], r.Index)
		}
	}
	switch {
	case unmatched != "":
		return ReceiptFinding{ReceiptUnchecked, unmatched + ", so it says nothing of the decision"}
	case !held:
		return ReceiptFinding{ReceiptUnmatched, fmt.Sprintf("the journal does not hold the decision at position %d (%d records) and no signed head reaches that position. Without an anchoring proof in the receipt this is the approver's claim, not evidence of a deletion", r.Index, len(journal))}
	}
	return ReceiptFinding{ReceiptPending, "the journal holds the decision and no signed head covers its position yet; the application's next write to the run signs it into a head"}
}

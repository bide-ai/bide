package audit

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// A Signed Tree Head (STH) is the anchoring artifact for the RFC 6962 Merkle commitment,
// modeled on Certificate Transparency. It binds the Merkle Root to the tree Size and a
// Timestamp and signs the bundle, so the signature commits to WHICH tree and WHEN, not
// just to root bytes that could be replayed across sizes. Publish/anchor the STH; then
// inclusion and consistency proofs are checked against its (verified) Root.
//
// One key signs several kinds of tree for a run: the journal itself, the absence key sets
// projected from it (tool uses, used policies), and the event stream. The signed encoding
// therefore also commits to the tree's Kind and to the RunID it is about, and a key-set tree
// commits to the journal tree it was projected from (Journal). Every verifier requires the
// kind it expects, so a head signed for one tree never verifies as another, and the run a
// proof reports is the run the signer committed to.

// Tree kinds, committed in every signed tree head.
const (
	// TreeJournal is a run's journal: one leaf per record, in persisted order (NewTreeHead).
	TreeJournal = "journal"
	// TreeEvents is a run's semantic event stream (EventLog.TreeHead).
	TreeEvents = "events"
	// TreeToolUse is the sorted, distinct tool-use keys of a run's completed tool calls
	// (ToolUseKeys), the tree tool-call absence proofs verify against.
	TreeToolUse = absenceKindPrefix + "tool-use"
	// TreePolicyUsed is the sorted, distinct policy digests a run's governed actions used
	// (PolicyUsedKeys), the tree used-policy absence proofs and run certificates verify against.
	TreePolicyUsed = absenceKindPrefix + "policy-used"
)

// absenceKindPrefix starts every absence key-set kind, built in or caller-defined, so a key-set
// tree can never share a kind with a journal or event tree.
const absenceKindPrefix = "absence/"

// sthTag domain-separates the signed tree head encoding from every other message this package
// signs. It names the encoding version: v2 added Kind, RunID, and Journal, v3 marked journal roots
// over the journal encoding, v4 marked roots over tagged, salted leaves (see merkle.go), and v5
// signs the scheme (Alg) with the head, so a head's signature is never read under another scheme.
const sthTag = STHFormat + "\x00"

// TreeRef names one tree of a run by its size and root.
type TreeRef struct {
	Size int    `json:"size"` // number of leaves
	Root []byte `json:"root"` // RFC 6962 Merkle root over them
}

// TreeHead is a commitment to one tree of a run at a point in time.
type TreeHead struct {
	Kind  string `json:"kind"`   // which tree: TreeJournal, TreeEvents, or an absence kind
	RunID string `json:"run_id"` // the run the tree is about
	Size  int    `json:"size"`   // number of leaves committed
	Root  []byte `json:"root"`   // RFC 6962 Merkle root over those leaves
	// TimestampNanos is when the head was signed, in Unix nanoseconds (time.Now().UnixNano()); see
	// CheckTimestamp.
	TimestampNanos int64 `json:"timestamp_nanos"`
	// Journal is set on an absence key-set tree: the journal tree (of the same run) whose
	// records the key set was projected from. It is nil on a journal or event tree.
	Journal *TreeRef `json:"journal,omitempty"`
}

// canonical is the deterministic, domain-separated, length-prefixed encoding an STH under scheme
// alg signs, so two different (scheme, TreeHead) pairs can never share an encoding:
//
//	"bide.audit.sth.v5\x00" || field(alg) || field(kind) || field(run_id) || u64(size) ||
//	field(root) || u64(timestamp_nanos) || (0x00 | 0x01 || u64(journal.size) || field(journal.root))
//
// where field(x) is u64(len(x)) || x and u64 is 8 bytes big-endian.
func (th TreeHead) canonical(alg Alg) []byte {
	b := append([]byte(nil), sthTag...)
	b = appendField(b, []byte(alg))
	b = appendField(b, []byte(th.Kind))
	b = appendField(b, []byte(th.RunID))
	b = binary.BigEndian.AppendUint64(b, uint64(th.Size))
	b = appendField(b, th.Root)
	b = binary.BigEndian.AppendUint64(b, uint64(th.TimestampNanos))
	if th.Journal == nil {
		return append(b, 0)
	}
	b = append(b, 1)
	b = binary.BigEndian.AppendUint64(b, uint64(th.Journal.Size))
	return appendField(b, th.Journal.Root)
}

// appendField appends a uint64 big-endian length prefix and then f.
func appendField(b, f []byte) []byte {
	b = binary.BigEndian.AppendUint64(b, uint64(len(f)))
	return append(b, f...)
}

// wellFormed reports whether th can be a signed commitment at all: a known shape of kind, no
// negative sizes, and a Journal reference exactly on absence key-set trees.
func (th TreeHead) wellFormed() bool {
	if th.Size < 0 {
		return false
	}
	switch {
	case th.Kind == TreeJournal || th.Kind == TreeEvents:
		return th.Journal == nil
	case isAbsenceKind(th.Kind):
		return th.Journal != nil && th.Journal.Size >= 0
	default:
		return false
	}
}

func isAbsenceKind(kind string) bool {
	return strings.HasPrefix(kind, absenceKindPrefix) && len(kind) > len(absenceKindPrefix)
}

// SameTree reports whether th and o commit to the same tree: the same kind, run, size, root,
// and (for a key-set tree) source journal. Timestamps may differ.
func (th TreeHead) SameTree(o TreeHead) bool {
	if th.Kind != o.Kind || th.RunID != o.RunID || th.Size != o.Size || !bytes.Equal(th.Root, o.Root) {
		return false
	}
	if (th.Journal == nil) != (o.Journal == nil) {
		return false
	}
	return th.Journal == nil || (th.Journal.Size == o.Journal.Size && bytes.Equal(th.Journal.Root, o.Journal.Root))
}

// SignedTreeHead is a TreeHead with a signature over its canonical encoding under the scheme Alg
// names. The scheme is part of the signed bytes.
type SignedTreeHead struct {
	Format   string `json:"format"` // STHFormat
	TreeHead        // the commitment being signed
	// Alg names the signature scheme (see signing.go). It is signed with the head.
	Alg       Alg    `json:"alg"`
	Signature []byte `json:"signature"` // signature over TreeHead.canonical(Alg) under Alg
}

// NewTreeHead builds a TreeHead committing to runID's journal at the given timestamp.
func NewTreeHead(ctx context.Context, store *agent.Journal, runID string, timestamp int64) (TreeHead, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return TreeHead{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	return journalHead(runID, recs, timestamp)
}

// journalHead is the journal TreeHead over recs (the whole slice) for runID.
func journalHead(runID string, recs []agent.Record, timestamp int64) (TreeHead, error) {
	leaves, err := journalLeafHashes(recs)
	if err != nil {
		return TreeHead{}, err
	}
	return TreeHead{Kind: TreeJournal, RunID: runID, Size: len(recs), Root: hashRoot(leaves), TimestampNanos: timestamp}, nil
}

// journalPrefix returns the first th.Size records of recs after confirming they are the journal
// th commits to: th is a journal head of runID and those records hash to its root.
func journalPrefix(runID string, recs []agent.Record, th TreeHead) ([]agent.Record, error) {
	if th.Kind != TreeJournal {
		return nil, fmt.Errorf("audit: tree head is a %q tree, not a journal", th.Kind)
	}
	if th.RunID != runID {
		return nil, fmt.Errorf("audit: tree head is for run %q, not %q", th.RunID, runID)
	}
	if th.Size < 0 || th.Size > len(recs) {
		return nil, fmt.Errorf("audit: tree head size %d out of range for run %s (%d records)", th.Size, runID, len(recs))
	}
	leaves, err := journalLeafHashes(recs[:th.Size])
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(th.Root, hashRoot(leaves)) {
		return nil, fmt.Errorf("audit: tree head root does not match run %s at size %d (wrong head or history diverged)", runID, th.Size)
	}
	return recs[:th.Size], nil
}

// SignTreeHead signs th under s's scheme. Anchor the result out-of-band (this is what makes the
// journal tamper-evident against later rewrites). Build th with NewTreeHead, NewAbsenceTreeHead, or
// EventLog.TreeHead. It refuses a head of an unknown kind or shape, which no verifier accepts, and a
// signer without a usable key.
func SignTreeHead(th TreeHead, s Signer) (SignedTreeHead, error) {
	if err := checkSigner(s); err != nil {
		return SignedTreeHead{}, err
	}
	if !th.wellFormed() {
		return SignedTreeHead{}, fmt.Errorf("audit: sign tree head: a %q head of size %d is not a tree head any verifier reads: %w", th.Kind, th.Size, agent.ErrConfig)
	}
	sig, err := s.Sign(th.canonical(s.Alg()))
	if err != nil {
		return SignedTreeHead{}, fmt.Errorf("audit: sign tree head: %w", err)
	}
	return SignedTreeHead{Format: STHFormat, TreeHead: th, Alg: s.Alg(), Signature: sig}, nil
}

// Verify returns nil if the STH's signature is valid under v: the head is of format STHFormat, of a
// known kind and shape, signed under v's scheme, and its signature over its canonical encoding
// (scheme included) verifies. Any change to any field invalidates it. It checks authenticity
// only: a caller that needs a particular tree also checks Kind and RunID (the bundle verifiers in
// this package do), and a caller that relies on when the head was signed applies CheckTimestamp
// (EvidencePackage.Verify and bide-audit do). The error wraps ErrFormat, ErrMalformed, or
// ErrNotVerified.
func (sth SignedTreeHead) Verify(v Verifier) error {
	if err := checkVerifier(v); err != nil {
		return err
	}
	if err := formatOf(sth, sth.Format); err != nil {
		return err
	}
	if !sth.wellFormed() {
		return fmt.Errorf("audit: a %q tree head of size %d is not a shape any head has: %w", sth.Kind, sth.Size, ErrMalformed)
	}
	if sth.Alg != v.Alg() {
		return notVerified("audit: the %s head of run %q is signed under %q, not the verifier's %q", sth.Kind, sth.RunID, sth.Alg, v.Alg())
	}
	if !v.Verify(sth.canonical(sth.Alg), sth.Signature) {
		return notVerified("audit: the %s head of run %q does not verify under this key", sth.Kind, sth.RunID)
	}
	return nil
}

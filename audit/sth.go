package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
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

// sthTag domain-separates the signed tree head encoding from every other message this
// package signs. It names the encoding version: v2 added Kind, RunID, and Journal, v3 marks
// journal roots over the journal encoding (agent.EncodeRecord), which does not HTML-escape, and v4
// marks roots over tagged leaves, with each journal record salted (see merkle.go), so heads of
// different versions over the same journal differ by version rather than read as a fork.
const sthTag = "bide.audit.sth.v4\x00"

// TreeRef names one tree of a run by its size and root.
type TreeRef struct {
	Size int    `json:"size"` // number of leaves
	Root []byte `json:"root"` // RFC 6962 Merkle root over them
}

// TreeHead is a commitment to one tree of a run at a point in time.
type TreeHead struct {
	Kind      string `json:"kind"`      // which tree: TreeJournal, TreeEvents, or an absence kind
	RunID     string `json:"run_id"`    // the run the tree is about
	Size      int    `json:"size"`      // number of leaves committed
	Root      []byte `json:"root"`      // RFC 6962 Merkle root over those leaves
	Timestamp int64  `json:"timestamp"` // caller-supplied (e.g. time.Now().UnixNano())
	// Journal is set on an absence key-set tree: the journal tree (of the same run) whose
	// records the key set was projected from. It is nil on a journal or event tree.
	Journal *TreeRef `json:"journal,omitempty"`
}

// canonical is the deterministic, domain-separated, length-prefixed encoding signed by an
// STH, so two different TreeHeads can never share an encoding.
func (th TreeHead) canonical() []byte {
	b := append([]byte(nil), sthTag...)
	b = appendField(b, []byte(th.Kind))
	b = appendField(b, []byte(th.RunID))
	b = binary.BigEndian.AppendUint64(b, uint64(th.Size))
	b = appendField(b, th.Root)
	b = binary.BigEndian.AppendUint64(b, uint64(th.Timestamp))
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

// SignedTreeHead is a TreeHead with a signature over its canonical encoding.
type SignedTreeHead struct {
	TreeHead         // the commitment being signed
	Signature []byte `json:"signature"` // signature over TreeHead.canonical() under the scheme named by Alg
	// Alg names the signature scheme (see signing.go). Empty means "ed25519", so heads produced
	// by the ed25519-only SignTreeHead / Verify path stay compact. Agility-aware producers set
	// it explicitly (SignTreeHeadWith).
	Alg string `json:"alg,omitempty"`
}

// NewTreeHead builds a TreeHead committing to runID's journal at the given timestamp.
func NewTreeHead(ctx context.Context, store agent.Durable, runID string, timestamp int64) (TreeHead, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return TreeHead{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	return journalHead(runID, recs, timestamp)
}

// journalHead is the journal TreeHead over recs (the whole slice) for runID.
func journalHead(runID string, recs []agent.Record, timestamp int64) (TreeHead, error) {
	leaves, err := canonicalLeaves(recs)
	if err != nil {
		return TreeHead{}, err
	}
	return TreeHead{Kind: TreeJournal, RunID: runID, Size: len(recs), Root: merkleRoot(leaves), Timestamp: timestamp}, nil
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
	leaves, err := canonicalLeaves(recs[:th.Size])
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(th.Root, merkleRoot(leaves)) {
		return nil, fmt.Errorf("audit: tree head root does not match run %s at size %d (wrong head or history diverged)", runID, th.Size)
	}
	return recs[:th.Size], nil
}

// SignTreeHead signs a TreeHead with an ed25519 key. Anchor the result out-of-band (this is
// what makes the journal tamper-evident against later rewrites). Build th with NewTreeHead,
// NewAbsenceTreeHead, or EventLog.TreeHead: a head of an unknown kind never verifies.
func SignTreeHead(th TreeHead, priv ed25519.PrivateKey) SignedTreeHead {
	return SignedTreeHead{TreeHead: th, Signature: ed25519.Sign(priv, th.canonical())}
}

// Verify reports whether the STH's signature is valid under pub. Any change to any field
// invalidates it. This is the ed25519 fast path; it accepts an STH with Alg empty or
// "ed25519" and rejects any other scheme (use VerifyWith for those). It checks authenticity
// only: a caller that needs a particular tree also checks Kind and RunID (the bundle
// verifiers in this package do).
func (sth SignedTreeHead) Verify(pub ed25519.PublicKey) bool {
	if sth.Alg != "" && sth.Alg != AlgEd25519 {
		return false
	}
	return sth.VerifyWith(Ed25519Verifier{Pub: pub})
}

// SignTreeHeadWith signs a TreeHead under any scheme (ed25519, ML-DSA, or the hybrid of both),
// tagging the result with the signer's algorithm. Anchor the result out-of-band as usual.
func SignTreeHeadWith(th TreeHead, s Signer) (SignedTreeHead, error) {
	sig, err := s.Sign(th.canonical())
	if err != nil {
		return SignedTreeHead{}, err
	}
	return SignedTreeHead{TreeHead: th, Signature: sig, Alg: s.Alg()}, nil
}

// VerifyWith reports whether the STH's signature is valid under v, requiring the STH's algorithm
// to match the verifier's (an empty Alg is treated as ed25519). Use this for ML-DSA or hybrid
// STHs; Verify remains the ed25519-only convenience.
func (sth SignedTreeHead) VerifyWith(v Verifier) bool {
	alg := sth.Alg
	if alg == "" {
		alg = AlgEd25519
	}
	if v == nil || alg != v.Alg() || !sth.wellFormed() {
		return false
	}
	return v.Verify(sth.canonical(), sth.Signature)
}

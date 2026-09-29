package audit

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"sort"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// Absence proofs answer the negative question an inclusion proof cannot: prove a thing did NOT
// happen (no charge was made, no approval for this ID was recorded). The inclusion/consistency
// tree commits the journal in CHRONOLOGICAL order, which cannot show non-membership. So absence
// is proven over a second commitment: an RFC 6962 Merkle tree over the SORTED, DISTINCT keys
// present in the run. A key is absent iff the two committed keys that bracket where it would
// sort are ADJACENT (consecutive indices): sorted order + adjacency leaves no room for it.
//
// Technique adapted from the sibling merkle-strata module (MIT, same author), reimplemented
// dependency-free on our existing RFC 6962 inclusion machinery. One deliberate correction:
// merkle-strata's absence verifier checks left < missing < right and inclusion, but NOT that
// the two neighbors are adjacent, so a prover could bracket with non-consecutive leaves and
// hide a present key between them. Because our Inclusion proof carries Index and Size, we
// verify Left.Index + 1 == Right.Index (and the boundary cases), closing that gap.
//
// Domain separation: a run has several key sets (tool uses, used policies) and a journal, all
// signed under one key. Each key set is a KeySet with its own tree Kind and key Prefix, and its
// signed tree head commits to that Kind, to the run, and to the journal tree it was projected
// from. A verifier names the KeySet it expects: the head must be of that kind and the key must
// carry that prefix, so a tool-use head cannot prove a policy absent and a journal head cannot
// prove anything absent.
//
// Trust model: absence is "absent from the key set the signer projected from this journal
// tree." The key set is a deterministic projection of the journal, so an auditor holding the
// journal recomputes it (ProveAbsentBundle does) before trusting any absence proof against it.
// Combined with the journal's own append-only anchoring, that means absent from the real
// history up to the committed journal size, not merely from a set the prover chose.

// KeyFunc extracts an absence key from a record, returning false to exclude the record from the
// key set. Absence is proven over the sorted set of distinct keys the KeyFunc yields.
type KeyFunc func(agent.Record) (string, bool)

// KeySet names one absence key set: the tree Kind its signed heads commit to, the Prefix every
// key it yields starts with, and the KeyFunc that projects a journal onto it. Kind must start
// with "absence/" and be distinct per key set; Prefix must be distinct per key set.
type KeySet struct {
	Kind   string  // tree kind committed in the signed head, e.g. TreeToolUse
	Prefix string  // every key the set yields (and every key proven absent from it) starts with Prefix
	Key    KeyFunc // projects one journal record onto the set
}

// ToolUseKeys is the key set of completed tool calls, keyed "tooluse:<ToolUseID>". It proves "no
// tool call with this ID happened in the run."
var ToolUseKeys = KeySet{Kind: TreeToolUse, Prefix: toolUseKeyPrefix, Key: ToolUseKey}

// PolicyUsedKeys is the key set of policy digests exercised by governed actions, keyed
// "policy_used:<digest>" (see PolicyUsedKey).
var PolicyUsedKeys = KeySet{Kind: TreePolicyUsed, Prefix: policyUsedKeyPrefix, Key: PolicyUsedKey}

// KeySetForKey returns the built-in key set a key belongs to, by its prefix.
func KeySetForKey(key string) (KeySet, bool) {
	for _, s := range []KeySet{ToolUseKeys, PolicyUsedKeys} {
		if strings.HasPrefix(key, s.Prefix) {
			return s, true
		}
	}
	return KeySet{}, false
}

func (s KeySet) check() error {
	if !isAbsenceKind(s.Kind) || s.Prefix == "" || s.Key == nil {
		return fmt.Errorf("audit: key set needs a kind starting with %q, a key prefix, and a key func (got kind %q, prefix %q)", absenceKindPrefix, s.Kind, s.Prefix)
	}
	return nil
}

const toolUseKeyPrefix = "tooluse:"

// ToolUseKey is the KeyFunc of ToolUseKeys: completed tool calls, keyed by their ToolUseID.
func ToolUseKey(r agent.Record) (string, bool) {
	if r.Kind == agent.StepToolResult {
		return toolUseKeyPrefix + r.ToolUseID, true
	}
	return "", false
}

// absenceKeys returns the sorted, de-duplicated keys the key set yields over records.
func absenceKeys(records []agent.Record, set KeySet) []string {
	seen := map[string]struct{}{}
	keys := make([]string, 0, len(records))
	for _, r := range records {
		if k, ok := set.Key(r); ok {
			if _, dup := seen[k]; !dup {
				seen[k] = struct{}{}
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// keyLeaf is the leaf data of one absence key: the key leaf tag followed by the key.
func keyLeaf(key string) []byte { return tagged(keyLeafTag, []byte(key)) }

func keyLeaves(keys []string) [][]byte {
	leaves := make([][]byte, len(keys))
	for i, k := range keys {
		leaves[i] = keyLeaf(k) // leaf order == key order, so bracketing is sound
	}
	return leaves
}

// AbsenceRoot is the RFC 6962 Merkle root over the run's sorted, distinct keys in set. Anyone
// holding the journal recomputes it to confirm a signed key-set head reflects the run.
func AbsenceRoot(records []agent.Record, set KeySet) []byte {
	return merkleRoot(keyLeaves(absenceKeys(records, set)))
}

// Neighbor is one committed key adjacent to an absent key, with its inclusion proof.
type Neighbor struct {
	Key   string    `json:"key"`   // the committed key adjacent to the absent key
	Proof Inclusion `json:"proof"` // inclusion proof placing Key at its index in the committed key set
}

// Absence is a non-membership proof: Key is not among the Size committed keys, shown by the
// adjacent committed keys that bracket it. Left is nil when Key sorts before all keys; Right is
// nil when it sorts after all; both nil only for the empty key set.
//
// What it discloses: the absent key, the number of distinct keys in the set (Size), and, in
// plain text, the one or two committed keys adjacent to it in sort order with their indices,
// which place them in the sorted set. For ToolUseKeys that is the ID of up to two tool calls
// the run did make; for PolicyUsedKeys, up to two policy digests the run did use. This is
// inherent to a sorted-set absence proof: the verifier must see the neighbours to check they
// bracket the key. It does not disclose the neighbours' journal records (their tool names,
// arguments, or results), and each neighbour's audit path carries hashes of further keys, which
// are not salted: a holder of the proof who can guess a key of the set (a policy digest is
// public; a tool-use ID usually is not) can confirm it against those hashes.
type Absence struct {
	Key   string    `json:"key"`             // the key claimed absent from the committed set
	Size  int       `json:"size"`            // number of committed keys the proof is against
	Left  *Neighbor `json:"left,omitempty"`  // committed key just below Key in sort order; nil if Key sorts before all
	Right *Neighbor `json:"right,omitempty"` // committed key just above Key in sort order; nil if Key sorts after all
}

// ProveAbsent builds an absence proof for key over records in set, or errors if the key is
// actually present (you cannot prove absence of something that happened) or does not carry the
// set's prefix (it could never be present, so its absence would say nothing).
func ProveAbsent(records []agent.Record, set KeySet, key string) (Absence, error) {
	if err := set.check(); err != nil {
		return Absence{}, err
	}
	if !strings.HasPrefix(key, set.Prefix) {
		return Absence{}, fmt.Errorf("audit: key %q is not in the %s key set (keys start with %q)", key, set.Kind, set.Prefix)
	}
	keys := absenceKeys(records, set)
	leaves := keyLeaves(keys)
	idx := sort.SearchStrings(keys, key)
	if idx < len(keys) && keys[idx] == key {
		return Absence{}, fmt.Errorf("audit: cannot prove absence: key %q is present", key)
	}
	proof := Absence{Key: key, Size: len(keys)}
	if idx > 0 {
		li := idx - 1
		proof.Left = &Neighbor{Key: keys[li], Proof: Inclusion{Index: li, Size: len(keys), Path: auditPath(li, leaves)}}
	}
	if idx < len(keys) {
		proof.Right = &Neighbor{Key: keys[idx], Proof: Inclusion{Index: idx, Size: len(keys), Path: auditPath(idx, leaves)}}
	}
	return proof, nil
}

// VerifyAbsence reports whether proof shows Key is absent from the key set committed by root:
// each named neighbor is included at its index, they bracket Key in sort order, and they are
// ADJACENT (or Key sits before the first / after the last / the set is empty). Adjacency is the
// load-bearing check: without it, bracketing alone does not preclude Key being present between
// two non-consecutive neighbors. It checks the proof against a bare root; AbsenceBundle.Verify
// is the form that also authenticates the root and its key set.
func VerifyAbsence(root []byte, proof Absence) (bool, error) {
	if proof.Size == 0 { // empty key set: everything is absent
		return proof.Left == nil && proof.Right == nil && bytes.Equal(root, merkleRoot(nil)), nil
	}
	if proof.Left != nil {
		if proof.Left.Proof.Size != proof.Size || proof.Left.Key >= proof.Key {
			return false, nil
		}
		if !verifyPath(root, keyLeaf(proof.Left.Key), proof.Left.Proof.Index, proof.Size, proof.Left.Proof.Path) {
			return false, nil
		}
	}
	if proof.Right != nil {
		if proof.Right.Proof.Size != proof.Size || proof.Key >= proof.Right.Key {
			return false, nil
		}
		if !verifyPath(root, keyLeaf(proof.Right.Key), proof.Right.Proof.Index, proof.Size, proof.Right.Proof.Path) {
			return false, nil
		}
	}
	switch {
	case proof.Left != nil && proof.Right != nil:
		return proof.Left.Proof.Index+1 == proof.Right.Proof.Index, nil // consecutive: no room between
	case proof.Left == nil && proof.Right != nil:
		return proof.Right.Proof.Index == 0, nil // Key sorts before all: right is the first key
	case proof.Right == nil && proof.Left != nil:
		return proof.Left.Proof.Index == proof.Size-1, nil // Key sorts after all: left is the last key
	default:
		return false, nil // both nil but non-empty set: malformed
	}
}

// AbsenceBundle is the portable, anchorable form of an absence proof, mirroring ProofBundle:
// the proof plus the signed tree head committing the key set it is proven against. The STH
// commits to the key set's kind, the run, and the journal tree the set was projected from.
type AbsenceBundle struct {
	RunID   string         `json:"run_id"`  // the run whose key set the proof is against; must equal STH.RunID
	Absence Absence        `json:"absence"` // the non-membership proof
	STH     SignedTreeHead `json:"sth"`     // the signed commitment to the key set the proof is proven against
}

// Verify reports whether the bundle authentically proves Absence.Key absent from the key set
// set of run RunID, under pub (obtained out of band). It checks the STH signature, that the STH
// is a head of set's kind for RunID (with its source journal named), that the key carries the
// set's prefix, that the proof is bound to the signed size, and non-membership under the signed
// root. The absence holds for the journal prefix STH.Journal names; confirm from the anchor log
// that it is the run's latest head before reading it as "never happened in the run."
func (b AbsenceBundle) Verify(pub ed25519.PublicKey, set KeySet) (bool, error) {
	if b.STH.Alg != "" && b.STH.Alg != AlgEd25519 {
		return false, nil
	}
	return b.VerifyWith(Ed25519Verifier{Pub: pub}, set)
}

// VerifyWith is the scheme-agnostic form of Verify.
func (b AbsenceBundle) VerifyWith(v Verifier, set KeySet) (bool, error) {
	if err := set.check(); err != nil {
		return false, err
	}
	if !b.STH.VerifyWith(v) {
		return false, nil
	}
	if b.STH.Kind != set.Kind || b.STH.RunID != b.RunID || b.STH.Journal == nil {
		return false, nil // a head of another tree, or another run
	}
	if !strings.HasPrefix(b.Absence.Key, set.Prefix) {
		return false, nil // the key is not one this set could contain
	}
	if b.Absence.Size != b.STH.Size {
		return false, nil
	}
	return VerifyAbsence(b.STH.Root, b.Absence)
}

// ProveAbsentBundle builds an anchorable absence proof bound to sth, a signed key-set head of
// set (see SignAbsenceRoot). records is the run's journal: its prefix of sth.Journal.Size records
// must hash to sth.Journal.Root, and the key set projected from that prefix must be the one sth
// signs (the fidelity guard: a stale, foreign, or wrong-kind STH is rejected).
func ProveAbsentBundle(records []agent.Record, set KeySet, key string, sth SignedTreeHead) (AbsenceBundle, error) {
	if err := set.check(); err != nil {
		return AbsenceBundle{}, err
	}
	if sth.Kind != set.Kind || sth.Journal == nil {
		return AbsenceBundle{}, fmt.Errorf("audit: STH is a %q tree, not a %s key set", sth.Kind, set.Kind)
	}
	journal, err := journalPrefix(sth.RunID, records, TreeHead{Kind: TreeJournal, RunID: sth.RunID, Size: sth.Journal.Size, Root: sth.Journal.Root})
	if err != nil {
		return AbsenceBundle{}, fmt.Errorf("audit: STH's source journal: %w", err)
	}
	if sth.Size != len(absenceKeys(journal, set)) || !bytes.Equal(sth.Root, AbsenceRoot(journal, set)) {
		return AbsenceBundle{}, fmt.Errorf("audit: STH does not commit to run %s's %s key set (wrong or stale STH)", sth.RunID, set.Kind)
	}
	proof, err := ProveAbsent(journal, set, key)
	if err != nil {
		return AbsenceBundle{}, err
	}
	return AbsenceBundle{RunID: sth.RunID, Absence: proof, STH: sth}, nil
}

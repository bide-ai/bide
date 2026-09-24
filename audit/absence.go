package audit

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"sort"

	agent "github.com/dayna/go-agents"
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
// Trust model: absence is "absent from the run committed by this key-set root." The key set is
// a deterministic projection of the journal, so an auditor holding the journal recomputes
// AbsenceRoot and confirms it equals the signed root before trusting any absence proof against
// it. Combined with the journal's own append-only anchoring, that means absent from the real
// history, not merely from a set the prover chose.

// KeyFunc extracts an absence key from a record, returning false to exclude the record from the
// key set. Absence is proven over the sorted set of distinct keys the KeyFunc yields.
type KeyFunc func(agent.Record) (string, bool)

// ToolUseKey is the default KeyFunc: completed tool calls, keyed by their ToolUseID. It lets
// you prove "no tool call with this ID happened in the run."
func ToolUseKey(r agent.Record) (string, bool) {
	if r.Kind == agent.StepToolResult {
		return "tooluse:" + r.ToolUseID, true
	}
	return "", false
}

// absenceKeys returns the sorted, de-duplicated keys the KeyFunc yields over records.
func absenceKeys(records []agent.Record, keyFn KeyFunc) []string {
	seen := map[string]struct{}{}
	keys := make([]string, 0, len(records))
	for _, r := range records {
		if k, ok := keyFn(r); ok {
			if _, dup := seen[k]; !dup {
				seen[k] = struct{}{}
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func keyLeaves(keys []string) [][]byte {
	leaves := make([][]byte, len(keys))
	for i, k := range keys {
		leaves[i] = []byte(k) // leaf order == key order, so bracketing is sound
	}
	return leaves
}

// AbsenceRoot is the RFC 6962 Merkle root over the run's sorted, distinct keys under keyFn.
// Sign it (SignTreeHead) and anchor it like any root; anyone holding the journal recomputes it
// to confirm the key set faithfully reflects the run.
func AbsenceRoot(records []agent.Record, keyFn KeyFunc) []byte {
	return merkleRoot(keyLeaves(absenceKeys(records, keyFn)))
}

// Neighbor is one committed key adjacent to an absent key, with its inclusion proof.
type Neighbor struct {
	Key   string    `json:"key"`
	Proof Inclusion `json:"proof"`
}

// Absence is a non-membership proof: Key is not among the Size committed keys, shown by the
// adjacent committed keys that bracket it. Left is nil when Key sorts before all keys; Right is
// nil when it sorts after all; both nil only for the empty key set.
type Absence struct {
	Key   string    `json:"key"`
	Size  int       `json:"size"`
	Left  *Neighbor `json:"left,omitempty"`
	Right *Neighbor `json:"right,omitempty"`
}

// ProveAbsent builds an absence proof for key over records under keyFn, or errors if the key is
// actually present (you cannot prove absence of something that happened).
func ProveAbsent(records []agent.Record, keyFn KeyFunc, key string) (Absence, error) {
	keys := absenceKeys(records, keyFn)
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
// two non-consecutive neighbors.
func VerifyAbsence(root []byte, proof Absence) (bool, error) {
	if proof.Size == 0 { // empty key set: everything is absent
		return proof.Left == nil && proof.Right == nil, nil
	}
	if proof.Left != nil {
		if proof.Left.Proof.Size != proof.Size || proof.Left.Key >= proof.Key {
			return false, nil
		}
		if !verifyPath(root, []byte(proof.Left.Key), proof.Left.Proof.Index, proof.Size, proof.Left.Proof.Path) {
			return false, nil
		}
	}
	if proof.Right != nil {
		if proof.Right.Proof.Size != proof.Size || proof.Key >= proof.Right.Key {
			return false, nil
		}
		if !verifyPath(root, []byte(proof.Right.Key), proof.Right.Proof.Index, proof.Size, proof.Right.Proof.Path) {
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
// the proof plus the signed tree head committing the key set it is proven against. Verify with
// an out-of-band public key; it checks the STH signature, binds the proof to the signed size,
// and verifies non-membership against the signed root.
type AbsenceBundle struct {
	RunID   string         `json:"run_id"`
	Absence Absence        `json:"absence"`
	STH     SignedTreeHead `json:"sth"`
}

// Verify reports whether the bundle authentically proves absence under pub.
func (b AbsenceBundle) Verify(pub ed25519.PublicKey) (bool, error) {
	if !b.STH.Verify(pub) {
		return false, nil
	}
	if b.Absence.Size != b.STH.Size {
		return false, nil
	}
	return VerifyAbsence(b.STH.Root, b.Absence)
}

// ProveAbsentBundle builds an anchorable absence proof bound to sth, whose Root must be the
// AbsenceRoot of records under keyFn (the fidelity guard: the STH must commit to THIS run's key
// set, recomputed here, so a stale or foreign STH is rejected).
func ProveAbsentBundle(records []agent.Record, keyFn KeyFunc, key string, runID string, sth SignedTreeHead) (AbsenceBundle, error) {
	if sth.Size != len(absenceKeys(records, keyFn)) || !bytes.Equal(sth.Root, AbsenceRoot(records, keyFn)) {
		return AbsenceBundle{}, fmt.Errorf("audit: STH does not commit to run %s's key set (wrong or stale STH)", runID)
	}
	proof, err := ProveAbsent(records, keyFn, key)
	if err != nil {
		return AbsenceBundle{}, err
	}
	return AbsenceBundle{RunID: runID, Absence: proof, STH: sth}, nil
}

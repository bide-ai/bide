package audit

import (
	"bytes"
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

// KeyFunc extracts the absence keys a record adds to a key set: none (nil) to exclude the record,
// or one or more (a model turn that requests several tool calls adds one key per call). Absence is
// proven over the sorted set of distinct keys the KeyFunc yields over the journal.
type KeyFunc func(agent.Record) []string

// KeySet names one absence key set: the tree Kind its signed heads commit to, the Prefix every
// key it yields starts with, and the KeyFunc that projects a journal onto it. Kind must start
// with "absence/" and be distinct per key set; Prefix must be distinct per key set.
type KeySet struct {
	Kind   string  // tree kind committed in the signed head, e.g. TreeToolUse
	Prefix string  // every key the set yields (and every key proven absent from it) starts with Prefix
	Key    KeyFunc // projects one journal record onto the set
}

// ToolUseKeys is the key set of the tool calls a run's model requested, keyed
// "tooluse:<ToolUseID>" (see ToolUseKey). It holds every call the run started, since the run
// starts only a call its model requested, so it proves "no tool call with this ID was requested,
// and so none happened, in the run": neither completed, nor started and left unresolved, nor
// failed in a saga.
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
		return fmt.Errorf("audit: key set needs a kind starting with %q, a key prefix, and a key func (got kind %q, prefix %q): %w", absenceKindPrefix, s.Kind, s.Prefix, agent.ErrConfig)
	}
	return nil
}

const toolUseKeyPrefix = "tooluse:"

// ToolUseKey is the KeyFunc of ToolUseKeys: every record that shows a call ran or may have run,
// keyed by its ToolUseID. The model turn that requested calls (StepModel) adds each call it
// requested, whether or not the call ran: a call can run and leave no other record (a retry-safe
// call journals no attempt marker, and a crash or a lost write can drop its result), and the
// request is journaled before the call starts. The completed call's result (StepToolResult), the
// attempt marker journaled before a side effect fired (StepAttempt), and a saga step's failure
// (StepSagaFail) add their ToolUseID too. An attempt marker of a durable Step carries the step's
// name as its ToolUseID, so it adds that name as a key: the set may hold a key no tool call has,
// or a call the model requested that never ran, which only makes an absence proof for that key
// impossible, never a wrong one. Journal records a redaction replaced cannot be projected;
// producers refuse a journal holding one (ErrRedacted).
func ToolUseKey(r agent.Record) []string {
	switch {
	case r.Kind == agent.StepModel && r.Message != nil:
		var keys []string
		for _, p := range r.Message.Parts {
			if tu, ok := p.(agent.ToolUse); ok {
				keys = append(keys, toolUseKeyPrefix+tu.ID)
			}
		}
		return keys
	case r.Kind == agent.StepToolResult:
		return []string{toolUseKeyPrefix + r.ToolUseID}
	case (r.Kind == agent.StepAttempt || r.Kind == agent.StepSagaFail) && r.ToolUseID != "":
		return []string{toolUseKeyPrefix + r.ToolUseID}
	}
	return nil
}

// absenceKeys returns the sorted, de-duplicated keys the key set yields over records. It projects
// records as they are; the exported producers call projectKeys, which projects what each record's
// stored bytes say (projectRecords).
func absenceKeys(records []agent.Record, set KeySet) []string {
	seen := map[string]struct{}{}
	keys := make([]string, 0, len(records))
	for _, r := range records {
		for _, k := range set.Key(r) {
			if _, dup := seen[k]; !dup {
				seen[k] = struct{}{}
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// projectKeys returns the sorted, de-duplicated keys set yields over what records' stored bytes
// say (projectRecords; what names the projection, for errors).
func projectKeys(records []agent.Record, set KeySet, what string) ([]string, error) {
	recs, err := projectRecords(records, what)
	if err != nil {
		return nil, err
	}
	return absenceKeys(recs, set), nil
}

// projectRecords returns the records a projection (an absence key set, the used-policy set, the
// event stream) reads: for each record read back from a journal, the record its stored bytes
// decode to, since the journal tree binds those bytes (Raw) and not the record's fields, which a
// caller can change in memory. It refuses:
//   - a redacted record (ErrRedacted): its content is gone, so the projection would omit it;
//   - stored bytes that do not read one way to every JSON reader (checkRecordBytes; ErrMalformed),
//     so nothing is projected from one reading of a record while a reader of the journal sees
//     another;
//   - a record whose fields do not say what its stored bytes say (ErrMalformed), so a projection
//     never differs from the one the journal tree commits.
//
// A record built in memory has no stored bytes and is projected as it is.
func projectRecords(records []agent.Record, what string) ([]agent.Record, error) {
	if err := refuseRedacted(records, what); err != nil {
		return nil, err
	}
	out := make([]agent.Record, len(records))
	for i, r := range records {
		raw := r.Raw()
		if len(raw) == 0 {
			out[i] = r
			continue
		}
		bad := func(format string, args ...any) error {
			return fmt.Errorf("audit: %s: record %d (%q): %s: %w", what, i, r.Name, fmt.Sprintf(format, args...), ErrMalformed)
		}
		if err := checkRecordBytes(raw); err != nil {
			return nil, bad("its stored bytes do not read one way to every JSON reader (%v)", err)
		}
		stored, err := agent.DecodeRecord(raw)
		if err != nil {
			return nil, bad("its stored bytes do not decode as a journal record (%v)", err)
		}
		got, err1 := agent.EncodeRecord(r)
		want, err2 := agent.EncodeRecord(stored)
		if err1 != nil || err2 != nil || !bytes.Equal(got, want) {
			return nil, bad("its fields do not say what its stored bytes say")
		}
		out[i] = stored
	}
	return out, nil
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

// AbsenceRoot is the RFC 6962 Merkle root over the run's sorted, distinct keys in set, and the
// number of keys. Anyone holding the journal recomputes it to confirm a signed key-set head
// reflects the run: the head's Root and Size must equal it. It refuses records it cannot project
// as NewAbsenceTreeHead does, so a recomputation never confirms a head that omits a record: a
// journal holding a redacted record is ErrRedacted (the record's keys cannot be read), and a
// record whose stored bytes read two ways is ErrMalformed.
func AbsenceRoot(records []agent.Record, set KeySet) (root []byte, size int, err error) {
	if err := set.check(); err != nil {
		return nil, 0, err
	}
	keys, err := projectKeys(records, set, "the "+set.Kind+" key set")
	if err != nil {
		return nil, 0, err
	}
	return merkleRoot(keyLeaves(keys)), len(keys), nil
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
	keys, err := projectKeys(records, set, "absence of "+key)
	if err != nil {
		return Absence{}, err
	}
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

// VerifyAbsence returns nil if proof shows Key is absent from the key set committed by root:
// each named neighbor is included at its index, they bracket Key in sort order, and they are
// ADJACENT (or Key sits before the first / after the last / the set is empty). Adjacency is the
// load-bearing check: without it, bracketing alone does not preclude Key being present between
// two non-consecutive neighbors. It checks the proof against a bare root; AbsenceBundle.Verify
// is the form that also authenticates the root and its key set. A proof that does not hold is an
// error wrapping ErrNotVerified.
func VerifyAbsence(root []byte, proof Absence) error {
	if !absent(root, proof) {
		return notVerified("audit: the proof does not show %q absent from the %d-key set under this root", proof.Key, proof.Size)
	}
	return nil
}

func absent(root []byte, proof Absence) bool {
	if proof.Size == 0 { // empty key set: everything is absent
		return proof.Left == nil && proof.Right == nil && bytes.Equal(root, merkleRoot(nil))
	}
	if proof.Left != nil {
		if proof.Left.Proof.Size != proof.Size || proof.Left.Key >= proof.Key {
			return false
		}
		if !verifyPath(root, keyLeaf(proof.Left.Key), proof.Left.Proof.Index, proof.Size, proof.Left.Proof.Path) {
			return false
		}
	}
	if proof.Right != nil {
		if proof.Right.Proof.Size != proof.Size || proof.Key >= proof.Right.Key {
			return false
		}
		if !verifyPath(root, keyLeaf(proof.Right.Key), proof.Right.Proof.Index, proof.Size, proof.Right.Proof.Path) {
			return false
		}
	}
	switch {
	case proof.Left != nil && proof.Right != nil:
		return proof.Left.Proof.Index+1 == proof.Right.Proof.Index // consecutive: no room between
	case proof.Left == nil && proof.Right != nil:
		return proof.Right.Proof.Index == 0 // Key sorts before all: right is the first key
	case proof.Right == nil && proof.Left != nil:
		return proof.Left.Proof.Index == proof.Size-1 // Key sorts after all: left is the last key
	default:
		return false // both nil but non-empty set: malformed
	}
}

// AbsenceBundle is the portable, anchorable form of an absence proof, mirroring ProofBundle:
// the proof plus the signed tree head committing the key set it is proven against. The STH
// commits to the key set's kind, the run, and the journal tree the set was projected from.
type AbsenceBundle struct {
	Format  string         `json:"format"`  // AbsenceFormat
	RunID   string         `json:"run_id"`  // the run whose key set the proof is against; must equal STH.RunID
	Absence Absence        `json:"absence"` // the non-membership proof
	STH     SignedTreeHead `json:"sth"`     // the signed commitment to the key set the proof is proven against
}

// Verify returns nil if the bundle authentically proves Absence.Key absent from the key set set
// of run RunID, under v (obtained out of band). It checks the STH signature, that the STH is a head
// of set's kind for RunID (with its source journal named), that the key carries the set's prefix,
// that the proof is bound to the signed size, and non-membership under the signed root. The
// absence holds for the journal prefix STH.Journal names; confirm from the anchor log that it is
// the run's latest head before reading it as "never happened in the run." A bundle that does not
// hold is an error wrapping ErrNotVerified; one of another format, ErrFormat.
func (b AbsenceBundle) Verify(v Verifier, set KeySet) error {
	if err := set.check(); err != nil {
		return err
	}
	if err := formatOf(b, b.Format); err != nil {
		return err
	}
	if err := b.STH.Verify(v); err != nil {
		return err
	}
	if b.STH.Kind != set.Kind || b.STH.RunID != b.RunID || b.STH.Journal == nil {
		return notVerified("audit: the absence proof's head is a %q head of run %q, not the %s key set of run %q", b.STH.Kind, b.STH.RunID, set.Kind, b.RunID)
	}
	if !strings.HasPrefix(b.Absence.Key, set.Prefix) {
		return notVerified("audit: key %q is not one the %s key set could contain", b.Absence.Key, set.Kind)
	}
	if b.Absence.Size != b.STH.Size {
		return notVerified("audit: the absence proof is for a set of %d keys, not the signed %d", b.Absence.Size, b.STH.Size)
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
	root, size, err := AbsenceRoot(journal, set)
	if err != nil {
		return AbsenceBundle{}, err
	}
	if sth.Size != size || !bytes.Equal(sth.Root, root) {
		return AbsenceBundle{}, fmt.Errorf("audit: STH does not commit to run %s's %s key set (wrong or stale STH)", sth.RunID, set.Kind)
	}
	proof, err := ProveAbsent(journal, set, key)
	if err != nil {
		return AbsenceBundle{}, err
	}
	return AbsenceBundle{Format: AbsenceFormat, RunID: sth.RunID, Absence: proof, STH: sth}, nil
}

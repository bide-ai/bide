package verify_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/verify"
)

// The standalone verifier must agree, bit for bit, with the full audit package on the same
// proofs; that cross-check is what licenses the intentional duplication. It also must depend
// on nothing but the record's canonical leaf bytes (agent.EncodeRecord of the record, which is
// exactly what a store persists), never the typed record, so a third party can verify without
// the SDK.

func journal(t *testing.T) (agent.Durable, string) {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	for i, v := range []string{"a", "b", "c", "d", "e"} {
		name := v
		if _, err := agent.Step(ctx, store, "run", name, func(context.Context) (string, error) { return v, nil }, agent.StepSafety(agent.Safety{ReadOnly: true})); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	// A tool result whose JSON carries HTML-significant characters: the leaf is the journal's
	// bytes for it, which keep them as written.
	if _, err := store.Do(ctx, "run", "c1", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"html":"<b>a & b</b>"}`)}, nil
	}); err != nil {
		t.Fatalf("tool result: %v", err)
	}
	return store, "run"
}

func leafBytes(t *testing.T, rec agent.Record) []byte {
	t.Helper()
	b, err := agent.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("encode record: %v", err)
	}
	return verify.JournalLeaf(b)
}

// TestVerify_InclusionMatchesAudit: every record's inclusion proof verifies via the standalone
// verifier on leaf bytes, and agrees with audit.VerifyInclusion.
func TestVerify_InclusionMatchesAudit(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	root, _ := audit.Root(ctx, store, runID)
	recs, _ := store.History(ctx, runID)

	for i := range recs {
		proof, err := audit.Prove(ctx, store, runID, i)
		if err != nil {
			t.Fatalf("Prove(%d): %v", i, err)
		}
		viaAudit, _ := audit.VerifyInclusion(root, recs[i], proof)
		viaStandalone := verify.Inclusion(root, leafBytes(t, recs[i]), proof.Index, proof.Size, proof.Path)
		if !viaAudit || !viaStandalone {
			t.Fatalf("record %d: audit=%v standalone=%v (both must be true)", i, viaAudit, viaStandalone)
		}
	}

	// A wrong leaf must fail the standalone verifier.
	proof0, _ := audit.Prove(ctx, store, runID, 0)
	if verify.Inclusion(root, verify.JournalLeaf([]byte(`{"forged":true}`)), proof0.Index, proof0.Size, proof0.Path) {
		t.Fatal("standalone verifier accepted a forged leaf")
	}
}

// TestVerify_ConsistencyMatchesAudit: the standalone consistency check agrees with audit that
// the size-2 prefix is append-only-contained in the full tree.
func TestVerify_ConsistencyMatchesAudit(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	rootFull, _ := audit.Root(ctx, store, runID)

	proof, err := audit.ProveConsistency(ctx, store, runID, 2)
	if err != nil {
		t.Fatalf("ProveConsistency: %v", err)
	}

	// The size-2 root, recomputed from just the first two records (salts included).
	recs, _ := store.History(ctx, runID)
	rootEarly, _ := audit.Root(ctx, fixedHistory(recs[:2]), runID)

	viaAudit := audit.VerifyConsistency(rootEarly, rootFull, proof)
	viaStandalone := verify.Consistency(proof.First, proof.Size, proof.Path, rootEarly, rootFull)
	if !viaAudit || !viaStandalone {
		t.Fatalf("consistency: audit=%v standalone=%v (both must be true)", viaAudit, viaStandalone)
	}
}

// The signed tree head encoding names its version, bide.audit.sth.v4: v4 is the first whose
// leaves carry a versioned tag and whose journal records are salted, so a head over an older
// encoding cannot be mistaken for a fork of a head over the newer one. Both the SDK and the
// standalone verifier use exactly the documented encoding.
func TestVerify_TreeHeadEncodingIsVersioned(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	th, _ := audit.NewTreeHead(ctx, store, runID, 1700000000)
	field := func(b, f []byte) []byte {
		b = binary.BigEndian.AppendUint64(b, uint64(len(f)))
		return append(b, f...)
	}
	msg := append([]byte(nil), "bide.audit.sth.v4\x00"...)
	msg = field(msg, []byte(th.Kind))
	msg = field(msg, []byte(th.RunID))
	msg = binary.BigEndian.AppendUint64(msg, uint64(th.Size))
	msg = field(msg, th.Root)
	msg = binary.BigEndian.AppendUint64(msg, uint64(th.Timestamp))
	msg = append(msg, 0)
	if sth := audit.SignTreeHead(th, priv); !ed25519.Verify(pub, msg, sth.Signature) {
		t.Fatal("the SDK does not sign the bide.audit.sth.v4 encoding of the head")
	}
	if !verify.TreeHead(th.Kind, th.RunID, th.Size, th.Root, th.Timestamp, nil, ed25519.Sign(priv, msg), pub) {
		t.Fatal("the standalone verifier does not check the bide.audit.sth.v4 encoding of the head")
	}
}

// TestVerify_TreeHeadMatchesAudit: an STH signed by the SDK verifies via the standalone
// verifier, and any tamper to its fields breaks the signature.
func TestVerify_TreeHeadMatchesAudit(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	th, _ := audit.NewTreeHead(ctx, store, runID, 1700000000)
	sth := audit.SignTreeHead(th, priv)

	if !sth.Verify(pub) {
		t.Fatal("audit STH did not self-verify")
	}
	if !verify.TreeHead(sth.Kind, sth.RunID, sth.Size, sth.Root, sth.Timestamp, nil, sth.Signature, pub) {
		t.Fatal("standalone verifier rejected a valid SDK-signed STH")
	}
	for name, ok := range map[string]bool{
		"size":      verify.TreeHead(sth.Kind, sth.RunID, sth.Size+1, sth.Root, sth.Timestamp, nil, sth.Signature, pub),
		"timestamp": verify.TreeHead(sth.Kind, sth.RunID, sth.Size, sth.Root, sth.Timestamp+1, nil, sth.Signature, pub),
		"kind":      verify.TreeHead(audit.TreePolicyUsed, sth.RunID, sth.Size, sth.Root, sth.Timestamp, nil, sth.Signature, pub),
		"run id":    verify.TreeHead(sth.Kind, "other", sth.Size, sth.Root, sth.Timestamp, nil, sth.Signature, pub),
		"journal":   verify.TreeHead(sth.Kind, sth.RunID, sth.Size, sth.Root, sth.Timestamp, &verify.TreeRef{}, sth.Signature, pub),
	} {
		if ok {
			t.Fatalf("standalone verifier accepted an STH with a tampered %s", name)
		}
	}

	// An absence key-set head, which names its source journal, verifies the same way.
	recs, _ := store.History(ctx, runID)
	abs, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, th, priv, 7)
	if err != nil {
		t.Fatal(err)
	}
	ref := &verify.TreeRef{Size: abs.Journal.Size, Root: abs.Journal.Root}
	if !verify.TreeHead(abs.Kind, abs.RunID, abs.Size, abs.Root, abs.Timestamp, ref, abs.Signature, pub) {
		t.Fatal("standalone verifier rejected a valid SDK-signed absence head")
	}
	if verify.TreeHead(abs.Kind, abs.RunID, abs.Size, abs.Root, abs.Timestamp, &verify.TreeRef{Size: ref.Size + 1, Root: ref.Root}, abs.Signature, pub) {
		t.Fatal("standalone verifier accepted an absence head with a tampered journal size")
	}
}

// Each kind of leaf has its own tag, and the standalone leaf builders match the SDK's: a proof of
// an absence key's neighbour, of an event, and of an anchor entry verify from KeyLeaf, EventLeaf,
// and AnchorLeaf, and not from the untagged bytes or another kind's tag.
func TestVerify_LeafKindsMatchAudit(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	recs, _ := store.History(ctx, runID)

	keys := audit.ToolUseKeys
	abs, err := audit.ProveAbsent(recs, keys, "tooluse:zzz")
	if err != nil || abs.Left == nil {
		t.Fatalf("ProveAbsent = %+v, %v", abs, err)
	}
	keyRoot := audit.AbsenceRoot(recs, keys)

	events := audit.NewEventLog()
	for i := range 3 {
		if err := events.Add(agent.TurnStarted{Seq: i}); err != nil {
			t.Fatal(err)
		}
	}
	evProof, _ := events.Prove(0)
	inner, _ := json.Marshal(agent.TurnStarted{Seq: 0})
	evJSON, _ := json.Marshal(struct {
		Kind  string          `json:"kind"`
		Event json.RawMessage `json:"event"`
		Salt  []byte          `json:"salt"`
	}{"TurnStarted", inner, evProof.Salt})

	anchors := audit.NewMemAnchorLog()
	_, priv, _ := ed25519.GenerateKey(nil)
	th, _ := audit.NewTreeHead(ctx, store, runID, 1)
	if err := anchors.Publish(ctx, runID, audit.SignTreeHead(th, priv)); err != nil {
		t.Fatal(err)
	}
	anchorRoot, _ := anchors.Root()
	anchorProof, _ := anchors.Prove(0)
	entryJSON, _ := json.Marshal(anchors.Entries()[0])

	for name, c := range map[string]struct {
		root, content []byte
		leaf          func([]byte) []byte
		proof         audit.Inclusion
	}{
		"key":    {keyRoot, []byte(abs.Left.Key), func(b []byte) []byte { return verify.KeyLeaf(string(b)) }, abs.Left.Proof},
		"event":  {events.Root(), evJSON, verify.EventLeaf, evProof.Inclusion},
		"anchor": {anchorRoot, entryJSON, verify.AnchorLeaf, anchorProof},
	} {
		if !verify.Inclusion(c.root, c.leaf(c.content), c.proof.Index, c.proof.Size, c.proof.Path) {
			t.Errorf("%s: the standalone leaf does not verify against the SDK's proof", name)
		}
		for _, wrong := range [][]byte{c.content, verify.JournalLeaf(c.content), verify.KeyLeaf(string(c.content)), verify.EventLeaf(c.content), verify.AnchorLeaf(c.content)} {
			if !bytes.Equal(wrong, c.leaf(c.content)) && verify.Inclusion(c.root, wrong, c.proof.Index, c.proof.Size, c.proof.Path) {
				t.Errorf("%s: a leaf with the wrong tag verified: %q", name, wrong)
			}
		}
	}
}

// fixedHistory is a read-only Durable whose history is exactly the records it holds, salts
// included, as a journal exported from a store is.
type fixedHistory []agent.Record

func (h fixedHistory) History(context.Context, string) ([]agent.Record, error) { return h, nil }

func (fixedHistory) Do(context.Context, string, string, func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return agent.Record{}, errors.New("fixedHistory is read-only")
}

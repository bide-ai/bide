package verify_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/mldsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/verify"
	"github.com/bide-ai/bide/internal/journaltest"
)

// The standalone verifier must agree, bit for bit, with the full audit package on the same
// proofs; that cross-check is what licenses the intentional duplication. It also must depend
// on nothing but the record's canonical leaf bytes (agent.EncodeRecord of the record, which is
// exactly what a store persists), never the typed record, so a third party can verify without
// the SDK.

func journal(t *testing.T) (*agent.MemStore, string) {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	j := agenttest.MustJournal(store)
	for i, v := range []string{"a", "b", "c", "d", "e"} {
		name := v
		if _, err := agent.Step(ctx, j, "run", name, func(context.Context) (string, error) { return v, nil }, agent.WithSafety(agent.Safety{ReadOnly: true})); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	// A tool result whose JSON carries HTML-significant characters: the leaf is the journal's
	// bytes for it, which keep them as written.
	if _, err := journaltest.Do(ctx, j, "run", "c1", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"html":"<b>a & b</b>"}`)}, nil
	}); err != nil {
		t.Fatalf("tool result: %v", err)
	}
	return store, "run"
}

// leafBytes is the standalone leaf of a record read back from a journal: the tag and the bytes the
// store holds for it, which is what a proof carries as record_bytes.
func leafBytes(t *testing.T, rec agent.Record) []byte {
	t.Helper()
	if rec.Raw() == nil {
		t.Fatalf("record %q has no stored bytes", rec.Name)
	}
	return verify.JournalLeaf(rec.Raw())
}

// TestVerify_InclusionMatchesAudit: every record's inclusion proof verifies via the standalone
// verifier on leaf bytes, and agrees with audit.VerifyInclusion.
func TestVerify_InclusionMatchesAudit(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	j := agenttest.MustJournal(store)
	root, _ := audit.Root(ctx, j, runID)
	recs, _ := j.History(ctx, runID)

	for i := range recs {
		proof, err := audit.Prove(ctx, j, runID, i)
		if err != nil {
			t.Fatalf("Prove(%d): %v", i, err)
		}
		viaAudit := audit.VerifyInclusion(root, recs[i].Raw(), proof) == nil
		viaStandalone := verify.Inclusion(root, leafBytes(t, recs[i]), proof.Index, proof.Size, proof.Path)
		if !viaAudit || !viaStandalone {
			t.Fatalf("record %d: audit=%v standalone=%v (both must be true)", i, viaAudit, viaStandalone)
		}
	}

	// A wrong leaf must fail the standalone verifier.
	proof0, _ := audit.Prove(ctx, j, runID, 0)
	if verify.Inclusion(root, verify.JournalLeaf([]byte(`{"forged":true}`)), proof0.Index, proof0.Size, proof0.Path) {
		t.Fatal("standalone verifier accepted a forged leaf")
	}
}

// TestVerify_ConsistencyMatchesAudit: the standalone consistency check agrees with audit that
// the size-2 prefix is append-only-contained in the full tree.
func TestVerify_ConsistencyMatchesAudit(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	j := agenttest.MustJournal(store)
	rootFull, _ := audit.Root(ctx, j, runID)

	proof, err := audit.ProveConsistency(ctx, j, runID, 2)
	if err != nil {
		t.Fatalf("ProveConsistency: %v", err)
	}

	// The size-2 root, recomputed from just the first two records (salts included).
	recs, _ := j.History(ctx, runID)
	rootEarly, _ := audit.Root(ctx, fixedJournal(t, runID, recs[:2]), runID)

	viaAudit := audit.VerifyConsistency(rootEarly, rootFull, proof) == nil
	viaStandalone := verify.Consistency(proof.First, proof.Size, proof.Path, rootEarly, rootFull)
	if !viaAudit || !viaStandalone {
		t.Fatalf("consistency: audit=%v standalone=%v (both must be true)", viaAudit, viaStandalone)
	}
}

// canonicalV5 is the documented bide.audit.sth.v5 encoding of a head under scheme alg.
func canonicalV5(alg string, th audit.TreeHead) []byte {
	field := func(b, f []byte) []byte {
		b = binary.BigEndian.AppendUint64(b, uint64(len(f)))
		return append(b, f...)
	}
	msg := append([]byte(nil), "bide.audit.sth.v5\x00"...)
	msg = field(msg, []byte(alg))
	msg = field(msg, []byte(th.Kind))
	msg = field(msg, []byte(th.RunID))
	msg = binary.BigEndian.AppendUint64(msg, uint64(th.Size))
	msg = field(msg, th.Root)
	msg = binary.BigEndian.AppendUint64(msg, uint64(th.TimestampNanos))
	if th.Journal == nil {
		return append(msg, 0)
	}
	msg = append(msg, 1)
	msg = binary.BigEndian.AppendUint64(msg, uint64(th.Journal.Size))
	return field(msg, th.Journal.Root)
}

// head is the standalone form of a signed head's content.
func head(sth audit.SignedTreeHead) verify.Head {
	h := verify.Head{Alg: string(sth.Alg), Kind: sth.Kind, RunID: sth.RunID, Size: sth.Size, Root: sth.Root, TimestampNanos: sth.TimestampNanos}
	if sth.Journal != nil {
		h.Journal = &verify.TreeRef{Size: sth.Journal.Size, Root: sth.Journal.Root}
	}
	return h
}

// standaloneVerifier is the standalone verifier for an audit signer's key.
func standaloneVerifier(t *testing.T, s audit.Signer) verify.Verifier {
	t.Helper()
	v, err := verify.NewVerifier(string(s.Alg()), s.PublicKey())
	if err != nil {
		t.Fatalf("verify.NewVerifier(%s): %v", s.Alg(), err)
	}
	return v
}

// signers returns one signer of each scheme.
func signers(t *testing.T) []audit.Signer {
	t.Helper()
	_, edPriv, _ := ed25519.GenerateKey(nil)
	ml, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	ed := audit.Ed25519Signer{Priv: edPriv}
	return []audit.Signer{ed, audit.MLDSASigner{Priv: ml}, audit.HybridSigner{Ed: ed, ML: audit.MLDSASigner{Priv: ml}}}
}

// The signed tree head encoding names its version, bide.audit.sth.v5, and signs the scheme with
// the head. Both the SDK and the standalone verifier use exactly the documented encoding.
func TestVerify_TreeHeadEncodingIsVersioned(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	j := agenttest.MustJournal(store)
	pub, priv, _ := ed25519.GenerateKey(nil)
	th, _ := audit.NewTreeHead(ctx, j, runID, 1700000000)
	msg := canonicalV5("ed25519", th)
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, msg, sth.Signature) {
		t.Fatal("the SDK does not sign the bide.audit.sth.v5 encoding of the head")
	}
	v, _ := verify.NewVerifier("ed25519", pub)
	h := head(sth)
	if !verify.TreeHead(h, ed25519.Sign(priv, msg), v) {
		t.Fatal("the standalone verifier does not check the bide.audit.sth.v5 encoding of the head")
	}
	// The scheme is signed: the same signature read under another scheme name is refused, even by
	// a verifier of that name.
	h.Alg = "ml-dsa-65"
	if verify.TreeHead(h, sth.Signature, v) {
		t.Fatal("the standalone verifier checked a head under a scheme other than its own")
	}
}

// The standalone verifier checks a head's scheme against its own before the signature: an ed25519
// key's signature over the encoding of a head labelled ml-dsa-65 does not verify under an ed25519
// verifier, as in the SDK.
func TestVerify_TreeHeadRequiresTheVerifiersScheme(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	th := audit.TreeHead{Kind: audit.TreeJournal, RunID: "r", Size: 0, Root: make([]byte, 32), TimestampNanos: 1}
	sig := ed25519.Sign(priv, canonicalV5("ml-dsa-65", th))
	v, _ := verify.NewVerifier("ed25519", pub)
	h := head(audit.SignedTreeHead{TreeHead: th, Alg: audit.AlgMLDSA65})
	if verify.TreeHead(h, sig, v) {
		t.Fatal("an ed25519 signature over a head labelled ml-dsa-65 verified under an ed25519 verifier")
	}
}

// TestVerify_TreeHeadMatchesAudit: an STH signed by the SDK under each scheme verifies via the
// standalone verifier, and any tamper to its fields breaks the signature.
func TestVerify_TreeHeadMatchesAudit(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	j := agenttest.MustJournal(store)
	th, _ := audit.NewTreeHead(ctx, j, runID, 1700000000)
	recs, _ := j.History(ctx, runID)
	for _, s := range signers(t) {
		sth, err := audit.SignTreeHead(th, s)
		if err != nil {
			t.Fatal(err)
		}
		av, _ := audit.VerifierOf(s)
		if err := sth.Verify(av); err != nil {
			t.Fatalf("%s: audit STH did not self-verify: %v", s.Alg(), err)
		}
		v := standaloneVerifier(t, s)
		if !verify.TreeHead(head(sth), sth.Signature, v) {
			t.Fatalf("%s: standalone verifier rejected a valid SDK-signed STH", s.Alg())
		}
		tamper := func(f func(*verify.Head)) bool {
			h := head(sth)
			f(&h)
			return verify.TreeHead(h, sth.Signature, v)
		}
		for name, ok := range map[string]bool{
			"size":      tamper(func(h *verify.Head) { h.Size++ }),
			"timestamp": tamper(func(h *verify.Head) { h.TimestampNanos++ }),
			"kind":      tamper(func(h *verify.Head) { h.Kind = audit.TreePolicyUsed }),
			"run id":    tamper(func(h *verify.Head) { h.RunID = "other" }),
			"journal":   tamper(func(h *verify.Head) { h.Journal = &verify.TreeRef{} }),
			"alg": tamper(func(h *verify.Head) {
				if h.Alg = "ed25519"; s.Alg() == audit.AlgEd25519 {
					h.Alg = "ml-dsa-65"
				}
			}),
		} {
			if ok {
				t.Fatalf("%s: standalone verifier accepted an STH with a tampered %s", s.Alg(), name)
			}
		}

		// An absence key-set head, which names its source journal, verifies the same way.
		abs, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, th, s, 7)
		if err != nil {
			t.Fatal(err)
		}
		if !verify.TreeHead(head(abs), abs.Signature, v) {
			t.Fatalf("%s: standalone verifier rejected a valid SDK-signed absence head", s.Alg())
		}
		h := head(abs)
		h.Journal.Size++
		if verify.TreeHead(h, abs.Signature, v) {
			t.Fatalf("%s: standalone verifier accepted an absence head with a tampered journal size", s.Alg())
		}
	}
}

// The ed25519 half of a hybrid head's signature is a signature over the hybrid label and the head,
// not over the head: it verifies neither as an ed25519 head nor, with the ML-DSA half dropped, as a
// hybrid one, in the standalone verifier as in the SDK.
func TestVerify_StrippedHybridHalfFails(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	j := agenttest.MustJournal(store)
	th, _ := audit.NewTreeHead(ctx, j, runID, 1700000000)
	all := signers(t)
	hyb := all[2].(audit.HybridSigner)
	sth, err := audit.SignTreeHead(th, hyb)
	if err != nil {
		t.Fatal(err)
	}
	edHalf := sth.Signature[4 : 4+binary.BigEndian.Uint32(sth.Signature[:4])]
	edV, _ := verify.NewVerifier("ed25519", hyb.Ed.PublicKey())
	for _, alg := range []string{"ed25519", "ed25519+ml-dsa-65"} {
		h := head(sth)
		h.Alg = alg
		if verify.TreeHead(h, edHalf, edV) {
			t.Fatalf("the stripped ed25519 half verified as a %s head", alg)
		}
	}
	// It is not a signature over the ed25519 encoding of the head either.
	if ed25519.Verify(hyb.Ed.PublicKey(), canonicalV5("ed25519", th), edHalf) {
		t.Fatal("the stripped ed25519 half is a plain ed25519 signature over the head")
	}
	hv := standaloneVerifier(t, hyb)
	onlyEd := binary.BigEndian.AppendUint32(nil, uint32(len(edHalf)))
	onlyEd = append(onlyEd, edHalf...)
	if verify.TreeHead(head(sth), onlyEd, hv) {
		t.Fatal("a hybrid head verified with its ML-DSA half dropped")
	}
}

// A record written with a field this version does not know verifies through the bytes a proof
// carries: the standalone verifier hashes record_bytes, never a re-encoding.
func TestVerify_RecordWithUnknownFieldVerifiesByItsBytes(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	j := agenttest.MustJournal(store)
	salt := bytes.Repeat([]byte{7}, agent.SaltSize)
	future := []byte(`{"name":"future","kind":"value","result":{"v":1},"added_in_1_1":{"x":[1,2]},"salt":"` + base64.StdEncoding.EncodeToString(salt) + `"}`)
	if _, _, err := store.Insert(ctx, runID, "future", future); err != nil {
		t.Fatal(err)
	}
	recs, err := j.History(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	last := len(recs) - 1
	if !bytes.Equal(recs[last].Raw(), future) {
		t.Fatalf("the journal hands back %q, not the stored bytes", recs[last].Raw())
	}
	s := signers(t)[0]
	th, _ := audit.NewTreeHead(ctx, j, runID, 1700000000)
	sth, _ := audit.SignTreeHead(th, s)
	pb, err := audit.ProveRecord(ctx, j, runID, last, sth)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pb.RecordBytes, future) {
		t.Fatalf("record_bytes = %q, want the stored bytes verbatim", pb.RecordBytes)
	}
	if !verify.Inclusion(sth.Root, verify.JournalLeaf(pb.RecordBytes), pb.Inclusion.Index, pb.Inclusion.Size, pb.Inclusion.Path) {
		t.Fatal("the standalone verifier rejected a record with an unknown field")
	}
	// A re-encoding of the decoded record drops the field, and is not the leaf.
	rec, _ := pb.Record()
	reenc, _ := agent.EncodeRecord(rec)
	if bytes.Equal(reenc, future) || verify.Inclusion(sth.Root, verify.JournalLeaf(reenc), pb.Inclusion.Index, pb.Inclusion.Size, pb.Inclusion.Path) {
		t.Fatal("the re-encoded record verified: the leaf is not the stored bytes")
	}
}

// Each kind of leaf has its own tag, and the standalone leaf builders match the SDK's: a proof of
// an absence key's neighbour, of an event, and of an anchor entry verify from KeyLeaf, EventLeaf,
// and AnchorLeaf, and not from the untagged bytes or another kind's tag.
func TestVerify_LeafKindsMatchAudit(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	j := agenttest.MustJournal(store)
	recs, _ := j.History(ctx, runID)

	keys := audit.ToolUseKeys
	abs, err := audit.ProveAbsent(recs, keys, "tooluse:zzz")
	if err != nil || abs.Left == nil {
		t.Fatalf("ProveAbsent = %+v, %v", abs, err)
	}
	keyRoot, _, err := audit.AbsenceRoot(recs, keys)
	if err != nil {
		t.Fatal(err)
	}

	// Event leaves are bide.audit.event-leaf.v3: a snake_case kind and snake_case event JSON, written
	// out here rather than marshaled, so the test pins the wire form a third party reimplements.
	events := audit.NewEventLog()
	for i := range 3 {
		if err := events.Add(agent.TurnStarted{Seq: i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := events.Add(agent.ToolCompleted{ToolUseID: "c1", Name: "pay", Result: json.RawMessage(`{"ok":true}`), IsError: false}); err != nil {
		t.Fatal(err)
	}
	evProof, _ := events.Prove(0)
	evJSON := []byte(`{"kind":"turn_started","event":{"seq":0},"salt":"` + base64.StdEncoding.EncodeToString(evProof.Salt) + `"}`)
	tcProof, _ := events.Prove(3)
	tcJSON := []byte(`{"kind":"tool_completed","event":{"tool_use_id":"c1","name":"pay","result":{"ok":true},"is_error":false},"salt":"` + base64.StdEncoding.EncodeToString(tcProof.Salt) + `"}`)

	anchors := audit.NewMemAnchorLog()
	_, priv, _ := ed25519.GenerateKey(nil)
	th, _ := audit.NewTreeHead(ctx, j, runID, 1)
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatal(err)
	}
	if err := anchors.Publish(ctx, runID, sth); err != nil {
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
		"key":                  {keyRoot, []byte(abs.Left.Key), func(b []byte) []byte { return verify.KeyLeaf(string(b)) }, abs.Left.Proof},
		"event":                {events.Root(), evJSON, verify.EventLeaf, evProof.Inclusion},
		"tool completed event": {events.Root(), tcJSON, verify.EventLeaf, tcProof.Inclusion},
		"anchor":               {anchorRoot, entryJSON, verify.AnchorLeaf, anchorProof},
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

// fixedJournal returns a Journal over a new store whose run runID holds exactly recs (read from a
// journal, header first), each stored as its stored bytes, salts included, as a journal exported
// from a store is.
func fixedJournal(t *testing.T, runID string, recs []agent.Record) *agent.Journal {
	t.Helper()
	s := agent.NewMemStore()
	for _, r := range recs {
		if _, _, err := s.Insert(context.Background(), runID, r.Name, r.Raw()); err != nil {
			t.Fatal(err)
		}
	}
	return agenttest.MustJournal(s)
}

package audit_test

// Adversarial review of #105. Each Test_R105_* asserts the property a verifier should hold; a
// failing test is a finding.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// unredacted returns recs without the records a redaction replaced: the projection a key holder
// signing by hand over a redacted journal would make.
func unredacted(recs []agent.Record) []agent.Record {
	var out []agent.Record
	for _, r := range recs {
		if !r.Redacted {
			out = append(out, r)
		}
	}
	return out
}

func r105Salt(b byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, agent.SaltSize))
}

// r105Copy copies run "gov" of src into a new MemStore, replacing each named entry's bytes with a
// redaction tombstone that records its leaf hash (what a redaction does).
func r105Copy(t *testing.T, src *agent.MemStore, run string, redact ...string) *agent.MemStore {
	t.Helper()
	ctx := context.Background()
	dst := agent.NewMemStore()
	for e, err := range src.Load(ctx, run, -1) {
		if err != nil {
			t.Fatal(err)
		}
		data := e.Data
		for _, n := range redact {
			if e.Name == n {
				data = []byte(`{"redacted":{"leaf_hash":"` + hex.EncodeToString(audit.JournalLeafHash(e.Data)) + `","at_ms":1}}`)
			}
		}
		if _, _, err := dst.Insert(ctx, run, e.Name, data); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// FINDING: after a tool result is redacted, the tool-use key set projected from the (unchanged)
// journal tree omits it, so a verifiable absence bundle "proves" the call never happened.
func Test_R105_RedactedToolCallIsNotProvablyAbsent(t *testing.T) {
	ctx := context.Background()
	orig, _ := p11GovernedRun(t)
	j := agenttest.MustJournal(orig) // journals tool call "pay" as step "call:pay"
	signer := p11Signers(t)["ed25519"]
	v := p11Verifier(t, signer)

	// Sanity: before redaction, "pay" cannot be proven absent.
	recs, _ := j.History(ctx, "gov")
	jth, _ := audit.NewTreeHead(ctx, j, "gov", p11Now())
	abs, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, jth, signer, p11Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.ProveAbsentBundle(recs, audit.ToolUseKeys, audit.ToolUseKeyFor("pay"), abs); err == nil {
		t.Fatal("sanity: proved a present call absent before redaction")
	}

	red := r105Copy(t, orig, "gov", "call:pay")
	j2 := agenttest.MustJournal(red)
	rrecs, _ := j2.History(ctx, "gov")
	rjth, _ := audit.NewTreeHead(ctx, j2, "gov", p11Now())
	if !bytes.Equal(rjth.Root, jth.Root) {
		t.Fatal("sanity: redaction changed the journal root")
	}
	rabs, err := audit.SignAbsenceRoot(rrecs, audit.ToolUseKeys, rjth, signer, p11Now())
	if err != nil {
		t.Logf("SignAbsenceRoot refused a journal with a redacted record: %v (good)", err)
		return
	}
	b, err := audit.ProveAbsentBundle(rrecs, audit.ToolUseKeys, audit.ToolUseKeyFor("pay"), rabs)
	if err != nil {
		t.Logf("ProveAbsentBundle refused: %v (good)", err)
		return
	}
	if err := b.Verify(v, audit.ToolUseKeys); err == nil {
		t.Fatalf("an absence bundle proves tool call %q absent from the same journal tree (size %d, root %x) in which it is leaf-committed", "pay", b.STH.Journal.Size, b.STH.Journal.Root[:6])
	}
}

// FINDING: a run certificate issued after a governed action under an unapproved policy was redacted
// omits that policy, and verifies against an allowlist that does not contain it.
func Test_R105_RedactedGovernedActionDropsOutOfRunCertificate(t *testing.T) {
	ctx := context.Background()
	s, _ := p11GovernedRun(t)
	j := agenttest.MustJournal(s) // "pay" runs under D1 (approved, anchored and certified)
	if _, err := journaltest.Do(ctx, j, "gov", "call:rogue", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "rogue", Result: json.RawMessage(`{"policy_digest":"EVIL","ok":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	signer := p11Signers(t)["ed25519"]
	v := p11Verifier(t, signer)
	ts := p11Now()

	jth, _ := audit.NewTreeHead(ctx, j, "gov", ts)
	sth, _ := audit.SignTreeHead(jth, signer)
	if _, err := audit.CertifyRun(ctx, j, "gov", sth, audit.RunCertSpec{ApprovedPolicies: []string{"D1"}, Signer: signer, TimestampNanos: ts}); err == nil {
		t.Fatal("sanity: certified a run that used EVIL")
	}

	red := agenttest.MustJournal(r105Copy(t, s, "gov", "call:rogue"))
	// The anchored head signed BEFORE the redaction still matches the redacted journal.
	cert, err := audit.CertifyRun(ctx, red, "gov", sth, audit.RunCertSpec{ApprovedPolicies: []string{"D1"}, Signer: signer, TimestampNanos: ts})
	if err != nil {
		t.Logf("CertifyRun refused the redacted journal: %v (good)", err)
		return
	}
	res, err := audit.VerifyRun(cert, []string{"D1"}, v)
	if err == nil && res.OK {
		t.Fatalf("a run certificate over journal tree %x (which commits a governed action under EVIL) verifies with allowlist [D1]; used=%v", sth.Root[:6], cert.UsedPolicies)
	}
}

// FINDING (regression from proof.v2): record_bytes are decoded leniently, so one committed leaf
// reads one way to bide (case-insensitive, last duplicate wins) and another to an exact-name
// reader. The strict-JSON promise ("what a person reads is what is verified") stops at the base64.
func Test_R105_RecordBytesWithCaseVariantOrDuplicateNamesAreRefused(t *testing.T) {
	ctx := context.Background()
	cases := map[string]string{
		"case variant": `{"name":"call:x","kind":"value","Kind":"tool_result","tool_use_id":"x","result":{"ok":1},"salt":"` + r105Salt(7) + `"}`,
		"duplicate":    `{"name":"call:x","kind":"value","tool_use_id":"x","result":{"ok":1},"kind":"tool_result","salt":"` + r105Salt(7) + `"}`,
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			s := agent.NewMemStore()
			j := agenttest.MustJournal(s)
			if _, err := journaltest.Do(ctx, j, "r", "first", func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Insert(ctx, "r", "call:x", []byte(rec)); err != nil {
				t.Fatal(err)
			}
			signer := p11Signers(t)["ed25519"]
			v := p11Verifier(t, signer)
			pkg, err := audit.Evidence(ctx, j, "r", signer, p11Now(), audit.WithToolCall("x"))
			if err != nil {
				t.Logf("producer refused: %v", err)
				return
			}
			raw, _ := json.Marshal(pkg)
			var back audit.EvidencePackage
			if err := audit.UnmarshalStrict(raw, &back); err != nil {
				t.Fatal(err)
			}
			rep, err := back.Verify(v)
			// What an exact-name JSON reader (Python, jq, JS) sees in the same bytes:
			var m map[string]any
			_ = json.Unmarshal(back.Actions[0].Bundle.RecordBytes, &m)
			if err == nil && rep.OK {
				t.Fatalf("verified %q as a tool call; an exact-name reader of the same leaf sees kind=%v", rep.Items[0].Note, m["kind"])
			}
		})
	}
}

// FINDING (pre-existing semantics, exposed by #92 kinds): a tool call whose side effect was
// attempted (attempt marker journaled, then a crash) or that failed in a saga has no tool_result, so
// the tool-use key set says it never happened.
func Test_R105_AttemptedToolCallIsNotProvablyAbsent(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []agent.StepKind{agent.StepAttempt, agent.StepSagaFail} {
		s := agenttest.MemJournal()
		if _, err := journaltest.Do(ctx, s, "r", "marker", func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: kind, ToolUseID: "wire", AttemptedAt: 1, Result: json.RawMessage(`"x"`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
		recs, _ := s.History(ctx, "r")
		if _, err := audit.ProveAbsent(recs, audit.ToolUseKeys, audit.ToolUseKeyFor("wire")); err == nil {
			t.Errorf("%s record for tool call %q journaled, yet ProveAbsent proves %q absent (ToolUseKeys doc: \"no tool call with this ID happened\")", kind, "wire", audit.ToolUseKeyFor("wire"))
		}
	}
}

// FINDING (low): VerifyAnchorInclusion accepts an entry whose nested head is of an old format.
func Test_R105_AnchorEntryNestedFormatIsChecked(t *testing.T) {
	ctx := context.Background()
	s, _ := p11GovernedRun(t)
	j := agenttest.MustJournal(s)
	signer := p11Signers(t)["ed25519"]
	th, _ := audit.NewTreeHead(ctx, j, "gov", p11Now())
	sth, _ := audit.SignTreeHead(th, signer)
	sth.Format = "bide.audit.sth.v4"
	log := audit.NewMemAnchorLog()
	if err := log.Publish(ctx, "gov", sth); err != nil {
		t.Fatal(err)
	}
	root, _ := log.Root()
	proof, _ := log.Prove(0)
	if err := audit.VerifyAnchorInclusion(root, log.Entries()[0], proof); err == nil {
		t.Fatal("an anchor entry carrying a bide.audit.sth.v4 head verifies")
	}
}

// FINDING (low): the event tree projected from the journal changes when a record is redacted,
// although a redacted record "keeps its place in every tree over the journal".
func Test_R105_RedactionKeepsTheEventTree(t *testing.T) {
	ctx := context.Background()
	orig, _ := p11GovernedRun(t)
	j := agenttest.MustJournal(orig)
	a, err := audit.EventLogFromJournal(ctx, j, "gov")
	if err != nil {
		t.Fatal(err)
	}
	red := agenttest.MustJournal(r105Copy(t, orig, "gov", "call:pay"))
	b, err := audit.EventLogFromJournal(ctx, red, "gov")
	if err != nil {
		t.Logf("refused: %v (acceptable)", err)
		return
	}
	if !bytes.Equal(a.Root(), b.Root()) || a.Len() != b.Len() {
		t.Fatalf("event tree changed silently on redaction: size %d -> %d", a.Len(), b.Len())
	}
}

// Property: mutating any byte of record_bytes, any path hash, the index, the size, or the head
// signature, or swapping in a head of another run, never verifies. (Expected to PASS.)
func Test_R105_ProofMutationsNeverVerify(t *testing.T) {
	ctx := context.Background()
	s, _ := p11GovernedRun(t)
	j := agenttest.MustJournal(s)
	for alg, signer := range p11Signers(t) {
		v := p11Verifier(t, signer)
		th, _ := audit.NewTreeHead(ctx, j, "gov", p11Now())
		sth, _ := audit.SignTreeHead(th, signer)
		recs, _ := j.History(ctx, "gov")
		r := rand.New(rand.NewPCG(1, 2))
		for idx := range recs {
			pb, err := audit.ProveRecord(ctx, j, "gov", idx, sth)
			if err != nil {
				t.Fatal(err)
			}
			if err := pb.Verify(v); err != nil {
				t.Fatalf("%s: clean proof %d: %v", alg, idx, err)
			}
			clone := func() audit.ProofBundle {
				raw, _ := json.Marshal(pb)
				var c audit.ProofBundle
				if err := json.Unmarshal(raw, &c); err != nil {
					t.Fatal(err)
				}
				return c
			}
			for range 200 {
				c := clone()
				switch r.IntN(6) {
				case 0:
					c.RecordBytes[r.IntN(len(c.RecordBytes))] ^= byte(1 + r.IntN(255))
				case 1:
					if len(c.Inclusion.Path) == 0 {
						continue
					}
					p := c.Inclusion.Path[r.IntN(len(c.Inclusion.Path))]
					p[r.IntN(len(p))] ^= 1
				case 2:
					c.Inclusion.Index += 1 + r.IntN(3)
				case 3:
					c.Inclusion.Size += 1 + r.IntN(3)
					c.STH.Size = c.Inclusion.Size
				case 4:
					c.STH.Signature[r.IntN(len(c.STH.Signature))] ^= 1
				case 5:
					c.RecordBytes = append(c.RecordBytes, ' ')
				}
				if err := c.Verify(v); err == nil {
					t.Fatalf("%s: mutated proof of record %d verifies", alg, idx)
				}
			}
		}
	}
}

// Probe: two approver ids resolving to the same key both count toward m-of-n. (Hardening probe.)
func Test_R105_SameKeyUnderTwoApproverIdsCountsTwice(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v := audit.Ed25519Verifier{Pub: pub}
	s := agent.ApprovalSubject{RunID: "r", ToolUseID: "c", ToolName: "wire", Args: json.RawMessage(`{}`)}
	var recs []agent.Record
	for _, id := range []string{"alice", "bob"} {
		recs = append(recs, agent.Record{Name: "approval:c:" + id, Kind: agent.StepApproval, ToolUseID: "c", Approved: true, ApproverSignature: &agent.ApproverSignature{Approver: id, ApproverAlg: audit.AlgEd25519, Signature: ed25519.Sign(priv, agent.ApprovalDecisionBytes(s, id, true))}})
	}
	p := agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob"}}
	got, _ := agent.TallyApprovals(recs, s, p, func(string) (agent.ApproverVerifier, bool) { return v, true })
	if got.Passed() {
		t.Logf("NOTE: 2-of-2 passed with one key behind both approver ids (the resolver maps both to %x...)", pub[:4])
	}
}

var _ = errors.Is
var _ = time.Now

// L3: an auditor's approver-key resolver that maps two eligible approver ids to one key is refused
// (agent.ErrConfig) before any evidence is read: one key holder would otherwise fill two seats.
func Test_R105_VerifyApprovalsRefusesASharedApproverKey(t *testing.T) {
	signer := p11Signers(t)["ed25519"]
	v := p11Verifier(t, signer)
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob"}}
	_, err := audit.VerifyApprovals(nil, "c1", policy, func(string) (agent.ApproverVerifier, bool) { return v, true }, v)
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig for two approvers with one key", err)
	}
}

// Each producer that projects a journal refuses one holding a redacted record on its own, with
// ErrRedacted, whichever entry point a caller uses.
func Test_R105_EveryProjectionRefusesARedactedJournal(t *testing.T) {
	ctx := context.Background()
	orig, _ := p11GovernedRun(t)
	j2 := agenttest.MustJournal(orig)
	signer := p11Signers(t)["ed25519"]
	jth, _ := audit.NewTreeHead(ctx, j2, "gov", p11Now())
	sth, _ := audit.SignTreeHead(jth, signer)
	red := r105Copy(t, orig, "gov", "call:pay")
	j := agenttest.MustJournal(red)
	recs, _ := j.History(ctx, "gov")

	if _, err := audit.NewAbsenceTreeHead(recs, audit.ToolUseKeys, jth, 1); !errors.Is(err, audit.ErrRedacted) {
		t.Errorf("NewAbsenceTreeHead: err = %v, want ErrRedacted", err)
	}
	if _, err := audit.ProveAbsent(recs, audit.ToolUseKeys, audit.ToolUseKeyFor("pay")); !errors.Is(err, audit.ErrRedacted) {
		t.Errorf("ProveAbsent: err = %v, want ErrRedacted", err)
	}
	// A key-set head a key holder signed by hand over the redacted projection: the key set of the
	// records that are not redacted.
	root, size, err := audit.AbsenceRoot(unredacted(recs), audit.ToolUseKeys)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := audit.SignTreeHead(audit.TreeHead{Kind: audit.TreeToolUse, RunID: "gov", Size: size, Root: root,
		TimestampNanos: 1, Journal: &audit.TreeRef{Size: jth.Size, Root: jth.Root}}, signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.ProveAbsentBundle(recs, audit.ToolUseKeys, audit.ToolUseKeyFor("pay"), abs); !errors.Is(err, audit.ErrRedacted) {
		t.Errorf("ProveAbsentBundle: err = %v, want ErrRedacted", err)
	}
	if _, err := audit.CertifyRun(ctx, j, "gov", sth, audit.RunCertSpec{ApprovedPolicies: []string{"D1"}, Signer: signer, TimestampNanos: sth.TimestampNanos}); !errors.Is(err, audit.ErrRedacted) {
		t.Errorf("CertifyRun: err = %v, want ErrRedacted", err)
	}
	if _, err := audit.EventLogFromJournal(ctx, j, "gov"); !errors.Is(err, audit.ErrRedacted) {
		t.Errorf("EventLogFromJournal: err = %v, want ErrRedacted", err)
	}
	if err := audit.PersistJournal(ctx, audit.NewMemEventStore(), j, "gov"); !errors.Is(err, audit.ErrRedacted) {
		t.Errorf("PersistJournal: err = %v, want ErrRedacted", err)
	}
}

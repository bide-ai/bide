package audit_test

// Re-review of the #105 review fixes (head 8a302fe). Each Test_R105b_* asserts a property the
// fixes or the docs claim; a failing test is a finding.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// M1 follow-up. The fix holds record_bytes to "one reading for every JSON reader" in the proof
// path (ProofBundle.Record, ProveRecord, and JournalExport.Journal), but the absence producers
// project records the journal decoded leniently. A stored record an exact-name reader reads as the
// tool_result of call "x" ("kind":"tool_result", then a case variant "Kind":"value" that Go's
// case-insensitive decoder reads last) is then proven absent by ProveAbsentBundle over the store's
// history, and the bundle verifies. The same journal given as a journal export is refused.
func Test_R105b_AbsenceOverRecordBytesThatReadTwoWays(t *testing.T) {
	ctx := context.Background()
	cases := map[string]string{
		"case variant": `{"name":"call:x","kind":"tool_result","tool_use_id":"x","result":"ok","Kind":"value","salt":"` + r105Salt(3) + `"}`,
		"duplicate":    `{"name":"call:x","kind":"tool_result","tool_use_id":"x","result":"ok","kind":"value","salt":"` + r105Salt(3) + `"}`,
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			s := agent.NewMemStore()
			if _, err := s.Do(ctx, "r", "first", func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Insert(ctx, "r", "call:x", []byte(rec)); err != nil {
				t.Fatal(err)
			}
			signer := p11Signers(t)["ed25519"]
			v := p11Verifier(t, signer)
			recs, err := s.History(ctx, "r")
			if err != nil {
				t.Fatal(err)
			}
			// What an exact-name reader of the committed bytes sees.
			var m map[string]any
			if err := json.Unmarshal([]byte(rec), &m); err != nil {
				t.Fatal(err)
			}
			first := bytes.Index([]byte(rec), []byte(`"kind":"tool_result"`)) >= 0
			t.Logf("Go journal reads kind=%q; the bytes spell \"kind\":\"tool_result\" first (%v)", recs[1].Kind, first)

			// The proof path refuses these bytes (the M1 fix).
			exp, err := audit.ExportJournal(ctx, s, "r")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := exp.Journal(); !errors.Is(err, audit.ErrMalformed) {
				t.Fatalf("sanity: the journal export path accepted the record: %v", err)
			}

			jth, err := audit.NewTreeHead(ctx, s, "r", p11Now())
			if err != nil {
				t.Fatal(err)
			}
			abs, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, jth, signer, p11Now())
			if err != nil {
				t.Logf("SignAbsenceRoot refused: %v (good)", err)
				return
			}
			b, err := audit.ProveAbsentBundle(recs, audit.ToolUseKeys, audit.ToolUseKeyFor("x"), abs)
			if err != nil {
				t.Logf("ProveAbsentBundle refused: %v (good)", err)
				return
			}
			if err := b.Verify(v, audit.ToolUseKeys); err == nil {
				t.Fatalf("absence of tool call %q verifies over journal tree %x, whose leaf %d an exact-name reader reads as its tool_result (the journal export of the same run is refused as ErrMalformed)", "x", jth.Root[:6], 1)
			}
		})
	}
}

// failResultStore fails, without storing it, the first write of a tool_result record: a crash or
// a lost write between the tool running and its outcome being journaled.
type failResultStore struct {
	m    *agent.MemStore
	mu   sync.Mutex
	done bool
}

var errLostWrite = errors.New("write lost")

func (c *failResultStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	c.mu.Lock()
	fail := !c.done && bytes.Contains(data, []byte(`"kind":"tool_result"`))
	if fail {
		c.done = true
	}
	c.mu.Unlock()
	if fail {
		return agent.Entry{}, false, errLostWrite
	}
	return c.m.Insert(ctx, runID, name, data)
}
func (c *failResultStore) Get(ctx context.Context, r, n string) (agent.Entry, bool, error) {
	return c.m.Get(ctx, r, n)
}
func (c *failResultStore) Load(ctx context.Context, r string, a int64) iter.Seq2[agent.Entry, error] {
	return c.m.Load(ctx, r, a)
}

// M2 follow-up. The fix puts attempt markers and saga failures in the tool-use key set, and the
// audit guide now says the set "holds every call the run started, not only completed ones". A
// retry-safe call (Idempotent here) gets no attempt marker. If it runs and its result is never
// journaled (the run crashes, or the write is lost, and the run is not driven again), the journal
// holds the model's request for it and nothing else, and an absence bundle proves the call never
// happened although its effect fired.
func Test_R105b_StartedRetrySafeCallIsNotProvablyAbsent(t *testing.T) {
	for _, safety := range []struct {
		name string
		s    agent.Safety
	}{{"idempotent", agent.Safety{Idempotent: true}}, {"non-retriable (control)", agent.Safety{}}} {
		t.Run(safety.name, func(t *testing.T) {
			ctx := context.Background()
			m := agent.NewMemStore()
			fired := 0
			charge := agent.Func("charge", "", safety.s, func(context.Context, struct{}) (string, error) { fired++; return "charged", nil })
			j, err := agent.NewJournal(&failResultStore{m: m})
			if err != nil {
				t.Fatal(err)
			}
			_, runErr := agent.New(agent.NewScriptedModel(agent.ToolTurn("c1", "charge", `{}`), agent.TextTurn("done")), j, charge).
				SetMaxConcurrency(1).Run(ctx, "r", "hi")
			if runErr == nil || fired != 1 {
				t.Fatalf("setup: run err %v, fired %d (want an error and one firing)", runErr, fired)
			}
			j2, _ := agent.NewJournal(m)
			recs, err := j2.History(ctx, "r")
			if err != nil {
				t.Fatal(err)
			}
			kinds := make([]string, len(recs))
			for i, r := range recs {
				kinds[i] = string(r.Kind) + ":" + r.Name
			}
			t.Logf("fired %d; run error: %v; journal: %v", fired, runErr, kinds)
			signer := p11Signers(t)["ed25519"]
			v := p11Verifier(t, signer)
			jth, err := audit.NewTreeHead(ctx, j2, "r", p11Now())
			if err != nil {
				t.Fatal(err)
			}
			abs, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, jth, signer, p11Now())
			if err != nil {
				t.Fatal(err)
			}
			b, err := audit.ProveAbsentBundle(recs, audit.ToolUseKeys, audit.ToolUseKeyFor("c1"), abs)
			if err != nil {
				t.Logf("refused: %v (good)", err)
				return
			}
			if err := b.Verify(v, audit.ToolUseKeys); err == nil {
				t.Fatalf("tool call c1 ran (fired %d) and its request is journaled, yet an absence bundle proves it never happened", fired)
			}
		})
	}
}

// H1/H2 follow-up. The producers now refuse a redacted journal, and the reply to the finding rests
// on the auditor: "a dishonest key holder can sign any key set anyway (the absence trust model is
// that an auditor holding the journal recomputes it)". The audit guide's auditor recomputes with
// audit.PoliciesUsed (and AbsenceRoot for a key-set head). Over the redacted journal both silently
// omit the governed action the journal tree still commits, so a head or certificate a key holder
// signs by hand over the omitted set passes the auditor's recomputation.
func Test_R105b_AuditorRecomputeOverRedactedJournalOmitsAPolicy(t *testing.T) {
	ctx := context.Background()
	s, _ := p11GovernedRun(t)
	if _, err := s.Do(ctx, "gov", "call:rogue", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "rogue", Result: json.RawMessage(`{"policy_digest":"EVIL","ok":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	recs, _ := s.History(ctx, "gov")
	if used, err := audit.PoliciesUsed(recs); err != nil || !slices.Contains(used, "EVIL") {
		t.Fatalf("sanity: EVIL not used before redaction (%v, %v)", used, err)
	}
	jth, _ := audit.NewTreeHead(ctx, s, "gov", p11Now())

	red := r105Copy(t, s, "gov", "call:rogue")
	rrecs, _ := red.History(ctx, "gov")
	rjth, _ := audit.NewTreeHead(ctx, red, "gov", p11Now())
	if !bytes.Equal(rjth.Root, jth.Root) {
		t.Fatal("sanity: redaction changed the journal root")
	}
	// The auditor, holding the journal, recomputes (audit guide: PoliciesUsed, then the root). Over
	// the redacted journal neither may return a set: both report the redaction.
	if used, err := audit.PoliciesUsed(rrecs); !errors.Is(err, audit.ErrRedacted) {
		t.Fatalf("PoliciesUsed over journal tree %x (which commits a governed action under EVIL) = %v, %v; want ErrRedacted", rjth.Root[:6], used, err)
	}
	if _, _, err := audit.AbsenceRoot(rrecs, audit.PolicyUsedKeys); !errors.Is(err, audit.ErrRedacted) {
		t.Fatalf("AbsenceRoot over the redacted journal: err %v, want ErrRedacted", err)
	}
}

// M1 follow-up. The one role check that decodes a proven record's result leniently is
// VerifyApprovals' read of the journaled tally (json.Unmarshal; the run certificate's leaves use
// UnmarshalStrict). A tally result with "need" and a case variant "Need" passes checkRecordBytes
// (the result is raw JSON, so no field shape applies) and is proven by ProveRecord, and it reads
// as the expected need 2 to bide and as need 1 to an exact-name reader. VerifyApprovals must refuse
// it as malformed rather than report the gate held.
func Test_R105b_ProvenTallyResultReadsTwoWays(t *testing.T) {
	ctx := context.Background()
	g := passedGate(t)
	orig := evidenceFor(t, g)

	// The same journal, with the tally's result rewritten to read two ways.
	tallyName := agent.ApprovalTallyStep("c1")
	dst := agent.NewMemStore()
	found := false
	for e, err := range g.store.(*agent.MemStore).Load(ctx, gateRun, -1) {
		if err != nil {
			t.Fatal(err)
		}
		data := e.Data
		if e.Name == tallyName {
			if !bytes.Contains(data, []byte(`"result":{"need":2,`)) {
				t.Fatalf("setup: tally bytes %s", data)
			}
			data = bytes.Replace(data, []byte(`"result":{"need":2,`), []byte(`"result":{"need":1,"Need":2,`), 1)
			found = true
		}
		if _, _, err := dst.Insert(ctx, gateRun, e.Name, data); err != nil {
			t.Fatal(err)
		}
	}
	if !found {
		t.Fatal("setup: no tally record")
	}
	th, err := audit.NewTreeHead(ctx, dst, gateRun, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	sth := signTH(t, th, g.logPriv)
	acts := make([]audit.EvidenceAction, len(orig))
	for i, a := range orig {
		pb, err := audit.ProveRecord(ctx, dst, gateRun, a.Bundle.Inclusion.Index, sth)
		if err != nil {
			t.Logf("ProveRecord refused %s: %v (good)", a.Ref, err)
			return
		}
		a.Bundle = pb
		acts[i] = a
	}
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), audit.Ed25519Verifier{Pub: g.logPub})
	if !errors.Is(err, audit.ErrMalformed) {
		t.Fatalf("a proven tally that reads need 2 to bide and need 1 to an exact-name reader: verdict ok=%v, err %v; want ErrMalformed", v.OK, err)
	}
}

// L3 follow-up, the audit side of agent's Test_R105b_KeylessApproverIsRefused. distinctApproverKeys
// skipped a verifier with an empty PublicKey, so two approvers whose keys name nothing were taken
// as distinct while the gate's counting rule took them as one. An empty key names no key: it is
// refused with ErrConfig, as the gate refuses it.
func Test_R105b_VerifyApprovalsRefusesAKeylessApprover(t *testing.T) {
	signer := p11Signers(t)["ed25519"]
	v := p11Verifier(t, signer)
	policy := agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}
	_, err := audit.VerifyApprovals(nil, "c1", policy, func(id string) (agent.ApproverVerifier, bool) {
		if id == "alice" {
			return v, true
		}
		return audit.Ed25519Verifier{}, true
	}, v)
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig for an approver whose verifier has an empty public key", err)
	}
}

// M1 follow-up. Every producer that projects what a journal's records say (the absence key sets
// and the run certificate's used-policy set) checks each record's stored bytes as the proof path
// does, so a record that reads two ways is refused as ErrMalformed rather than projected one way.
func Test_R105b_ProjectionsRefuseRecordBytesThatReadTwoWays(t *testing.T) {
	ctx := context.Background()
	s, _ := p11GovernedRun(t)
	// A record an exact-name reader reads as the result of tool call "twoways" and bide reads as a
	// value record ("Kind", a case variant, is read last by encoding/json).
	rec := `{"name":"call:twoways","kind":"tool_result","tool_use_id":"twoways","result":"x","Kind":"value","salt":"` + r105Salt(9) + `"}`
	if _, _, err := s.Insert(ctx, "gov", "call:twoways", []byte(rec)); err != nil {
		t.Fatal(err)
	}
	recs, err := s.History(ctx, "gov")
	if err != nil {
		t.Fatal(err)
	}
	signer := p11Signers(t)["ed25519"]
	jth, err := audit.NewTreeHead(ctx, s, "gov", p11Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, set := range []audit.KeySet{audit.ToolUseKeys, audit.PolicyUsedKeys} {
		if _, err := audit.NewAbsenceTreeHead(recs, set, jth, p11Now()); !errors.Is(err, audit.ErrMalformed) {
			t.Errorf("NewAbsenceTreeHead(%s): err %v, want ErrMalformed", set.Kind, err)
		}
		if _, err := audit.ProveAbsent(recs, set, set.Prefix+"nothing"); !errors.Is(err, audit.ErrMalformed) {
			t.Errorf("ProveAbsent(%s): err %v, want ErrMalformed", set.Kind, err)
		}
	}
	sth, err := audit.SignTreeHead(jth, signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.CertifyRun(ctx, s, "gov", sth, audit.RunCertSpec{ApprovedPolicies: []string{"D1"}, Signer: signer, TimestampNanos: p11Now()}); !errors.Is(err, audit.ErrMalformed) {
		t.Errorf("CertifyRun: err %v, want ErrMalformed", err)
	}
}

// L1 follow-up. EvidencePackage.Verify now checks a carried run certificate's own format, but not
// the formats of the heads the certificate carries: a certificate for another run (so VerifyRun is
// never reached) carrying a v4 used-policy head is reported as not verified, not as ErrFormat.
func Test_R105b_EvidenceRunCertNestedHeadFormat(t *testing.T) {
	ctx := context.Background()
	s, _ := p11GovernedRun(t)
	signer := p11Signers(t)["ed25519"]
	v := p11Verifier(t, signer)
	ts := p11Now()
	pkg, err := audit.Evidence(ctx, s, "gov", signer, ts, audit.WithToolCall("pay"),
		audit.WithRunCertificate(audit.RunCertSpec{ApprovedPolicies: []string{"D1"}, Signer: signer, TimestampNanos: ts}))
	if err != nil {
		t.Fatal(err)
	}
	c := *pkg.RunCertificate
	c.RunID = "other"
	c.UsedPolicyAbsence.Format = "bide.audit.sth.v4"
	pkg.RunCertificate = &c
	_, err = pkg.Verify(v)
	if !errors.Is(err, audit.ErrFormat) {
		t.Fatalf("a package carrying a run certificate with a bide.audit.sth.v4 head: err = %v, want ErrFormat", err)
	}
}

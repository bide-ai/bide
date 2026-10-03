package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"iter"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit/verify"
	"github.com/bide-ai/bide/internal/journaltest"
)

// Regression tests for the audit findings of 2026-09. Each test failed on the code before its fix.

// secJournal records recs into a fresh MemStore under runID, naming unnamed records.
func secJournal(t *testing.T, runID string, recs []agent.Record) *agent.Journal {
	t.Helper()
	s := agenttest.MemJournal()
	for i, r := range recs {
		r := r
		if r.Name == "" {
			r.Name = "s" + string(rune('a'+i))
		}
		if _, err := journaltest.Do(context.Background(), s, runID, r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// secHead returns runID's journal records and its journal tree head.
func secHead(t *testing.T, s *agent.Journal, runID string) ([]agent.Record, TreeHead) {
	t.Helper()
	recs, err := s.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	th, err := NewTreeHead(context.Background(), s, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	return recs, th
}

// used is PoliciesUsed, failing t on an error.
func used(t *testing.T, recs []agent.Record) []string {
	t.Helper()
	u, err := PoliciesUsed(recs)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func secKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// A tool-use absence STH must not prove a POLICY absent: the two key sets are different trees.
func TestAbsence_ToolUseTreeCannotProvePolicyAbsent(t *testing.T) {
	pub, priv := secKey(t)
	s := secJournal(t, "r", []agent.Record{{Kind: agent.StepToolResult, ToolUseID: "charge", Result: json.RawMessage(`{"policy_digest":"EVIL"}`)}})
	recs, th := secHead(t, s, "r")
	toolSTH, err := SignAbsenceRoot(recs, ToolUseKeys, th, edS(priv), 1)
	if err != nil {
		t.Fatal(err)
	}
	forged := AbsenceBundle{Format: AbsenceFormat, RunID: "r", STH: toolSTH, Absence: Absence{Key: PolicyUsedKeyFor("EVIL"), Size: 1,
		Right: &Neighbor{Key: "tooluse:charge", Proof: Inclusion{Index: 0, Size: 1}}}}
	for _, set := range []KeySet{PolicyUsedKeys, ToolUseKeys} {
		if err := forged.Verify(edV(pub), set); err == nil {
			t.Fatalf("a tool-use absence STH proves policy EVIL absent (as %s), but the run used it (%v)", set.Kind, used(t, recs))
		}
	}
	// A genuine tool-use absence proof against the same head verifies.
	good, err := ProveAbsentBundle(recs, ToolUseKeys, ToolUseKeyFor("refund"), toolSTH)
	if err != nil {
		t.Fatal(err)
	}
	if err := good.Verify(edV(pub), ToolUseKeys); err != nil {
		t.Fatal("a genuine tool-use absence proof failed to verify")
	}
}

// The journal STH must not serve as an absence commitment either.
func TestAbsence_JournalTreeCannotProveAbsence(t *testing.T) {
	pub, priv := secKey(t)
	s := secJournal(t, "r", []agent.Record{{Kind: agent.StepToolResult, ToolUseID: "charge", Result: json.RawMessage(`"ok"`)}})
	recs, th := secHead(t, s, "r")
	sth := signTH(t, th, priv)
	leaf0, _ := json.Marshal(recs[0])
	forged := AbsenceBundle{Format: AbsenceFormat, RunID: "r", STH: sth, Absence: Absence{Key: "tooluse:charge", Size: 1,
		Right: &Neighbor{Key: string(leaf0), Proof: Inclusion{Index: 0, Size: 1}}}}
	if err := forged.Verify(edV(pub), ToolUseKeys); err == nil {
		t.Fatal("the journal STH proves the executed tool call 'charge' absent")
	}
	// Nor can an absence head stand in for a journal head in a ProofBundle.
	abs, err := SignAbsenceRoot(recs, ToolUseKeys, th, edS(priv), 1)
	if err != nil {
		t.Fatal(err)
	}
	pb := ProofBundle{Format: ProofFormat, RunID: "r", RecordBytes: recs[0].Raw(), Inclusion: Inclusion{Index: 0, Size: 1}, STH: abs}
	if err := pb.Verify(edV(pub)); err == nil {
		t.Fatal("an absence head verified as a journal head")
	}
}

// A run certificate must not accept a used-policy STH from another run, another journal tree, or
// another key set, however it was signed.
func TestVerifyRun_RejectsSubstitutedAbsenceSTH(t *testing.T) {
	pub, priv := secKey(t)
	s := secJournal(t, "X", []agent.Record{{Kind: agent.StepToolResult, ToolUseID: "c", Result: json.RawMessage(`{"policy_digest":"EVIL"}`)}})
	recs, th := secHead(t, s, "X")
	sth := signTH(t, th, priv)

	// A signed empty used-policy set from another run with no governed actions.
	other := secJournal(t, "Y", []agent.Record{{Kind: agent.StepValue, Name: "x", Result: json.RawMessage(`1`)}})
	orecs, oth := secHead(t, other, "Y")
	emptyY, err := SignAbsenceRoot(orecs, PolicyUsedKeys, oth, edS(priv), 2)
	if err != nil {
		t.Fatal(err)
	}
	// Relabelled as run X (signature breaks), and a genuine empty set of run X's empty prefix.
	emptyPrefix, err := SignAbsenceRoot(recs[:0], PolicyUsedKeys, TreeHead{Kind: TreeJournal, RunID: "X", Size: 0, Root: merkleRoot(nil)}, edS(priv), 2)
	if err != nil {
		t.Fatal(err)
	}
	// Run X's own tool-use set is empty of policies too, but it is the wrong key set.
	toolX, err := SignAbsenceRoot(recs, ToolUseKeys, th, edS(priv), 2)
	if err != nil {
		t.Fatal(err)
	}
	relabelled := emptyY
	relabelled.RunID = "X"
	for name, abs := range map[string]SignedTreeHead{
		"another run's empty set":           emptyY,
		"another run's set relabelled as X": relabelled,
		"run X's empty prefix":              emptyPrefix,
		"run X's tool-use set":              toolX,
	} {
		cert := RunCertificate{Format: RunCertificateFormat, RunID: "X", Properties: runCertProperties, UsedPolicies: []string{}, UsedPolicyAbsence: abs, STH: sth}
		if res, _ := VerifyRun(cert, []string{"GOOD"}, edV(pub)); res.OK || res.OnlyApprovedPolicies {
			t.Errorf("%s: run X used %v but its certificate verifies only-approved-policies", name, used(t, recs))
		}
	}
}

func secThreeCalls(t *testing.T, runID string) *agent.Journal {
	return secJournal(t, runID, []agent.Record{
		{Kind: agent.StepToolResult, ToolUseID: "a", Result: json.RawMessage(`"1"`)},
		{Kind: agent.StepToolResult, ToolUseID: "b", Result: json.RawMessage(`"2"`)},
		{Kind: agent.StepToolResult, ToolUseID: "c", Result: json.RawMessage(`"3"`)},
	})
}

// secEvidence builds a sealed package over three tool calls in run A with a consistency proof
// from the one-record head, and returns it with the log key.
func secEvidence(t *testing.T, opts ...EvidenceOption) (EvidencePackage, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv := secKey(t)
	s := secThreeCalls(t, "A")
	recs, _ := s.History(context.Background(), "A")
	early, err := journalHead("A", recs[:1], 1)
	if err != nil {
		t.Fatal(err)
	}
	opts = append([]EvidenceOption{WithConsistencyFrom(signTH(t, early, priv))}, opts...)
	pkg, err := Evidence(context.Background(), s, "A", edS(priv), 1, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if rep, err := pkg.Verify(edV(pub), WithApprovedPolicies()); err != nil || !rep.OK {
		t.Fatalf("the untouched package does not verify: %+v %v", rep, err)
	}
	return pkg, pub, priv
}

// secReject asserts the package fails to verify, and, when priv is given, still fails after being
// resealed with the log key itself: the seal attributes a package, it does not vouch for its proofs.
func secReject(t *testing.T, what string, pkg EvidencePackage, pub ed25519.PublicKey, priv ed25519.PrivateKey) {
	t.Helper()
	if rep, _ := pkg.Verify(edV(pub), WithApprovedPolicies()); rep.OK {
		t.Fatalf("%s: the package verifies", what)
	}
	if priv != nil {
		if err := pkg.Seal(edS(priv)); err != nil {
			t.Fatal(err)
		}
		if rep, _ := pkg.Verify(edV(pub), WithApprovedPolicies()); rep.OK {
			t.Fatalf("%s: the resealed package verifies", what)
		}
	}
}

// A garbage consistency proof (First > Size) must not verify, even when resealed by the log key.
func TestEvidence_ConsistencyProofIsChecked(t *testing.T) {
	pkg, pub, priv := secEvidence(t)
	pkg.Consistency.Proof.Path = [][]byte{[]byte("garbage")}
	pkg.Consistency.Proof.First = 99
	secReject(t, "garbage consistency proof (First 99 > Size 3)", pkg, pub, priv)

	pkg, pub, priv = secEvidence(t)
	pkg.Consistency.Proof.Path = [][]byte{[]byte("garbage")}
	secReject(t, "garbage consistency path", pkg, pub, priv)

	// An earlier head of another run cannot anchor the proof.
	pkg, pub, priv = secEvidence(t)
	pkg.Consistency.From.RunID = "B"
	pkg.Consistency.From = signTH(t, pkg.Consistency.From.TreeHead, priv)
	secReject(t, "earlier head of another run", pkg, pub, priv)
}

// Relabelling a tool result as an "approval" by "cfo" must not verify, even if resealed.
func TestEvidence_ItemKindAndLabelAreChecked(t *testing.T) {
	for name, mutate := range map[string]func(*EvidenceAction){
		"kind and label": func(a *EvidenceAction) { a.Kind, a.Label = KindApproval, "cfo" },
		"label":          func(a *EvidenceAction) { a.Label = "refund-1000000" },
		"ref":            func(a *EvidenceAction) { a.Ref = "z" },
		"kind step":      func(a *EvidenceAction) { a.Kind = KindStep },
		"kind call":      func(a *EvidenceAction) { a.Kind = KindCall },
		"kind tally":     func(a *EvidenceAction) { a.Kind, a.Label = KindApprovalTally, "approval tally" },
		"kind grant":     func(a *EvidenceAction) { a.Kind = KindGrant },
		"unknown kind":   func(a *EvidenceAction) { a.Kind = "other" },
	} {
		pkg, pub, priv := secEvidence(t)
		mutate(&pkg.Actions[0])
		secReject(t, "relabelled "+name, pkg, pub, priv)
	}
}

// Renaming the run in a package (and in every bundle) must not verify.
func TestEvidence_RunIDIsAuthenticated(t *testing.T) {
	pkg, pub, priv := secEvidence(t)
	pkg.RunID = "B"
	for i := range pkg.Actions {
		pkg.Actions[i].Bundle.RunID = "B"
	}
	secReject(t, "run A's evidence as run B's", pkg, pub, priv)

	pkg, pub, _ = secEvidence(t)
	b := pkg.Actions[0].Bundle
	b.RunID = "B"
	if err := b.Verify(edV(pub)); err == nil {
		t.Fatal("a ProofBundle from run A verifies with RunID B")
	}
	// Two runs with byte-identical journals have the same root; their heads are still distinct.
	other := secThreeCalls(t, "B")
	_, thB := secHead(t, other, "B")
	if thB.SameTree(pkg.STH.TreeHead) {
		t.Fatal("identical journals of runs A and B count as the same tree")
	}
}

// An unknown format tag, a foreign public key, or any edit after sealing must not verify.
func TestEvidence_FormatKeyAndSealAreChecked(t *testing.T) {
	pkg, pub, priv := secEvidence(t)
	pkg.Format = "bide.audit.evidence.v999"
	secReject(t, "unknown format", pkg, pub, priv)

	pkg, pub, priv = secEvidence(t)
	other, _ := secKey(t)
	pkg.PublicKey = other
	secReject(t, "foreign public key", pkg, pub, nil)
	// The log key will not seal a package that names another key.
	if err := pkg.Seal(edS(priv)); err == nil {
		t.Fatal("Seal sealed a package naming a foreign public key")
	}

	pkg, pub, _ = secEvidence(t, WithLabel("Q3 refunds"))
	pkg.Label = "Q3 refunds, all approved by the CFO"
	secReject(t, "edited label", pkg, pub, nil)

	pkg, pub, _ = secEvidence(t)
	pkg.Actions = pkg.Actions[:1]
	secReject(t, "dropped item", pkg, pub, nil)
}

// A run certificate for another run (another tree) must not verify inside this package, and a
// run certificate is checked against the auditor's allowlist, not one it carries.
func TestEvidence_RunCertificateIsBoundToPackage(t *testing.T) {
	ctx := context.Background()
	pkg, pub, priv := secEvidence(t)
	other := secJournal(t, "B", []agent.Record{{Kind: agent.StepValue, Name: "x", Result: json.RawMessage(`1`)}})
	oth, _ := NewTreeHead(ctx, other, "B", 1)
	cert, err := CertifyRun(ctx, other, "B", signTH(t, oth, priv), RunCertSpec{Signer: edS(priv), TimestampNanos: 1})
	if err != nil {
		t.Fatal(err)
	}
	pkg.RunCertificate = &cert
	secReject(t, "run B's certificate in run A's package", pkg, pub, priv)

	// Its own certificate verifies only with an allowlist supplied.
	pkg, pub, priv = secEvidence(t, WithRunCertificate(RunCertSpec{}))
	if rep, _ := pkg.Verify(edV(pub)); rep.OK {
		t.Fatal("a run certificate verified with no allowlist supplied")
	}
	if rep, _ := pkg.Verify(edV(pub), WithApprovedPolicies()); !rep.OK {
		t.Fatalf("a run that used no policy failed against an empty allowlist: %+v", rep)
	}
	_ = priv
}

// The grant chain a package shows must be the grants it proves anchored.
func TestEvidence_GrantChainMatchesAnchoredLeaves(t *testing.T) {
	ctx := context.Background()
	pub, priv := secKey(t)
	_, ipriv := secKey(t)
	s := secThreeCalls(t, "A")
	g, _ := SignGrant(Grant{ID: "g", Issuer: "corp", Subject: "bot", Scope: map[string]string{"limit": "5"}}, Ed25519Signer{Priv: ipriv})
	if _, err := RecordGrant(ctx, s, "A", g); err != nil {
		t.Fatal(err)
	}
	build := func() EvidencePackage {
		pkg, err := Evidence(ctx, s, "A", edS(priv), 1, WithGrants())
		if err != nil {
			t.Fatal(err)
		}
		if rep, _ := pkg.Verify(edV(pub)); !rep.OK {
			t.Fatalf("untouched grant package does not verify: %+v", rep)
		}
		return pkg
	}
	wide, _ := SignGrant(Grant{ID: "g", Issuer: "corp", Subject: "bot", Scope: map[string]string{"limit": "5000"}}, Ed25519Signer{Priv: ipriv})
	pkg := build()
	pkg.Grants.Chain[0] = wide
	secReject(t, "a limit-5000 grant shown for the anchored limit-5 grant", pkg, pub, priv)

	pkg = build()
	pkg.Grants.Anchored = nil
	secReject(t, "a grant chain with no anchoring proofs", pkg, pub, priv)

	pkg = build()
	pkg.Grants.Anchored[0].Label = "grant corp -> cfo"
	secReject(t, "a relabelled grant", pkg, pub, priv)
}

// growingStore appends a record (through j, a Journal on the store it wraps) the second time a
// run is loaded, standing in for a journal that grows while a package is assembled.
type growingStore struct {
	agent.Store
	j     *agent.Journal
	calls int
}

func (g *growingStore) Unwrap() agent.Store { return g.Store }

func (g *growingStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	g.calls++
	if g.calls == 2 {
		_, _ = journaltest.Put(ctx, g.j, runID, "late", agent.Record{Kind: agent.StepToolResult, ToolUseID: "late", Result: json.RawMessage(`"x"`)})
	}
	return g.Store.Load(ctx, runID, after)
}

// The consistency proof a package carries must be a proof for the size it claims.
func TestEvidence_ConsistencyProofIsForItsClaimedSize(t *testing.T) {
	ctx := context.Background()
	pub, priv := secKey(t)
	inner := secThreeCalls(t, "A")
	recs, _ := inner.History(ctx, "A")
	early, _ := journalHead("A", recs[:2], 1)
	grow := &growingStore{Store: inner.Store(), j: inner}
	pkg, err := Evidence(ctx, agenttest.MustJournal(grow), "A", edS(priv), 1, WithToolCall("a"), WithConsistencyFrom(signTH(t, early, priv)))
	if err != nil {
		t.Fatal(err)
	}
	if grow.calls < 2 {
		t.Fatalf("the journal was loaded %d time(s) while the package was assembled; it never grew", grow.calls)
	}
	c := pkg.Consistency
	if c.Proof.Size != pkg.STH.Size || VerifyConsistency(early.Root, pkg.STH.Root, c.Proof) != nil {
		t.Fatalf("the package's consistency proof (first %d, size %d) does not prove the prefix against its STH", c.Proof.First, c.Proof.Size)
	}
	if rep, _ := pkg.Verify(edV(pub)); !rep.OK {
		t.Fatalf("package assembled while the journal grew does not verify: %+v", rep)
	}
}

// Delegation: issuer continuity, bounded expiry, and every scope key kept.
func TestDelegation_ChainRules(t *testing.T) {
	_, alicePriv := secKey(t)
	_, malPriv := secKey(t)
	keys := map[string]Verifier{
		"alice":   Ed25519Verifier{Pub: alicePriv.Public().(ed25519.PublicKey)},
		"bob":     Ed25519Verifier{Pub: alicePriv.Public().(ed25519.PublicKey)},
		"mallory": Ed25519Verifier{Pub: malPriv.Public().(ed25519.PublicKey)},
	}
	iv := func(id string) (Verifier, bool) { v, ok := keys[id]; return v, ok }
	rules := ScopeRules{"limit": NumericAtMost}
	root, _ := SignGrant(Grant{ID: "root", Issuer: "alice", Subject: "bob", NotAfterUnix: 1000,
		Scope: map[string]string{"limit": "100", "tool": "refund"}}, Ed25519Signer{Priv: alicePriv})
	good := func() Grant {
		return Grant{ID: "c", Issuer: "bob", Subject: "carol", ParentRef: root.Grant.Digest(), NotAfterUnix: 900,
			Scope: map[string]string{"limit": "50", "tool": "refund"}}
	}
	sign := func(g Grant, priv ed25519.PrivateKey) SignedGrant {
		sg, err := SignGrant(g, Ed25519Signer{Priv: priv})
		if err != nil {
			t.Fatal(err)
		}
		return sg
	}
	if err := VerifyDelegationChain([]SignedGrant{root, sign(good(), alicePriv)}, iv, rules); err != nil {
		t.Fatalf("a valid child was rejected: %v", err)
	}
	extra := good()
	extra.Scope["region"] = "EU" // an added constraint narrows
	if err := VerifyDelegationChain([]SignedGrant{root, sign(extra, alicePriv)}, iv, rules); err != nil {
		t.Fatalf("a child adding a constraint was rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Grant)
		signer ed25519.PrivateKey
	}{
		{"issuer is not the parent's subject", func(g *Grant) { g.Issuer = "mallory" }, malPriv},
		{"child never expires under an expiring parent", func(g *Grant) { g.NotAfterUnix = 0 }, alicePriv},
		{"child outlives its parent", func(g *Grant) { g.NotAfterUnix = 2000 }, alicePriv},
		{"child drops the tool constraint", func(g *Grant) { delete(g.Scope, "tool") }, alicePriv},
		{"child changes the tool constraint", func(g *Grant) { g.Scope["tool"] = "charge" }, alicePriv},
		{"child raises the limit", func(g *Grant) { g.Scope["limit"] = "101" }, alicePriv},
		{"child breaks the parent link", func(g *Grant) { g.ParentRef = "x" }, alicePriv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := good()
			tc.mutate(&g)
			if err := VerifyDelegationChain([]SignedGrant{root, sign(g, tc.signer)}, iv, rules); err == nil {
				t.Fatalf("chain verifies although the %s", tc.name)
			}
		})
	}
	// A child of a never-expiring parent may itself never expire.
	forever, _ := SignGrant(Grant{ID: "root2", Issuer: "alice", Subject: "bob", Scope: map[string]string{"limit": "1"}}, Ed25519Signer{Priv: alicePriv})
	child := Grant{ID: "c2", Issuer: "bob", Subject: "carol", ParentRef: forever.Grant.Digest(), Scope: map[string]string{"limit": "1"}}
	if err := VerifyDelegationChain([]SignedGrant{forever, sign(child, alicePriv)}, iv, rules); err != nil {
		t.Fatalf("a non-expiring child of a non-expiring parent was rejected: %v", err)
	}
}

// Earned authority: after a demotion the old promoted grant must stop verifying.
func TestEarnedAuthority_DemotionRevokesOldGrant(t *testing.T) {
	ctx := context.Background()
	pub, priv := secKey(t)
	logPub, logPriv := secKey(t)
	iv := func(string) (Verifier, bool) { return Ed25519Verifier{Pub: pub}, true }
	root, _ := SignGrant(Grant{ID: "root", Issuer: "corp", Subject: "ops", NotAfterUnix: 1000,
		Scope: map[string]string{"limit": "100"}}, Ed25519Signer{Priv: priv})
	ledger := agenttest.MemJournal()
	ea, err := NewEarnedAuthority(ctx, []int{10, 100}, 1, root, Ed25519Signer{Priv: priv}, "agent", ledger, "ledger")
	if err != nil {
		t.Fatal(err)
	}
	current := func() CurrentGrantProof {
		th, err := NewTreeHead(ctx, ledger, "ledger", 1)
		if err != nil {
			t.Fatal(err)
		}
		pb, err := ProveCurrentGrant(ctx, ledger, "ledger", signTH(t, th, logPriv), 0)
		if err != nil {
			t.Fatal(err)
		}
		return pb
	}
	low := ea.Grant()
	if _, err := ea.RecordCompliant(ctx); err != nil {
		t.Fatal(err)
	}
	high := ea.Grant()
	if err := VerifyCurrentGrant(high, "ledger", current(), nil, edV(logPub)); err != nil {
		t.Fatalf("the promoted grant is not current: %v", err)
	}
	if err := VerifyCurrentGrant(low, "ledger", current(), nil, edV(logPub)); err == nil {
		t.Fatal("the baseline grant is still current after promotion")
	}
	if _, err := ea.FlagAnomaly(ctx); err != nil {
		t.Fatal(err)
	}
	demoted := ea.Grant()
	if err := VerifyCurrentGrant(demoted, "ledger", current(), nil, edV(logPub)); err != nil {
		t.Fatalf("the demoted grant is not current: %v", err)
	}
	if err := VerifyCurrentGrant(high, "ledger", current(), nil, edV(logPub)); err == nil {
		t.Fatalf("after demotion to limit %d, the limit %s grant is still current", ea.Limit(), high.Grant.Scope["limit"])
	}
	// The demoted grant is a new issue, not the old baseline one, so the old baseline stays superseded.
	if demoted.Grant.Digest() == low.Grant.Digest() {
		t.Fatal("the re-issued baseline grant is the original baseline grant")
	}
	// Every earned grant also expires with its root.
	for _, g := range []SignedGrant{low, high, demoted} {
		err := VerifyDelegationChain([]SignedGrant{root, g}, iv, EarnedRules)
		if err != nil || g.Grant.NotAfterUnix != 1000 || !g.Grant.Expired(5000) {
			t.Fatalf("earned grant %s: chain err=%v not_after_unix=%d", g.Grant.ID, err, g.Grant.NotAfterUnix)
		}
	}
	// A proof from another ledger head or signed by another key does not count.
	pb := current()
	pb.Leaf.Inclusion.Index = 0
	if err := VerifyCurrentGrant(demoted, "ledger", pb, nil, edV(logPub)); err == nil {
		t.Fatal("a tampered current-grant proof verified")
	}
	if err := VerifyCurrentGrant(demoted, "ledger", current(), nil, edV(pub)); err == nil {
		t.Fatal("a current-grant proof verified under the wrong log key")
	}
}

// Negative indexes must be rejected, and the audit package and the standalone verifier must agree on
// every index and size, including negative ones, for inclusion and consistency proofs.
func TestVerifyPath_NegativeIndexAgreesWithStandalone(t *testing.T) {
	leaves := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), []byte("e")}
	root4 := merkleRoot(leaves[:4])
	if verifyPath(root4, leaves[3], -1, 4, auditPath(3, leaves[:4])) {
		t.Fatal("index -1 verifies as the last leaf")
	}
	for n := 0; n <= len(leaves); n++ {
		root := merkleRoot(leaves[:n])
		for size := -2; size <= n+1; size++ {
			for idx := -3; idx <= n+1; idx++ {
				for leaf := 0; leaf < n; leaf++ {
					path := auditPath(leaf, leaves[:n])
					lib := verifyPath(root, leaves[leaf], idx, size, path)
					std := verify.Inclusion(root, leaves[leaf], idx, size, path)
					if lib != std || (lib && (idx < 0 || size < 0)) {
						t.Fatalf("leaf %d at index %d size %d (tree of %d): audit=%v standalone=%v", leaf, idx, size, n, lib, std)
					}
				}
			}
			for m := 0; m <= n; m++ {
				early := merkleRoot(leaves[:m])
				path := consistencyProof(m, leafHashes(leaves[:n]))
				for first := -3; first <= n+1; first++ {
					lib := verifyConsistency(first, size, path, early, root)
					std := verify.Consistency(first, size, path, early, root)
					if lib != std || (lib && (first < 0 || size < 0)) {
						t.Fatalf("consistency %d..%d with a proof for %d..%d: audit=%v standalone=%v", first, size, m, n, lib, std)
					}
				}
			}
		}
	}
	if err := VerifyAbsence(merkleRoot(nil), Absence{Key: "k", Size: -1}); err == nil {
		t.Fatal("an absence proof against a negative size verified")
	}
}

// Wrong-length keys must fail to verify, not panic.
func TestWrongLengthKeys_DoNotPanic(t *testing.T) {
	for name, f := range map[string]func() bool{
		"SignedTreeHead.Verify(nil)": func() bool { return SignedTreeHead{}.Verify(nil) == nil },
		"Ed25519Verifier short key":  func() bool { return Ed25519Verifier{Pub: []byte{0xab}}.Verify([]byte("m"), nil) },
		"VerifySignature short key": func() bool {
			return VerifySignature([]byte("h"), nil, Ed25519Verifier{Pub: []byte{1}}) == nil
		},
		"MLDSAVerifier nil key": func() bool { return MLDSAVerifier{}.Verify([]byte("m"), nil) },
		"SignedTreeHead.Verify short key": func() bool {
			return SignedTreeHead{Format: STHFormat, TreeHead: TreeHead{Kind: TreeJournal}, Alg: AlgEd25519}.Verify(Ed25519Verifier{Pub: []byte{1}}) == nil
		},
		"SignedGrant.Verify short key": func() bool {
			return SignedGrant{Alg: AlgEd25519}.Verify(Ed25519Verifier{Pub: []byte{1, 2}}) == nil
		},
		"SignedGrant.Verify nil": func() bool {
			return SignedGrant{Alg: AlgEd25519}.Verify(nil) == nil
		},
		"HybridVerifier zero": func() bool { return HybridVerifier{}.Verify([]byte("m"), []byte{0, 0, 0, 0}) },
		"Ed25519Signer short key": func() bool {
			_, err := Ed25519Signer{Priv: []byte{1}}.Sign([]byte("m"))
			return err == nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked: %v", r)
				}
			}()
			if f() {
				t.Fatal("verified (or signed) with a malformed key")
			}
		})
	}
}

// A journal leaf is the record's stored bytes, so two records share a leaf only if the journal
// stores the same bytes for them. A Go string with invalid UTF-8 is stored as U+FFFD (the journal
// encoding), so the leaf commits to what the journal holds and what every reader reads; raw JSON
// keeps its bytes, so records that differ only in invalid bytes there have different leaves.
func TestLeaf_InvalidUTF8DoesNotCollide(t *testing.T) {
	salt := make([]byte, agent.SaltSize)
	a := stored(withSalt(agent.Record{Kind: agent.StepValue, Name: "pay\xff", Result: json.RawMessage(`1`)}, salt))
	la, err := canonicalRecord(a)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(la, tagged(journalLeafTag, a.Raw())) || !utf8.Valid(a.Raw()) {
		t.Fatalf("leaf %q is not the tag and the valid stored bytes %q", la, a.Raw())
	}
	// Raw JSON is committed byte for byte.
	r1 := stored(withSalt(agent.Record{Kind: agent.StepValue, Name: "raw", Result: json.RawMessage("\"\xff\"")}, salt))
	r2 := stored(withSalt(agent.Record{Kind: agent.StepValue, Name: "raw", Result: json.RawMessage("\"\xfe\"")}, salt))
	l1, err1 := canonicalRecord(r1)
	l2, err2 := canonicalRecord(r2)
	if err1 != nil || err2 != nil {
		t.Fatalf("raw JSON bytes were refused: %v, %v", err1, err2)
	}
	if bytes.Equal(l1, l2) {
		t.Fatal("records whose raw JSON differs only in invalid bytes share a leaf")
	}
	// Anchor log entries are leaves too.
	if _, err := canonicalAnchorEntry(AnchorEntry{RunID: "r\xff"}); err == nil {
		t.Fatal("an anchor entry with invalid UTF-8 was committed")
	}
}

// A grant signature must not cover a different grant that differs only in invalid UTF-8.
func TestGrant_InvalidUTF8DoesNotCollide(t *testing.T) {
	_, priv := secKey(t)
	v := Ed25519Verifier{Pub: priv.Public().(ed25519.PublicKey)}
	if _, err := SignGrant(Grant{ID: "g", Issuer: "i", Subject: "s", Scope: map[string]string{"tool": "refund\xff"}}, Ed25519Signer{Priv: priv}); err == nil {
		t.Fatal("SignGrant signed a grant with invalid UTF-8")
	}
	// A grant signed over the U+FFFD form does not verify for a raw-byte form of it.
	signed, err := SignGrant(Grant{ID: "g", Issuer: "i", Subject: "s", Scope: map[string]string{"tool": "refund�"}}, Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatal(err)
	}
	other := signed
	other.Grant.Scope = map[string]string{"tool": "refund\xfe"}
	if err := other.Verify(v); err == nil {
		t.Fatal("the signature over tool=refund\\uFFFD verifies for tool=refund\\xfe")
	}
	// Scope keys, too.
	if _, err := SignGrant(Grant{ID: "g", Scope: map[string]string{"t\xff": "x"}}, Ed25519Signer{Priv: priv}); err == nil {
		t.Fatal("SignGrant signed a grant with an invalid UTF-8 scope key")
	}
}

// narrowTo returns an AttenuateFunc that sets the child's grant to g (Issuer/Subject filled by the tool).
func narrowTo(g Grant) AttenuateFunc { return func(Grant, string) Grant { return g } }

// secDelegate runs a parent agent in runID that delegates once (tool-use id c1) through tool.
func secDelegate(t *testing.T, store *agent.Journal, tool agent.Tool, ctx context.Context, runID string) error {
	t.Helper()
	parent := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"go"}`), agent.TextTurn("ok")),
		store,
		agent.WithTools(tool),
	)
	_, err := parent.Run(ctx, runID, agent.UserText("go"))
	return err
}

func secGrantsIn(t *testing.T, store *agent.Journal, runID string) []SignedGrant {
	t.Helper()
	recs, _ := store.History(context.Background(), runID)
	var out []SignedGrant
	for _, r := range recs {
		var sg SignedGrant
		if r.Kind == agent.StepValue && json.Unmarshal(r.Result, &sg) == nil && len(sg.Sig) > 0 {
			out = append(out, sg)
		}
	}
	return out
}

// AttenuatingSubAgent must refuse to sign a child that does not narrow its parent.
func TestAttenuatingSubAgent_RefusesWidening(t *testing.T) {
	_, priv := secKey(t)
	signer := Ed25519Signer{Priv: priv}
	rootSG, _ := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", NotAfterUnix: 1000, Scope: map[string]string{"limit": "7", "tool": "refund"}}, signer)
	for name, child := range map[string]Grant{
		"a wider limit":      {ID: "wide", Scope: map[string]string{"limit": "7000", "tool": "refund"}},
		"a dropped tool":     {ID: "drop", Scope: map[string]string{"limit": "3"}},
		"a later expiry":     {ID: "late", NotAfterUnix: 2000, Scope: map[string]string{"limit": "3", "tool": "refund"}},
		"a different issuer": {ID: "iss", Issuer: "mallory", Scope: map[string]string{"limit": "3", "tool": "refund"}},
	} {
		store := agenttest.MemJournal()
		tool := AttenuatingSubAgent("exec", "x", agenttest.MustNew(answerModel{"done"}, store), AttenuationConfig{Store: store, Narrow: narrowTo(child), Rules: ScopeRules{"limit": NumericAtMost}})
		_, err := tool.Call(WithGrant(context.Background(), rootSG, signer), []byte(`{"task":"go"}`))
		if err == nil || !strings.Contains(err.Error(), "grant") {
			t.Fatalf("the tool delegated a child grant with %s (err %v)", name, err)
		}
	}
}

// AttenuatingSubAgent must carry the parent's expiry to a child that sets none.
func TestAttenuatingSubAgent_InheritsNotAfter(t *testing.T) {
	store := agenttest.MemJournal()
	_, priv := secKey(t)
	signer := Ed25519Signer{Priv: priv}
	notAfter := time.Now().Add(time.Hour).Unix() // unexpired: an expired grant cannot be delegated
	rootSG, _ := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", NotAfterUnix: notAfter, Scope: map[string]string{"limit": "7"}}, signer)
	tool := AttenuatingSubAgent("exec", "x", agenttest.MustNew(answerModel{"done"}, store), AttenuationConfig{Store: store, Narrow: narrowTo(Grant{ID: "narrow", Scope: map[string]string{"limit": "3"}}), Rules: ScopeRules{"limit": NumericAtMost}})
	if err := secDelegate(t, store, tool, WithGrant(context.Background(), rootSG, signer), "p1"); err != nil {
		t.Fatal(err)
	}
	grants := secGrantsIn(t, store, agent.SubRunID("p1", "c1"))
	if len(grants) != 1 || grants[0].Grant.NotAfterUnix != notAfter {
		t.Fatalf("child grants %+v, want one with the parent's not_after %d", grants, notAfter)
	}
}

// Two delegations from different parent runs must not share one sub-run journal.
func TestAttenuatingSubAgent_DoesNotShareSubRunAcrossParents(t *testing.T) {
	store := agenttest.MemJournal()
	_, priv := secKey(t)
	signer := Ed25519Signer{Priv: priv}
	tool := AttenuatingSubAgent("exec", "x", agenttest.MustNew(answerModel{"done"}, store), AttenuationConfig{Store: store, Narrow: narrowTo(Grant{ID: "narrow", Scope: map[string]string{"limit": "3"}}), Rules: ScopeRules{"limit": NumericAtMost}})
	for _, desk := range []string{"desk-a", "desk-b"} {
		sg, _ := SignGrant(Grant{ID: desk, Issuer: "corp", Subject: desk, Scope: map[string]string{"limit": "7"}}, signer)
		ctx := WithGrant(context.Background(), sg, signer)
		// Called outside an agent run, it refuses instead of using a sub-run ID shared by every parent.
		if _, err := tool.Call(ctx, []byte(`{"task":"go"}`)); err == nil {
			t.Fatal("a delegation with no run scope was accepted")
		}
		if err := secDelegate(t, store, tool, ctx, "run-"+desk); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(secGrantsIn(t, store, "sub/exec")); n != 0 {
		t.Fatalf("%d grants recorded in the shared sub-run %q", n, "sub/exec")
	}
	for _, desk := range []string{"desk-a", "desk-b"} {
		grants := secGrantsIn(t, store, agent.SubRunID("run-"+desk, "c1"))
		if len(grants) != 1 || grants[0].Grant.Issuer != desk {
			t.Fatalf("sub-run of run-%s holds grants %+v, want only its own", desk, grants)
		}
	}
}

// A constraint whose value is empty is still a constraint: a child cannot drop it.
func TestCheckAttenuation_EmptyConstraintIsKept(t *testing.T) {
	parent := Grant{ID: "p", Subject: "bob", Scope: map[string]string{"region": ""}}
	child := Grant{ID: "c", Issuer: "bob", ParentRef: parent.Digest()}
	if err := CheckAttenuation(parent, child, nil); err == nil {
		t.Fatal("a child dropped its parent's empty-valued constraint")
	}
	child.Scope = map[string]string{"region": ""}
	if err := CheckAttenuation(parent, child, nil); err != nil {
		t.Fatalf("a child keeping the constraint was rejected: %v", err)
	}
}

// A package with invalid UTF-8 is refused a seal, since JSON would rewrite it.
func TestEvidence_SealRefusesInvalidUTF8(t *testing.T) {
	pkg, _, priv := secEvidence(t)
	pkg.Label = "refunds\xff"
	if err := pkg.Seal(edS(priv)); err == nil {
		t.Fatal("a package with an invalid UTF-8 label was sealed")
	}
}

// interferingStore records a foreign leaf in the ledger (through j, a Journal on the store it
// wraps) just before each grant is inserted, as a second writer racing the controller would.
type interferingStore struct {
	agent.Store
	j *agent.Journal
	n int
}

func (s *interferingStore) Unwrap() agent.Store { return s.Store }

func (s *interferingStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if strings.HasPrefix(name, grantLeafPrefix) {
		s.n++
		_, _ = journaltest.Put(ctx, s.j, runID, "foreign/"+string(rune('a'+s.n)), agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)})
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// interfering returns a Journal over an interferingStore on a new MemStore.
func interfering() *agent.Journal {
	mem := agent.NewMemStore()
	return agenttest.MustJournal(&interferingStore{Store: mem, j: agenttest.MustJournal(mem)})
}

// The controller refuses to hand out a grant that did not land where it expected in the ledger.
func TestEarnedAuthority_DetectsAnotherLedgerWriter(t *testing.T) {
	_, priv := secKey(t)
	root, _ := SignGrant(Grant{ID: "root", Issuer: "corp", Subject: "ops", Scope: map[string]string{"limit": "100"}}, Ed25519Signer{Priv: priv})
	if _, err := NewEarnedAuthority(context.Background(), []int{10}, 1, root, Ed25519Signer{Priv: priv}, "agent", interfering(), "ledger"); err == nil {
		t.Fatal("the controller issued a grant into a ledger another writer was appending to")
	}
}

// A signed key-set head commits to the journal tree it was projected from.
func TestAbsenceHead_JournalReferenceIsSigned(t *testing.T) {
	pub, priv := secKey(t)
	s := secThreeCalls(t, "A")
	recs, th := secHead(t, s, "A")
	abs, err := SignAbsenceRoot(recs, ToolUseKeys, th, edS(priv), 1)
	if err != nil {
		t.Fatal(err)
	}
	for name, ref := range map[string]TreeRef{
		"size": {Size: abs.Journal.Size - 1, Root: abs.Journal.Root},
		"root": {Size: abs.Journal.Size, Root: merkleRoot(nil)},
	} {
		bad := abs
		bad.Journal = &ref
		if bad.Verify(edV(pub)) == nil {
			t.Fatalf("a key-set head verified with its journal %s changed", name)
		}
	}
}

// Each check on an absence bundle and its producer stands on its own.
func TestAbsenceBundle_EveryBindingIsChecked(t *testing.T) {
	pub, priv := secKey(t)
	s := secThreeCalls(t, "A")
	recs, th := secHead(t, s, "A")
	toolSTH, err := SignAbsenceRoot(recs, ToolUseKeys, th, edS(priv), 1)
	if err != nil {
		t.Fatal(err)
	}
	good, err := ProveAbsentBundle(recs, ToolUseKeys, ToolUseKeyFor("z"), toolSTH)
	if err != nil {
		t.Fatal(err)
	}
	if err := good.Verify(edV(pub), ToolUseKeys); err != nil {
		t.Fatal("the genuine bundle does not verify")
	}
	relabelled := good
	relabelled.RunID = "B"
	if err := relabelled.Verify(edV(pub), ToolUseKeys); err == nil {
		t.Fatal("run A's absence bundle verified as run B's")
	}
	// A key set with no prefix would accept any key, e.g. a policy key in the tool-use tree.
	forged := AbsenceBundle{Format: AbsenceFormat, RunID: "A", STH: toolSTH, Absence: Absence{Key: PolicyUsedKeyFor("EVIL"), Size: 3,
		Right: good.Absence.Left}}
	if err := forged.Verify(edV(pub), KeySet{Kind: TreeToolUse, Key: ToolUseKey}); err == nil {
		t.Fatal("a bundle verified against a key set with no prefix")
	}
	if _, err := ProveAbsent(recs, ToolUseKeys, PolicyUsedKeyFor("x")); err == nil {
		t.Fatal("ProveAbsent proved a policy key absent from the tool-use set")
	}
	// With no tool calls and no policies, the two key sets have the same (empty) root; only the
	// kind tells them apart.
	vals := secJournal(t, "V", []agent.Record{{Kind: agent.StepValue, Name: "v", Result: json.RawMessage(`1`)}})
	vrecs, vth := secHead(t, vals, "V")
	emptyTool, err := SignAbsenceRoot(vrecs, ToolUseKeys, vth, edS(priv), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ProveAbsentBundle(vrecs, PolicyUsedKeys, PolicyUsedKeyFor("x"), emptyTool); err == nil {
		t.Fatal("ProveAbsentBundle proved a policy absent against a tool-use head")
	}
	// Records of another journal with the same key set are not the journal the head names.
	other := secJournal(t, "A", []agent.Record{
		{Kind: agent.StepToolResult, ToolUseID: "a", Result: json.RawMessage(`"9"`)},
		{Kind: agent.StepToolResult, ToolUseID: "b", Result: json.RawMessage(`"9"`)},
		{Kind: agent.StepToolResult, ToolUseID: "c", Result: json.RawMessage(`"9"`)},
	})
	orecs, _ := other.History(context.Background(), "A")
	if _, err := ProveAbsentBundle(orecs, ToolUseKeys, ToolUseKeyFor("z"), toolSTH); err == nil {
		t.Fatal("ProveAbsentBundle used records of another journal with the same key set")
	}
	// An empty key set has one root; a proof against any other root is not an absence proof.
	if err := VerifyAbsence([]byte("not the empty root"), Absence{Key: "tooluse:x"}); err == nil {
		t.Fatal("an empty-set absence proof verified against a non-empty root")
	}
}

// A key-set head never verifies as a journal head, even when its leaves are journal leaves.
func TestProofBundle_RequiresAJournalHead(t *testing.T) {
	pub, priv := secKey(t)
	s := secJournal(t, "A", []agent.Record{{Kind: agent.StepValue, Name: "v", Result: json.RawMessage(`1`)}})
	recs, th := secHead(t, s, "A")
	// A caller-defined key set whose key is the record's own JSON: its tree equals the journal tree.
	raw := KeySet{Kind: "absence/raw", Prefix: "{", Key: func(r agent.Record) []string {
		b, _ := json.Marshal(r)
		return []string{string(b)}
	}}
	abs, err := SignAbsenceRoot(recs, raw, th, edS(priv), 1)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := ProveRecord(context.Background(), s, "A", 0, signTH(t, th, priv))
	if err != nil {
		t.Fatal(err)
	}
	pb.STH = abs
	if err := pb.Verify(edV(pub)); err == nil {
		t.Fatal("a record proof verified against a key-set head")
	}
}

// Evidence items must be proven against the package's own tree, and the package's run and grant
// labels are checked on their own.
func TestEvidence_EveryBindingIsChecked(t *testing.T) {
	ctx := context.Background()

	// The package run ID alone relabelled and resealed by the log key (no consistency proof, whose
	// earlier head would also name run A).
	pub, priv := secKey(t)
	pkg, err := Evidence(ctx, secThreeCalls(t, "A"), "A", edS(priv), 1)
	if err != nil {
		t.Fatal(err)
	}
	pkg.RunID = "B"
	secReject(t, "the package relabelled as run B", pkg, pub, priv)

	// An action proven against an earlier head of the same run.
	pkg, pub, priv = secEvidence(t)
	s := secThreeCalls(t, "A")
	recs, _ := s.History(ctx, "A")
	early, _ := journalHead("A", recs[:1], 1)
	pb, err := ProveRecord(ctx, s, "A", 0, signTH(t, early, priv))
	if err != nil {
		t.Fatal(err)
	}
	pkg.Actions[0].Bundle = pb
	secReject(t, "an action proven against another tree", pkg, pub, priv)

	// A consistency proof from the empty tree, presented as one from the earlier head.
	pkg, pub, priv = secEvidence(t)
	pkg.Consistency.Proof = Consistency{First: 0, Size: pkg.STH.Size}
	secReject(t, "a consistency proof from size 0 labelled as from size 1", pkg, pub, priv)

	// A proof whose sizes are not the two signed sizes, from an empty earlier head.
	pkg, pub, priv = secEvidence(t)
	empty, _ := journalHead("A", nil, 0)
	pkg.Consistency = &EvidenceConsistency{From: signTH(t, empty, priv), Proof: Consistency{First: 0, Size: 99}}
	secReject(t, "a consistency proof for size 99 against a head of size 3", pkg, pub, priv)

	// An anchoring proof relabelled as a step (its label and ref match a step of that record).
	gs := secThreeCalls(t, "G")
	_, ipriv := secKey(t)
	g, _ := SignGrant(Grant{ID: "g", Issuer: "corp", Subject: "bot", Scope: map[string]string{"limit": "5"}}, Ed25519Signer{Priv: ipriv})
	if _, err := RecordGrant(ctx, gs, "G", g); err != nil {
		t.Fatal(err)
	}
	gpkg, err := Evidence(ctx, gs, "G", edS(priv), 1, WithGrants())
	if err != nil {
		t.Fatal(err)
	}
	a := &gpkg.Grants.Anchored[0]
	a.Kind, a.Label, a.Ref = KindStep, recOf(t, a.Bundle).Name, recOf(t, a.Bundle).Name
	secReject(t, "a grant anchoring proof relabelled as a step", gpkg, pub, priv)
}

// An anchor log entry's run is the run its signed head names.
func TestMemAnchorLog_RejectsAHeadOfAnotherRun(t *testing.T) {
	_, priv := secKey(t)
	s := secThreeCalls(t, "A")
	_, th := secHead(t, s, "A")
	if err := NewMemAnchorLog().Publish(context.Background(), "B", signTH(t, th, priv)); err == nil {
		t.Fatal("the anchor log recorded run A's head as run B's")
	}
}

// A genuine inclusion proof of a superseded grant in the latest ledger head is not a current proof.
func TestEarnedAuthority_OldLeafIsNotCurrent(t *testing.T) {
	ctx := context.Background()
	_, priv := secKey(t)
	logPub, logPriv := secKey(t)
	root, _ := SignGrant(Grant{ID: "root", Issuer: "corp", Subject: "ops", Scope: map[string]string{"limit": "100"}}, Ed25519Signer{Priv: priv})
	ledger := agenttest.MemJournal()
	ea, err := NewEarnedAuthority(ctx, []int{10, 100}, 1, root, Ed25519Signer{Priv: priv}, "agent", ledger, "ledger")
	if err != nil {
		t.Fatal(err)
	}
	first := ea.Grant()
	if _, err := ea.RecordCompliant(ctx); err != nil {
		t.Fatal(err)
	}
	th, _ := NewTreeHead(ctx, ledger, "ledger", 1)
	old, err := ProveRecord(ctx, ledger, "ledger", 0, signTH(t, th, logPriv))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyCurrentGrant(first, "ledger", CurrentGrantProof{Format: CurrentGrantFormat, Leaf: old}, nil, edV(logPub)); err == nil {
		t.Fatal("a superseded grant verified as current from a proof of its (non-last) ledger leaf")
	}
}

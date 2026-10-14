package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

func edKeys(t *testing.T) (ed25519.PublicKey, audit.Signer) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, edS(priv)
}

// A delegation chain is refused in every way it can break, each as ErrNotVerified (or ErrConfig for
// a caller that gave no way to find issuer keys).
func TestVerifyDelegationChain_Refusals(t *testing.T) {
	pubA, sA := edKeys(t)
	pubB, sB := edKeys(t)
	resolve := func(issuer string) (audit.Verifier, bool) {
		switch issuer {
		case "a":
			return edV(pubA), true
		case "b":
			return edV(pubB), true
		case "nil":
			return nil, true
		}
		return nil, false
	}
	rules := audit.EarnedRules
	root, err := audit.SignGrant(audit.Grant{ID: "root", Issuer: "a", Subject: "boss", Scope: map[string]string{"limit": "10"}}, sA)
	if err != nil {
		t.Fatal(err)
	}
	child := func(g audit.Grant, s audit.Signer) audit.SignedGrant {
		sg, err := audit.SignGrant(g, s)
		if err != nil {
			t.Fatal(err)
		}
		return sg
	}
	okChild := child(audit.Grant{ID: "c", Issuer: "boss", Subject: "desk", Scope: map[string]string{"limit": "5"}, ParentRef: root.Grant.Digest()}, sB)
	resolveBoss := func(issuer string) (audit.Verifier, bool) {
		if issuer == "boss" {
			return edV(pubB), true
		}
		return resolve(issuer)
	}
	if err := audit.VerifyDelegationChain([]audit.SignedGrant{root, okChild}, resolveBoss, rules); err != nil {
		t.Fatalf("genuine chain: %v", err)
	}
	tampered := root
	tampered.Sig = append([]byte(nil), root.Sig...)
	tampered.Sig[0] ^= 1
	rootWithParent := child(audit.Grant{ID: "r2", Issuer: "a", Subject: "boss", ParentRef: "xx", Scope: map[string]string{"limit": "10"}}, sA)
	wider := child(audit.Grant{ID: "w", Issuer: "boss", Subject: "desk", Scope: map[string]string{"limit": "50"}, ParentRef: root.Grant.Digest()}, sB)
	_, mlPriv := func() (*mldsa.PublicKey, *mldsa.PrivateKey) {
		k, err := mldsa.GenerateKey(mldsa.MLDSA65())
		if err != nil {
			t.Fatal(err)
		}
		return k.PublicKey(), k
	}()
	mlV, err := audit.NewVerifier(audit.AlgMLDSA65, mlPriv.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		chain   []audit.SignedGrant
		resolve func(string) (audit.Verifier, bool)
		want    error
		text    string
	}{
		"empty":                {nil, resolve, audit.ErrNotVerified, "empty delegation chain"},
		"no resolver":          {[]audit.SignedGrant{root}, nil, agent.ErrConfig, "no issuer verifier resolver"},
		"unknown issuer":       {[]audit.SignedGrant{root, okChild}, resolve, audit.ErrNotVerified, `no verifier for issuer "boss"`},
		"resolver returns nil": {[]audit.SignedGrant{child(audit.Grant{ID: "n", Issuer: "nil"}, sA)}, resolve, audit.ErrNotVerified, `no verifier for issuer "nil"`},
		"bad signature":        {[]audit.SignedGrant{tampered}, resolve, audit.ErrNotVerified, "signature does not verify"},
		"root with a parent":   {[]audit.SignedGrant{rootWithParent}, resolve, audit.ErrNotVerified, "carries a parent_ref"},
		"child widens scope":   {[]audit.SignedGrant{root, wider}, resolveBoss, audit.ErrNotVerified, ""},
		"scheme mismatch": {[]audit.SignedGrant{root}, func(string) (audit.Verifier, bool) { return mlV, true }, audit.ErrNotVerified,
			`is signed under "ed25519", not the verifier's "ml-dsa-65"`},
	} {
		err := audit.VerifyDelegationChain(tc.chain, tc.resolve, rules)
		if !errors.Is(err, tc.want) || !strings.Contains(fmtErr(err), tc.text) {
			t.Errorf("%s: err = %v, want %v containing %q", name, err, tc.want, tc.text)
		}
	}
	if err := root.Verify(nil); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("SignedGrant.Verify(nil) = %v, want ErrConfig", err)
	}
}

func fmtErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// A superseded earned grant is not the current one, even though its leaf is in the ledger and its
// proof verifies; and flagging an anomaly at the baseline changes nothing.
func TestEarnedAuthority_SupersededGrantIsNotCurrent(t *testing.T) {
	ctx := context.Background()
	pub, signer := edKeys(t)
	root, err := audit.SignGrant(audit.Grant{ID: "root", Issuer: "corp", Subject: "desk", Scope: map[string]string{audit.EarnedScopeLimit: "10"}}, signer)
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenttest.MemJournal()
	ea, err := audit.NewEarnedAuthority(ctx, []int{2, 5}, 1, root, signer, "exec", ledger, "ledger")
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := ea.FlagAnomaly(ctx); err != nil || changed {
		t.Errorf("FlagAnomaly at the baseline = %v, %v; want no change", changed, err)
	}
	before, _ := ledger.History(ctx, "ledger")
	if after := mustHistory(t, ledger, "ledger"); len(after) != len(before) {
		t.Errorf("a no-op FlagAnomaly wrote to the ledger")
	}
	old := ea.Grant()
	if promoted, err := ea.RecordCompliant(ctx); err != nil || !promoted {
		t.Fatalf("RecordCompliant = %v, %v", promoted, err)
	}
	recs := mustHistory(t, ledger, "ledger")
	th, err := audit.NewTreeHead(ctx, ledger, "ledger", 1)
	if err != nil {
		t.Fatal(err)
	}
	sth, err := audit.SignTreeHead(th, signer)
	if err != nil {
		t.Fatal(err)
	}
	logV := edV(pub)
	// The current grant proves as current.
	cur, err := audit.ProveCurrentGrant(ctx, ledger, "ledger", sth, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.VerifyCurrentGrant(ea.Grant(), "ledger", cur, nil, logV); err != nil {
		t.Fatalf("the current grant: %v", err)
	}
	// The old grant's leaf, proven under the same head, is not the last leaf.
	var oldIdx = -1
	for i, r := range recs {
		if strings.HasPrefix(r.Name, "audit:grant:"+old.Grant.Digest()) {
			oldIdx = i
		}
	}
	if oldIdx < 0 {
		t.Fatal("old grant leaf not found")
	}
	leaf, err := audit.ProveRecord(ctx, ledger, "ledger", oldIdx, sth)
	if err != nil {
		t.Fatal(err)
	}
	stale := audit.CurrentGrantProof{Format: audit.CurrentGrantFormat, Leaf: leaf, Consistency: cur.Consistency}
	err = audit.VerifyCurrentGrant(old, "ledger", stale, nil, logV)
	if !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(err.Error(), "superseded") {
		t.Errorf("a superseded grant = %v, want ErrNotVerified saying superseded", err)
	}
	// ProveCurrentGrant refuses an empty ledger head and a last-seen size past the head.
	if _, err := audit.ProveCurrentGrant(ctx, ledger, "ledger", audit.SignedTreeHead{}, 0); err == nil || !strings.Contains(err.Error(), "is empty at this head") {
		t.Errorf("ProveCurrentGrant(empty head) = %v", err)
	}
	if _, err := audit.ProveCurrentGrant(ctx, ledger, "ledger", sth, sth.Size+1); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("ProveCurrentGrant(last seen past the head) = %v", err)
	}
}

func mustHistory(t *testing.T, j *agent.Journal, run string) []agent.Record {
	t.Helper()
	recs, err := j.History(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// The anchor log refuses an entry that is not there, and an inclusion check refuses an entry that
// misstates its position, its run, or its leaf.
func TestAnchorLog_Refusals(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	record(t, j, "r", "a", "b")
	pub, signer := edKeys(t)
	th, _ := audit.NewTreeHead(ctx, j, "r", 1)
	sth, err := audit.SignTreeHead(th, signer)
	if err != nil {
		t.Fatal(err)
	}
	log := audit.NewMemAnchorLog()
	if err := log.Publish(ctx, "other", sth); err == nil || !strings.Contains(err.Error(), `tree head is for run "r", not "other"`) {
		t.Errorf("Publish under another run = %v", err)
	}
	if err := log.Publish(ctx, "r", sth); err != nil {
		t.Fatal(err)
	}
	if err := log.Publish(ctx, "r", sth); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int{-1, 2, 99} {
		if _, err := log.Prove(seq); err == nil || !strings.Contains(err.Error(), "out of range [0,2)") {
			t.Errorf("Prove(%d) = %v, want out of range", seq, err)
		}
	}
	for _, first := range []int{-1, 3} {
		if _, err := log.ProveConsistency(first); err == nil || !strings.Contains(err.Error(), "out of range [0,2]") {
			t.Errorf("ProveConsistency(%d) = %v, want out of range", first, err)
		}
	}
	root, err := log.Root()
	if err != nil {
		t.Fatal(err)
	}
	entries := log.Entries()
	proof, err := log.Prove(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.VerifyAnchorInclusion(root, entries[1], proof); err != nil {
		t.Fatalf("genuine inclusion: %v", err)
	}
	for name, mutate := range map[string]func(e *audit.AnchorEntry){
		"another position": func(e *audit.AnchorEntry) { e.Seq = 0 },
		"another run":      func(e *audit.AnchorEntry) { e.RunID = "x" },
		"a changed head":   func(e *audit.AnchorEntry) { e.STH.TimestampNanos++ },
	} {
		e := entries[1]
		mutate(&e)
		if err := audit.VerifyAnchorInclusion(root, e, proof); !errors.Is(err, audit.ErrNotVerified) {
			t.Errorf("%s: err = %v, want ErrNotVerified", name, err)
		}
	}
	bad := entries[1]
	bad.Format = "bide.audit.anchor.v0"
	if err := audit.VerifyAnchorInclusion(root, bad, proof); !errors.Is(err, audit.ErrFormat) {
		t.Errorf("an entry of another format = %v, want ErrFormat", err)
	}
	if err := entries[1].STH.Verify(edV(pub)); err != nil {
		t.Errorf("the anchored head: %v", err)
	}
}

// A key set needs a kind, a prefix and a key function; every producer that takes one says so
// instead of building a tree over nothing.
func TestKeySet_ZeroValueIsRefused(t *testing.T) {
	_, signer := edKeys(t)
	var zero audit.KeySet
	th := audit.TreeHead{Kind: audit.TreeJournal, RunID: "r"}
	if _, _, err := audit.AbsenceRoot(nil, zero); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("AbsenceRoot(zero set) = %v, want ErrConfig", err)
	}
	if _, err := audit.ProveAbsent(nil, zero, "k"); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("ProveAbsent(zero set) = %v, want ErrConfig", err)
	}
	if _, err := audit.NewAbsenceTreeHead(nil, zero, th, 1); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("NewAbsenceTreeHead(zero set) = %v, want ErrConfig", err)
	}
	if _, err := audit.SignAbsenceRoot(nil, zero, th, signer, 1); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("SignAbsenceRoot(zero set) = %v, want ErrConfig", err)
	}
	if _, err := audit.ProveAbsent(nil, audit.ToolUseKeys, "policy_used:x"); err == nil || !strings.Contains(err.Error(), "is not in the") {
		t.Errorf("ProveAbsent(a key of another set) = %v", err)
	}
	if set, ok := audit.KeySetForKey("tooluse:c1"); !ok || set.Kind != audit.TreeToolUse {
		t.Errorf("KeySetForKey(tool key) = %v, %v", set.Kind, ok)
	}
	if set, ok := audit.KeySetForKey("policy_used:d"); !ok || set.Kind != audit.TreePolicyUsed {
		t.Errorf("KeySetForKey(policy key) = %v, %v", set.Kind, ok)
	}
	if _, ok := audit.KeySetForKey("nonsense:1"); ok {
		t.Error("KeySetForKey(unknown prefix) found a set")
	}
}

func TestUnmarshalStrict_Targets(t *testing.T) {
	var v struct{ A int }
	if err := audit.UnmarshalStrict([]byte(`{"A":1}`), v); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("a non-pointer target = %v, want ErrConfig", err)
	}
	var np *struct{ A int }
	if err := audit.UnmarshalStrict([]byte(`{"A":1}`), np); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("a nil pointer target = %v, want ErrConfig", err)
	}
	if err := audit.UnmarshalStrict([]byte("{\"A\":1,\"B\":\"\xff\"}"), &v); !errors.Is(err, audit.ErrMalformed) || !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Errorf("invalid UTF-8 = %v, want ErrMalformed", err)
	}
	if err := audit.UnmarshalStrict([]byte(`{"A":1}`), &v); err != nil || v.A != 1 {
		t.Errorf("a plain object = %v, %d", err, v.A)
	}
}

// DecisionTime.Gap is the recording delay, and is unknown rather than wrong when either time is
// missing or not canonical, or when the two are too far apart to hold in a duration.
func TestDecisionTime_Gap(t *testing.T) {
	for name, tc := range map[string]struct {
		d   audit.DecisionTime
		gap string
		ok  bool
	}{
		"after":         {audit.DecisionTime{DecidedAt: "2026-10-13T12:00:00.000Z", RecordedAt: "2026-10-13T12:00:02.500Z"}, "2.5s", true},
		"before":        {audit.DecisionTime{DecidedAt: "2026-10-13T12:01:00.000Z", RecordedAt: "2026-10-13T12:00:00.000Z"}, "-1m0s", true},
		"no recorded":   {audit.DecisionTime{DecidedAt: "2026-10-13T12:00:00.000Z"}, "0s", false},
		"not canonical": {audit.DecisionTime{DecidedAt: "2026-10-13 12:00:00", RecordedAt: "2026-10-13T12:00:00.000Z"}, "0s", false},
		"too far":       {audit.DecisionTime{DecidedAt: "0001-01-01T00:00:00.000Z", RecordedAt: "9999-12-31T23:59:59.999Z"}, "0s", false},
		"too far back":  {audit.DecisionTime{DecidedAt: "9999-12-31T23:59:59.999Z", RecordedAt: "0001-01-01T00:00:00.000Z"}, "0s", false},
	} {
		gap, ok := tc.d.Gap()
		if ok != tc.ok || gap.String() != tc.gap {
			t.Errorf("%s: Gap = %v, %v; want %s, %v", name, gap, ok, tc.gap, tc.ok)
		}
	}
}

// VerifyRun reports why a certificate fails rather than only that it does: a property set that is
// not the one this version certifies, and convergence evidence that does not cover the used policies.
func TestVerifyRun_ReasonsForAMutatedCertificate(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	record(t, j, "r", "a")
	pub, signer := edKeys(t)
	th, _ := audit.NewTreeHead(ctx, j, "r", 1)
	sth, err := audit.SignTreeHead(th, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := audit.CertifyRun(ctx, j, "r", sth, audit.RunCertSpec{Signer: signer, TimestampNanos: 2})
	if err != nil {
		t.Fatal(err)
	}
	res, err := audit.VerifyRun(cert, nil, edV(pub))
	if err != nil || !res.OK {
		t.Fatalf("a genuine certificate: %+v, %v", res, err)
	}
	extra := cert
	extra.Properties = append([]string{"also-trust-me"}, cert.Properties...)
	extra.UsedPolicies = []string{"d1"}
	res, err = audit.VerifyRun(extra, []string{"d1"}, edV(pub))
	if !errors.Is(err, audit.ErrNotVerified) || res.OK {
		t.Fatalf("a mutated certificate verified: %+v, %v", res, err)
	}
	joined := strings.Join(res.Reasons, "\n")
	for _, want := range []string{"are not the v1 set", "0 convergence entries for 1 used policies"} {
		if !strings.Contains(joined, want) {
			t.Errorf("reasons lack %q:\n%s", want, joined)
		}
	}
}

// A run with no anchored grants cannot be given grant evidence, and a consistency claim from a
// head that is not an earlier state of this journal is refused.
func TestEvidence_OptionsThatCannotBeMet(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	record(t, j, "r", "a", "b")
	_, signer := edKeys(t)
	if _, err := audit.Evidence(ctx, j, "r", signer, 1, audit.WithGrants()); err == nil || !strings.Contains(err.Error(), "WithGrants requested but run r has no anchored grant leaves") {
		t.Errorf("WithGrants over a run with none = %v", err)
	}
	other := agenttest.MemJournal()
	record(t, other, "r", "x")
	th, _ := audit.NewTreeHead(ctx, other, "r", 1)
	foreign, err := audit.SignTreeHead(th, signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Evidence(ctx, j, "r", signer, 1, audit.WithConsistencyFrom(foreign)); err == nil || !strings.Contains(err.Error(), "consistency") {
		t.Errorf("WithConsistencyFrom a foreign head = %v, want a consistency error", err)
	}
}

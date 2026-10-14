package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

var errBoom = errors.New("store is down")

// flakyStore is an agent.Store whose reads fail once failLoad is set.
type flakyStore struct {
	agent.Store
	failLoad atomic.Bool
}

func (s *flakyStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	if s.failLoad.Load() {
		return func(yield func(agent.Entry, error) bool) { yield(agent.Entry{}, errBoom) }
	}
	return s.Store.Load(ctx, runID, after)
}

// A journal that cannot be read must fail every producer that reads it, with the store's error
// reachable through errors.Is, never a proof or head built over a partial journal.
func TestProducers_PropagateAJournalReadFailure(t *testing.T) {
	ctx := context.Background()
	fs := &flakyStore{Store: agent.NewMemStore()}
	j := agenttest.MustJournal(fs)
	const run = "r"
	record(t, j, run, "a", "b", "c")
	policy, digest := []byte("(doms 1 1)\n"), strings.Repeat("ab", 32)
	if _, err := audit.RecordPolicy(ctx, j, run, policy, digest); err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := edS(priv)
	th, err := audit.NewTreeHead(ctx, j, run, 1)
	if err != nil {
		t.Fatal(err)
	}
	sth, err := audit.SignTreeHead(th, signer)
	if err != nil {
		t.Fatal(err)
	}
	evs := audit.NewMemEventStore()

	fs.failLoad.Store(true)
	for name, call := range map[string]func() error{
		"Head":              func() error { _, err := audit.Head(ctx, j, run); return err },
		"Root":              func() error { _, err := audit.Root(ctx, j, run); return err },
		"Prove":             func() error { _, err := audit.Prove(ctx, j, run, 1); return err },
		"NewTreeHead":       func() error { _, err := audit.NewTreeHead(ctx, j, run, 1); return err },
		"ProveRecord":       func() error { _, err := audit.ProveRecord(ctx, j, run, 1, sth); return err },
		"ProveToolCall":     func() error { _, err := audit.ProveToolCall(ctx, j, run, "t", sth); return err },
		"ProveStep":         func() error { _, err := audit.ProveStep(ctx, j, run, "s0", sth); return err },
		"ProvePolicy":       func() error { _, err := audit.ProvePolicy(ctx, j, run, digest, sth); return err },
		"ProveConvergence":  func() error { _, err := audit.ProveConvergence(ctx, j, run, digest, sth); return err },
		"ProveGrant":        func() error { _, err := audit.ProveGrant(ctx, j, run, digest, sth); return err },
		"ProveRunCert":      func() error { _, err := audit.ProveRunCertificate(ctx, j, run, sth); return err },
		"ProveConsistency":  func() error { _, err := audit.ProveConsistency(ctx, j, run, 1); return err },
		"ProveApproval":     func() error { _, err := audit.ProveApproval(ctx, j, run, "s0", sth); return err },
		"ApprovalEvidence":  func() error { _, err := audit.ApprovalEvidence(ctx, j, run, "c1", sth); return err },
		"ProveCurrentGrant": func() error { _, err := audit.ProveCurrentGrant(ctx, j, run, sth, 0); return err },
		"ExportJournal":     func() error { _, err := audit.ExportJournal(ctx, j, run); return err },
		"Evidence":          func() error { _, err := audit.Evidence(ctx, j, run, signer, 1); return err },
		"CertifyRun": func() error {
			_, err := audit.CertifyRun(ctx, j, run, sth, audit.RunCertSpec{Signer: signer, TimestampNanos: 2})
			return err
		},
		"EventLogFromJournal": func() error { _, err := audit.EventLogFromJournal(ctx, j, run); return err },
		"PersistJournal":      func() error { return audit.PersistJournal(ctx, evs, j, run) },
	} {
		err := call()
		if !errors.Is(err, errBoom) {
			t.Errorf("%s over an unreadable journal = %v, want an error wrapping the store's", name, err)
		}
	}
}

// failingEvents is an audit.EventStore whose calls fail on demand.
type failingEvents struct {
	audit.EventStore
	failLoad, failAppend bool
}

func (f *failingEvents) Load(ctx context.Context, runID string) ([][]byte, error) {
	if f.failLoad {
		return nil, errBoom
	}
	return f.EventStore.Load(ctx, runID)
}

func (f *failingEvents) Append(ctx context.Context, runID string, seq int, leaf []byte) error {
	if f.failAppend {
		return errBoom
	}
	return f.EventStore.Append(ctx, runID, seq, leaf)
}

func TestEventStore_FailuresReachTheCaller(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	tool := agent.MustFunc("lookup", "", func(_ context.Context, _ struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
	if _, err := agenttest.MustNew(&twoTurnModel{}, j, agent.WithTools(tool)).Run(ctx, "r", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	good := &failingEvents{EventStore: audit.NewMemEventStore()}
	if err := audit.PersistJournal(ctx, good, j, "r"); err != nil {
		t.Fatal(err)
	}
	good.failLoad = true
	if _, err := audit.LoadEventLog(ctx, good, "r"); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "load event trail r") {
		t.Errorf("LoadEventLog over a failing store = %v", err)
	}
	if err := audit.PersistEvent(ctx, good, "r", 0, agent.TurnStarted{}); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "load event trail r") {
		t.Errorf("PersistEvent when the trail cannot be read = %v", err)
	}
	fresh := &failingEvents{EventStore: audit.NewMemEventStore(), failAppend: true}
	if err := audit.PersistJournal(ctx, fresh, j, "r"); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "persist event 0 of run r") {
		t.Errorf("PersistJournal when appends fail = %v", err)
	}
	if err := audit.PersistEvent(ctx, fresh, "r", 0, agent.TurnStarted{}); !errors.Is(err, errBoom) {
		t.Errorf("PersistEvent when appends fail = %v", err)
	}
	if err := audit.PersistEvent(ctx, audit.NewMemEventStore(), "r", -1, agent.TurnStarted{}); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("PersistEvent at a negative position = %v, want a negative-position error", err)
	}
}

// A leaf is looked up by name or digest; one that was never recorded is an error naming it, never
// an empty proof.
func TestProve_MissingLeaves(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	record(t, j, "r", "a", "b")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, _ := audit.NewTreeHead(ctx, j, "r", 1)
	sth, err := audit.SignTreeHead(th, edS(priv))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		call func() error
		want string
	}{
		"step":             {func() error { _, err := audit.ProveStep(ctx, j, "r", "nope", sth); return err }, `no completed step "nope" in run r`},
		"policy":           {func() error { _, err := audit.ProvePolicy(ctx, j, "r", "dd", sth); return err }, `no policy leaf for digest "dd" in run r`},
		"convergence":      {func() error { _, err := audit.ProveConvergence(ctx, j, "r", "dd", sth); return err }, `no convergence leaf for digest "dd" in run r`},
		"grant":            {func() error { _, err := audit.ProveGrant(ctx, j, "r", "dd", sth); return err }, `no grant leaf for digest "dd" in run r`},
		"run cert":         {func() error { _, err := audit.ProveRunCertificate(ctx, j, "r", sth); return err }, "no run certificate leaf for run r"},
		"index low":        {func() error { _, err := audit.ProveRecord(ctx, j, "r", -1, sth); return err }, "record -1 is not in the tree the STH commits to"},
		"index high":       {func() error { _, err := audit.ProveRecord(ctx, j, "r", sth.Size, sth); return err }, "is not in the tree the STH commits to"},
		"consistency low":  {func() error { _, err := audit.ProveConsistency(ctx, j, "r", -1); return err }, "first -1 out of range"},
		"consistency high": {func() error { _, err := audit.ProveConsistency(ctx, j, "r", 99); return err }, "first 99 out of range"},
	} {
		if err := tc.call(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
		}
	}
}

// A run certificate recorded in the journal can be proven, and verifies as the leaf it is.
func TestRunCertificate_RecordAndProve(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	record(t, j, "r", "a")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, _ := audit.NewTreeHead(ctx, j, "r", 1)
	sth, err := audit.SignTreeHead(th, edS(priv))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.CertifyRun(ctx, j, "r", sth, audit.RunCertSpec{Signer: edS(priv), TimestampNanos: 0}); err == nil ||
		!strings.Contains(err.Error(), "earlier than the journal head's") || !errors.Is(err, agent.ErrConfig) {
		t.Errorf("CertifyRun with a used-policy timestamp before the head's = %v, want ErrConfig naming both times", err)
	}
	cert, err := audit.CertifyRun(ctx, j, "r", sth, audit.RunCertSpec{Signer: edS(priv), TimestampNanos: 2})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := audit.RecordRunCertificate(ctx, j, "r", cert)
	if err != nil {
		t.Fatal(err)
	}
	th2, _ := audit.NewTreeHead(ctx, j, "r", 3)
	sth2, err := audit.SignTreeHead(th2, edS(priv))
	if err != nil {
		t.Fatal(err)
	}
	b, err := audit.ProveRunCertificate(ctx, j, "r", sth2)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Verify(edV(pub)); err != nil {
		t.Fatalf("the proven run certificate leaf does not verify: %v", err)
	}
	got, err := b.Record()
	if err != nil || got.Kind != agent.StepValue || got.Name != rec.Name {
		t.Errorf("proven record = %+v, %v; want the recorded leaf %q", got, err, rec.Name)
	}
	if !strings.Contains(string(got.Result), cert.RunID) {
		t.Errorf("the leaf does not carry the certificate: %s", got.Result)
	}
}

// Producers and verifiers refuse a missing or unusable key before they sign or verify, with an
// error that says which.
func TestSigning_RefusesMissingAndUnusableKeys(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	good := audit.Ed25519Signer{Priv: priv}
	head := []byte("head")
	if _, err := audit.Sign(head, nil); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "no signer") {
		t.Errorf("Sign(nil signer) = %v", err)
	}
	if _, err := audit.Sign(head, audit.Ed25519Signer{}); err == nil || !strings.Contains(err.Error(), "holds no usable key") {
		t.Errorf("Sign(empty ed25519 signer) = %v", err)
	}
	for name, s := range map[string]audit.Signer{
		"empty ml-dsa":           audit.MLDSASigner{},
		"empty hybrid":           audit.HybridSigner{},
		"hybrid, ed key missing": audit.HybridSigner{ML: audit.MLDSASigner{}},
		"hybrid, ml key missing": audit.HybridSigner{Ed: good},
	} {
		if _, err := audit.Sign(head, s); err == nil || !strings.Contains(err.Error(), "holds no usable key") {
			t.Errorf("Sign(%s) = %v, want a no-usable-key error", name, err)
		}
	}
	if err := audit.VerifySignature(head, []byte("sig"), nil); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "no verifier") {
		t.Errorf("VerifySignature(nil verifier) = %v", err)
	}
	sig, err := audit.Sign(head, good)
	if err != nil {
		t.Fatal(err)
	}
	v := audit.Ed25519Verifier{Pub: priv.Public().(ed25519.PublicKey)}
	if err := audit.VerifySignature(head, sig, v); err != nil {
		t.Errorf("a genuine head signature: %v", err)
	}
	if err := audit.VerifySignature([]byte("other"), sig, v); !errors.Is(err, audit.ErrNotVerified) {
		t.Errorf("a signature over another head = %v, want ErrNotVerified", err)
	}
	if _, err := audit.VerifierOf(nil); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("VerifierOf(nil) = %v, want ErrConfig", err)
	}
	if _, err := audit.SignTreeHead(audit.TreeHead{Kind: audit.TreeJournal, RunID: "r"}, nil); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("SignTreeHead(nil signer) = %v", err)
	}
	if _, err := audit.SignGrant(audit.Grant{ID: "g1"}, nil); err == nil || !strings.Contains(err.Error(), `sign grant "g1"`) {
		t.Errorf("SignGrant(nil signer) = %v", err)
	}
	if _, err := audit.Evidence(context.Background(), agenttest.MemJournal(), "r", nil, 1); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("Evidence(nil signer) = %v", err)
	}
	var pkg audit.EvidencePackage
	if err := pkg.Seal(nil); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("Seal(nil signer) = %v", err)
	}
	if _, err := pkg.Verify(nil); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("EvidencePackage.Verify(nil verifier) = %v", err)
	}
	if _, err := audit.VerifyRun(audit.RunCertificate{}, nil, nil); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("VerifyRun(nil verifier) = %v", err)
	}
}

// NewVerifier and ParsePublicKey reject keys that are not of their scheme's form, each wrapping
// ErrMalformed, and read a bare ed25519 key.
func TestParsePublicKey_Malformed(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	h := hex.EncodeToString(pub)
	for name, s := range map[string]string{
		"not hex":                 "ed25519:zz",
		"bare, not hex":           "not a key",
		"ed25519, wrong length":   "ed25519:" + h[:10],
		"ml-dsa, wrong length":    "ml-dsa-65:" + h,
		"hybrid, no length":       "ed25519+ml-dsa-65:" + h[:4],
		"hybrid, bad ed25519":     "ed25519+ml-dsa-65:00000004" + "00000000",
		"hybrid, bad ml-dsa part": "ed25519+ml-dsa-65:00000020" + h + "0102",
		"unknown scheme":          "rsa-4096:" + h,
		"empty":                   "",
	} {
		_, err := audit.ParsePublicKey(s)
		if !errors.Is(err, audit.ErrMalformed) {
			t.Errorf("%s: ParsePublicKey(%q) = %v, want ErrMalformed", name, s, err)
		}
	}
	for _, s := range []string{h, "ed25519:" + h, "  " + h + "\n", "ed25519:" + strings.ToUpper(h)} {
		v, err := audit.ParsePublicKey(s)
		if err != nil || v.Alg() != audit.AlgEd25519 || hex.EncodeToString(v.PublicKey()) != h {
			t.Errorf("ParsePublicKey(%q) = %v, %v; want the ed25519 key", s, v, err)
		}
	}
}

func TestConstructors_RefuseBadArguments(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_ = pub
	anchor := audit.NewMemAnchorLog()
	if _, err := audit.NewAuditedStore(nil, edS(priv), anchor); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("NewAuditedStore(nil store) = %v, want ErrConfig", err)
	}
	if _, err := audit.NewAuditedStore(agent.NewMemStore(), edS(priv), nil); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("NewAuditedStore(nil anchor) = %v, want ErrConfig", err)
	}
	if _, err := audit.NewAuditedStore(agent.NewMemStore(), audit.Ed25519Signer{}, anchor); err == nil || !strings.Contains(err.Error(), "holds no usable key") {
		t.Errorf("NewAuditedStore(signer with no key) = %v", err)
	}
	if err := audit.PersistEvent(ctx, &audit.MemEventStore{}, "r", 0, agent.ModelEvent{}); err == nil || !strings.Contains(err.Error(), "unknown model event type") {
		t.Errorf("PersistEvent(model event of no type) = %v, want it refused", err)
	}
	root, err := audit.SignGrant(audit.Grant{ID: "root", Issuer: "i", Subject: "boss", Scope: map[string]string{audit.EarnedScopeLimit: "100"}}, edS(priv))
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenttest.MemJournal()
	for name, tc := range map[string]struct {
		ladder []int
		every  int
		ledger *agent.Journal
		runID  string
		root   audit.SignedGrant
		want   string
	}{
		"empty ladder":           {nil, 1, ledger, "l", root, "non-empty ladder"},
		"zero promotion":         {[]int{10}, 0, ledger, "l", root, "promoteEvery must be positive"},
		"no ledger":              {[]int{10}, 1, nil, "l", root, "needs a ledger store and run id"},
		"no ledger run id":       {[]int{10}, 1, ledger, "", root, "needs a ledger store and run id"},
		"root with no limit":     {[]int{10}, 1, ledger, "l", mustGrant(t, audit.Grant{ID: "r2", Subject: "boss", Scope: map[string]string{}}, priv), "no numeric limit scope"},
		"rung above the ceiling": {[]int{10, 101}, 1, ledger, "l", root, "ladder rung 1 (101) exceeds the root ceiling (100)"},
	} {
		if _, err := audit.NewEarnedAuthority(ctx, tc.ladder, tc.every, tc.root, edS(priv), "agent", tc.ledger, tc.runID); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
		}
	}
}

func mustGrant(t *testing.T, g audit.Grant, priv ed25519.PrivateKey) audit.SignedGrant {
	t.Helper()
	sg, err := audit.SignGrant(g, edS(priv))
	if err != nil {
		t.Fatal(err)
	}
	return sg
}

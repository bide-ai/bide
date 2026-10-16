// Package testcertreceipt builds, for tests, a certificate-bound decision and the files the receipt
// commands read: the receipt (with and without its anchoring proof), the journal export, signed heads,
// the log's public key, and the certificate policy file with its trust anchor.
package testcertreceipt

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// DecidedAt is the signed time of the decision; the head that anchors it is stamped a minute later.
var DecidedAt = time.Date(2026, 10, 13, 12, 14, 3, 123000000, time.UTC)

// Fixture is a certificate-bound approval by "alice" and the files that describe it.
type Fixture struct {
	Dir string
	// Receipt holds the anchored receipt, NoProof the same receipt without its anchoring proof, and
	// Bare a bare-key receipt of another decision.
	Receipt, NoProof string
	Journal          string
	// Heads are the signed heads (before and after the decision), HeadsBefore only the first.
	Heads, HeadsBefore string
	// Policy is the certificate policy file; OtherPolicy trusts another root.
	Policy, OtherPolicy string
	// LogKey is the log's public key text.
	LogKey string
	// Identifier and DisplayName are what the certificate says; AnchoredAt is the covering head's time.
	Identifier, DisplayName string
	AnchoredAt              time.Time
	LeafHash                string
}

type model struct{}

func (model) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "charge", ArgsFragment: json.RawMessage(`{"amount":120}`)}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	close(ch)
	return agent.NewStream(ch), nil
}

func writeJSON(t testing.TB, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func policy(t testing.TB, dir, name string, root, inter *testca.CA) string {
	t.Helper()
	pemPath := name + "-root.pem"
	if err := os.WriteFile(filepath.Join(dir, pemPath), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	spki := audit.SPKIHashOf(inter.Cert)
	p := filepath.Join(dir, name+".json")
	writeJSON(t, p, map[string]any{
		"anchors":       []string{pemPath},
		"intermediates": []string{hex.EncodeToString(spki[:])},
		"identity":      map[string]string{"alice": testca.Person(1)},
		"id_kind":       "person-uri",
		"eku":           testca.EKU.String(),
		"policy_oid":    testca.PolicyOID.String(),
		"canonical":     map[string]string{"host": "idp.bank.example"},
	})
	return p
}

// New builds the fixture in a fresh temporary directory.
func New(t testing.TB) *Fixture {
	t.Helper()
	ctx := context.Background()
	f := &Fixture{Dir: t.TempDir(), Identifier: testca.Person(1)}
	root := testca.NewRoot(t, "root", testca.Opts{})
	inter := root.NewIntermediate(t, "inter", testca.Opts{})
	leaf := inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(c *x509.Certificate) {
		c.NotBefore, c.NotAfter = DecidedAt.Add(-time.Hour), DecidedAt.Add(time.Hour)
	}})
	f.DisplayName = leaf.Cert.Subject.CommonName
	lh := agent.CertLeafHash(leaf.DER)
	f.LeafHash = hex.EncodeToString(lh[:])
	f.Policy = policy(t, f.Dir, "policy", root, inter)
	otherRoot := testca.NewRoot(t, "other", testca.Opts{})
	f.OtherPolicy = policy(t, f.Dir, "other-policy", otherRoot, otherRoot.NewIntermediate(t, "inter", testca.Opts{}))

	logPub, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	f.LogKey = audit.FormatPublicKey(audit.AlgEd25519, logPub)
	logSigner := audit.Ed25519Signer{Priv: logPriv}

	const runID = "run-cert"
	store := agenttest.MemJournal()
	approval := agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}
	charge := agent.MustFunc("charge", "charge the card", func(context.Context, struct {
		Amount int `json:"amount"`
	}) (string, error) {
		return "ok", nil
	}, agent.WithApproval(&approval))
	resolver := func(id string) (agent.ApproverVerifier, bool) {
		return audit.Ed25519Verifier{Pub: logPub}, id == "alice"
	}
	a := agenttest.MustNew(model{}, store, agent.WithTools(charge), agent.WithApproverVerifiers(resolver))
	if _, err := a.Run(ctx, runID, agent.UserText("pay")); err == nil {
		t.Fatal("the gate did not pause")
	}
	paused, _ := store.History(ctx, runID)
	before, err := audit.NewTreeHead(ctx, store, runID, DecidedAt.Add(-time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	_, call, _ := agent.FindToolCall(paused, "c1")
	s := agent.ApprovalSubject{RunID: runID, ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
	sig := ed25519.Sign(leaf.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(s, "alice", true, DecidedAt, lh))
	d := agent.Decision{RunID: runID, ToolUseID: "c1", ApproverID: "alice", Approved: true, DecidedAt: DecidedAt, Alg: audit.AlgEd25519, Signature: sig, Cert: leaf.Chain()}
	if err := agent.SubmitDecision(ctx, store, d, agent.WithClock(func() time.Time { return DecidedAt.Add(time.Second) })); err != nil {
		t.Fatal(err)
	}
	recs, _ := store.History(ctx, runID)
	var rc audit.DecisionReceipt
	for i, r := range recs {
		if r.Kind == agent.StepApproval && r.Approver() == "alice" {
			if rc, err = audit.NewDecisionReceipt(s, d, r, i); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.AnchoredAt = DecidedAt.Add(time.Minute)
	after, err := audit.NewTreeHead(ctx, store, runID, f.AnchoredAt.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	sign := func(th audit.TreeHead) audit.SignedTreeHead {
		h, err := audit.SignTreeHead(th, logSigner)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	heads := func(path string, hs ...audit.SignedTreeHead) string {
		var b []byte
		for _, h := range hs {
			l, _ := json.Marshal(h)
			b = append(append(b, l...), '\n')
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	hb, ha := sign(before), sign(after)
	f.HeadsBefore = heads(filepath.Join(f.Dir, "before.heads"), hb)
	f.Heads = heads(filepath.Join(f.Dir, "all.heads"), hb, ha)
	anchored, err := audit.AnchorReceipt(rc, recs, []audit.SignedTreeHead{ha}, audit.Ed25519Verifier{Pub: logPub})
	if err != nil {
		t.Fatal(err)
	}
	f.Receipt = filepath.Join(f.Dir, "anchored.receipt.json")
	writeJSON(t, f.Receipt, anchored)
	f.NoProof = filepath.Join(f.Dir, "noproof.receipt.json")
	writeJSON(t, f.NoProof, rc)
	exp, err := audit.ExportJournal(ctx, store, runID)
	if err != nil {
		t.Fatal(err)
	}
	f.Journal = filepath.Join(f.Dir, "journal.json")
	writeJSON(t, f.Journal, exp)
	return f
}

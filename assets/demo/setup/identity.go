//go:build ignore

// identity.go writes the journal behind the run viewer guide's approver identity screenshots: refunds
// that wait at gates with certificate-bound approvers, in each state the viewer tells apart. The real
// agent runtime runs them to their gates on a scripted model; a store wrapper adds the identity fields
// to the gate's pause note (the gate does not write them until the identity feature is wired into a
// gate), and the identity records are then written beside the gate's tally with the journal's own step
// API, exactly as the record format in docs/design/approver-identity.md has them. The certificates are
// real X.509 (a demo CA, one leaf per approver, URI identifiers), so the viewer reads display names
// from them. Run it from cmd/bide, which carries the modules it imports:
//
//	go run ../../assets/demo/setup/identity.go -dir DIR
//
// DIR gets identity.db.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/store/sqlite"
)

func main() {
	dir := flag.String("dir", "", "directory that gets identity.db")
	flag.Parse()
	if err := run(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "identity.go:", err)
		os.Exit(1)
	}
}

const policyDigest = "9f2c41d07ab3e65c1d8e0a47b92f36c5e8d1a07b64c39f25e1d80a3b7c46f912"

// note rewrites the gate's pause note as it is inserted: the fields a gate with certificate-bound
// approvers adds (identity_bound, the bound approvers, the policy digest, why it waits).
type note struct {
	agent.Store
	fields func(runID string, w *agent.ApprovalWait)
}

func (s *note) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if len(name) > 17 && name[:17] == "approval-pending:" {
		var rec agent.Record
		if err := json.Unmarshal(data, &rec); err != nil {
			return agent.Entry{}, false, err
		}
		var w agent.ApprovalWait
		if err := json.Unmarshal(rec.Result, &w); err != nil {
			return agent.Entry{}, false, err
		}
		s.fields(runID, &w)
		b, _ := json.Marshal(w)
		rec.Result = b
		var err error
		if data, err = agent.EncodeRecord(rec); err != nil {
			return agent.Entry{}, false, err
		}
	}
	return s.Store.Insert(ctx, runID, name, data)
}

func seedKey(id string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("bide identity demo " + id))
	return ed25519.NewKeyFromSeed(seed[:])
}

type person struct {
	approver, name, uri string
	cert                []byte
}

func person1(approver, name string, n int) person {
	return person{approver: approver, name: name, uri: fmt.Sprintf("https://idp.bank.example/person/%032x", n)}
}

func run(dir string) error {
	ctx := context.Background()
	s, err := sqlite.Open(filepath.Join(dir, "identity.db"))
	if err != nil {
		return err
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)

	// A demo CA and a leaf per bound approver. The viewer reads the subject name; it does not verify
	// the chain (the gate does), so a self-made root is enough.
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Example Bank Approver CA"}, NotBefore: now.Add(-48 * time.Hour), NotAfter: now.Add(24 * time.Hour * 365),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, caKey.Public(), caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	people := map[string]*person{}
	for i, p := range []person{person1("ops", "Priya Raman", 1), person1("finance", "Marcus Webb", 2)} {
		u, _ := url.Parse(p.uri)
		tpl := &x509.Certificate{SerialNumber: big.NewInt(int64(100 + i)), Subject: pkix.Name{CommonName: p.name}, NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, UnknownExtKeyUsage: []asn1.ObjectIdentifier{{1, 3, 6, 1, 4, 1, 99999, 1, 1}}, URIs: []*url.URL{u}}
		der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, seedKey(p.approver).Public(), caKey)
		if err != nil {
			return err
		}
		p.cert = der
		people[p.approver] = &p
	}

	pause := map[string]string{} // run -> pause phase
	detail := map[string]string{}
	wrapped, err := agent.NewJournal(&note{Store: s, fields: func(runID string, w *agent.ApprovalWait) {
		w.IdentityBound, w.IdentityApprovers, w.IdentityPolicyDigest = true, []string{"ops", "finance"}, policyDigest
		w.Pause, w.PauseDetail = pause[runID], detail[runID]
	}})
	if err != nil {
		return err
	}
	j, err := agent.NewJournal(s)
	if err != nil {
		return err
	}
	verify := func(id string) (agent.ApproverVerifier, bool) {
		return audit.Ed25519Verifier{Pub: seedKey(id).Public().(ed25519.PublicKey)}, true
	}
	clock := now
	clk := func() time.Time { return clock }

	type refund struct {
		id, order, phase, why string
		asked                 time.Duration
	}
	ask := func(r refund) error {
		pause[r.id], detail[r.id] = r.phase, r.why
		policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}
		tool := agent.MustFunc("refund", "refund an order to the customer's card", func(context.Context, json.RawMessage) (string, error) { return "refunded", nil }, agent.WithApproval(&policy))
		model := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", fmt.Sprintf(`{"order":%q,"amount":4800,"currency":"USD"}`, r.order)), agenttest.TextTurn("Refunded."))
		a, err := agent.New(model, wrapped, agent.WithTools(tool), agent.WithApproverVerifiers(verify), agent.WithClock(clk))
		if err != nil {
			return err
		}
		clock = now.Add(-r.asked)
		_, err = a.Run(ctx, r.id, agent.UserText("Refund order "+r.order+"."))
		if !errors.As(err, new(*agent.ApprovalPending)) {
			return fmt.Errorf("%s did not pause at its gate: %v", r.id, err)
		}
		return nil
	}
	// decide records a certificate-bound decision of who, signed over the v4 bytes of the leaf.
	decide := func(id, who string, when time.Time) (string, error) {
		recs, err := j.History(ctx, id)
		if err != nil {
			return "", err
		}
		_, call, _ := agent.FindToolCall(recs, "c1")
		subj := agent.ApprovalSubject{RunID: id, ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
		p := people[who]
		sig := ed25519.Sign(seedKey(who), agent.ApprovalDecisionBytesCert(subj, who, true, when, agent.CertLeafHash(p.cert)))
		clock = when.Add(2 * time.Second)
		if err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: id, ToolUseID: "c1", ApproverID: who, Approved: true, DecidedAt: when, Alg: audit.AlgEd25519, Signature: sig, Cert: [][]byte{p.cert}}, agent.WithClock(clk)); err != nil {
			return "", err
		}
		recs, _ = j.History(ctx, id)
		for _, r := range recs {
			if r.Kind == agent.StepApproval && r.Approver() == who {
				return r.Name, nil
			}
		}
		return "", fmt.Errorf("no decision record for %s", who)
	}
	stamp := agent.FormatTimestamp
	// put writes a value record under a reserved name, straight to the store: the engine's own step API
	// refuses these names, since only a gate writes them.
	put := func(id, name string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		data, err := agent.JournalEntry(name, agent.Record{Kind: agent.StepValue, Result: b})
		if err != nil {
			return err
		}
		_, _, err = s.Insert(ctx, id, name, data)
		return err
	}
	idDecision := func(who, step, tier, basis string, decided time.Time, status time.Duration, src string) agent.IdentityDecision {
		p := people[who]
		leaf := sha256.Sum256(p.cert)
		d := agent.IdentityDecision{Approver: who, Step: step, Identifier: p.uri, LeafHash: hex.EncodeToString(leaf[:]), Chain: []string{hex.EncodeToString(leaf[:])},
			Anchor: hex.EncodeToString(leaf[:]), AnchoredAt: stamp(decided.Add(time.Minute)), TimeSource: src, HeadSize: 9, Tier: tier, Basis: basis, Window: "1h0m0s"}
		if status != 0 {
			d.StatusTime = stamp(decided.Add(status))
		}
		return d
	}
	count := func(id string, ds ...agent.IdentityDecision) error {
		var by []string
		for _, d := range ds {
			by = append(by, d.Approver)
		}
		clock = now
		tally := agent.ApprovalTally{Need: 2, Approvers: []string{"ops", "finance", "risk"}, Approved: len(ds), ApprovedBy: by, Pending: []string{"risk"}, DecisionFormat: agent.DecisionFormat}
		if len(ds) == 2 {
			tally.Pending = nil
		}
		if err := put(id, agent.ApprovalIdentTallyStep("c1"), tally); err != nil {
			return err
		}
		return put(id, agent.ApprovalIdentityStep("c1"), agent.ApprovalIdentity{Format: agent.ApprovalIdentityFormat, ToolUseID: "c1", PolicyDigest: policyDigest,
			RosterDigest: policyDigest, Decisions: ds, Rules: agent.ApprovalIdentityRulesVer, Verifier: "go1.27.0", RecordedAt: stamp(now)})
	}

	for _, r := range []refund{
		{"refund-3001", "3001", "", "", 50 * time.Minute},
		{"refund-3002", "3002", agent.PauseAwaitingStatus, "no revocation list for the issuing CA is newer than the decision", 30 * time.Minute},
		{"refund-3003", "3003", agent.PauseAwaitingAnchor, "", 12 * time.Minute},
		{"refund-3004", "3004", "", "", 40 * time.Minute},
	} {
		if err := ask(r); err != nil {
			return err
		}
	}
	t1 := now.Add(-40 * time.Minute)
	opsStep, err := decide("refund-3001", "ops", t1)
	if err != nil {
		return err
	}
	finStep, err := decide("refund-3001", "finance", t1.Add(5*time.Minute))
	if err != nil {
		return err
	}
	// refund-3001: ops counted final on an independent time; finance counted only provisionally, because
	// the status in hand was issued before the decision became provable.
	if err := count("refund-3001",
		idDecision("ops", opsStep, agent.TierIndependentFinal, "status issued after the decision became provable and covering its window", t1, 20*time.Minute, "rfc3161 tsa.bank.example"),
		idDecision("finance", finStep, agent.TierProvisional, "live count: status issued before the decision (provisional only)", t1.Add(5*time.Minute), -10*time.Minute, "log-signer clock")); err != nil {
		return err
	}
	// refund-3002 (waiting for status) and refund-3003 (waiting for a covering head) hold ops' decision.
	if _, err := decide("refund-3002", "ops", now.Add(-25*time.Minute)); err != nil {
		return err
	}
	if _, err := decide("refund-3003", "ops", now.Add(-10*time.Minute)); err != nil {
		return err
	}
	// refund-3004: both counted, then an audit found newer status that voids finance's count.
	o4, err := decide("refund-3004", "ops", now.Add(-38*time.Minute))
	if err != nil {
		return err
	}
	f4, err := decide("refund-3004", "finance", now.Add(-36*time.Minute))
	if err != nil {
		return err
	}
	d4 := idDecision("finance", f4, agent.TierLogclockFinal, "status issued after the decision became provable and covering its window", now.Add(-36*time.Minute), 15*time.Minute, "log-signer clock")
	d4.Disputed = true
	return count("refund-3004",
		idDecision("ops", o4, agent.TierLogclockFinal, "status issued after the decision became provable and covering its window", now.Add(-38*time.Minute), 15*time.Minute, "log-signer clock"), d4)
}

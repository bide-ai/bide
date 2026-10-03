package main

import (
	"context"
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// algChargeModel calls the "charge" tool until a tool result is in the conversation, then answers.
type algChargeModel struct{}

func (algChargeModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	answered := false
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if _, ok := p.(agent.ToolResult); ok {
				answered = true
			}
		}
	}
	if answered {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "charged"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	} else {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "charge", ArgsFragment: json.RawMessage(`{"amount":120}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// newMLDSASigner returns an ML-DSA-65 signer with a fresh key.
func newMLDSASigner(t *testing.T) audit.MLDSASigner {
	t.Helper()
	k, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	return audit.MLDSASigner{Priv: k}
}

// newHybridSigner returns a hybrid ed25519 + ML-DSA-65 signer with fresh keys.
func newHybridSigner(t *testing.T) audit.HybridSigner {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	return audit.HybridSigner{Ed: audit.Ed25519Signer{Priv: priv}, ML: newMLDSASigner(t)}
}

// Every verify verb works end to end under ML-DSA-65 and under the hybrid scheme: artifacts signed
// under the scheme verify with the key given as "<scheme>:<hex>" (exit 0), and a key of another
// scheme does not verify them (exit 1). The approver's decision is signed and journaled under the
// same scheme, and verify-approvals reads the approver's key in the same text form.
func TestCLI_MLDSAAndHybridEndToEnd(t *testing.T) {
	_, otherEd, _ := ed25519.GenerateKey(rand.Reader)
	wrongScheme := keyText(audit.Ed25519Signer{Priv: otherEd})
	for name, mk := range map[string]func(*testing.T) audit.Signer{
		"ml-dsa-65": func(t *testing.T) audit.Signer { return newMLDSASigner(t) },
		"hybrid":    func(t *testing.T) audit.Signer { return newHybridSigner(t) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			logSigner, approver := mk(t), mk(t)
			logKey := keyText(logSigner)
			approverV, err := audit.VerifierOf(approver)
			if err != nil {
				t.Fatal(err)
			}

			// A 1-of-1 gated charge, approved under the scheme.
			store := agenttest.MemJournal()
			const runID = "run-alg"
			policy := agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice"}}
			charge := agent.Func("charge", "charge the card", agent.Safety{},
				func(context.Context, struct {
					Amount int `json:"amount"`
				}) (string, error) {
					return "ok", nil
				}, agent.WithApproval(&policy))
			resolver := func(id string) (agent.ApproverVerifier, bool) { return approverV, id == "alice" }
			a := agenttest.MustNew(algChargeModel{}, store, agent.WithTools(charge), agent.WithApproverVerifiers(resolver))
			if _, err := a.Run(ctx, runID, "pay"); err == nil {
				t.Fatal("the gated run did not pause")
			}
			recs, err := store.History(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			_, call, ok := agent.FindToolCall(recs, "c1")
			if !ok {
				t.Fatal("no recorded call c1")
			}
			subject := agent.ApprovalSubject{RunID: runID, ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
			sig, err := approver.Sign(agent.ApprovalDecisionBytes(subject, "alice", true))
			if err != nil {
				t.Fatal(err)
			}
			if err := agent.SubmitDecision(ctx, store, agent.Decision{RunID: runID, ToolUseID: "c1", ApproverID: "alice", Approved: true, Alg: approver.Alg(), Signature: sig}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Run(ctx, runID, "pay"); err != nil {
				t.Fatalf("the approved run: %v", err)
			}

			// A governed action under an anchored, certified policy, for the run certificate.
			pol, digest := testPolicy()
			if _, err := audit.RecordPolicy(ctx, store, runID, []byte(pol), digest); err != nil {
				t.Fatal(err)
			}
			if _, err := audit.RecordConvergence(ctx, store, runID, []byte(testCert(t, digest)), digest); err != nil {
				t.Fatal(err)
			}
			act := toolLeaf("act", `{"event":"approve","applied":true,"policy_digest":"`+digest+`"}`)
			if _, err := journaltest.Do(ctx, store, runID, act.Name, func(context.Context) (agent.Record, error) { return act, nil }); err != nil {
				t.Fatal(err)
			}

			ts := time.Now().Add(-time.Minute).UnixNano()
			pkg, err := audit.Evidence(ctx, store, runID, logSigner, ts, audit.WithToolCall("act"), audit.WithToolCall("c1"),
				audit.WithRunCertificate(audit.RunCertSpec{ApprovedPolicies: []string{digest}}))
			if err != nil {
				t.Fatal(err)
			}
			gate, err := audit.ApprovalEvidence(ctx, store, runID, "c1", pkg.STH)
			if err != nil {
				t.Fatal(err)
			}
			pkg.Actions = append(pkg.Actions, gate[:len(gate)-1]...) // the package already carries the call's result
			if err := pkg.Seal(logSigner); err != nil {
				t.Fatal(err)
			}
			// A package without a run certificate, which verify-approvals checks with no allowlist.
			gatePkg, err := audit.Evidence(ctx, store, runID, logSigner, ts, audit.WithToolCall("c1"))
			if err != nil {
				t.Fatal(err)
			}
			gate2, err := audit.ApprovalEvidence(ctx, store, runID, "c1", gatePkg.STH)
			if err != nil {
				t.Fatal(err)
			}
			gatePkg.Actions = append(gatePkg.Actions, gate2[:len(gate2)-1]...)
			if err := gatePkg.Seal(logSigner); err != nil {
				t.Fatal(err)
			}
			bundle, err := audit.ProveToolCall(ctx, store, runID, "c1", pkg.STH)
			if err != nil {
				t.Fatal(err)
			}
			all, err := store.History(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			absSTH, err := audit.SignAbsenceRoot(all, audit.ToolUseKeys, pkg.STH.TreeHead, logSigner, ts)
			if err != nil {
				t.Fatal(err)
			}
			absent, err := audit.ProveAbsentBundle(all, audit.ToolUseKeys, audit.ToolUseKeyFor("refund"), absSTH)
			if err != nil {
				t.Fatal(err)
			}

			file := func(n string, v any) string {
				p := filepath.Join(dir, n)
				writeJSON(t, p, v)
				return p
			}
			pkgPath, bundlePath, absPath := file("evidence.json", pkg), file("bundle.json", bundle), file("absent.json", absent)
			gatePath := file("evidence-gate.json", gatePkg)
			certPath := file("runcert.json", pkg.RunCertificate)
			keysPath := file("keys.json", map[string]string{"alice": keyText(approver)})
			journalPath, absSTHPath := file("journal.json", exportJournal(t, store, runID)), file("abs-sth.json", absSTH)

			for _, tc := range []struct {
				name string
				args []string
			}{
				{"verify", []string{"verify", "-bundle", bundlePath}},
				{"verify-evidence", []string{"verify-evidence", "-evidence", pkgPath, "-approved", digest}},
				{"verify-run", []string{"verify-run", "-cert", certPath, "-approved", digest}},
				{"verify-approvals", []string{"verify-approvals", "-evidence", gatePath, "-call", "c1", "-need", "1", "-approvers", "alice", "-approver-keys", keysPath}},
				{"verify-absent", []string{"verify-absent", "-bundle", absPath}},
			} {
				if code, stdout, stderr := runCLI(append(tc.args, "-pubkey", logKey)...); code != 0 {
					t.Errorf("%s under the %s key: exit %d, want 0\n%s%s", tc.name, name, code, stdout, stderr)
				}
				if code, stdout, stderr := runCLI(append(tc.args, "-pubkey", wrongScheme)...); code != 1 {
					t.Errorf("%s under an ed25519 key: exit %d, want 1\n%s%s", tc.name, code, stdout, stderr)
				}
			}
			// verify-approvals on a package that carries a run certificate: it passes only with an
			// allowlist (-approved / -approved-file, as verify-run and verify-evidence take) that
			// holds the run's policy. An allowlist file that cannot be read is an unusable input.
			allowPath := filepath.Join(dir, "approved.txt")
			if err := os.WriteFile(allowPath, []byte("# approved\n"+digest+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			approvals := []string{"verify-approvals", "-evidence", pkgPath, "-call", "c1", "-need", "1", "-approvers", "alice", "-approver-keys", keysPath, "-pubkey", logKey}
			for _, tc := range []struct {
				name  string
				extra []string
				want  int
			}{
				{"-approved", []string{"-approved", digest}, 0},
				{"-approved-file", []string{"-approved-file", allowPath}, 0},
				{"no allowlist", nil, 1},
				{"an allowlist without the run's policy", []string{"-approved", strings.Repeat("0", 64)}, 1},
				{"a missing allowlist file", []string{"-approved-file", filepath.Join(dir, "missing.txt")}, 4},
			} {
				if code, stdout, stderr := runCLI(append(append([]string(nil), approvals...), tc.extra...)...); code != tc.want {
					t.Errorf("verify-approvals of a package with a run certificate, %s: exit %d, want %d\n%s%s", tc.name, code, tc.want, stdout, stderr)
				}
			}
			if code, stdout, stderr := runCLI("prove", "-journal", journalPath, "-sth", file("sth.json", pkg.STH), "-tool", "c1"); code != 0 {
				t.Errorf("prove from a %s head: exit %d\n%s%s", name, code, stdout, stderr)
			}
			if code, stdout, stderr := runCLI("prove-absent", "-journal", journalPath, "-sth", absSTHPath, "-key", "tool:refund"); code != 0 {
				t.Errorf("prove-absent from a %s head: exit %d\n%s%s", name, code, stdout, stderr)
			}
		})
	}
}

// A hybrid signature's Ed25519 half, relabelled as a plain ed25519 head, does not verify under
// the Ed25519 component's key: the CLI reports it not verified (exit 1), never verified.
func TestCLI_StrippedHybridHalfIsNotVerified(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	signer := newHybridSigner(t)
	store := agenttest.MemJournal()
	r := toolLeaf("c1", `{"ok":true}`)
	if _, err := journaltest.Do(ctx, store, "r", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
		t.Fatal(err)
	}
	th, err := audit.NewTreeHead(ctx, store, "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := audit.ProveToolCall(ctx, store, "r", "c1", signHead(t, th, signer))
	if err != nil {
		t.Fatal(err)
	}
	sig := b.STH.Signature
	n := int(sig[0])<<24 | int(sig[1])<<16 | int(sig[2])<<8 | int(sig[3])
	b.STH.Alg, b.STH.Signature = audit.AlgEd25519, sig[4:4+n]
	path := filepath.Join(dir, "stripped.json")
	writeJSON(t, path, b)
	code, stdout, stderr := runCLI("verify", "-bundle", path, "-pubkey", keyText(signer.Ed))
	if code != 1 {
		t.Fatalf("a stripped hybrid half: exit %d, want 1\n%s%s", code, stdout, stderr)
	}
	// It is a verdict, reported as one: a FAIL line, not an error about the input.
	if !strings.Contains(stdout, "FAIL: proof did not verify under this key") || strings.Contains(stderr, "error:") {
		t.Fatalf("a stripped hybrid half is not reported as a FAIL verdict:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
}

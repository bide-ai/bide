package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// exitFixture is one signed run with every artifact the verbs read, genuine and tampered, written
// to files.
type exitFixture struct {
	dir, pub, otherPub, digest                       string
	journal, sth, sthFuture                          string
	bundle, bundleTampered, bundleFuture             string
	absBundle, absTampered, absSTH                   string
	policyFile, policyBundle, policyTampered         string
	action, actionTampered, certBundle, certTampered string
	voteA, voteB, voteATampered, tally               string
	runCert, runCertTampered                         string
	evidence, evidenceTampered, evidenceCert         string
	keys, sharedKeys, weakKeys, notJSON, missing     string
	identityPub                                      string // the identity point: small order, accepts forged signatures
	sthNoFormat, sthV4, bundleNestedV4, proofV2      string // artifacts in older layouts
	evidenceV4, journalWrongFormat, journalV0        string
	keyUnknownScheme, keyPrefixed                    string
	agree, nonConvergent, broken                     string
	brokenFirst, brokenSecond                        string // no verdict on one policy, a disagreement on the other
	fakePolicy                                       string // a tool result shaped like the policy leaf
	absUnknownKey, allow                             string // allow: both policies' digests, one per line
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func flipSig(sig []byte) []byte {
	out := slices.Clone(sig)
	out[0] ^= 1
	return out
}

func newExitFixture(t *testing.T) *exitFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	f := &exitFixture{dir: dir}
	file := func(name string, v any) string {
		p := filepath.Join(dir, name)
		writeJSON(t, p, v)
		return p
	}
	policy, digest := testPolicy()
	f.digest = digest

	store := agent.NewMemStore()
	const runID = "run1"
	if _, err := audit.RecordPolicy(ctx, store, runID, []byte(policy), digest); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.RecordConvergence(ctx, store, runID, []byte(testCert(t, digest)), digest); err != nil {
		t.Fatal(err)
	}
	// A second governed policy, so verify-run -checker checks two.
	policy2 := "(doms 2 2)\n(ev (do (set 1 (lit 1))))\n"
	digest2 := policyDigest([]byte(policy2))
	if _, err := audit.RecordPolicy(ctx, store, runID, []byte(policy2), digest2); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.RecordConvergence(ctx, store, runID, []byte(testCert(t, digest2)), digest2); err != nil {
		t.Fatal(err)
	}
	for _, r := range []agent.Record{
		toolLeaf("act", `{"event":"approve","applied":true,"policy_digest":"`+digest+`"}`),
		valueLeaf("quorum/q/vote/a", `{"voter":"a","decision":"approve"}`),
		valueLeaf("quorum/q/vote/b", `{"voter":"b","decision":"approve"}`),
		valueLeaf("quorum/q/tally", `{"decision":"approve","votes_for":2,"total":2,"agreed":true,"votes":[{"voter":"a","decision":"approve"},{"voter":"b","decision":"approve"}]}`),
		toolLeaf("c1", `{"ok":true}`),
		toolLeaf("fake-policy", string(must(json.Marshal(audit.PolicyContent{Digest: digest, Policy: policy})))),
		toolLeaf("act2", `{"event":"approve","applied":true,"policy_digest":"`+digest2+`"}`),
	} {
		if _, err := store.Do(ctx, runID, r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	signer := audit.Ed25519Signer{Priv: priv}
	f.pub, f.otherPub = hex.EncodeToString(pub), hex.EncodeToString(other)
	f.keyPrefixed, f.keyUnknownScheme = keyText(signer), "rsa-4096:"+hex.EncodeToString(pub)
	head := func(ts int64) audit.SignedTreeHead {
		th, err := audit.NewTreeHead(ctx, store, runID, ts)
		if err != nil {
			t.Fatal(err)
		}
		return signHead(t, th, signer)
	}
	sth, future := head(1), head(time.Now().Add(time.Hour).UnixNano())
	recs, err := store.History(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	export := exportJournal(t, store, runID)
	f.journal, f.sth, f.sthFuture = file("journal.json", export), file("sth.json", sth), file("sth-future.json", future)
	f.sthNoFormat = rewriteJSON(t, filepath.Join(dir, "sth-no-format.json"), sth, `"format":"`+audit.STHFormat+`",`, "")
	f.sthV4 = rewriteJSON(t, filepath.Join(dir, "sth-v4.json"), sth, audit.STHFormat, "bide.audit.sth.v4")
	f.journalWrongFormat = rewriteJSON(t, filepath.Join(dir, "journal-wrong-format.json"), export, audit.JournalExportFormat, "bide.audit.journal-export.v0")
	f.journalV0 = file("journal-records.json", recs) // the layout before exports: a JSON array of records

	// prove(bundle, err)(name) writes the bundle and a copy with its head's signature broken.
	prove := func(b audit.ProofBundle, err error) func(string) (string, string) {
		return func(name string) (string, string) {
			t.Helper()
			if err != nil {
				t.Fatalf("prove %s: %v", name, err)
			}
			good := file(name+".json", b)
			b.STH.Signature = flipSig(b.STH.Signature)
			return good, file(name+"-tampered.json", b)
		}
	}
	f.bundle, f.bundleTampered = prove(audit.ProveToolCall(ctx, store, runID, "c1", sth))("bundle")
	c1 := must(audit.ProveToolCall(ctx, store, runID, "c1", sth))
	f.bundleNestedV4 = rewriteJSON(t, filepath.Join(dir, "bundle-nested-sth-v4.json"), c1, audit.STHFormat, "bide.audit.sth.v4")
	f.proofV2 = rewriteJSON(t, filepath.Join(dir, "bundle-proof-v2.json"), c1, audit.ProofFormat, "bide.audit.proof.v2")
	f.bundleFuture, _ = prove(audit.ProveToolCall(ctx, store, runID, "c1", future))("bundle-future")
	f.action, f.actionTampered = prove(audit.ProveToolCall(ctx, store, runID, "act", sth))("action")
	f.policyBundle, f.policyTampered = prove(audit.ProvePolicy(ctx, store, runID, digest, sth))("policy")
	f.certBundle, f.certTampered = prove(audit.ProveConvergence(ctx, store, runID, digest, sth))("cert")
	f.voteA, f.voteATampered = prove(audit.ProveStep(ctx, store, runID, "quorum/q/vote/a", sth))("vote-a")
	f.voteB, _ = prove(audit.ProveStep(ctx, store, runID, "quorum/q/vote/b", sth))("vote-b")
	f.fakePolicy, _ = prove(audit.ProveToolCall(ctx, store, runID, "fake-policy", sth))("fake-policy")
	f.tally, _ = prove(audit.ProveStep(ctx, store, runID, "quorum/q/tally", sth))("tally")

	absSTH, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, sth.TreeHead, signer, 1)
	if err != nil {
		t.Fatal(err)
	}
	f.absSTH = file("abs-sth.json", absSTH)
	ab, err := audit.ProveAbsentBundle(recs, audit.ToolUseKeys, audit.ToolUseKeyFor("refund"), absSTH)
	if err != nil {
		t.Fatal(err)
	}
	f.absBundle = file("absent.json", ab)
	ab.STH.Signature = flipSig(ab.STH.Signature)
	f.absTampered = file("absent-tampered.json", ab)
	ab.Absence.Key = "future-set:refund"
	f.absUnknownKey = file("absent-unknown-key.json", ab)

	rc, err := audit.CertifyRun(ctx, store, runID, sth, audit.RunCertSpec{ApprovedPolicies: []string{digest, digest2}, Signer: signer, TimestampNanos: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.runCert = file("runcert.json", rc)
	rc.STH.Signature = flipSig(rc.STH.Signature)
	f.runCertTampered = file("runcert-tampered.json", rc)

	pkg, err := audit.Evidence(ctx, store, runID, signer, 1)
	if err != nil {
		t.Fatal(err)
	}
	f.evidence = file("evidence.json", pkg)
	f.evidenceV4 = rewriteJSON(t, filepath.Join(dir, "evidence-v4.json"), pkg, audit.EvidenceFormat, "bide.audit.evidence.v4")
	pkg.Signature = flipSig(pkg.Signature)
	f.evidenceTampered = file("evidence-tampered.json", pkg)
	pkg, err = audit.Evidence(ctx, store, runID, signer, 1, audit.WithAllToolCalls(), audit.WithRunCertificate(audit.RunCertSpec{ApprovedPolicies: []string{digest, digest2}}))
	if err != nil {
		t.Fatal(err)
	}
	f.evidenceCert = file("evidence-cert.json", pkg)

	f.keys = file("keys.json", map[string]string{"a": f.pub})
	f.sharedKeys = file("shared-keys.json", map[string]string{"a": f.pub, "b": f.pub})
	f.identityPub = "01" + strings.Repeat("00", 31)
	f.weakKeys = file("weak-keys.json", map[string]string{"a": f.identityPub})
	f.allow = filepath.Join(dir, "approved.txt")
	if err := os.WriteFile(f.allow, []byte("# approved policies\n"+digest+"\n"+digest2+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.policyFile = filepath.Join(dir, "policy.machine")
	f.notJSON = filepath.Join(dir, "not.json")
	f.missing = filepath.Join(dir, "no-such-file.json")
	for p, b := range map[string]string{f.policyFile: policy, f.notJSON: "{not json"} {
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := func(name, body string) string {
		p := filepath.Join(dir, name+".sh")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f.agree = script("agree", "echo compensation_free=false; exit 0")
	f.nonConvergent = script("non-convergent", "echo compensation_free=false; exit 1")
	f.broken = script("broken", "echo compensation_free=false; exit 7")
	// The checker is handed a temporary copy of the policy, so these tell the two apart by content.
	f.brokenFirst = script("broken-first", "grep -q 'set 0' \"$1\" && exit 7; exit 1")
	f.brokenSecond = script("broken-second", "grep -q 'set 1' \"$1\" && exit 7; exit 1")
	return f
}

// runCLI runs a command line in process and returns its exit status and output.
func runCLI(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// exitCase is one command line and the status it must exit with.
type exitCase struct {
	name    string
	args    []string
	want    int
	checker bool // runs a stand-in checker, a POSIX shell script
}

func exitCases(f *exitFixture) []exitCase {
	gov := func(action, policy string, extra ...string) []string {
		return append([]string{"verify-governed-action", "-action", action, "-policy-bundle", policy, "-pubkey", f.pub}, extra...)
	}
	conv := func(cert, policy string, extra ...string) []string {
		return append([]string{"verify-convergence", "-cert-bundle", cert, "-policy-bundle", policy, "-pubkey", f.pub}, extra...)
	}
	quorum := func(name, voteA, voteB string, extra ...string) []string {
		return append([]string{"verify-quorum", "-name", name, "-tally", f.tally, "-vote", voteA, "-vote", voteB, "-pubkey", f.pub, "-k", "2"}, extra...)
	}
	runc := func(cert string, extra ...string) []string {
		return append([]string{"verify-run", "-cert", cert, "-pubkey", f.pub}, extra...)
	}
	approvals := func(evidence, keys string, extra ...string) []string {
		return append([]string{"verify-approvals", "-evidence", evidence, "-pubkey", f.pub, "-call", "c1", "-need", "1", "-approvers", "a", "-approver-keys", keys}, extra...)
	}
	return []exitCase{
		// The command line itself.
		{name: "no verb", args: nil, want: 2},
		{name: "unknown verb", args: []string{"verify-everything"}, want: 2},
		{name: "unknown top-level flag", args: []string{"-bogus", "verify"}, want: 2},
		{name: "-version", args: []string{"-version"}, want: 0},
		{name: "-version with a verb", args: []string{"-version", "verify"}, want: 2},

		// prove
		{name: "prove: genuine", args: []string{"prove", "-journal", f.journal, "-sth", f.sth, "-tool", "c1"}, want: 0},
		{name: "prove: missing -sth", args: []string{"prove", "-journal", f.journal, "-tool", "c1"}, want: 2},
		{name: "prove: missing journal file", args: []string{"prove", "-journal", f.missing, "-sth", f.sth, "-tool", "c1"}, want: 4},
		{name: "prove: journal not JSON", args: []string{"prove", "-journal", f.notJSON, "-sth", f.sth, "-tool", "c1"}, want: 4},
		{name: "prove: no such tool call", args: []string{"prove", "-journal", f.journal, "-sth", f.sth, "-tool", "nope"}, want: 4},
		{name: "prove: a head signed in the future", args: []string{"prove", "-journal", f.journal, "-sth", f.sthFuture, "-tool", "c1"}, want: 1},
		{name: "prove: future head beside a missing journal", args: []string{"prove", "-journal", f.missing, "-sth", f.sthFuture, "-tool", "c1"}, want: 1},
		{name: "prove: an STH with no format (v4 layout)", args: []string{"prove", "-journal", f.journal, "-sth", f.sthNoFormat, "-tool", "c1"}, want: 4},
		{name: "prove: an STH of format bide.audit.sth.v4", args: []string{"prove", "-journal", f.journal, "-sth", f.sthV4, "-tool", "c1"}, want: 4},
		{name: "prove: a journal export of another format", args: []string{"prove", "-journal", f.journalWrongFormat, "-sth", f.sth, "-tool", "c1"}, want: 4},
		{name: "prove: a bare record array (the layout before exports)", args: []string{"prove", "-journal", f.journalV0, "-sth", f.sth, "-tool", "c1"}, want: 4},

		// verify
		{name: "verify: genuine", args: []string{"verify", "-bundle", f.bundle, "-pubkey", f.pub}, want: 0},
		{name: "verify: tampered", args: []string{"verify", "-bundle", f.bundleTampered, "-pubkey", f.pub}, want: 1},
		{name: "verify: another key", args: []string{"verify", "-bundle", f.bundle, "-pubkey", f.otherPub}, want: 1},
		{name: "verify: a small-order public key", args: []string{"verify", "-bundle", f.bundle, "-pubkey", f.identityPub}, want: 4},
		{name: "verify: head signed in the future", args: []string{"verify", "-bundle", f.bundleFuture, "-pubkey", f.pub}, want: 1},
		{name: "verify: stray argument", args: []string{"verify", "-bundle", f.bundle, "stray", "-pubkey", f.pub}, want: 2},
		{name: "verify: -h", args: []string{"verify", "-h"}, want: 2},
		{name: "verify: missing bundle file", args: []string{"verify", "-bundle", f.missing, "-pubkey", f.pub}, want: 4},
		{name: "verify: bundle not JSON", args: []string{"verify", "-bundle", f.notJSON, "-pubkey", f.pub}, want: 4},
		{name: "verify: an absence bundle as -bundle (format)", args: []string{"verify", "-bundle", f.absBundle, "-pubkey", f.pub}, want: 4},
		{name: "verify: -max-input-bytes below the bundle", args: []string{"verify", "-max-input-bytes", "10", "-bundle", f.bundle, "-pubkey", f.pub}, want: 4},
		{name: "verify: public key neither hex nor a file", args: []string{"verify", "-bundle", f.bundle, "-pubkey", f.missing}, want: 4},
		{name: "verify: future head beside a missing key file", args: []string{"verify", "-bundle", f.bundleFuture, "-pubkey", f.missing}, want: 1},
		{name: "verify: genuine, key as <scheme>:<hex>", args: []string{"verify", "-bundle", f.bundle, "-pubkey", f.keyPrefixed}, want: 0},
		{name: "verify: a key of an unknown scheme", args: []string{"verify", "-bundle", f.bundle, "-pubkey", f.keyUnknownScheme}, want: 4},
		{name: "verify: a bundle whose head is bide.audit.sth.v4", args: []string{"verify", "-bundle", f.bundleNestedV4, "-pubkey", f.pub}, want: 4},
		{name: "verify: a bide.audit.proof.v2 bundle", args: []string{"verify", "-bundle", f.proofV2, "-pubkey", f.pub}, want: 4},

		// verify-governance
		{name: "verify-governance: genuine", args: []string{"verify-governance", "-policy", f.policyFile, "-digest", f.digest}, want: 0},
		{name: "verify-governance: digest mismatch", args: []string{"verify-governance", "-policy", f.policyFile, "-digest", strings.Repeat("00", 32)}, want: 1},
		{name: "verify-governance: missing policy file", args: []string{"verify-governance", "-policy", f.missing}, want: 4},
		{name: "verify-governance: no -policy", args: []string{"verify-governance"}, want: 2},
		{name: "verify-governance: agreeing checker", args: []string{"verify-governance", "-policy", f.policyFile, "-checker", f.agree}, want: 0, checker: true},
		{name: "verify-governance: non-convergent", args: []string{"verify-governance", "-policy", f.policyFile, "-checker", f.nonConvergent}, want: 1, checker: true},
		{name: "verify-governance: checker gives no verdict", args: []string{"verify-governance", "-policy", f.policyFile, "-checker", f.broken}, want: 3, checker: true},
		{name: "verify-governance: digest mismatch beside a broken checker", args: []string{"verify-governance", "-policy", f.policyFile, "-digest", strings.Repeat("00", 32), "-checker", f.broken}, want: 1, checker: true},

		// verify-governed-action
		{name: "verify-governed-action: genuine", args: gov(f.action, f.policyBundle), want: 0},
		{name: "verify-governed-action: no -pubkey", args: []string{"verify-governed-action", "-action", f.action, "-policy-bundle", f.policyBundle}, want: 2},
		{name: "verify-governed-action: tampered action", args: gov(f.actionTampered, f.policyBundle), want: 1},
		{name: "verify-governed-action: missing action", args: gov(f.missing, f.policyBundle), want: 4},
		{name: "verify-governed-action: policy leaf as the action (wrong type)", args: gov(f.policyBundle, f.policyBundle), want: 4},
		{name: "verify-governed-action: action as the policy leaf (wrong type)", args: gov(f.action, f.action), want: 4},
		{name: "verify-governed-action: a tool result shaped like the policy leaf (wrong type)", args: gov(f.action, f.fakePolicy), want: 4},
		{name: "verify-governed-action: tampered action beside a missing policy", args: gov(f.actionTampered, f.missing), want: 1},
		{name: "verify-governed-action: agreeing checker", args: gov(f.action, f.policyBundle, "-checker", f.agree), want: 0, checker: true},
		{name: "verify-governed-action: checker gives no verdict", args: gov(f.action, f.policyBundle, "-checker", f.broken), want: 3, checker: true},
		{name: "verify-governed-action: missing action beside a broken checker", args: gov(f.missing, f.policyBundle, "-checker", f.broken), want: 4, checker: true},
		{name: "verify-governed-action: missing action beside a non-convergent policy", args: gov(f.missing, f.policyBundle, "-checker", f.nonConvergent), want: 1, checker: true},

		// verify-convergence
		{name: "verify-convergence: genuine", args: conv(f.certBundle, f.policyBundle), want: 0},
		{name: "verify-convergence: trailing argument", args: append(conv(f.certBundle, f.policyBundle), "stray"), want: 2},
		{name: "verify-convergence: tampered certificate", args: conv(f.certTampered, f.policyBundle), want: 1},
		{name: "verify-convergence: policy leaf as the certificate (wrong type)", args: conv(f.policyBundle, f.policyBundle), want: 4},
		{name: "verify-convergence: tampered policy beside a missing certificate", args: conv(f.missing, f.policyTampered), want: 1},
		{name: "verify-convergence: agreeing checker", args: conv(f.certBundle, f.policyBundle, "-checker", f.agree), want: 0, checker: true},
		{name: "verify-convergence: checker disagrees", args: conv(f.certBundle, f.policyBundle, "-checker", f.nonConvergent), want: 1, checker: true},
		{name: "verify-convergence: checker gives no verdict", args: conv(f.certBundle, f.policyBundle, "-checker", f.broken), want: 3, checker: true},
		{name: "verify-convergence: missing certificate beside a broken checker", args: conv(f.missing, f.policyBundle, "-checker", f.broken), want: 4, checker: true},

		// verify-quorum
		{name: "verify-quorum: genuine", args: quorum("q", f.voteA, f.voteB), want: 0},
		{name: "verify-quorum: tampered vote", args: quorum("q", f.voteATampered, f.voteB), want: 1},
		{name: "verify-quorum: another quorum's name", args: quorum("other", f.voteA, f.voteB), want: 1},
		{name: "verify-quorum: k above the votes", args: []string{"verify-quorum", "-name", "q", "-tally", f.tally, "-vote", f.voteA, "-vote", f.voteB, "-pubkey", f.pub, "-k", "3"}, want: 1},
		{name: "verify-quorum: missing vote", args: quorum("q", f.voteA, f.missing), want: 4},
		{name: "verify-quorum: no -k", args: []string{"verify-quorum", "-name", "q", "-tally", f.tally, "-vote", f.voteA, "-pubkey", f.pub}, want: 2},
		{name: "verify-quorum: a vote as the commit (wrong type)", args: quorum("q", f.voteA, f.voteB, "-commit", f.voteB), want: 4},
		{name: "verify-quorum: a governed commit", args: quorum("q", f.voteA, f.voteB, "-commit", f.action), want: 0},
		{name: "verify-quorum: tampered vote beside a missing vote", args: quorum("q", f.voteATampered, f.missing), want: 1},
		{name: "verify-quorum: missing vote beside a tampered commit", args: quorum("q", f.voteA, f.missing, "-commit", f.actionTampered), want: 1},

		// verify-run
		{name: "verify-run: genuine", args: runc(f.runCert, "-approved-file", f.allow), want: 0},
		{name: "verify-run: tampered", args: runc(f.runCertTampered, "-approved", f.digest), want: 1},
		{name: "verify-run: policy outside the allowlist", args: runc(f.runCert, "-approved", "other"), want: 1},
		{name: "verify-run: no allowlist", args: runc(f.runCert), want: 2},
		{name: "verify-run: missing certificate", args: runc(f.missing, "-approved", f.digest), want: 4},
		{name: "verify-run: a proof bundle as the certificate (format)", args: runc(f.bundle, "-approved", f.digest), want: 4},
		{name: "verify-run: missing allowlist file", args: runc(f.runCert, "-approved-file", f.missing), want: 4},
		{name: "verify-run: tampered beside a missing allowlist file", args: runc(f.runCertTampered, "-approved-file", f.missing), want: 1},
		{name: "verify-run: agreeing checker", args: runc(f.runCert, "-approved-file", f.allow, "-checker", f.agree), want: 0, checker: true},
		{name: "verify-run: checker disagrees", args: runc(f.runCert, "-approved", f.digest, "-checker", f.nonConvergent), want: 1, checker: true},
		{name: "verify-run: checker gives no verdict", args: runc(f.runCert, "-approved-file", f.allow, "-checker", f.broken), want: 3, checker: true},
		{name: "verify-run: no verdict on one policy, a disagreement on the other", args: runc(f.runCert, "-approved-file", f.allow, "-checker", f.brokenFirst), want: 1, checker: true},
		{name: "verify-run: a disagreement on one policy, no verdict on the other", args: runc(f.runCert, "-approved-file", f.allow, "-checker", f.brokenSecond), want: 1, checker: true},
		{name: "verify-run: missing allowlist file beside a broken checker", args: runc(f.runCert, "-approved-file", f.missing, "-checker", f.broken), want: 4, checker: true},
		{name: "verify-run: missing allowlist file beside a disagreeing checker", args: runc(f.runCert, "-approved-file", f.missing, "-checker", f.nonConvergent), want: 1, checker: true},

		// verify-evidence
		{name: "verify-evidence: genuine", args: []string{"verify-evidence", "-evidence", f.evidence, "-pubkey", f.pub}, want: 0},
		{name: "verify-evidence: tampered", args: []string{"verify-evidence", "-evidence", f.evidenceTampered, "-pubkey", f.pub}, want: 1},
		{name: "verify-evidence: missing package", args: []string{"verify-evidence", "-evidence", f.missing, "-pubkey", f.pub}, want: 4},
		{name: "verify-evidence: a proof bundle as the package (format)", args: []string{"verify-evidence", "-evidence", f.bundle, "-pubkey", f.pub}, want: 4},
		{name: "verify-evidence: missing allowlist file", args: []string{"verify-evidence", "-evidence", f.evidence, "-pubkey", f.pub, "-approved-file", f.missing}, want: 4},
		{name: "verify-evidence: with a run certificate", args: []string{"verify-evidence", "-evidence", f.evidenceCert, "-pubkey", f.pub, "-approved-file", f.allow}, want: 0},
		{name: "verify-evidence: run certificate policy outside the allowlist", args: []string{"verify-evidence", "-evidence", f.evidenceCert, "-pubkey", f.pub, "-approved", "other"}, want: 1},
		{name: "verify-evidence: run certificate beside a missing allowlist file", args: []string{"verify-evidence", "-evidence", f.evidenceCert, "-pubkey", f.pub, "-approved-file", f.missing}, want: 4},
		{name: "verify-evidence: tampered beside a missing allowlist file", args: []string{"verify-evidence", "-evidence", f.evidenceTampered, "-pubkey", f.pub, "-approved-file", f.missing}, want: 1},
		{name: "verify-evidence: no -pubkey", args: []string{"verify-evidence", "-evidence", f.evidence}, want: 2},
		{name: "verify-evidence: a bide.audit.evidence.v4 package", args: []string{"verify-evidence", "-evidence", f.evidenceV4, "-pubkey", f.pub}, want: 4},
		{name: "verify-evidence: another key", args: []string{"verify-evidence", "-evidence", f.evidence, "-pubkey", f.otherPub}, want: 1},

		// verify-approvals
		{name: "verify-approvals: no gate for the call", args: approvals(f.evidence, f.keys), want: 1},
		{name: "verify-approvals: the key file gives two ids one key, one outside the policy", args: approvals(f.evidence, f.sharedKeys), want: 4},
		{name: "verify-approvals: no -need", args: []string{"verify-approvals", "-evidence", f.evidence, "-pubkey", f.pub, "-call", "c1", "-approvers", "a", "-approver-keys", f.keys}, want: 2},
		{name: "verify-approvals: an approver listed twice", args: []string{"verify-approvals", "-evidence", f.missing, "-pubkey", f.pub, "-call", "c1", "-need", "1", "-approvers", "a,a", "-approver-keys", f.missing}, want: 2},
		{name: "verify-approvals: missing package", args: approvals(f.missing, f.keys), want: 4},
		{name: "verify-approvals: approver keys not JSON", args: approvals(f.evidence, f.notJSON), want: 4},
		{name: "verify-approvals: a small-order approver key", args: approvals(f.evidence, f.weakKeys), want: 4},
		{name: "verify-approvals: two approvers on one key", args: []string{"verify-approvals", "-evidence", f.evidence, "-pubkey", f.pub, "-call", "c1", "-need", "1", "-approvers", "a,b", "-approver-keys", f.sharedKeys}, want: 4},

		// prove-absent
		{name: "prove-absent: genuine", args: []string{"prove-absent", "-journal", f.journal, "-sth", f.absSTH, "-key", "tool:refund"}, want: 0},
		{name: "prove-absent: bad -key", args: []string{"prove-absent", "-journal", f.journal, "-sth", f.absSTH, "-key", "refund"}, want: 2},
		{name: "prove-absent: missing journal", args: []string{"prove-absent", "-journal", f.missing, "-sth", f.absSTH, "-key", "tool:refund"}, want: 4},
		{name: "prove-absent: a journal export of another format", args: []string{"prove-absent", "-journal", f.journalWrongFormat, "-sth", f.absSTH, "-key", "tool:refund"}, want: 4},
		{name: "prove-absent: a present key", args: []string{"prove-absent", "-journal", f.journal, "-sth", f.absSTH, "-key", "tool:c1"}, want: 4},
		{name: "prove-absent: a policy key against a tool-use head", args: []string{"prove-absent", "-journal", f.journal, "-sth", f.absSTH, "-key", "policy:x"}, want: 4},

		// verify-absent
		{name: "verify-absent: genuine", args: []string{"verify-absent", "-bundle", f.absBundle, "-pubkey", f.pub}, want: 0},
		{name: "verify-absent: tampered", args: []string{"verify-absent", "-bundle", f.absTampered, "-pubkey", f.pub}, want: 1},
		{name: "verify-absent: a proof bundle as -bundle (format)", args: []string{"verify-absent", "-bundle", f.bundle, "-pubkey", f.pub}, want: 4},
		{name: "verify-absent: a key of no known set (unsupported)", args: []string{"verify-absent", "-bundle", f.absUnknownKey, "-pubkey", f.pub}, want: 4},
		{name: "verify-absent: missing bundle", args: []string{"verify-absent", "-bundle", f.missing, "-pubkey", f.pub}, want: 4},
		{name: "verify-absent: unknown flag", args: []string{"verify-absent", "-bundel", f.absBundle}, want: 2},
	}
}

// TestExitTable runs every verb over genuine, tampered, unreadable and wrong-type inputs, and over
// inputs that meet several conditions at once, and checks each exits with its status: 0 verified,
// 1 not verified, 2 usage, 3 no verdict, 4 unusable, with 1 over 4 over 3.
func TestExitTable(t *testing.T) {
	f := newExitFixture(t)
	for _, tc := range exitCases(f) {
		if tc.checker && runtime.GOOS == "windows" {
			continue
		}
		code, stdout, stderr := runCLI(tc.args...)
		if code != tc.want {
			t.Errorf("%s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", tc.name, code, tc.want, stdout, stderr)
		}
	}
}

// Every verb is covered by the exit table, with a genuine case (exit 0) and a failing one.
func TestExitTableCoversEveryVerb(t *testing.T) {
	f := &exitFixture{}
	seen := map[string]map[int]bool{}
	for _, tc := range exitCases(f) {
		if len(tc.args) == 0 {
			continue
		}
		if seen[tc.args[0]] == nil {
			seen[tc.args[0]] = map[int]bool{}
		}
		seen[tc.args[0]][tc.want] = true
	}
	for verb := range verbs {
		codes := seen[verb]
		if len(codes) < 2 || !codes[1] && !codes[4] {
			t.Errorf("verb %s: exit table covers codes %v, want at least one success or failure and one unusable or not-verified case", verb, codes)
		}
		if !codes[2] {
			t.Errorf("verb %s: exit table has no usage case", verb)
		}
	}
}

// Only 0 means verified: under -json, "verified" is true exactly when the exit status is 0 for a
// verify verb (never for a produce verb), and the report's exit_code is the process's.
func TestOnlyZeroMeansVerified(t *testing.T) {
	f := newExitFixture(t)
	for _, tc := range exitCases(f) {
		if tc.checker && runtime.GOOS == "windows" || len(tc.args) == 0 || tc.args[0] == "-version" {
			continue
		}
		if _, known := verbs[tc.args[0]]; !known {
			continue
		}
		code, stdout, stderr := runCLI(append([]string{"-json"}, tc.args...)...)
		if code != tc.want {
			t.Errorf("%s under -json: exit %d, want %d\n%s", tc.name, code, tc.want, stderr)
			continue
		}
		var rep report
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
			t.Errorf("%s: stdout under -json is not one JSON report: %v\n%s", tc.name, err, stdout)
			continue
		}
		wantVerified := code == 0 && !produceVerbs[tc.args[0]]
		if rep.ExitCode != code || rep.Verified != wantVerified || rep.Result != resultName(tc.args[0], code) || rep.Verb != tc.args[0] {
			t.Errorf("%s: report exit_code=%d verified=%v result=%q verb=%q, want %d %v %q %q",
				tc.name, rep.ExitCode, rep.Verified, rep.Result, rep.Verb, code, wantVerified, resultName(tc.args[0], code), tc.args[0])
		}
		if code != 0 && len(rep.Errors) == 0 && !slices.ContainsFunc(rep.Output, func(l string) bool { return strings.HasPrefix(l, "FAIL") || strings.HasPrefix(l, "ERROR") }) && code != 2 {
			t.Errorf("%s: exit %d with no error and no FAIL or ERROR line in the report\n%s", tc.name, code, stdout)
		}
	}
}

// exitFor maps each error class to its status, and a joined error to the highest-ranked class in
// it: 2 exclusive, then 1, then 4, then 3. No error but nil maps to 0.
func TestExitForPrecedence(t *testing.T) {
	notVerified := &cliError{class: errNotVerified, err: errors.New("tampered")}
	unreadable := unusable(&fs.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist})
	noVerdict := &cliError{class: errNoVerdict, err: errors.New("checker exit 7")}
	usage := &cliError{class: errUsage, err: errors.New("bad flag")}
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"nil":                         {nil, 0},
		"not verified":                {notVerified, 1},
		"usage":                       {usage, 2},
		"no verdict":                  {noVerdict, 3},
		"unreadable":                  {unreadable, 4},
		"unclassified error":          {errors.New("boom"), 3},
		"bare ErrFormat":              {fmt.Errorf("x: %w", audit.ErrFormat), 4},
		"bare not-exist":              {fmt.Errorf("x: %w", fs.ErrNotExist), 4},
		"bare permission":             {fmt.Errorf("x: %w", fs.ErrPermission), 4},
		"output write failure":        {internal(&fs.PathError{Op: "open", Path: "out", Err: fs.ErrPermission}), 3},
		"tampered + unreadable":       {errors.Join(notVerified, unreadable), 1},
		"unreadable + tampered":       {errors.Join(unreadable, notVerified), 1},
		"checker + unreadable":        {errors.Join(noVerdict, unreadable), 4},
		"unreadable + checker":        {errors.Join(unreadable, noVerdict), 4},
		"checker + tampered":          {errors.Join(noVerdict, notVerified), 1},
		"all three":                   {errors.Join(noVerdict, unreadable, notVerified), 1},
		"usage beside anything":       {errors.Join(notVerified, usage, unreadable), 2},
		"nested join":                 {errors.Join(noVerdict, errors.Join(unreadable, notVerified)), 1},
		"unclassified + unreadable":   {errors.Join(errors.New("boom"), unreadable), 4},
		"format + checker":            {errors.Join(noVerdict, unusable(fmt.Errorf("f: %w", audit.ErrFormat))), 4},
		"not verified wrapping a 404": {&cliError{class: errNotVerified, err: fmt.Errorf("x: %w", fs.ErrNotExist)}, 1},
		"bare audit.ErrNotVerified":   {fmt.Errorf("x: %w", audit.ErrNotVerified), 1},
		"bare audit.ErrMalformed":     {fmt.Errorf("x: %w", audit.ErrMalformed), 4},
		"malformed + audit verdict":   {errors.Join(unusable(fmt.Errorf("m: %w", audit.ErrMalformed)), fmt.Errorf("v: %w", audit.ErrNotVerified)), 1},
		"audit verdict + usage":       {errors.Join(fmt.Errorf("v: %w", audit.ErrNotVerified), usage), 2},
		"checker + audit malformed":   {errors.Join(noVerdict, fmt.Errorf("m: %w", audit.ErrMalformed)), 4},
	} {
		if got := exitFor(tc.err); got != tc.want {
			t.Errorf("%s: exitFor = %d, want %d", name, got, tc.want)
		}
		if tc.err != nil && exitFor(tc.err) == 0 {
			t.Errorf("%s: a non-nil error maps to 0", name)
		}
	}
}

// A panic inside a verb is a bug in the CLI, not a verdict: it is no verdict (3), not Go's own
// panic status (2, the usage status).
func TestPanicIsNoVerdict(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr, out: &stdout}
	err := c.call(func([]string) { panic("boom") }, nil)
	if got := exitFor(err); got != exitNoVerdict || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("a panicking verb: exit %d, err %v; want %d naming the panic", got, err, exitNoVerdict)
	}
}

// -json may come before or after the verb; either way stdout is exactly one JSON report, and a
// produce verb's artifact is embedded in it.
func TestJSONReport(t *testing.T) {
	f := newExitFixture(t)
	for _, args := range [][]string{
		{"-json", "verify", "-bundle", f.bundle, "-pubkey", f.pub},
		{"verify", "-json", "-bundle", f.bundle, "-pubkey", f.pub},
		{"verify", "-bundle", f.bundle, "-pubkey", f.pub, "-json"},
	} {
		code, stdout, stderr := runCLI(args...)
		var rep report
		dec := json.NewDecoder(strings.NewReader(stdout))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rep); err != nil || dec.More() {
			t.Fatalf("%v: stdout is not one JSON report (%v):\n%s", args, err, stdout)
		}
		if code != 0 || !rep.Verified || rep.Tool != "bide-audit" || len(rep.Output) != 1 || !strings.HasPrefix(rep.Output[0], "OK: ") || stderr != "" {
			t.Fatalf("%v: exit %d, report %+v, stderr %q", args, code, rep, stderr)
		}
	}
	code, stdout, _ := runCLI("-json", "prove", "-journal", f.journal, "-sth", f.sth, "-tool", "c1")
	var rep report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil || code != 0 || rep.Verified || rep.Result != "produced" {
		t.Fatalf("prove -json: exit %d, err %v, report %+v", code, err, rep)
	}
	var bundle audit.ProofBundle
	if err := audit.UnmarshalStrict(rep.Artifact, &bundle); err != nil || recordOf(bundle).ToolUseID != "c1" {
		t.Fatalf("prove -json: the artifact is not the bundle (%v): %s", err, rep.Artifact)
	}
	code, stdout, _ = runCLI("-json", "verify", "-bundle", f.missing, "-pubkey", f.pub)
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil || code != 4 || rep.Verified || rep.Result != "unusable_input" || len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "no-such-file") {
		t.Fatalf("verify -json of a missing bundle: exit %d, err %v, report %+v", code, err, rep)
	}
}

// -version prints the version and every artifact format this version reads, as text or JSON.
func TestVersion(t *testing.T) {
	code, stdout, _ := runCLI("-version")
	if code != 0 || !strings.HasPrefix(stdout, "bide-audit ") {
		t.Fatalf("-version: exit %d:\n%s", code, stdout)
	}
	formats := []string{audit.ProofFormat, audit.AbsenceFormat, audit.RunCertificateFormat, audit.EvidenceFormat, audit.STHFormat, audit.JournalExportFormat}
	for _, f := range formats {
		if !strings.Contains(stdout, f) {
			t.Errorf("-version does not name format %s:\n%s", f, stdout)
		}
	}
	code, stdout, _ = runCLI("-version", "-json")
	var v struct {
		Tool, Version, Go string
		Formats           []artifactFormat
	}
	if err := json.Unmarshal([]byte(stdout), &v); err != nil || code != 0 || v.Tool != "bide-audit" || v.Version == "" {
		t.Fatalf("-version -json: exit %d, err %v:\n%s", code, err, stdout)
	}
	var got []string
	for _, f := range v.Formats {
		got = append(got, f.Format)
	}
	if !slices.Equal(got, formats) {
		t.Errorf("-version -json formats %v, want %v", got, formats)
	}
}

// A bundle the verifier cannot check (here, of another format) is an unusable input, not a verdict
// and not a checker failure.
func TestVerifyBundleErrorIsUnusable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr, out: &stdout}
	err := c.verifyBundle(audit.ProofBundle{Format: "bide.audit.proof.v0"}, audit.Ed25519Verifier{Pub: make([]byte, ed25519.PublicKeySize)}, "action")
	if got := exitFor(err); got != exitUnusable || !errors.Is(err, audit.ErrFormat) {
		t.Fatalf("verifyBundle of an unreadable bundle: %v, exit %d; want %d wrapping audit.ErrFormat", err, got, exitUnusable)
	}
}

// The version comes from the linker stamp when a release build sets one, and otherwise falls back
// to the module version in the build information, then to "devel".
func TestVersionFrom(t *testing.T) {
	info := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/bide-ai/bide", Version: v}}
	}
	for name, tc := range map[string]struct {
		stamped string
		bi      *debug.BuildInfo
		ok      bool
		want    string
	}{
		"stamp wins over build info": {"v0.7.0", info("v0.6.0"), true, "v0.7.0"},
		"build info when unstamped":  {"", info("v0.6.0"), true, "v0.6.0"},
		"pseudo-version":             {"", info("v0.6.1-0.20260929120000-abcdef123456+dirty"), true, "v0.6.1-0.20260929120000-abcdef123456+dirty"},
		"devel build":                {"", info("(devel)"), true, "devel"},
		"empty module version":       {"", info(""), true, "devel"},
		"no build info":              {"", nil, false, "devel"},
	} {
		if got := versionFrom(tc.stamped, tc.bi, tc.ok); got != tc.want {
			t.Errorf("%s: versionFrom = %q, want %q", name, got, tc.want)
		}
	}
}

// A binary built the way .goreleaser.yaml builds it reports the stamped version, and one built
// without the stamp still reports a version. It also holds the release config to the stamp, and
// the stamp to the variable's name: renaming main.version makes -X a no-op, which this catches.
func TestVersionStamp(t *testing.T) {
	cfg, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "-X main.version={{ .Tag }}") {
		t.Fatalf(".goreleaser.yaml does not stamp main.version:\n%s", cfg)
	}
	dir := t.TempDir()
	for _, tc := range []struct{ ldflags, want string }{
		{"-X main.version=v9.8.7", "bide-audit v9.8.7 "},
		{"", "bide-audit "},
	} {
		bin := auditBin(dir)
		args := []string{"build", "-o", bin}
		if tc.ldflags != "" {
			args = append(args, "-ldflags", tc.ldflags)
		}
		if out, err := exec.Command("go", append(args, ".")...).CombinedOutput(); err != nil {
			t.Fatalf("build %q: %v\n%s", tc.ldflags, err, out)
		}
		out, err := exec.Command(bin, "-version").Output()
		if err != nil || !strings.HasPrefix(string(out), tc.want) || strings.HasPrefix(string(out), "bide-audit  ") {
			t.Errorf("ldflags %q: -version printed %q (err %v), want prefix %q and a version", tc.ldflags, out, err, tc.want)
		}
	}
}

// A weak approver key is refused when the key file is read, naming the approver and the reason,
// before the policy's key check would refuse it for reporting no key identity.
func TestVerifyApprovals_WeakApproverKeyNamed(t *testing.T) {
	f := newExitFixture(t)
	code, stdout, stderr := runCLI("verify-approvals", "-evidence", f.evidence, "-pubkey", f.pub, "-call", "c1", "-need", "1", "-approvers", "a", "-approver-keys", f.weakKeys)
	if out := stdout + stderr; code != 4 || !strings.Contains(out, `approver "a" key: audit: weak ed25519 public key`) {
		t.Fatalf("exit %d, want 4 naming approver a's weak key\n%s", code, out)
	}
}

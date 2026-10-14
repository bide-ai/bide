package auditcli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// runOut runs the CLI in process and returns the exit status and stdout and stderr together.
func runOut(args ...string) (int, string) {
	code, stdout, stderr := runCLI(args...)
	return code, stdout + stderr
}

// scriptChecker writes an executable stand-in for the external oracle that runs body.
func scriptChecker(t *testing.T, dir, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in oracle is a POSIX shell script")
	}
	p := filepath.Join(dir, name+".sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// leafFilesAt is leafFiles with a head per record: bundle i is proven against a head signed when
// heads[i] records had been journaled, so two bundles can sit in different signed trees of one run.
func leafFilesAt(t *testing.T, dir string, recs []agent.Record, heads []int) ([]string, string) {
	t.Helper()
	ctx := context.Background()
	store := agenttest.MemJournal()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := audit.Ed25519Signer{Priv: priv}
	sub, err := os.MkdirTemp(dir, "leaves")
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(heads))
	for n := 1; n <= len(recs); n++ {
		r := recs[n-1]
		if _, err := journaltest.Do(ctx, store, "run1", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
		var sth *audit.SignedTreeHead
		for i, h := range heads {
			if h != n {
				continue
			}
			if sth == nil {
				th, err := audit.NewTreeHead(ctx, store, "run1", 1)
				if err != nil {
					t.Fatal(err)
				}
				s := signHead(t, th, signer)
				sth = &s
			}
			b, err := audit.ProveRecord(ctx, store, "run1", i+1, *sth)
			if err != nil {
				t.Fatal(err)
			}
			paths[i] = filepath.Join(sub, fmt.Sprintf("leaf%d.json", i))
			writeJSON(t, paths[i], b)
		}
	}
	return paths, hex.EncodeToString(pub)
}

func policyLeafJSON(digest, policy string) string {
	p, _ := json.Marshal(policy)
	return `{"digest":"` + digest + `","policy":` + string(p) + `}`
}

func convLeafJSON(digest, cert string) string {
	return `{"digest":"` + digest + `","certificate":` + cert + `}`
}

func certJSON(t *testing.T, digest string, converges, compFree bool) string {
	t.Helper()
	b, err := json.Marshal(confluenceCert{Machine: "kyc", PolicyDigest: digest, Converges: converges, WFC: true, CC: true,
		MaxRepairLen: 1, PairsTotal: 1, PairsBrute: 1, States: 4, CompensationFree: compFree})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func wantAll(t *testing.T, what, out string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(out, s) {
			t.Errorf("%s: output lacks %q:\n%s", what, s, out)
		}
	}
}

// verify-governed-action refuses a policy leaf whose bytes do not hash to the digest it claims, an
// action that names another digest or none, and an action and policy in different signed trees.
func TestVerifyGovernedAction_Refusals(t *testing.T) {
	dir := t.TempDir()
	policy, digest := testPolicy()
	action := `{"event":"approve","applied":true,"policy_digest":"` + digest + `"}`
	run := func(recs []agent.Record, heads []int) (int, string) {
		paths, pub := leafFilesAt(t, dir, recs, heads)
		return runOut("verify-governed-action", "-action", paths[0], "-policy-bundle", paths[1], "-pubkey", pub)
	}
	good := []agent.Record{toolLeaf("act", action), valueLeaf("audit:policy:"+digest, policyLeafJSON(digest, policy)), toolLeaf("pad", `{}`)}
	if code, out := run(good, []int{3, 3}); code != 0 {
		t.Fatalf("genuine pair: exit %d\n%s", code, out)
	}
	for name, tc := range map[string]struct {
		recs  []agent.Record
		heads []int
		want  string
	}{
		"bytes do not hash to the digest": {
			[]agent.Record{toolLeaf("act", action), valueLeaf("audit:policy:"+digest, policyLeafJSON(digest, "(doms 9 9)\n"))},
			[]int{2, 2}, "policy leaf lies about its digest",
		},
		"action names another policy": {
			[]agent.Record{toolLeaf("act", `{"event":"approve","applied":true,"policy_digest":"00ff"}`), valueLeaf("audit:policy:"+digest, policyLeafJSON(digest, policy))},
			[]int{2, 2}, `action policy digest "00ff" does not link to the anchored policy leaf`,
		},
		"action names no policy": {
			[]agent.Record{toolLeaf("act", `{"event":"approve","applied":true,"policy_digest":""}`), valueLeaf("audit:policy:"+digest, policyLeafJSON(digest, policy))},
			[]int{2, 2}, `action policy digest "" does not link`,
		},
		"different signed trees": {good, []int{2, 3}, "the action and policy are not committed in the same signed tree"},
	} {
		code, out := run(tc.recs, tc.heads)
		if code != exitNotVerified {
			t.Errorf("%s: exit %d, want %d\n%s", name, code, exitNotVerified, out)
		}
		wantAll(t, name, out, tc.want)
	}
}

// verify-convergence: a certificate for another digest, one whose payload does not read strictly,
// and one in another tree than the policy are refused; with -checker, the oracle's verdict and
// compensation_free classification must agree with the certificate.
func TestVerifyConvergence_Refusals(t *testing.T) {
	dir := t.TempDir()
	policy, digest := testPolicy()
	pl := valueLeaf("audit:policy:"+digest, policyLeafJSON(digest, policy))
	cert := certJSON(t, digest, true, false)
	run := func(recs []agent.Record, heads []int, extra ...string) (int, string) {
		paths, pub := leafFilesAt(t, dir, recs, heads)
		return runOut(append([]string{"verify-convergence", "-cert-bundle", paths[0], "-policy-bundle", paths[1], "-pubkey", pub}, extra...)...)
	}
	cl := valueLeaf("audit:convergence:"+digest, convLeafJSON(digest, cert))

	code, out := run([]agent.Record{valueLeaf("audit:convergence:other", convLeafJSON("other", cert)), pl}, []int{2, 2})
	if code != exitNotVerified {
		t.Errorf("certificate for another digest: exit %d\n%s", code, out)
	}
	wantAll(t, "another digest", out, `certificate digest "other" does not link to the anchored policy leaf`)

	code, out = run([]agent.Record{valueLeaf("audit:convergence:"+digest, convLeafJSON(digest, `{"bogus":1}`)), pl}, []int{2, 2})
	if code != exitUnusable {
		t.Errorf("certificate payload with an unknown field: exit %d, want %d\n%s", code, exitUnusable, out)
	}
	wantAll(t, "payload", out, "convergence certificate payload")

	code, out = run([]agent.Record{cl, pl, toolLeaf("pad", `{}`)}, []int{2, 3})
	if code != exitNotVerified {
		t.Errorf("different trees: exit %d\n%s", code, out)
	}
	wantAll(t, "different trees", out, "the certificate and policy are not committed in the same signed tree")

	// With the oracle.
	for name, tc := range map[string]struct {
		cert    string
		checker string
		code    int
		want    []string
	}{
		"oracle disagrees on convergence": {certJSON(t, digest, true, false), "echo compensation_free=false; exit 1", exitNotVerified,
			[]string{"certificate claims converges=true but the verified oracle says converges=false"}},
		"oracle disagrees on classification": {certJSON(t, digest, true, true), "echo compensation_free=false; exit 0", exitNotVerified,
			[]string{"certificate claims compensation_free=true but the verified oracle says compensation_free=false"}},
		"oracle reports no classification": {certJSON(t, digest, true, false), "echo converges; exit 0", exitVerified,
			[]string{"note: this oracle does not certify the compensation-free classification", "OK: the verified oracle's verdict agrees"}},
		"oracle output without a newline": {certJSON(t, digest, true, false), "printf compensation_free=false; exit 0", exitVerified,
			[]string{"oracle: compensation_free=false\n"}},
		"both agree it does not converge": {certJSON(t, digest, false, false), "echo compensation_free=false; exit 1", exitNotVerified,
			[]string{"the certificate and the oracle agree the policy does NOT converge; do not deploy it"}},
		"unreadable classification": {certJSON(t, digest, true, false), "echo compensation_free=maybe; exit 0", exitNoVerdict,
			[]string{`printed an unreadable classification line "compensation_free=maybe"`, "no verdict on whether it converges"}},
		"contradictory classification": {certJSON(t, digest, true, false), "echo compensation_free=true; echo compensation_free=false; exit 0", exitNoVerdict,
			[]string{"printed both compensation_free=true and compensation_free=false"}},
		"checker exits with another status": {certJSON(t, digest, true, false), "exit 2", exitNoVerdict,
			[]string{"gave no verdict"}},
		"classification true agrees": {certJSON(t, digest, true, true), "echo compensation_free=true; exit 0", exitVerified, nil},
	} {
		checker := scriptChecker(t, dir, "oracle", tc.checker)
		recs := []agent.Record{valueLeaf("audit:convergence:"+digest, convLeafJSON(digest, tc.cert)), pl}
		code, out := run(recs, []int{2, 2}, "-checker", checker)
		if code != tc.code {
			t.Errorf("%s: exit %d, want %d\n%s", name, code, tc.code, out)
		}
		wantAll(t, name, out, tc.want...)
	}
}

func TestVerifyConvergence_ObligationsLines(t *testing.T) {
	legacy := confluenceCert{}.obligations()
	if len(legacy) != 1 || !strings.Contains(legacy[0], "records no delivery obligations") {
		t.Errorf("a certificate with no obligation lists: %q", legacy)
	}
	// Lists that exist and are empty record that there are none: no line at all.
	if got := (confluenceCert{CausalOrderRequired: []eventPair{}, NotIdempotent: []string{}, Saturations: []saturation{}}).obligations(); len(got) != 0 {
		t.Errorf("empty obligation lists: %q", got)
	}
	got := confluenceCert{
		CausalOrderRequired: []eventPair{{"open", "close"}, {"a", "b"}},
		NotIdempotent:       []string{"inc", "dec"},
		Saturations:         []saturation{{Rule: `event "inc"`, Var: "n", States: 2}},
	}.obligations()
	want := []string{
		"obligation: causal order required for 2 undeclared pair(s): open/close, a/b (convergence holds only if the two events of each pair are never concurrent)",
		"obligation: delivery exactly once: inc, dec (a duplicate changes the result)",
		`note: saturation: event "inc" clamps its write to n on 2 state(s)`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("obligations:\n%q\nwant\n%q", got, want)
	}
}

// verify-quorum recomputes the tally from the disclosed votes and refuses every way the two can
// disagree.
func TestVerifyQuorum_Refusals(t *testing.T) {
	dir := t.TempDir()
	vote := func(voter, decision string) agent.Record {
		return valueLeaf("quorum/q/vote/"+voter, `{"voter":"`+voter+`","decision":"`+decision+`"}`)
	}
	tally := func(decision string, forN, total int, votes ...string) agent.Record {
		var vs []string
		for _, v := range votes {
			p := strings.SplitN(v, "=", 2)
			vs = append(vs, `{"voter":"`+p[0]+`","decision":"`+p[1]+`"}`)
		}
		return valueLeaf("quorum/q/tally", fmt.Sprintf(`{"decision":%q,"votes_for":%d,"total":%d,"agreed":true,"votes":[%s]}`, decision, forN, total, strings.Join(vs, ",")))
	}
	// recs ends with the tally; the votes disclosed are all the leaves before it.
	run := func(k int, recs ...agent.Record) (int, string) {
		paths, pub := leafFiles(t, dir, recs...)
		args := []string{"verify-quorum", "-name", "q", "-tally", paths[len(paths)-1], "-pubkey", pub, "-k", fmt.Sprint(k)}
		for _, p := range paths[:len(paths)-1] {
			args = append(args, "-vote", p)
		}
		return runOut(args...)
	}
	ok := tally("approve", 2, 2, "a=approve", "b=approve")
	if code, out := run(2, vote("a", "approve"), vote("b", "approve"), ok); code != 0 {
		t.Fatalf("genuine quorum: exit %d\n%s", code, out)
	}
	for name, tc := range map[string]struct {
		k     int
		recs  []agent.Record
		code  int
		wants []string
	}{
		"a voter recorded twice": {2, []agent.Record{vote("a", "approve"), vote("b", "approve"), tally("approve", 2, 2, "a=approve", "a=approve")}, exitNotVerified,
			[]string{`the tally records a vote by "a" more than once`}},
		"a recorded vote not disclosed": {2, []agent.Record{vote("a", "approve"), vote("b", "approve"), tally("approve", 2, 2, "a=approve", "c=approve")}, exitNotVerified,
			[]string{`the tally records a vote by "c" that is not disclosed`}},
		"a recorded decision differs from the disclosed one": {2, []agent.Record{vote("a", "approve"), vote("b", "approve"), tally("approve", 2, 2, "a=reject", "b=approve")}, exitNotVerified,
			[]string{`the tally records "a" voting "reject", but the disclosed vote is "approve"`}},
		"fewer votes disclosed than the total": {2, []agent.Record{vote("a", "approve"), tally("approve", 1, 2, "a=approve")}, exitNotVerified,
			[]string{"1 vote bundles disclosed but the tally records 1 votes and total=2"}},
		"a forged decision": {2, []agent.Record{vote("a", "approve"), vote("b", "approve"), tally("reject", 2, 2, "a=approve", "b=approve")}, exitNotVerified,
			[]string{`recorded tally (decision="reject" votes_for=2) does not match the disclosed votes (decision="approve" votes_for=2)`}},
		"a forged vote count": {2, []agent.Record{vote("a", "approve"), vote("b", "approve"), tally("approve", 1, 2, "a=approve", "b=approve")}, exitNotVerified,
			[]string{`does not match the disclosed votes (decision="approve" votes_for=2)`}},
		"a tie for the most votes": {1, []agent.Record{vote("a", "approve"), vote("b", "reject"), tally("approve", 1, 2, "a=approve", "b=reject")}, exitNotVerified,
			[]string{`no single decision has the most votes: "approve" and "reject" each have 1`}},
		"below k": {3, []agent.Record{vote("a", "approve"), vote("b", "approve"), ok}, exitNotVerified,
			[]string{`quorum not met: votes_for=2 < k=3`}},
		"a vote filed under another voter's step": {2, []agent.Record{valueLeaf("quorum/q/vote/zz", `{"voter":"a","decision":"approve"}`), vote("b", "approve"), ok}, exitNotVerified,
			[]string{`vote bundle 0 is step "quorum/q/vote/zz", not "quorum/q/vote/a"`}},
		"a vote that is not a vote leaf": {2, []agent.Record{valueLeaf("quorum/q/vote/a", `[1]`), vote("b", "approve"), ok}, exitUnusable,
			[]string{"vote bundle 0 is not a vote leaf"}},
		"a tally that is not a tally leaf": {2, []agent.Record{vote("a", "approve"), vote("b", "approve"), valueLeaf("quorum/q/tally", `"x"`)}, exitUnusable,
			[]string{"tally bundle is not a quorum tally"}},
		"the tally of another quorum": {2, []agent.Record{vote("a", "approve"), vote("b", "approve"), valueLeaf("quorum/other/tally", `{"decision":"approve","votes_for":2,"total":2,"agreed":true,"votes":[]}`)}, exitNotVerified,
			[]string{`the tally bundle is step "quorum/other/tally", not "quorum/q/tally"`}},
	} {
		code, out := run(tc.k, tc.recs...)
		if code != tc.code {
			t.Errorf("%s: exit %d, want %d\n%s", name, code, tc.code, out)
		}
		wantAll(t, name, out, tc.wants...)
		if tc.code != 0 && strings.Contains(out, "OK:") {
			t.Errorf("%s: a refused quorum printed an OK line:\n%s", name, out)
		}
	}
}

func TestVerifyQuorum_CommitChecks(t *testing.T) {
	dir := t.TempDir()
	_, digest := testPolicy()
	votes := []agent.Record{
		valueLeaf("quorum/q/vote/a", `{"voter":"a","decision":"approve"}`),
		valueLeaf("quorum/q/vote/b", `{"voter":"b","decision":"approve"}`),
		valueLeaf("quorum/q/tally", `{"decision":"approve","votes_for":2,"total":2,"agreed":true,"votes":[{"voter":"a","decision":"approve"},{"voter":"b","decision":"approve"}]}`),
	}
	run := func(commit agent.Record, heads []int) (int, string) {
		paths, pub := leafFilesAt(t, dir, append(append([]agent.Record(nil), votes...), commit, toolLeaf("pad", `{}`)), heads)
		return runOut("verify-quorum", "-name", "q", "-tally", paths[2], "-vote", paths[0], "-vote", paths[1], "-commit", paths[3], "-pubkey", pub, "-k", "2")
	}
	governed := toolLeaf("commit", `{"event":"approve","applied":true,"policy_digest":"`+digest+`"}`)
	if code, out := run(governed, []int{4, 4, 4, 4}); code != 0 || !strings.Contains(out, "a governed commit under policy "+digest) {
		t.Fatalf("governed commit: exit %d\n%s", code, out)
	}
	if code, out := run(toolLeaf("commit", `{"ok":true}`), []int{4, 4, 4, 4}); code != exitUnusable || !strings.Contains(out, "not a governed-action payload") {
		t.Errorf("a commit that is not a governed-action payload: exit %d\n%s", code, out)
	}
	code, out := run(toolLeaf("commit", `{"event":"approve","applied":true,"policy_digest":""}`), []int{4, 4, 4, 4})
	if code != exitNotVerified {
		t.Errorf("a commit naming no policy: exit %d\n%s", code, out)
	}
	wantAll(t, "ungoverned commit", out, "commit bundle is not a governed-action leaf: it names no policy digest")
	code, out = run(governed, []int{3, 3, 3, 4})
	if code != exitNotVerified {
		t.Errorf("a commit in a later tree: exit %d\n%s", code, out)
	}
	wantAll(t, "commit tree", out, "the commit is not in the same signed tree and run as the quorum")
	code, out = run(governed, []int{3, 4, 4, 4})
	if code != exitNotVerified {
		t.Errorf("a vote in an earlier tree: exit %d\n%s", code, out)
	}
	wantAll(t, "vote tree", out, "a vote is not committed in the same signed tree and run as the tally")
}

// verify-run's -checker cross-check: a certificate that overstates compensation_free, or that
// agrees with the oracle that the policy does not converge, fails.
func TestVerifyRun_OracleCrossChecks(t *testing.T) {
	f := newExitFixture(t)
	args := func(checker string) []string {
		return []string{"verify-run", "-cert", f.runCert, "-pubkey", f.pub, "-approved-file", f.allow, "-checker", checker}
	}
	dir := t.TempDir()
	code, out := runOut(args(scriptChecker(t, dir, "cf-true", "echo compensation_free=true; exit 0"))...)
	if code != exitNotVerified {
		t.Errorf("oracle says compensation_free=true, certificate says false: exit %d\n%s", code, out)
	}
	wantAll(t, "classification", out, "certificate claims compensation_free=false but the verified oracle says compensation_free=true")
	if strings.Contains(out, "whole run ran under approved") {
		t.Errorf("a disagreeing oracle still printed the success line:\n%s", out)
	}
	code, out = runOut(args(f.agree)...)
	if code != 0 {
		t.Errorf("agreeing oracle: exit %d\n%s", code, out)
	}
	// An obligations note is printed per policy.
	wantAll(t, "obligations", out, "policy "+f.digest+": note: this certificate records no delivery obligations")
}

// A run certificate whose used-policy head is dated before the journal head it is projected from is
// refused, whatever else holds.
func TestVerifyRun_UsedPolicyHeadCannotPredateTheJournalHead(t *testing.T) {
	f := newExitFixture(t)
	var rc audit.RunCertificate
	readFixture(t, f.runCert, &rc)
	rc.STH.TreeHead.TimestampNanos = rc.UsedPolicyAbsence.TreeHead.TimestampNanos + 8
	p := filepath.Join(t.TempDir(), "runcert-early.json")
	writeJSON(t, p, rc)
	code, out := runOut("verify-run", "-cert", p, "-pubkey", f.pub, "-approved-file", f.allow)
	if code != exitNotVerified {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if want := audit.CheckTimestampOrder(rc.STH.TreeHead, rc.UsedPolicyAbsence.TreeHead).Error(); !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

func TestProve_Selection(t *testing.T) {
	f := newExitFixture(t)
	for name, args := range map[string][]string{
		"neither -tool nor -index": {"prove", "-journal", f.journal, "-sth", f.sth},
		"both -tool and -index":    {"prove", "-journal", f.journal, "-sth", f.sth, "-tool", "c1", "-index", "1"},
	} {
		code, out := runOut(args...)
		if code != exitUsage || !strings.Contains(out, "give exactly one of -tool and -index") {
			t.Errorf("%s: exit %d\n%s", name, code, out)
		}
	}
	// -index and -tool build the same bundle for the same record.
	code, byTool, _ := runCLI("prove", "-journal", f.journal, "-sth", f.sth, "-tool", "c1")
	if code != 0 {
		t.Fatalf("prove -tool: exit %d", code)
	}
	var b audit.ProofBundle
	if err := json.Unmarshal([]byte(byTool), &b); err != nil {
		t.Fatal(err)
	}
	idx := fmt.Sprint(b.Inclusion.Index)
	code, byIndex, stderr := runCLI("prove", "-journal", f.journal, "-sth", f.sth, "-index", idx)
	if code != 0 || byIndex != byTool {
		t.Errorf("prove -index %s: exit %d (%s), output differs from -tool: %v", idx, code, stderr, byIndex != byTool)
	}
	// An index past the journal makes no proof: the inputs were read, and cannot make one.
	if code, out := runOut("prove", "-journal", f.journal, "-sth", f.sth, "-index", "999"); code != exitUnusable {
		t.Errorf("prove -index 999: exit %d\n%s", code, out)
	}
	// -out writes the bundle to the file, says so on stderr, and leaves stdout empty.
	out := filepath.Join(t.TempDir(), "bundle.json")
	code, stdout, stderr := runCLI("prove", "-journal", f.journal, "-sth", f.sth, "-tool", "c1", "-out", out)
	if code != 0 || stdout != "" || stderr != "wrote "+out+"\n" {
		t.Errorf("prove -out: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got, _ := os.ReadFile(out); strings.TrimSpace(string(got)) != strings.TrimSpace(byTool) {
		t.Errorf("-out wrote different bytes than stdout carries")
	}
	if code, verdict := runOut("verify", "-bundle", out, "-pubkey", f.pub); code != 0 {
		t.Errorf("the written bundle does not verify: exit %d\n%s", code, verdict)
	}
	// A path that cannot be written is an internal error, not a verdict on the inputs.
	if code, out := runOut("prove", "-journal", f.journal, "-sth", f.sth, "-tool", "c1", "-out", filepath.Join(t.TempDir(), "no", "dir", "b.json")); code != exitNoVerdict {
		t.Errorf("prove -out into a missing directory: exit %d, want %d\n%s", code, exitNoVerdict, out)
	}
}

// A journal export whose records do not decode is an unusable input, named by its path.
func TestProve_JournalWithUndecodableRecords(t *testing.T) {
	f := newExitFixture(t)
	p := filepath.Join(t.TempDir(), "journal-bad.json")
	writeJSON(t, p, audit.JournalExport{Format: audit.JournalExportFormat, RunID: "run1", Records: [][]byte{[]byte("not a record")}})
	code, out := runOut("prove", "-journal", p, "-sth", f.sth, "-tool", "c1")
	if code != exitUnusable || !strings.Contains(out, p) {
		t.Errorf("exit %d, want %d naming %s\n%s", code, exitUnusable, p, out)
	}
}

func TestStaticStore(t *testing.T) {
	ctx := context.Background()
	s := staticStore{recs: []agent.Record{valueLeaf("a", `1`), valueLeaf("b", `2`), valueLeaf("c", `3`)}}
	if _, _, err := s.Insert(ctx, "r", "x", nil); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("Insert = %v, want a read-only error", err)
	}
	if e, ok, err := s.Get(ctx, "r", "b"); err != nil || !ok || e.Name != "b" || e.Seq != 2 {
		t.Errorf("Get(b) = %+v, %v, %v; want seq 2", e, ok, err)
	}
	if _, ok, err := s.Get(ctx, "r", "zz"); err != nil || ok {
		t.Errorf("Get(zz) = %v, %v; want not found", ok, err)
	}
	var names []string
	for e, err := range s.Load(ctx, "r", 1) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "b,c" {
		t.Errorf("Load(after 1) = %v, want [b c]", names)
	}
	n := 0
	for range s.Load(ctx, "r", 0) {
		n++
		break
	}
	if n != 1 {
		t.Errorf("breaking out of Load: %d iterations", n)
	}
}

func TestReadInput_Refusals(t *testing.T) {
	// A directory opens and then fails to read: an unusable input, not a panic or an empty file.
	dir := t.TempDir()
	if _, err := readInput(dir); err == nil {
		t.Error("readInput(directory) succeeded")
	}
	if code, out := runOut("verify-governance", "-policy", dir); code != exitUnusable {
		t.Errorf("verify-governance on a directory: exit %d, want %d\n%s", code, exitUnusable, out)
	}
	// A file at exactly the cap reads; one byte over does not.
	p := filepath.Join(dir, "five")
	if err := os.WriteFile(p, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := runOut("verify-governance", "-policy", p, "-max-input-bytes", "5"); code != 0 {
		t.Errorf("a file at the cap: exit %d\n%s", code, out)
	}
	if code, out := runOut("verify-governance", "-policy", p, "-max-input-bytes", "4"); code != exitUnusable || !strings.Contains(out, "larger than the 4-byte input limit") {
		t.Errorf("a file over the cap: exit %d\n%s", code, out)
	}
}

func TestFlagLimits_AreUsageErrors(t *testing.T) {
	f := newExitFixture(t)
	for name, tc := range map[string]struct {
		extra []string
		want  string
	}{
		"zero -max-input-bytes":     {[]string{"-max-input-bytes", "0"}, "-max-input-bytes must be at least 1, got 0"},
		"negative -max-input-bytes": {[]string{"-max-input-bytes", "-7"}, "got -7"},
		"negative -max-clock-skew":  {[]string{"-max-clock-skew", "-1s"}, "-max-clock-skew must not be negative, got -1s"},
	} {
		code, out := runOut(append([]string{"verify-governance", "-policy", f.policyFile}, tc.extra...)...)
		if code != exitUsage || !strings.Contains(out, tc.want) {
			t.Errorf("%s: exit %d\n%s", name, code, out)
		}
	}
}

// -pubkey may name a file holding the key; the file is read under the input cap, with surrounding
// whitespace ignored.
func TestPubKey_FromFile(t *testing.T) {
	f := newExitFixture(t)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "log.pub")
	if err := os.WriteFile(keyFile, []byte("\n  "+f.pub+" \r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := runOut("verify", "-bundle", f.bundle, "-pubkey", keyFile); code != 0 {
		t.Errorf("key from a file: exit %d\n%s", code, out)
	}
	other := filepath.Join(dir, "other.pub")
	if err := os.WriteFile(other, []byte(f.otherPub), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := runOut("verify", "-bundle", f.bundle, "-pubkey", other); code != exitNotVerified {
		t.Errorf("another key from a file: exit %d\n%s", code, out)
	}
	// A key file over the cap is unusable and says so, rather than being read as a key.
	big := filepath.Join(dir, "big.pub")
	if err := os.WriteFile(big, bytes.Repeat([]byte("a"), 100), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runOut("verify", "-bundle", f.bundle, "-pubkey", big, "-max-input-bytes", "50")
	if code != exitUnusable || !strings.Contains(out, big+" is larger than the 50-byte input limit") {
		t.Errorf("oversized key file: exit %d\n%s", code, out)
	}
	if code, out := runOut("verify", "-bundle", f.bundle, "-pubkey", dir); code != exitUnusable {
		t.Errorf("a directory as the key: exit %d\n%s", code, out)
	}
	if code, out := runOut("verify", "-bundle", f.bundle, "-pubkey", filepath.Join(dir, "absent.pub")); code != exitUnusable || !strings.Contains(out, "-pubkey takes the log operator's public key") {
		t.Errorf("a missing key file: exit %d\n%s", code, out)
	}
}

// verify-governance's -digest is compared after trimming surrounding blanks, and a mismatch quotes
// the trimmed value.
func TestVerifyGovernance_DigestIsTrimmed(t *testing.T) {
	f := newExitFixture(t)
	if code, out := runOut("verify-governance", "-policy", f.policyFile, "-digest", " \t"+f.digest+" \t\r\n"); code != 0 {
		t.Errorf("padded digest: exit %d\n%s", code, out)
	}
	code, out := runOut("verify-governance", "-policy", f.policyFile, "-digest", "  abc \n")
	if code != exitNotVerified || !strings.Contains(out, "digest mismatch (expected abc)") {
		t.Errorf("wrong digest: exit %d\n%s", code, out)
	}
}

func TestTrimSpace(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "abc": "abc", " abc": "abc", "\tabc": "abc", "abc\n": "abc", "abc\r\n": "abc", "abc \t\n": "abc",
		"  abc  ": "abc", "\nabc": "\nabc", " \t ": "", "a b": "a b",
	} {
		if got := trimSpace(in); got != want {
			t.Errorf("trimSpace(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGapText(t *testing.T) {
	ts := func(d time.Duration) string {
		return agent.FormatTimestamp(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC).Add(d))
	}
	for name, tc := range map[string]struct {
		d    audit.DecisionTime
		want string
	}{
		"recorded after":  {audit.DecisionTime{DecidedAt: ts(0), RecordedAt: ts(2500 * time.Millisecond)}, "2.5s after the signed time"},
		"recorded before": {audit.DecisionTime{DecidedAt: ts(time.Minute), RecordedAt: ts(0)}, "1m0s before the signed time"},
		"same":            {audit.DecisionTime{DecidedAt: ts(0), RecordedAt: ts(0)}, "in the same millisecond as the signed time"},
		"not recorded":    {audit.DecisionTime{DecidedAt: ts(0)}, "gap unknown"},
		"not canonical":   {audit.DecisionTime{DecidedAt: ts(0), RecordedAt: "yesterday"}, "gap unknown"},
		"too far apart": {audit.DecisionTime{DecidedAt: "0001-01-01T00:00:00.000Z", RecordedAt: "9999-12-31T23:59:59.999Z"},
			"more than 290 years apart"},
	} {
		if got := gapText(tc.d); got != tc.want {
			t.Errorf("%s: gapText = %q, want %q", name, got, tc.want)
		}
	}
}

func TestEvidenceItemLine(t *testing.T) {
	for name, tc := range map[string]struct {
		it   audit.EvidenceItem
		want string
	}{
		"tool with a ref":     {audit.EvidenceItem{Kind: "tool", Label: "charge", Ref: "c1", Note: "included"}, `tool call "c1": included`},
		"tool without a ref":  {audit.EvidenceItem{Kind: "tool", Label: "charge", Note: "included"}, `tool call "charge": included`},
		"step":                {audit.EvidenceItem{Kind: "step", Label: "plan", Note: "included"}, `step "plan": included`},
		"grant":               {audit.EvidenceItem{Kind: "grant", Label: "ops", Note: "chain verifies"}, "grant ops: chain verifies"},
		"run certificate":     {audit.EvidenceItem{Kind: "run-certificate", Label: "x", Note: "ok"}, "run certificate: ok"},
		"consistency":         {audit.EvidenceItem{Kind: "consistency", Label: "growth from 3 to 5", Note: "ok"}, "growth from 3 to 5: ok"},
		"unknown kind":        {audit.EvidenceItem{Kind: "other", Label: "x", Note: "n"}, "x: n"},
		"a control in a note": {audit.EvidenceItem{Kind: "grant", Label: "ops\x1b[31m", Note: "line\nbreak"}, `grant "ops\x1b[31m": "line\nbreak"`},
	} {
		if got := evidenceItemLine(tc.it); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// decisionTimes prints nothing without decisions, says so when no approval-asked time is recorded,
// and prints a decision whose recorded time is missing as "not recorded".
func TestDecisionTimes(t *testing.T) {
	var buf bytes.Buffer
	c := &cli{out: &buf}
	c.decisionTimes(audit.ApprovalVerdict{}, time.Minute)
	if buf.Len() != 0 {
		t.Errorf("no decisions printed %q", buf.String())
	}
	c.decisionTimes(audit.ApprovalVerdict{Times: []audit.DecisionTime{{Approver: "alice", DecidedAt: "2026-10-13T12:00:00.000Z"}}}, time.Minute)
	wantAll(t, "decisionTimes", buf.String(),
		"INFO  the evidence does not say when approval was asked",
		"INFO  alice decided at 2026-10-13T12:00:00.000Z (signed, the approver's clock), recorded at not recorded, gap unknown")
}

// An INFO line is dimmed on a terminal, and a verb that could not reach a verdict ends with the
// NO VERDICT banner.
func TestColor_InfoLinesAndNoVerdictBanner(t *testing.T) {
	f := newExitFixture(t)
	code, stdout, _ := runCLI("verify-governance", "-color=always", "-policy", f.policyFile, "-checker", f.broken)
	if code != exitNoVerdict || !strings.Contains(stdout, "\x1b[1;31mNO VERDICT\x1b[0m") {
		t.Errorf("no verdict: exit %d\n%q", code, stdout)
	}
	if strings.Contains(stdout, "FAILED") {
		t.Errorf("a no-verdict run is not a FAILED one:\n%q", stdout)
	}
}

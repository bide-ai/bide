// Command bide-audit produces and verifies audit ProofBundles from the command line, for
// the auditor/compliance persona who does not write Go. It is deliberately dependency-light:
// it imports only the core + audit packages (stdlib under the hood), no store backend, so it
// works against any store by operating on an exported journal (a JSON array of records) and a
// signed tree head. `verify` needs neither: just the bundle and a public key.
//
//	# Produce a proof that one tool call happened, against an anchored STH:
//	bide-audit prove -journal run.json -sth sth.json -tool call_abc -out proof.json
//
//	# Verify it offline, trusting only an out-of-band public key:
//	bide-audit verify -bundle proof.json -pubkey 1a2b...   # exit 0 = verified
//
// Export the journal with: json.Marshal(store.History(ctx, runID)).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	agent "github.com/blackwell-systems/bide"
	"github.com/blackwell-systems/bide/audit"
)

// policyFormatVersion is the published domain-separation tag for the combinator policy
// serialization (gsm's PolicyFormatVersion). It is hardcoded here on purpose: the verifier
// recomputes a policy digest from the published format and bytes without importing or trusting
// gsm, so the two roots of trust (the log and the proof) stay independent of the producer.
const policyFormatVersion = "gsm-policy-v1"

func flagSet(name string) *flag.FlagSet { return flag.NewFlagSet(name, flag.ExitOnError) }

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "prove":
		prove(os.Args[2:])
	case "verify":
		verify(os.Args[2:])
	case "verify-governance":
		verifyGovernance(os.Args[2:])
	case "verify-governed-action":
		verifyGovernedAction(os.Args[2:])
	case "verify-convergence":
		verifyConvergence(os.Args[2:])
	case "verify-quorum":
		verifyQuorum(os.Args[2:])
	case "verify-run":
		verifyRun(os.Args[2:])
	case "prove-absent":
		proveAbsent(os.Args[2:])
	case "verify-absent":
		verifyAbsent(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `bide-audit: produce and verify audit proof bundles

  prove  -journal <file> -sth <file> (-tool <id> | -index <n>) [-out <file>]
         build a ProofBundle for one record against a signed tree head

  verify -bundle <file> -pubkey <hex|file>
         verify a ProofBundle offline; exit 0 if authentic, 1 otherwise

  verify-governance -policy <file> [-digest <hex>] [-checker <astchecker>]
         recompute the policy digest and, with -checker, run the external verified
         oracle to certify the policy converges; exit 0 if all checks pass

  verify-governed-action -action <bundle> -policy-bundle <bundle> -pubkey <hex|file> [-checker <astchecker>]
         verify a governed action end to end: both bundles authentic and in the same
         signed tree, the action's policy digest links to the anchored policy leaf, the
         leaf's bytes hash to that digest, and (with -checker) the policy converges

  verify-convergence -cert-bundle <bundle> -policy-bundle <bundle> -pubkey <hex|file> [-checker <astchecker>]
         verify an anchored convergence certificate: both bundles authentic and in the same
         signed tree, the certificate certifies the anchored policy's digest, the leaf's bytes
         hash to that digest, and (with -checker) the external oracle's verdict AGREES with the
         certificate's convergence claim, so a certificate that overstates convergence is caught

  verify-quorum -tally <bundle> -vote <bundle> [-vote <bundle>...] -pubkey <hex|file> -k <n> [-commit <bundle>]
         verify a governed k-of-n quorum: the tally and every vote bundle authentic and in the
         same signed tree and run, the recorded tally recomputes from the disclosed votes (a
         forged tally is caught), and votes_for >= k; with -commit, a governed commit is anchored
         in the same tree

  verify-run -cert <file> -pubkey <hex|file> (-approved <digest>... | -approved-file <file>) [-checker <astchecker>]
         verify a proof-carrying run certificate: the used-policy set is bound to the run's
         signed absence commitment and is a subset of the approved allowlist (only-approved-
         policies), and every used policy has an anchored, digest-linked convergence certificate
         in the run's signed tree (policies-convergence-certified); with -checker, the external
         oracle's convergence verdict on each used policy must AGREE with its certificate

  prove-absent -journal <file> -sth <file> -key (tool:<id>|policy:<digest>) [-out <file>]
         prove a thing did NOT happen (no such tool call / no action under that policy)
         against a signed absence tree head (see audit.SignAbsenceRoot)

  verify-absent -bundle <file> -pubkey <hex|file>
         verify an absence proof offline; exit 0 if authentic, 1 otherwise

Export a journal for `+"`prove`"+` with: json.Marshal(store.History(ctx, runID)).
`)
	os.Exit(2)
}

// staticStore is a read-only Durable backed by an exported journal, so the CLI can reuse the
// audit package's proof builders without a live store.
type staticStore struct{ recs []agent.Record }

func (s staticStore) History(context.Context, string) ([]agent.Record, error) { return s.recs, nil }
func (staticStore) Do(context.Context, string, string, func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return agent.Record{}, errors.New("bide-audit: journal is read-only")
}

func prove(args []string) {
	fs := flagSet("prove")
	journal := fs.String("journal", "", "path to the exported journal JSON ([]Record)")
	sthPath := fs.String("sth", "", "path to the signed tree head JSON")
	tool := fs.String("tool", "", "prove the tool call with this ToolUseID")
	index := fs.Int("index", -1, "prove the record at this journal index")
	out := fs.String("out", "", "write the bundle here (default: stdout)")
	_ = fs.Parse(args)

	if *journal == "" || *sthPath == "" || (*tool == "" && *index < 0) {
		usage()
	}

	var recs []agent.Record
	readJSON(*journal, &recs)
	var sth audit.SignedTreeHead
	readJSON(*sthPath, &sth)

	store := staticStore{recs: recs}
	var (
		bundle audit.ProofBundle
		err    error
	)
	if *tool != "" {
		bundle, err = audit.ProveToolCall(context.Background(), store, "", *tool, sth)
	} else {
		bundle, err = audit.ProveRecord(context.Background(), store, "", *index, sth)
	}
	if err != nil {
		fatal(err)
	}

	b, _ := json.MarshalIndent(bundle, "", "  ")
	if *out == "" {
		fmt.Println(string(b))
		return
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
}

func verify(args []string) {
	fs := flagSet("verify")
	bundlePath := fs.String("bundle", "", "path to the ProofBundle JSON")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	_ = fs.Parse(args)

	if *bundlePath == "" || *pubkey == "" {
		usage()
	}

	var bundle audit.ProofBundle
	readJSON(*bundlePath, &bundle)
	pub := readPubKey(*pubkey)

	ok, err := bundle.Verify(pub)
	if err != nil {
		fatal(err)
	}
	if !ok {
		fmt.Println("FAIL: proof did not verify under this key")
		os.Exit(1)
	}
	fmt.Printf("OK: run %q record verified in a signed tree of size %d\n", bundle.RunID, bundle.STH.Size)
}

// verifyGovernance closes the loop between the two roots of trust for a governed policy: it
// recomputes the policy digest from the published bytes (the cryptographic identity anchored in
// the log and embedded in each governed action's ProofBundle) and, given the external verified
// oracle, certifies that the policy actually converges (the mathematical guarantee). It imports
// neither gsm nor the runtime: it recomputes the digest from the published format and runs a
// checker the auditor supplies, so it trusts neither the producer nor any code the producer wrote.
func verifyGovernance(args []string) {
	fs := flagSet("verify-governance")
	policyPath := fs.String("policy", "", "path to the serialized combinator policy (gsm PolicyBytes / WriteMachineAST output)")
	expected := fs.String("digest", "", "expected policy digest as hex (e.g. from a ProofBundle or the anchor); must match if set")
	checker := fs.String("checker", "", "path to the external verified oracle (astchecker); if set, it is run on the policy")
	_ = fs.Parse(args)

	if *policyPath == "" {
		usage()
	}
	policy, err := os.ReadFile(*policyPath)
	if err != nil {
		fatal(err)
	}

	// Domain-separated SHA-256 over the published format tag and the policy bytes, recomputed
	// here rather than taken from gsm, so the digest check is independent of the producer.
	h := sha256.New()
	h.Write([]byte(policyFormatVersion + "\n"))
	h.Write(policy)
	digest := hex.EncodeToString(h.Sum(nil))
	fmt.Printf("policy digest: %s\n", digest)

	if *expected != "" && trimSpace(*expected) != digest {
		fmt.Printf("FAIL: digest mismatch (expected %s)\n", trimSpace(*expected))
		os.Exit(1)
	}

	if *checker == "" {
		fmt.Println("OK: digest computed. Pass -checker <astchecker> to also certify the policy converges.")
		return
	}

	out, runErr := exec.Command(*checker, *policyPath).CombinedOutput()
	fmt.Printf("oracle: %s", out)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		fmt.Println()
	}
	if runErr != nil {
		fmt.Println("FAIL: the verified oracle did not certify this policy as convergent")
		os.Exit(1)
	}
	fmt.Println("OK: digest verified and the external oracle certifies the policy converges")
}

// verifyGovernedAction verifies a governed action against its anchored policy from public
// artifacts alone: two ProofBundles (the action and the policy leaf) plus an out-of-band public
// key. It confirms (1) both bundles are authentic under the key, (2) they are in the SAME signed
// tree, (3) the action's embedded policy digest matches the anchored policy leaf's digest,
// (4) the leaf's bytes actually hash to that digest (so the leaf cannot lie about which policy it
// is), and (5) with -checker, that the external verified oracle certifies the policy converges.
// Steps 1 to 4 are the cryptographic root; step 5 is the independent mathematical root.
func verifyGovernedAction(args []string) {
	fs := flagSet("verify-governed-action")
	actionPath := fs.String("action", "", "path to the action ProofBundle JSON (from prove -tool)")
	policyPath := fs.String("policy-bundle", "", "path to the policy-leaf ProofBundle JSON (from ProvePolicy)")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	checker := fs.String("checker", "", "path to the external verified oracle (astchecker); if set, it certifies the policy converges")
	_ = fs.Parse(args)

	if *actionPath == "" || *policyPath == "" || *pubkey == "" {
		usage()
	}
	var action, policy audit.ProofBundle
	readJSON(*actionPath, &action)
	readJSON(*policyPath, &policy)
	pub := readPubKey(*pubkey)

	// (1) both bundles authentic under the out-of-band key.
	if ok, err := action.Verify(pub); err != nil {
		fatal(err)
	} else if !ok {
		fmt.Println("FAIL: action bundle did not verify under this key")
		os.Exit(1)
	}
	if ok, err := policy.Verify(pub); err != nil {
		fatal(err)
	} else if !ok {
		fmt.Println("FAIL: policy bundle did not verify under this key")
		os.Exit(1)
	}

	// (2) same signed tree.
	if action.STH.Size != policy.STH.Size || string(action.STH.Root) != string(policy.STH.Root) {
		fmt.Println("FAIL: the action and policy are not committed in the same signed tree")
		os.Exit(1)
	}

	// (3) the action's embedded policy digest matches the anchored policy leaf's digest.
	var actionPayload struct {
		PolicyDigest string `json:"policy_digest"`
	}
	if err := json.Unmarshal(action.Record.Result, &actionPayload); err != nil {
		fatal(fmt.Errorf("action result is not a governed-action payload: %w", err))
	}
	var pc audit.PolicyContent
	if err := json.Unmarshal(policy.Record.Result, &pc); err != nil {
		fatal(fmt.Errorf("policy bundle is not a policy leaf: %w", err))
	}
	if actionPayload.PolicyDigest == "" || actionPayload.PolicyDigest != pc.Digest {
		fmt.Printf("FAIL: action policy digest %q does not link to the anchored policy leaf %q\n", actionPayload.PolicyDigest, pc.Digest)
		os.Exit(1)
	}

	// (4) the leaf's bytes actually hash to that digest (recomputed independently of gsm).
	h := sha256.New()
	h.Write([]byte(policyFormatVersion + "\n"))
	h.Write([]byte(pc.Policy))
	if recomputed := hex.EncodeToString(h.Sum(nil)); recomputed != pc.Digest {
		fmt.Printf("FAIL: policy leaf lies about its digest (bytes hash to %s, leaf claims %s)\n", recomputed, pc.Digest)
		os.Exit(1)
	}
	fmt.Printf("OK: action in run %q ran under anchored policy %s, both in a signed tree of size %d\n", action.RunID, pc.Digest, action.STH.Size)

	// (5) the independent mathematical root: the policy converges.
	if *checker == "" {
		fmt.Println("OK: cryptographic root verified. Pass -checker <astchecker> to also certify the policy converges.")
		return
	}
	tmp, err := os.CreateTemp("", "policy-*.machine")
	if err != nil {
		fatal(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(pc.Policy); err != nil {
		fatal(err)
	}
	_ = tmp.Close()
	out, runErr := exec.Command(*checker, tmp.Name()).CombinedOutput()
	fmt.Printf("oracle: %s", out)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		fmt.Println()
	}
	if runErr != nil {
		fmt.Println("FAIL: the verified oracle did not certify the anchored policy as convergent")
		os.Exit(1)
	}
	fmt.Println("OK: both roots verified: the action ran under an anchored, provably convergent policy")
}

// verifyConvergence verifies an anchored convergence certificate from public artifacts alone, and
// crucially does NOT trust the certificate's convergence claim: it re-establishes convergence from
// the disclosed policy bytes using the external verified oracle and fails if the oracle's verdict
// disagrees with the certificate. So a certificate that overstates convergence (a producer bug or
// a forgery) is caught here, outside the trust boundary. Steps: (1) both bundles authentic under
// the out-of-band key, (2) in the same signed tree, (3) the certificate certifies the same digest
// the policy leaf carries, (4) the policy leaf's bytes hash to that digest, (5) with -checker, the
// oracle's convergence verdict on those bytes AGREES with the certificate. The certificate's CRDT
// classification is surfaced but noted as producer-reported: the oracle certifies convergence, not
// the compensation-free refinement.
func verifyConvergence(args []string) {
	fs := flagSet("verify-convergence")
	certPath := fs.String("cert-bundle", "", "path to the convergence-leaf ProofBundle JSON (from ProveConvergence)")
	policyPath := fs.String("policy-bundle", "", "path to the policy-leaf ProofBundle JSON (from ProvePolicy)")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	checker := fs.String("checker", "", "path to the external verified oracle (astchecker); if set, its verdict must agree with the certificate")
	_ = fs.Parse(args)

	if *certPath == "" || *policyPath == "" || *pubkey == "" {
		usage()
	}
	var certBundle, policy audit.ProofBundle
	readJSON(*certPath, &certBundle)
	readJSON(*policyPath, &policy)
	pub := readPubKey(*pubkey)

	// (1) both bundles authentic under the out-of-band key.
	if ok, err := certBundle.Verify(pub); err != nil {
		fatal(err)
	} else if !ok {
		fmt.Println("FAIL: certificate bundle did not verify under this key")
		os.Exit(1)
	}
	if ok, err := policy.Verify(pub); err != nil {
		fatal(err)
	} else if !ok {
		fmt.Println("FAIL: policy bundle did not verify under this key")
		os.Exit(1)
	}

	// (2) same signed tree.
	if certBundle.STH.Size != policy.STH.Size || string(certBundle.STH.Root) != string(policy.STH.Root) {
		fmt.Println("FAIL: the certificate and policy are not committed in the same signed tree")
		os.Exit(1)
	}

	// (3) the certificate certifies the same digest the policy leaf carries. The certificate is
	// decoded into a local struct so the CLI imports neither gsm nor govern.
	var cc audit.ConvergenceContent
	if err := json.Unmarshal(certBundle.Record.Result, &cc); err != nil {
		fatal(fmt.Errorf("certificate bundle is not a convergence leaf: %w", err))
	}
	var pc audit.PolicyContent
	if err := json.Unmarshal(policy.Record.Result, &pc); err != nil {
		fatal(fmt.Errorf("policy bundle is not a policy leaf: %w", err))
	}
	if cc.Digest == "" || cc.Digest != pc.Digest {
		fmt.Printf("FAIL: certificate digest %q does not link to the anchored policy leaf %q\n", cc.Digest, pc.Digest)
		os.Exit(1)
	}

	// (4) the leaf's bytes actually hash to that digest (recomputed independently of gsm).
	h := sha256.New()
	h.Write([]byte(policyFormatVersion + "\n"))
	h.Write([]byte(pc.Policy))
	if recomputed := hex.EncodeToString(h.Sum(nil)); recomputed != pc.Digest {
		fmt.Printf("FAIL: policy leaf lies about its digest (bytes hash to %s, leaf claims %s)\n", recomputed, pc.Digest)
		os.Exit(1)
	}

	var cert struct {
		Machine          string `json:"machine"`
		Converges        bool   `json:"converges"`
		MaxRepairLen     int    `json:"max_repair_len"`
		States           int    `json:"states"`
		CompensationFree bool   `json:"compensation_free"`
	}
	if err := json.Unmarshal(cc.Certificate, &cert); err != nil {
		fatal(fmt.Errorf("convergence certificate payload: %w", err))
	}
	fragment := "governed (compensation-bearing)"
	if cert.CompensationFree {
		fragment = "CRDT (compensation-free fragment)"
	}
	fmt.Printf("certificate: machine %q claims converges=%v, %s, checked over %d states (max repair depth %d)\n",
		cert.Machine, cert.Converges, fragment, cert.States, cert.MaxRepairLen)
	fmt.Printf("OK: certificate anchored for policy %s in a signed tree of size %d\n", pc.Digest, certBundle.STH.Size)

	// (5) the independent root: the oracle's verdict must AGREE with the certificate's claim.
	if *checker == "" {
		fmt.Println("OK: cryptographic root verified. Pass -checker <astchecker> to cross-check the convergence claim against the oracle.")
		return
	}
	tmp, err := os.CreateTemp("", "policy-*.machine")
	if err != nil {
		fatal(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(pc.Policy); err != nil {
		fatal(err)
	}
	_ = tmp.Close()
	out, runErr := exec.Command(*checker, tmp.Name()).CombinedOutput()
	fmt.Printf("oracle: %s", out)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		fmt.Println()
	}
	oracleConverges := runErr == nil
	if oracleConverges != cert.Converges {
		fmt.Printf("FAIL: certificate claims converges=%v but the verified oracle says converges=%v\n", cert.Converges, oracleConverges)
		os.Exit(1)
	}
	// If the oracle also certifies the CRDT-fragment classification (compensation_free=<bool>),
	// cross-check it, so a certificate cannot overstate that either. Older oracles omit the line;
	// then the classification stays producer-reported, which we say rather than silently pass.
	if cf, ok := parseCompensationFree(out); ok {
		if cf != cert.CompensationFree {
			fmt.Printf("FAIL: certificate claims compensation_free=%v but the verified oracle says compensation_free=%v\n", cert.CompensationFree, cf)
			os.Exit(1)
		}
	} else {
		fmt.Println("note: this oracle does not certify the compensation-free classification; that field stays producer-reported")
	}
	if !cert.Converges {
		fmt.Println("FAIL: the certificate and the oracle agree the policy does NOT converge; do not deploy it")
		os.Exit(1)
	}
	fmt.Println("OK: the verified oracle's verdict agrees with the certificate: the anchored policy provably converges")
}

// parseCompensationFree scans the oracle's output for a machine-readable classification line
// (compensation_free=true / compensation_free=false). The second return is false if no such line
// is present, so a caller can distinguish "oracle disagrees" from "oracle does not report it".
func parseCompensationFree(out []byte) (bool, bool) {
	for _, line := range strings.Split(string(out), "\n") {
		switch strings.TrimSpace(line) {
		case "compensation_free=true":
			return true, true
		case "compensation_free=false":
			return false, true
		}
	}
	return false, false
}

// stringList is a repeatable string flag (one -vote per voter bundle).
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// verifyQuorum verifies a governed k-of-n quorum from public artifacts alone, and does not trust the
// recorded tally: it recomputes the tally from the separately-anchored vote leaves and fails if they
// disagree, so a forged tally is caught. Steps (all FAIL exit 1): (1) the tally and every vote bundle
// authentic under the out-of-band key, (2) all in the same signed tree and the same run, (3) the
// recorded tally (decision, votes_for, total) recomputes exactly from the disclosed votes with the
// same plurality-and-lexical-tie-break rule govern.Quorum uses, and every vote is disclosed, (4)
// votes_for >= k. With -commit, the governed commit is confirmed anchored in the same tree and run.
//
// The CLI decodes the tally and vote leaves into local structs, so it imports neither gsm nor govern.
func verifyQuorum(args []string) {
	fs := flagSet("verify-quorum")
	tallyPath := fs.String("tally", "", "path to the quorum tally ProofBundle JSON (the quorum/tally step)")
	var votePaths stringList
	fs.Var(&votePaths, "vote", "path to one vote ProofBundle JSON; repeat once per voter")
	commitPath := fs.String("commit", "", "path to the commit action ProofBundle JSON (optional)")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	k := fs.Int("k", 0, "the quorum threshold to assert: votes_for must be >= k")
	_ = fs.Parse(args)

	if *tallyPath == "" || len(votePaths) == 0 || *pubkey == "" || *k <= 0 {
		usage()
	}
	pub := readPubKey(*pubkey)

	var tally audit.ProofBundle
	readJSON(*tallyPath, &tally)
	votes := make([]audit.ProofBundle, len(votePaths))
	for i, p := range votePaths {
		readJSON(p, &votes[i])
	}

	mustVerify := func(b audit.ProofBundle, what string) {
		if ok, err := b.Verify(pub); err != nil {
			fatal(err)
		} else if !ok {
			fmt.Printf("FAIL: %s bundle did not verify under this key\n", what)
			os.Exit(1)
		}
	}

	// (1) authenticity; (2) same signed tree and same run as the tally.
	mustVerify(tally, "tally")
	for i := range votes {
		mustVerify(votes[i], fmt.Sprintf("vote %d", i))
		if votes[i].STH.Size != tally.STH.Size || string(votes[i].STH.Root) != string(tally.STH.Root) {
			fmt.Println("FAIL: a vote is not committed in the same signed tree as the tally")
			os.Exit(1)
		}
		if votes[i].RunID != tally.RunID {
			fmt.Println("FAIL: a vote is not from the same run as the tally")
			os.Exit(1)
		}
	}

	// (3) recompute the tally from the disclosed votes; it must match the recorded tally exactly.
	var rec struct {
		Decision string `json:"decision"`
		VotesFor int    `json:"votes_for"`
		Total    int    `json:"total"`
	}
	if err := json.Unmarshal(tally.Record.Result, &rec); err != nil {
		fatal(fmt.Errorf("tally bundle is not a quorum tally: %w", err))
	}
	counts := map[string]int{}
	for i := range votes {
		var v struct {
			Voter    string `json:"voter"`
			Decision string `json:"decision"`
		}
		if err := json.Unmarshal(votes[i].Record.Result, &v); err != nil {
			fatal(fmt.Errorf("vote bundle %d is not a vote leaf: %w", i, err))
		}
		counts[v.Decision]++
	}
	decs := make([]string, 0, len(counts))
	for d := range counts {
		decs = append(decs, d)
	}
	sort.Strings(decs) // deterministic plurality, lexically smallest decision breaks ties
	winner, best := "", 0
	for _, d := range decs {
		if counts[d] > best {
			winner, best = d, counts[d]
		}
	}
	if len(votes) != rec.Total {
		fmt.Printf("FAIL: %d vote bundles disclosed but the tally records total=%d; all votes must be disclosed to verify the tally\n", len(votes), rec.Total)
		os.Exit(1)
	}
	if winner != rec.Decision || best != rec.VotesFor {
		fmt.Printf("FAIL: recorded tally (decision=%q votes_for=%d) does not match the disclosed votes (decision=%q votes_for=%d)\n",
			rec.Decision, rec.VotesFor, winner, best)
		os.Exit(1)
	}

	// (4) the quorum threshold.
	if rec.VotesFor < *k {
		fmt.Printf("FAIL: quorum not met: votes_for=%d < k=%d for decision %q\n", rec.VotesFor, *k, rec.Decision)
		os.Exit(1)
	}
	fmt.Printf("OK: %d-of-%d agreement on decision %q (votes_for=%d >= k=%d), recomputed from %d disclosed votes, in run %q of a signed tree of size %d\n",
		rec.VotesFor, rec.Total, rec.Decision, rec.VotesFor, *k, len(votes), tally.RunID, tally.STH.Size)

	// (optional) confirm a governed commit is anchored in the same tree and run.
	if *commitPath != "" {
		var commit audit.ProofBundle
		readJSON(*commitPath, &commit)
		mustVerify(commit, "commit")
		if commit.STH.Size != tally.STH.Size || string(commit.STH.Root) != string(tally.STH.Root) || commit.RunID != tally.RunID {
			fmt.Println("FAIL: the commit is not in the same signed tree and run as the quorum")
			os.Exit(1)
		}
		var payload struct {
			PolicyDigest string `json:"policy_digest"`
		}
		if err := json.Unmarshal(commit.Record.Result, &payload); err != nil || payload.PolicyDigest == "" {
			fmt.Println("FAIL: commit bundle is not a governed-action leaf")
			os.Exit(1)
		}
		fmt.Printf("OK: a governed commit under policy %s is anchored in the same tree; the decision committed under k-of-n agreement\n", payload.PolicyDigest)
	}
}

// verifyRun verifies a proof-carrying run certificate from public artifacts alone: the certificate
// bundle, an out-of-band public key, and the AUDITOR's own approved allowlist (never the certificate's
// embedded list, which the producer chose). It re-derives every property rather than trusting the
// certificate:
//
//  1. only-approved-policies (completeness-bearing): audit.VerifyRun confirms the used-policy set is
//     bound to the run's signed absence commitment (recomputing the absence root, so a used policy
//     cannot be hidden) and is a subset of the auditor-supplied allowlist.
//  2. policies-convergence-certified: audit.VerifyRun confirms every used policy has an anchored,
//     digest-linked policy leaf and convergence-certificate leaf in the run's signed tree.
//  3. with -checker, the independent mathematical root: for each used policy the external oracle is
//     run on the disclosed policy bytes and its convergence verdict must AGREE with the certificate's,
//     exactly as verify-convergence does, so a certificate that overstates convergence is caught.
//
// The CLI decodes the certificate and its leaves into audit's types (which import neither gsm nor
// govern) and the convergence certificate payload into a local struct, so it depends on neither.
func verifyRun(args []string) {
	fs := flagSet("verify-run")
	certPath := fs.String("cert", "", "path to the RunCertificate JSON (from audit.CertifyRun)")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	var approved stringList
	fs.Var(&approved, "approved", "an approved policy digest; repeat once per allowed policy")
	approvedFile := fs.String("approved-file", "", "path to a file of approved policy digests, one per line")
	checker := fs.String("checker", "", "path to the external verified oracle (astchecker); if set, its verdict must agree with each policy's certificate")
	_ = fs.Parse(args)

	if *certPath == "" || *pubkey == "" || (len(approved) == 0 && *approvedFile == "") {
		usage()
	}

	var cert audit.RunCertificate
	readJSON(*certPath, &cert)
	pub := readPubKey(*pubkey)

	// The auditor's own allowlist governs the only-approved-policies check, not the certificate's
	// embedded list: overwrite it before verifying, so a producer cannot pass by widening its own set.
	allow := append([]string(nil), approved...)
	if *approvedFile != "" {
		allow = append(allow, readDigestLines(*approvedFile)...)
	}
	cert.ApprovedPolicies = allow

	res, err := audit.VerifyRun(cert, pub)
	if err != nil {
		fatal(err)
	}
	for _, r := range res.Reasons {
		fmt.Printf("  - %s\n", r)
	}
	if !res.OK {
		fmt.Printf("FAIL: run %q certificate did not verify (only-approved-policies=%v, policies-convergence-certified=%v)\n",
			cert.RunID, res.OnlyApprovedPolicies, res.ConvergenceCertified)
		os.Exit(1)
	}
	fmt.Printf("OK: run %q: %d policies used, all in the approved set and bound to the signed absence root; each has an anchored convergence certificate in a signed tree of size %d\n",
		cert.RunID, len(cert.UsedPolicies), cert.STH.Size)

	// The independent mathematical root: cross-check each used policy's certificate against the oracle.
	if *checker == "" {
		fmt.Println("OK: cryptographic root verified. Pass -checker <astchecker> to cross-check each policy's convergence claim against the oracle.")
		return
	}
	for _, pc := range cert.Convergence {
		var polC audit.PolicyContent
		if err := json.Unmarshal(pc.PolicyLeaf.Record.Result, &polC); err != nil {
			fatal(fmt.Errorf("policy leaf for %s is not a policy content leaf: %w", pc.Digest, err))
		}
		var convC audit.ConvergenceContent
		if err := json.Unmarshal(pc.Certificate.Record.Result, &convC); err != nil {
			fatal(fmt.Errorf("convergence leaf for %s is not a convergence content leaf: %w", pc.Digest, err))
		}
		var claim struct {
			Converges        bool `json:"converges"`
			CompensationFree bool `json:"compensation_free"`
		}
		if err := json.Unmarshal(convC.Certificate, &claim); err != nil {
			fatal(fmt.Errorf("convergence certificate payload for %s: %w", pc.Digest, err))
		}

		tmp, err := os.CreateTemp("", "policy-*.machine")
		if err != nil {
			fatal(err)
		}
		if _, err := tmp.WriteString(polC.Policy); err != nil {
			fatal(err)
		}
		_ = tmp.Close()
		out, runErr := exec.Command(*checker, tmp.Name()).CombinedOutput()
		os.Remove(tmp.Name())
		fmt.Printf("oracle (%s): %s", polC.Digest, out)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			fmt.Println()
		}
		oracleConverges := runErr == nil
		if oracleConverges != claim.Converges {
			fmt.Printf("FAIL: policy %s certificate claims converges=%v but the verified oracle says converges=%v\n", polC.Digest, claim.Converges, oracleConverges)
			os.Exit(1)
		}
		if cf, ok := parseCompensationFree(out); ok {
			if cf != claim.CompensationFree {
				fmt.Printf("FAIL: policy %s certificate claims compensation_free=%v but the verified oracle says compensation_free=%v\n", polC.Digest, claim.CompensationFree, cf)
				os.Exit(1)
			}
		}
		if !claim.Converges {
			fmt.Printf("FAIL: policy %s does NOT converge (certificate and oracle agree); do not deploy it\n", polC.Digest)
			os.Exit(1)
		}
	}
	fmt.Println("OK: the verified oracle agrees with every policy's certificate: the whole run ran under approved, provably convergent policies")
}

// readDigestLines reads a file of approved policy digests, one per line, ignoring blank lines and
// # comments. It is the file form of the repeatable -approved flag.
func readDigestLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		s := trimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		out = append(out, s)
	}
	return out
}

// absenceSelector maps a CLI -key selector to the KeyFunc and the exact absence key to prove
// missing. "tool:<id>" proves no tool call with that id happened; "policy:<digest>" proves no
// governed action ran under that policy.
func absenceSelector(key string) (audit.KeyFunc, string, error) {
	switch {
	case strings.HasPrefix(key, "tool:"):
		return audit.ToolUseKey, audit.ToolUseKeyFor(strings.TrimPrefix(key, "tool:")), nil
	case strings.HasPrefix(key, "policy:"):
		return audit.PolicyUsedKey, audit.PolicyUsedKeyFor(strings.TrimPrefix(key, "policy:")), nil
	default:
		return nil, "", fmt.Errorf("key must be tool:<id> or policy:<digest>, got %q", key)
	}
}

func proveAbsent(args []string) {
	fs := flagSet("prove-absent")
	journal := fs.String("journal", "", "path to the exported journal JSON ([]Record)")
	sthPath := fs.String("sth", "", "path to the signed absence tree head JSON (see audit.SignAbsenceRoot)")
	key := fs.String("key", "", "what to prove absent: tool:<id> or policy:<digest>")
	out := fs.String("out", "", "write the absence bundle here (default: stdout)")
	_ = fs.Parse(args)

	if *journal == "" || *sthPath == "" || *key == "" {
		usage()
	}
	keyFn, absKey, err := absenceSelector(*key)
	if err != nil {
		fatal(err)
	}
	var recs []agent.Record
	readJSON(*journal, &recs)
	var sth audit.SignedTreeHead
	readJSON(*sthPath, &sth)

	bundle, err := audit.ProveAbsentBundle(recs, keyFn, absKey, "", sth)
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(bundle, "", "  ")
	if *out == "" {
		fmt.Println(string(b))
		return
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
}

func verifyAbsent(args []string) {
	fs := flagSet("verify-absent")
	bundlePath := fs.String("bundle", "", "path to the AbsenceBundle JSON")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	_ = fs.Parse(args)

	if *bundlePath == "" || *pubkey == "" {
		usage()
	}
	var bundle audit.AbsenceBundle
	readJSON(*bundlePath, &bundle)
	pub := readPubKey(*pubkey)

	ok, err := bundle.Verify(pub)
	if err != nil {
		fatal(err)
	}
	if !ok {
		fmt.Println("FAIL: absence proof did not verify under this key")
		os.Exit(1)
	}
	fmt.Printf("OK: %q is absent from run %q in a signed key set of size %d\n", bundle.Absence.Key, bundle.RunID, bundle.Absence.Size)
}

// readPubKey accepts a hex string directly, or a path to a file whose (trimmed) contents are
// hex. The key must come from out-of-band; that is the whole point of the trust model.
func readPubKey(s string) []byte {
	raw := s
	if b, err := os.ReadFile(s); err == nil {
		raw = string(b)
	}
	raw = trimSpace(raw)
	key, err := hex.DecodeString(raw)
	if err != nil {
		fatal(fmt.Errorf("public key must be hex (or a file of hex): %w", err))
	}
	return key
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	return s
}

func readJSON(path string, v any) {
	b, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		fatal(fmt.Errorf("parse %s: %w", path, err))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

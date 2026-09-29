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
	"bytes"
	"context"
	"crypto/ed25519"
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

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
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
	case "verify-evidence":
		verifyEvidence(os.Args[2:])
	case "verify-approvals":
		verifyApprovals(os.Args[2:])
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
         verify a ProofBundle offline (its head must be a journal head of the bundle's run);
         exit 0 if authentic, 1 otherwise

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

  verify-quorum -name <quorum> -tally <bundle> -vote <bundle> [-vote <bundle>...] -pubkey <hex|file> -k <n> [-commit <bundle>]
         verify a governed k-of-n quorum: the tally and every vote bundle authentic, in the
         same signed tree and run, and recorded by the quorum named <quorum>; every vote the
         tally records disclosed exactly once; the recorded tally recomputes from the disclosed
         votes (a forged tally is caught); and votes_for >= k; with -commit, a governed commit
         is anchored in the same tree

  verify-run -cert <file> -pubkey <hex|file> (-approved <digest>... | -approved-file <file>) [-checker <astchecker>]
         verify a proof-carrying run certificate: the used-policy set is bound by a signed
         used-policy head to this run and its journal tree, and is a subset of the approved
         allowlist (only-approved-policies), and every used policy has an anchored, digest-linked convergence certificate
         in the run's signed tree (policies-convergence-certified); with -checker, the external
         oracle's convergence verdict on each used policy must AGREE with its certificate

  verify-evidence -evidence <file> -pubkey <hex|file> [-approved <digest>... | -approved-file <file>]
         verify a portable evidence package offline and print a plain-English report: the
         format, seal, key, and run binding, then one line per proven action (tool call, step,
         grant), plus the run certificate (checked against the given allowlist, required when
         the package carries one) and consistency proof if present; exit 0 if the whole package
         verifies, 1 otherwise

  verify-approvals -evidence <file> -pubkey <hex|file> -call <tool-use-id> -need <k>
                   -approvers <id,id,...> -approver-keys <file>
         verify an m-of-n human approval gate from an evidence package: the request, every
         decision the gate read, its recorded tally, and the call's result all verify under the
         log key in one signed tree; recounting the decisions with each approver's key (a JSON
         object of approver id to ed25519 public key hex) against the exact call reproduces the
         recorded tally; the gate enforced the expected policy; and at least k approved. Exit 0
         if all hold, 1 otherwise

  prove-absent -journal <file> -sth <file> -key (tool:<id>|policy:<digest>) [-out <file>]
         prove a thing did NOT happen (no such tool call / no action under that policy)
         against a signed key-set head of the matching kind (see audit.SignAbsenceRoot)

  verify-absent -bundle <file> -pubkey <hex|file>
         verify an absence proof offline against a head of the key's own key set; exit 0 if
         authentic, 1 otherwise

Export a journal for `+"`prove`"+` with: json.Marshal(store.History(ctx, runID)). Every JSON
input is parsed strictly: a duplicate or case-variant key, an unknown field, or invalid UTF-8 is
an error, and a public key must be 32 bytes of hex.
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
		bundle, err = audit.ProveToolCall(context.Background(), store, sth.RunID, *tool, sth)
	} else {
		bundle, err = audit.ProveRecord(context.Background(), store, sth.RunID, *index, sth)
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

	// (2) same signed tree (of the same run).
	if !action.STH.SameTree(policy.STH.TreeHead) {
		fmt.Println("FAIL: the action and policy are not committed in the same signed tree")
		os.Exit(1)
	}

	// (3) the action's embedded policy digest matches the anchored policy leaf's digest.
	actionDigest, err := governedPolicyDigest(action.Record.Result)
	if err != nil {
		fatal(fmt.Errorf("action result is not a governed-action payload: %w", err))
	}
	var pc audit.PolicyContent
	readLeaf(policy, &pc, "policy bundle is not a policy leaf")
	if actionDigest == "" || actionDigest != pc.Digest {
		fmt.Printf("FAIL: action policy digest %q does not link to the anchored policy leaf %q\n", actionDigest, pc.Digest)
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

	// (2) same signed tree (of the same run).
	if !certBundle.STH.SameTree(policy.STH.TreeHead) {
		fmt.Println("FAIL: the certificate and policy are not committed in the same signed tree")
		os.Exit(1)
	}

	// (3) the certificate certifies the same digest the policy leaf carries. The certificate is
	// decoded into a local struct so the CLI imports neither gsm nor govern.
	var cc audit.ConvergenceContent
	readLeaf(certBundle, &cc, "certificate bundle is not a convergence leaf")
	var pc audit.PolicyContent
	readLeaf(policy, &pc, "policy bundle is not a policy leaf")
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

	var cert confluenceCert
	if err := audit.UnmarshalStrict(cc.Certificate, &cert); err != nil {
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
// authentic under the out-of-band key, (2) all in the same signed tree and the same run, and all
// recorded by the quorum named -name (the tally under quorum/<name>/tally, each vote under
// quorum/<name>/vote/<voter> for the voter it records), so votes from another quorum in the run
// cannot stand in, (3) the disclosed votes are exactly the votes the tally records, each once, and
// the recorded tally (decision, votes_for, total) recomputes from them with the same
// plurality-and-lexical-tie-break rule govern.Quorum uses, (4) votes_for >= k with a single
// most-supported decision. With -commit, the governed commit is confirmed anchored in the same tree
// and run.
//
// The CLI decodes the tally and vote leaves into local structs, so it imports neither gsm nor govern.
func verifyQuorum(args []string) {
	fs := flagSet("verify-quorum")
	name := fs.String("name", "", "the quorum's name, as passed to govern.Quorum")
	tallyPath := fs.String("tally", "", "path to the quorum tally ProofBundle JSON (the quorum/<name>/tally step)")
	var votePaths stringList
	fs.Var(&votePaths, "vote", "path to one vote ProofBundle JSON (a quorum/<name>/vote/<voter> step); repeat once per voter")
	commitPath := fs.String("commit", "", "path to the commit action ProofBundle JSON (optional)")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	k := fs.Int("k", 0, "the quorum threshold to assert: votes_for must be >= k")
	_ = fs.Parse(args)

	if *name == "" || *tallyPath == "" || len(votePaths) == 0 || *pubkey == "" || *k <= 0 {
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
		// The signed head commits to the run, so the same tree is also the same run.
		if !votes[i].STH.SameTree(tally.STH.TreeHead) {
			fmt.Println("FAIL: a vote is not committed in the same signed tree and run as the tally")
			os.Exit(1)
		}
	}

	// (2) every bundle was recorded by this quorum: the tally under its tally step, each vote under
	// the vote step of the voter it records.
	if want := "quorum/" + *name + "/tally"; tally.Record.Name != want {
		fmt.Printf("FAIL: the tally bundle is step %q, not %q: it is not the tally of quorum %q\n", tally.Record.Name, want, *name)
		os.Exit(1)
	}
	var rec quorumTally
	readLeaf(tally, &rec, "tally bundle is not a quorum tally")
	disclosed := map[string]string{} // voter -> decision
	counts := map[string]int{}
	for i := range votes {
		var v quorumVote
		readLeaf(votes[i], &v, fmt.Sprintf("vote bundle %d is not a vote leaf", i))
		if want := "quorum/" + *name + "/vote/" + v.Voter; votes[i].Record.Name != want {
			fmt.Printf("FAIL: vote bundle %d is step %q, not %q: it is not %q's vote in quorum %q\n", i, votes[i].Record.Name, want, v.Voter, *name)
			os.Exit(1)
		}
		disclosed[v.Voter] = v.Decision
		counts[v.Decision]++
	}

	// (3) the disclosed votes are exactly the votes the tally records (each recorded vote is
	// disclosed with the same decision here, and the count check below rules out extra or repeated
	// bundles), and the tally recomputes from them. A voter the tally records twice would let one
	// disclosed vote count twice, so each recorded voter must be distinct.
	recorded := map[string]bool{}
	for _, v := range rec.Votes {
		if recorded[v.Voter] {
			fmt.Printf("FAIL: the tally records a vote by %q more than once\n", v.Voter)
			os.Exit(1)
		}
		recorded[v.Voter] = true
		d, ok := disclosed[v.Voter]
		if !ok {
			fmt.Printf("FAIL: the tally records a vote by %q that is not disclosed; all votes must be disclosed to verify the tally\n", v.Voter)
			os.Exit(1)
		}
		if d != v.Decision {
			fmt.Printf("FAIL: the tally records %q voting %q, but the disclosed vote is %q\n", v.Voter, v.Decision, d)
			os.Exit(1)
		}
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
	if len(votes) != len(rec.Votes) || len(votes) != rec.Total {
		fmt.Printf("FAIL: %d vote bundles disclosed but the tally records %d votes and total=%d; disclose exactly the votes the tally records\n", len(votes), len(rec.Votes), rec.Total)
		os.Exit(1)
	}
	if winner != rec.Decision || best != rec.VotesFor {
		fmt.Printf("FAIL: recorded tally (decision=%q votes_for=%d) does not match the disclosed votes (decision=%q votes_for=%d)\n",
			rec.Decision, rec.VotesFor, winner, best)
		os.Exit(1)
	}

	// (4) the quorum threshold, and a single most-supported decision: in a tie for the most votes
	// every tied decision may reach k, but none has more support than another.
	for _, d := range decs {
		if d != winner && counts[d] == best {
			fmt.Printf("FAIL: no single decision has the most votes: %q and %q each have %d\n", winner, d, best)
			os.Exit(1)
		}
	}
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
		if !commit.STH.SameTree(tally.STH.TreeHead) {
			fmt.Println("FAIL: the commit is not in the same signed tree and run as the quorum")
			os.Exit(1)
		}
		commitDigest, err := governedPolicyDigest(commit.Record.Result)
		if err != nil || commitDigest == "" {
			fmt.Println("FAIL: commit bundle is not a governed-action leaf")
			os.Exit(1)
		}
		fmt.Printf("OK: a governed commit under policy %s is anchored in the same tree; the decision committed under k-of-n agreement\n", commitDigest)
	}
}

// verifyRun verifies a proof-carrying run certificate from public artifacts alone: the certificate
// bundle, an out-of-band public key, and the AUDITOR's own approved allowlist (the certificate
// carries none). It re-derives every property rather than trusting the certificate:
//
//  1. only-approved-policies (completeness-bearing): audit.VerifyRun confirms the used-policy set is
//     bound by a signed used-policy head to this run and to the certificate's journal tree
//     (recomputing its root, so a used policy cannot be hidden and a head from another run or
//     history cannot stand in) and is a subset of the auditor-supplied allowlist.
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

	// The auditor's own allowlist governs the only-approved-policies check; the certificate carries
	// none, so a producer cannot pass by widening its own set.
	allow := append([]string(nil), approved...)
	if *approvedFile != "" {
		allow = append(allow, readDigestLines(*approvedFile)...)
	}

	res, err := audit.VerifyRun(cert, allow, pub)
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
	fmt.Printf("OK: run %q: %d policies used, all in the approved set and bound to the run's signed used-policy set; each has an anchored convergence certificate in a signed tree of size %d\n",
		cert.RunID, len(cert.UsedPolicies), cert.STH.Size)

	// The independent mathematical root: cross-check each used policy's certificate against the oracle.
	if *checker == "" {
		fmt.Println("OK: cryptographic root verified. Pass -checker <astchecker> to cross-check each policy's convergence claim against the oracle.")
		return
	}
	// res.Policies is what VerifyRun read (strictly) from the leaves it verified, so the oracle
	// checks exactly those bytes.
	for _, polC := range res.Policies {
		var claim confluenceCert
		if err := audit.UnmarshalStrict(polC.Certificate, &claim); err != nil {
			fatal(fmt.Errorf("convergence certificate payload for %s: %w", polC.Digest, err))
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

// verifyEvidence verifies a portable evidence package (audit.Evidence) from public artifacts alone:
// the package JSON, an out-of-band public key, and (for a package with a run certificate) the
// auditor's approved-policy allowlist. It re-checks every field the package carries via
// audit.EvidencePackage.Verify (format, seal, key, the STH and its run, each action's inclusion proof
// and label, the grant chain, the run certificate and consistency proof if present) and prints a
// plain-English report, one line per proven item, then an overall verdict. It exits non-zero if the
// package did not verify.
func verifyEvidence(args []string) {
	fs := flagSet("verify-evidence")
	evidencePath := fs.String("evidence", "", "path to the EvidencePackage JSON (from audit.Evidence)")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	var approved stringList
	fs.Var(&approved, "approved", "an approved policy digest for the run certificate; repeat once per allowed policy")
	approvedFile := fs.String("approved-file", "", "path to a file of approved policy digests, one per line")
	_ = fs.Parse(args)

	if *evidencePath == "" || *pubkey == "" {
		usage()
	}

	var pkg audit.EvidencePackage
	readJSON(*evidencePath, &pkg)
	pub := readPubKey(*pubkey)

	var opts []audit.EvidenceVerifyOption
	if len(approved) > 0 || *approvedFile != "" {
		allow := append([]string(nil), approved...)
		if *approvedFile != "" {
			allow = append(allow, readDigestLines(*approvedFile)...)
		}
		opts = append(opts, audit.WithApprovedPolicies(allow...))
	}
	rep, err := pkg.Verify(pub, opts...)
	if err != nil {
		fatal(err)
	}

	if pkg.Label != "" {
		fmt.Printf("evidence for run %q: %s\n", rep.RunID, pkg.Label)
	} else {
		fmt.Printf("evidence for run %q\n", rep.RunID)
	}
	if rep.STHVerified {
		fmt.Printf("PASS  signed tree head authentic (size %d)\n", pkg.STH.Size)
	} else {
		fmt.Println("FAIL  signed tree head is NOT authentic under this key")
	}
	for _, p := range rep.Problems {
		fmt.Printf("FAIL  %s\n", p)
	}
	for _, it := range rep.Items {
		status := "FAIL"
		if it.Verified {
			status = "PASS"
		}
		fmt.Printf("%s  %s\n", status, evidenceItemLine(it))
	}

	if !rep.OK {
		fmt.Println("FAIL: evidence package did not verify under this key")
		os.Exit(1)
	}
	fmt.Printf("PASS: run %q evidence verified (%d items) in a signed tree of size %d\n", rep.RunID, len(rep.Items), pkg.STH.Size)
}

// verifyApprovals checks an m-of-n approval gate from an evidence package with
// audit.VerifyApprovals, trusting only the log key, the approvers' keys, and the expected
// policy given on the command line.
func verifyApprovals(args []string) {
	fs := flagSet("verify-approvals")
	evidencePath := fs.String("evidence", "", "path to the EvidencePackage JSON carrying the approval evidence")
	pubkey := fs.String("pubkey", "", "the log's ed25519 public key as hex, or a path to a file containing it")
	call := fs.String("call", "", "the gated call's tool-use id")
	need := fs.Int("need", 0, "approvals the policy requires (k)")
	approvers := fs.String("approvers", "", "the policy's eligible approver ids, comma-separated, in policy order")
	keysPath := fs.String("approver-keys", "", `JSON object of approver id to ed25519 public key hex, e.g. {"ops":"ab12..."}`)
	_ = fs.Parse(args)

	if *evidencePath == "" || *pubkey == "" || *call == "" || *need == 0 || *approvers == "" || *keysPath == "" {
		usage()
	}
	var pkg audit.EvidencePackage
	readJSON(*evidencePath, &pkg)
	logPub := readPubKey(*pubkey)
	var keyHex map[string]string
	readJSON(*keysPath, &keyHex)
	keys := make(map[string][]byte, len(keyHex))
	for id, h := range keyHex {
		k, err := hex.DecodeString(strings.TrimSpace(h))
		if err != nil {
			fatal(fmt.Errorf("approver %q key must be hex: %w", id, err))
		}
		if len(k) != ed25519.PublicKeySize {
			fatal(fmt.Errorf("approver %q key is %d bytes, want a %d-byte ed25519 public key", id, len(k), ed25519.PublicKeySize))
		}
		keys[id] = k
	}
	policy := agent.ApprovalPolicy{Need: *need, Approvers: strings.Split(*approvers, ",")}
	verifierFor := func(id string) (agent.ApproverVerifier, bool) {
		k, ok := keys[id]
		if !ok {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: k}, true
	}

	rep, err := pkg.Verify(logPub)
	if err != nil {
		fatal(err)
	}
	v, err := audit.VerifyApprovals(pkg.Actions, *call, policy, verifierFor, logPub)
	if err != nil {
		fmt.Printf("FAIL  %v\n", err)
		os.Exit(1)
	}
	var callArgs bytes.Buffer
	if err := json.Compact(&callArgs, v.Args); err != nil {
		callArgs.Reset()
		callArgs.Write(v.Args)
	}
	fmt.Printf("approval gate on call %q: %s %s\n", *call, v.ToolName, callArgs.String())
	proofs := "FAIL"
	if rep.OK {
		proofs = "PASS"
	}
	fmt.Printf("%s  every proof in the package verifies under the log key\n", proofs)
	for _, id := range v.Counted {
		fmt.Printf("PASS  %s approved (signature verifies for this exact call)\n", id)
	}
	for _, id := range v.DeniedBy {
		fmt.Printf("INFO  %s denied\n", id)
	}
	for _, d := range v.Ignored {
		fmt.Printf("INFO  ignored %s: %s\n", d.Approver, d.Reason)
	}
	for _, p := range v.Problems {
		fmt.Printf("FAIL  %s\n", p)
	}
	if !rep.OK || !v.OK {
		fmt.Printf("FAIL: %d of %d required approvals verified", len(v.Counted), v.Need)
		if len(v.Problems) > 0 || !rep.OK {
			fmt.Print(", and the evidence is not consistent")
		}
		fmt.Println()
		os.Exit(1)
	}
	fmt.Printf("PASS: %d of %d required approvals verified, from complete evidence\n", len(v.Counted), v.Need)
}

// evidenceItemLine phrases one report line in plain English, e.g.
// `tool "call-charge": tool call included in the signed log`.
func evidenceItemLine(it audit.EvidenceItem) string {
	subject := it.Label
	switch it.Kind {
	case "tool":
		if it.Ref != "" {
			subject = fmt.Sprintf("tool call %q", it.Ref)
		} else {
			subject = fmt.Sprintf("tool call %q", it.Label)
		}
	case "step":
		subject = fmt.Sprintf("step %q", it.Label)
	case "grant":
		subject = fmt.Sprintf("grant %s", it.Label)
	case "run-certificate":
		subject = "run certificate"
	case "consistency":
		subject = it.Label
	}
	return fmt.Sprintf("%s: %s", subject, it.Note)
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

// absenceSelector maps a CLI -key selector to the key set and the exact absence key to prove
// missing. "tool:<id>" proves no tool call with that id happened; "policy:<digest>" proves no
// governed action ran under that policy.
func absenceSelector(key string) (audit.KeySet, string, error) {
	switch {
	case strings.HasPrefix(key, "tool:"):
		return audit.ToolUseKeys, audit.ToolUseKeyFor(strings.TrimPrefix(key, "tool:")), nil
	case strings.HasPrefix(key, "policy:"):
		return audit.PolicyUsedKeys, audit.PolicyUsedKeyFor(strings.TrimPrefix(key, "policy:")), nil
	default:
		return audit.KeySet{}, "", fmt.Errorf("key must be tool:<id> or policy:<digest>, got %q", key)
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
	set, absKey, err := absenceSelector(*key)
	if err != nil {
		fatal(err)
	}
	var recs []agent.Record
	readJSON(*journal, &recs)
	var sth audit.SignedTreeHead
	readJSON(*sthPath, &sth)

	bundle, err := audit.ProveAbsentBundle(recs, set, absKey, sth)
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

	// The key names the key set it can be absent from; the bundle's head must be of that set.
	set, known := audit.KeySetForKey(bundle.Absence.Key)
	if !known {
		fmt.Printf("FAIL: %q is not a tool-use or used-policy key\n", bundle.Absence.Key)
		os.Exit(1)
	}
	ok, err := bundle.Verify(pub, set)
	if err != nil {
		fatal(err)
	}
	if !ok {
		fmt.Println("FAIL: absence proof did not verify under this key")
		os.Exit(1)
	}
	fmt.Printf("OK: %q is absent from run %q's first %d records (its signed %s key set of size %d)\n",
		bundle.Absence.Key, bundle.RunID, bundle.STH.Journal.Size, set.Kind, bundle.Absence.Size)
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
	if len(key) != ed25519.PublicKeySize {
		fatal(fmt.Errorf("public key is %d bytes, want a %d-byte ed25519 public key", len(key), ed25519.PublicKeySize))
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

// confluenceCert is the wire form of govern.ConfluenceCertificate, mirrored so the CLI imports
// neither gsm nor govern. It has every field govern writes, so a certificate decodes strictly
// (TestWireMirrorsMatchGovern keeps the two in step).
type confluenceCert struct {
	Machine          string `json:"machine"`
	PolicyDigest     string `json:"policy_digest"`
	Converges        bool   `json:"converges"`
	WFC              bool   `json:"wfc"`
	CC               bool   `json:"cc"`
	MaxRepairLen     int    `json:"max_repair_len"`
	PairsTotal       int    `json:"pairs_total"`
	PairsDisjoint    int    `json:"pairs_disjoint"`
	PairsBrute       int    `json:"pairs_brute"`
	States           int    `json:"states"`
	CompensationFree bool   `json:"compensation_free"`
}

// quorumVote and quorumTally are the wire forms of govern.Vote and govern.QuorumResult, mirrored
// like confluenceCert.
type quorumVote struct {
	Voter    string `json:"voter"`
	Decision string `json:"decision"`
}

type quorumTally struct {
	Decision string       `json:"decision"`
	VotesFor int          `json:"votes_for"`
	Total    int          `json:"total"`
	Agreed   bool         `json:"agreed"`
	Votes    []quorumVote `json:"votes"`
}

// readLeaf decodes a proven record's Result into v with audit.UnmarshalStrict, as readJSON does a
// file: the leaf is committed as written, so it must read as written. what names the failure.
func readLeaf(b audit.ProofBundle, v any, what string) {
	if err := audit.UnmarshalStrict(b.Record.Result, v); err != nil {
		fatal(fmt.Errorf("%s: %w", what, err))
	}
}

// governedPolicyDigest returns the policy digest a governed-action payload (a tool result, as
// govern journals it) carries. The payload is open: it may hold other fields, such as the acting
// identity, so its names are not checked against a type. It must still decode strictly as an object
// (no duplicate names, no lone surrogate escapes), and the digest is read from the exact name
// "policy_digest" only, never from a case variant, as a reader of the file would read it.
func governedPolicyDigest(result json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if err := audit.UnmarshalStrict(result, &fields); err != nil {
		return "", err
	}
	raw, ok := fields["policy_digest"]
	if !ok {
		return "", errors.New("no \"policy_digest\"")
	}
	var digest string
	if err := audit.UnmarshalStrict(raw, &digest); err != nil {
		return "", fmt.Errorf("policy_digest: %w", err)
	}
	return digest, nil
}

func readJSON(path string, v any) {
	b, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	// Strict: a duplicate or case-variant key, an unknown field, or invalid UTF-8 is an error, so the
	// file a person reads is exactly the data the verifier checks.
	if err := audit.UnmarshalStrict(b, v); err != nil {
		fatal(fmt.Errorf("parse %s: %w", path, err))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

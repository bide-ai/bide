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
//	bide-audit verify -bundle proof.json -pubkey 1a2b...   # only exit 0 means verified
//
// Exit status: 0 verified, 1 not verified, 2 usage error, 3 no verdict, 4 input unreadable or
// unusable (see exitFor). Only 0 means verified.
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
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// policyFormatVersion is the published domain-separation tag for the combinator policy
// serialization (gsm's PolicyFormatVersion). It is hardcoded here on purpose: the verifier
// recomputes a policy digest from the published format and bytes without importing or trusting
// gsm, so the two roots of trust (the log and the proof) stay independent of the producer.
const policyFormatVersion = "gsm-policy-v1"

// defaultMaxInputBytes caps every file a verb reads (a bundle, journal, key, digest list, policy,
// or evidence package): 256 MiB, far above any real artifact, so a huge or endless input fails
// fast instead of exhausting memory. -max-input-bytes raises (or lowers) it.
const defaultMaxInputBytes = 256 << 20

// maxInputBytes is the cap in force, set by the verb's -max-input-bytes flag.
var maxInputBytes int64 = defaultMaxInputBytes

// maxClockSkew is how far past this machine's clock a signed head's timestamp may be, set by the
// verb's -max-clock-skew flag (see audit.CheckTimestamp).
var maxClockSkew = audit.DefaultClockSkew

// readInput reads the file at path, refusing one larger than maxInputBytes. It reads through a
// limit of one byte past the cap, so neither a huge file nor an endless stream (a pipe, a device)
// is read further than that. Every error is an unusable input.
func readInput(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, unusable(err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxInputBytes+1))
	if err != nil {
		return nil, unusable(err)
	}
	if int64(len(b)) > maxInputBytes {
		return nil, unusable(fmt.Errorf("%s is larger than the %d-byte input limit (raise it with -max-input-bytes)", path, maxInputBytes))
	}
	return b, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usageText = `bide-audit: produce and verify audit proof bundles

  bide-audit [-json] <verb> [flags]
  bide-audit -version [-json]

  prove  -journal <file> -sth <file> (-tool <id> | -index <n>) [-out <file>]
         build a ProofBundle for one record against a signed tree head

  verify -bundle <file> -pubkey <hex|file>
         verify a ProofBundle offline (its head must be a journal head of the bundle's run)

  verify-governance -policy <file> [-digest <hex>] [-checker <astchecker>]
         recompute the policy digest and, with -checker, run the external verified
         oracle to certify the policy converges

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

  verify-run -cert <file> -pubkey <hex|file> [-approved <digest>...] [-approved-file <file>] [-checker <astchecker>]
         (at least one of -approved and -approved-file; together they form one allowlist)
         verify a proof-carrying run certificate: the used-policy set is bound by a signed
         used-policy head to this run and its journal tree, and is a subset of the approved
         allowlist (only-approved-policies), and every used policy has an anchored, digest-linked convergence certificate
         in the run's signed tree (policies-convergence-certified); with -checker, the external
         oracle's convergence verdict on each used policy must AGREE with its certificate

  verify-evidence -evidence <file> -pubkey <hex|file> [-approved <digest>...] [-approved-file <file>]
         verify a portable evidence package offline and print a plain-English report: the
         format, seal, key, and run binding, then one line per proven action (tool call, step,
         grant), plus the run certificate (checked against the given allowlist, required when
         the package carries one) and consistency proof if present

  verify-approvals -evidence <file> -pubkey <hex|file> -call <tool-use-id> -need <k>
                   -approvers <id,id,...> -approver-keys <file>
         verify an m-of-n human approval gate from an evidence package: the request, every
         decision the gate read, its recorded tally, and the call's result all verify under the
         log key in one signed tree; recounting the decisions with each approver's key (a JSON
         object of approver id to ed25519 public key hex) against the exact call reproduces the
         recorded tally; the gate enforced the expected policy; and at least k approved

  prove-absent -journal <file> -sth <file> -key (tool:<id>|policy:<digest>) [-out <file>]
         prove a thing did NOT happen (no such tool call / no action under that policy)
         against a signed key-set head of the matching kind (see audit.SignAbsenceRoot)

  verify-absent -bundle <file> -pubkey <hex|file>
         verify an absence proof offline against a head of the key's own key set

Export a journal for ` + "`prove`" + ` with: json.Marshal(store.History(ctx, runID)). Every JSON
input is parsed strictly: a duplicate or case-variant key, an unknown field, or invalid UTF-8 is
an error, a bundle, certificate or package must carry the "format" this version reads (one made
by an older release is refused with a message naming the format; -version lists them), and a
public key must be 32 bytes of hex. Every verb takes -max-input-bytes <n>: an input file larger
than n bytes (default 268435456, 256 MiB) is an error, and none is read past the cap. Every signed
tree head an input carries must have a positive timestamp (Unix nanoseconds) no later than this
machine's clock plus -max-clock-skew <duration> (default 5m). Every verb takes -json: it prints one
JSON report on stdout (exit_code, result, verified, output, errors) instead of the text report.

Exit status. Only 0 means verified; treat every other status as a failure, and 4 as a failure,
never as a reason to retry (a tampered "format" field yields 4):
  0  verified (for prove and prove-absent: the artifact was written)
  1  read and understood, and not verified
  2  usage error: a bad flag or argument; nothing was read
  3  no verdict: the -checker gave none (it could not be started, exited with a status other
     than 0 or 1, was killed, or printed an unreadable compensation_free line), or an internal error
  4  an input is unreadable or unusable: a missing or unreadable file, one over -max-input-bytes,
     JSON that does not read strictly, an unknown or unsupported format, or an artifact of the
     wrong type for its flag
When several apply, 1 wins over 4, and 4 over 3: a verb reads every input and runs every check
that does not depend on an unusable one, so a tampered input is reported as 1 even beside an
unreadable one.
`

// staticStore is a read-only Durable backed by an exported journal, so the CLI can reuse the
// audit package's proof builders without a live store.
type staticStore struct{ recs []agent.Record }

func (s staticStore) History(context.Context, string) ([]agent.Record, error) { return s.recs, nil }
func (staticStore) Do(context.Context, string, string, func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return agent.Record{}, errors.New("bide-audit: journal is read-only")
}

func (c *cli) prove(args []string) {
	fs := c.flagSet("prove")
	journal := fs.String("journal", "", "path to the exported journal JSON ([]Record)")
	sthPath := fs.String("sth", "", "path to the signed tree head JSON")
	tool := fs.String("tool", "", "prove the tool call with this ToolUseID")
	index := fs.Int("index", -1, "prove the record at this journal index")
	out := fs.String("out", "", "write the bundle here (default: stdout)")
	c.parse(fs, args)

	if *journal == "" || *sthPath == "" || (*tool == "") == (*index < 0) {
		c.usageError("")
	}

	var recs []agent.Record
	c.note(c.readJSON(*journal, &recs))
	var sth audit.SignedTreeHead
	c.note(c.readJSON(*sthPath, &sth))
	if !c.clean() {
		return
	}

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
		// The journal and head were read, but they cannot make this proof (no such record, or a
		// head that does not commit to this journal).
		c.note(unusable(err))
		return
	}
	c.emit(bundle, *out)
}

// emit writes a produced artifact to path, or to stdout (into the report under -json).
func (c *cli) emit(v any, path string) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		c.note(internal(err))
		return
	}
	switch {
	case path != "":
		if err := os.WriteFile(path, b, 0o644); err != nil {
			c.note(internal(err))
			return
		}
		fmt.Fprintf(c.stderr, "wrote %s\n", path)
	case c.json:
		c.artifact = b
	default:
		c.println(string(b))
	}
}

func (c *cli) verify(args []string) {
	fs := c.flagSet("verify")
	bundlePath := fs.String("bundle", "", "path to the ProofBundle JSON")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	c.parse(fs, args)

	if *bundlePath == "" || *pubkey == "" {
		c.usageError("")
	}

	var bundle audit.ProofBundle
	c.note(c.readJSON(*bundlePath, &bundle))
	pub, err := readPubKey(*pubkey)
	c.note(err)
	if !c.clean() {
		return
	}

	ok, err := bundle.Verify(pub)
	if err != nil {
		c.note(unusable(err))
		return
	}
	if !ok {
		c.fail("proof did not verify under this key")
	}
	c.printf("OK: run %q record verified in a signed tree of size %d\n", bundle.RunID, bundle.STH.Size)
}

// verifyBundle checks b is authentic under pub. A bundle that does not verify is a FAIL verdict
// (it unwinds the verb); one that cannot be checked (an unsupported format, a record that cannot
// be canonicalized) is an unusable input, returned.
func (c *cli) verifyBundle(b audit.ProofBundle, pub []byte, what string) error {
	ok, err := b.Verify(pub)
	if err != nil {
		return unusable(fmt.Errorf("%s bundle: %w", what, err))
	}
	if !ok {
		c.fail("%s bundle did not verify under this key", what)
	}
	return nil
}

// verifyGovernance closes the loop between the two roots of trust for a governed policy: it
// recomputes the policy digest from the published bytes (the cryptographic identity anchored in
// the log and embedded in each governed action's ProofBundle) and, given the external verified
// oracle, certifies that the policy actually converges (the mathematical guarantee). It imports
// neither gsm nor the runtime: it recomputes the digest from the published format and runs a
// checker the auditor supplies, so it trusts neither the producer nor any code the producer wrote.
func (c *cli) verifyGovernance(args []string) {
	fs := c.flagSet("verify-governance")
	policyPath := fs.String("policy", "", "path to the serialized combinator policy (gsm PolicyBytes / WriteMachineAST output)")
	expected := fs.String("digest", "", "expected policy digest as hex (e.g. from a ProofBundle or the anchor); must match if set")
	checker := fs.String("checker", "", "path to the external verified oracle (astchecker); if set, it is run on the policy")
	c.parse(fs, args)

	if *policyPath == "" {
		c.usageError("")
	}
	policy, err := readInput(*policyPath)
	if !c.note(err) {
		return
	}

	// Domain-separated SHA-256 over the published format tag and the policy bytes, recomputed
	// here rather than taken from gsm, so the digest check is independent of the producer.
	digest := policyDigest(policy)
	c.printf("policy digest: %s\n", digest)

	if *expected != "" && trimSpace(*expected) != digest {
		c.fail("digest mismatch (expected %s)", trimSpace(*expected))
	}

	if *checker == "" {
		c.println("OK: digest computed. Pass -checker <astchecker> to also certify the policy converges.")
		return
	}

	v, ok := c.checkPolicyFile(*checker, *policyPath, "oracle")
	if !ok {
		return
	}
	if !v.converges {
		c.fail("the verified oracle did not certify this policy as convergent")
	}
	c.println("OK: digest verified and the external oracle certifies the policy converges")
}

// policyDigest is the domain-separated SHA-256 of a policy's published bytes, recomputed
// independently of gsm.
func policyDigest(policy []byte) string {
	h := sha256.New()
	h.Write([]byte(policyFormatVersion + "\n"))
	h.Write(policy)
	return hex.EncodeToString(h.Sum(nil))
}

// policyLeaf reads b as the anchored policy leaf: its content, the leaf record it must be, and
// the digest its bytes must hash to. A bundle that is not a policy leaf is an unusable input,
// returned; a leaf whose bytes do not hash to the digest it claims is a FAIL verdict.
func (c *cli) policyLeaf(b audit.ProofBundle) (audit.PolicyContent, error) {
	var pc audit.PolicyContent
	if err := readLeaf(b, &pc, "policy bundle is not a policy leaf"); err != nil {
		return pc, err
	}
	if err := requireLeaf(b, audit.PolicyLeafName(pc.Digest), "policy"); err != nil {
		return pc, err
	}
	// The leaf's bytes actually hash to that digest (recomputed independently of gsm).
	if recomputed := policyDigest([]byte(pc.Policy)); recomputed != pc.Digest {
		c.fail("policy leaf lies about its digest (bytes hash to %s, leaf claims %s)", recomputed, pc.Digest)
	}
	return pc, nil
}

// verifyGovernedAction verifies a governed action against its anchored policy from public
// artifacts alone: two ProofBundles (the action and the policy leaf) plus an out-of-band public
// key. It confirms (1) both bundles are authentic under the key, (2) they are in the SAME signed
// tree, (3) the action bundle proves a tool call's result, the policy bundle proves the policy leaf
// (audit.PolicyLeafName) for the digest it carries, and the action's embedded policy digest
// matches that digest,
// (4) the leaf's bytes actually hash to that digest (so the leaf cannot lie about which policy it
// is), and (5) with -checker, that the external verified oracle certifies the policy converges.
// Steps 1 to 4 are the cryptographic root; step 5 is the independent mathematical root. Each
// check runs when the inputs it reads are usable, so a policy that does not converge is reported
// even when the action bundle cannot be read.
func (c *cli) verifyGovernedAction(args []string) {
	fs := c.flagSet("verify-governed-action")
	actionPath := fs.String("action", "", "path to the action ProofBundle JSON (from prove -tool)")
	policyPath := fs.String("policy-bundle", "", "path to the policy-leaf ProofBundle JSON (from ProvePolicy)")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	checker := fs.String("checker", "", "path to the external verified oracle (astchecker); if set, it certifies the policy converges")
	c.parse(fs, args)

	if *actionPath == "" || *policyPath == "" || *pubkey == "" {
		c.usageError("")
	}
	var action, policy audit.ProofBundle
	okAction := c.note(c.readJSON(*actionPath, &action))
	okPolicy := c.note(c.readJSON(*policyPath, &policy))
	pub, err := readPubKey(*pubkey)
	okKey := c.note(err)

	// (1) both bundles authentic under the out-of-band key.
	okAction = okAction && okKey && c.note(c.verifyBundle(action, pub, "action"))
	okPolicy = okPolicy && okKey && c.note(c.verifyBundle(policy, pub, "policy"))

	// (2) same signed tree (of the same run).
	if okAction && okPolicy && !action.STH.SameTree(policy.STH.TreeHead) {
		c.fail("the action and policy are not committed in the same signed tree")
	}

	// (3) the action's embedded policy digest matches the anchored policy leaf's digest, and (4)
	// the leaf's bytes hash to that digest.
	var actionDigest string
	if okAction {
		okAction = c.note(requireToolResult(action, "action"))
	}
	if okAction {
		actionDigest, err = governedPolicyDigest(action.Record.Result)
		okAction = c.note(err)
	}
	var pc audit.PolicyContent
	if okPolicy {
		pc, err = c.policyLeaf(policy)
		okPolicy = c.note(err)
	}
	if okAction && okPolicy && (actionDigest == "" || actionDigest != pc.Digest) {
		c.fail("action policy digest %q does not link to the anchored policy leaf %q", actionDigest, pc.Digest)
	}
	if c.clean() {
		c.printf("OK: action in run %q ran under anchored policy %s, both in a signed tree of size %d\n", action.RunID, pc.Digest, action.STH.Size)
	}

	// (5) the independent mathematical root: the policy converges.
	if *checker == "" {
		if c.clean() {
			c.println("OK: cryptographic root verified. Pass -checker <astchecker> to also certify the policy converges.")
		}
		return
	}
	if !okPolicy {
		return
	}
	v, ok := c.checkPolicy(*checker, pc.Policy, "oracle")
	if ok && !v.converges {
		c.fail("the verified oracle did not certify the anchored policy as convergent")
	}
	if c.clean() {
		c.println("OK: both roots verified: the action ran under an anchored, provably convergent policy")
	}
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
func (c *cli) verifyConvergence(args []string) {
	fs := c.flagSet("verify-convergence")
	certPath := fs.String("cert-bundle", "", "path to the convergence-leaf ProofBundle JSON (from ProveConvergence)")
	policyPath := fs.String("policy-bundle", "", "path to the policy-leaf ProofBundle JSON (from ProvePolicy)")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	checker := fs.String("checker", "", "path to the external verified oracle (astchecker); if set, its verdict must agree with the certificate")
	c.parse(fs, args)

	if *certPath == "" || *policyPath == "" || *pubkey == "" {
		c.usageError("")
	}
	var certBundle, policy audit.ProofBundle
	okCert := c.note(c.readJSON(*certPath, &certBundle))
	okPolicy := c.note(c.readJSON(*policyPath, &policy))
	pub, err := readPubKey(*pubkey)
	okKey := c.note(err)

	// (1) both bundles authentic under the out-of-band key.
	okCert = okCert && okKey && c.note(c.verifyBundle(certBundle, pub, "certificate"))
	okPolicy = okPolicy && okKey && c.note(c.verifyBundle(policy, pub, "policy"))

	// (2) same signed tree (of the same run).
	if okCert && okPolicy && !certBundle.STH.SameTree(policy.STH.TreeHead) {
		c.fail("the certificate and policy are not committed in the same signed tree")
	}

	// (3) the certificate certifies the same digest the policy leaf carries, and (4) the leaf's
	// bytes hash to that digest. The certificate is decoded into a local struct so the CLI imports
	// neither gsm nor govern.
	var cc audit.ConvergenceContent
	var cert confluenceCert
	if okCert {
		okCert = c.note(readLeaf(certBundle, &cc, "certificate bundle is not a convergence leaf")) &&
			c.note(requireLeaf(certBundle, audit.ConvergenceLeafName(cc.Digest), "certificate"))
	}
	var pc audit.PolicyContent
	if okPolicy {
		pc, err = c.policyLeaf(policy)
		okPolicy = c.note(err)
	}
	if okCert && okPolicy && (cc.Digest == "" || cc.Digest != pc.Digest) {
		c.fail("certificate digest %q does not link to the anchored policy leaf %q", cc.Digest, pc.Digest)
	}
	if okCert {
		if err := audit.UnmarshalStrict(cc.Certificate, &cert); err != nil {
			okCert = c.note(unusable(fmt.Errorf("convergence certificate payload: %w", err)))
		}
	}
	if c.clean() {
		fragment := "governed (compensation-bearing)"
		if cert.CompensationFree {
			fragment = "CRDT (compensation-free fragment)"
		}
		c.printf("certificate: machine %q claims converges=%v, %s, checked over %d states (max repair depth %d)\n",
			cert.Machine, cert.Converges, fragment, cert.States, cert.MaxRepairLen)
		c.printf("OK: certificate anchored for policy %s in a signed tree of size %d\n", pc.Digest, certBundle.STH.Size)
	}

	// (5) the independent root: the oracle's verdict must AGREE with the certificate's claim.
	if *checker == "" {
		if c.clean() {
			c.println("OK: cryptographic root verified. Pass -checker <astchecker> to cross-check the convergence claim against the oracle.")
		}
		return
	}
	if !okCert || !okPolicy {
		return
	}
	oracle, ok := c.checkPolicy(*checker, pc.Policy, "oracle")
	if !ok {
		return
	}
	if oracle.converges != cert.Converges {
		c.fail("certificate claims converges=%v but the verified oracle says converges=%v", cert.Converges, oracle.converges)
	}
	// If the oracle also certifies the CRDT-fragment classification (compensation_free=<bool>),
	// cross-check it, so a certificate cannot overstate that either. Older oracles omit the line;
	// then the classification stays producer-reported, which we say rather than silently pass.
	if oracle.classified {
		if oracle.compensationFree != cert.CompensationFree {
			c.fail("certificate claims compensation_free=%v but the verified oracle says compensation_free=%v", cert.CompensationFree, oracle.compensationFree)
		}
	} else {
		c.println("note: this oracle does not certify the compensation-free classification; that field stays producer-reported")
	}
	if !cert.Converges {
		c.fail("the certificate and the oracle agree the policy does NOT converge; do not deploy it")
	}
	c.println("OK: the verified oracle's verdict agrees with the certificate: the anchored policy provably converges")
}

// oracleVerdict is the external checker's verdict on one policy.
type oracleVerdict struct {
	converges bool
	// compensationFree is the checker's compensation_free=<bool> classification; classified is
	// false when it printed none (an older checker), so the caller can tell "the checker
	// disagrees" from "the checker does not report it".
	compensationFree, classified bool
}

// runOracle runs the external checker on the policy file at path, prints its output after label,
// and returns its verdict. The astchecker exits 0 for a convergent policy, 1 for one that does not
// converge, and 2 for a usage or parse error (as does any uncaught OCaml exception), so only 0 and
// 1 are verdicts. A checker that cannot be started, exits with any other status, or is killed by a
// signal gives none, and neither does output with a compensation_free line that reads as neither
// true nor false, or as both: runOracle returns an error, which the caller must not read as "does
// not converge".
func (c *cli) runOracle(checker, path, label string) (oracleVerdict, error) {
	out, err := exec.Command(checker, path).CombinedOutput()
	c.printf("%s: %s", label, out)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		c.println()
	}
	var v oracleVerdict
	var exit *exec.ExitError
	switch {
	case err == nil:
		v.converges = true
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		v.converges = false
	default:
		return v, fmt.Errorf("the checker %s gave no verdict (%v)", checker, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "compensation_free") {
			continue
		}
		var cf bool
		switch line {
		case "compensation_free=true":
			cf = true
		case "compensation_free=false":
		default:
			return v, fmt.Errorf("the checker %s printed an unreadable classification line %q", checker, line)
		}
		if v.classified && cf != v.compensationFree {
			return v, fmt.Errorf("the checker %s printed both compensation_free=true and compensation_free=false", checker)
		}
		v.compensationFree, v.classified = cf, true
	}
	return v, nil
}

// checkPolicyFile runs the external checker on the policy file at path (see runOracle). ok is
// false when it gave no verdict, which is recorded.
func (c *cli) checkPolicyFile(checker, path, label string) (oracleVerdict, bool) {
	v, err := c.runOracle(checker, path, label)
	if err != nil {
		c.noVerdict(err)
		return v, false
	}
	return v, true
}

// checkPolicy runs the external checker on policy, written to a temporary file for it (see
// runOracle). ok is false when it gave no verdict, which is recorded.
func (c *cli) checkPolicy(checker, policy, label string) (oracleVerdict, bool) {
	tmp, err := os.CreateTemp("", "policy-*.machine")
	if err != nil {
		c.note(internal(err))
		return oracleVerdict{}, false
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(policy)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		c.note(internal(err))
		return oracleVerdict{}, false
	}
	return c.checkPolicyFile(checker, tmp.Name(), label)
}

// noVerdict reports that the external checker gave no verdict, as an ERROR line and a
// no-verdict error. It does not unwind the verb: a check that does not depend on the checker can
// still find a verdict, which outranks it.
func (c *cli) noVerdict(err error) {
	line := fmt.Sprintf("ERROR: %v; the policy was not checked, so this is no verdict on whether it converges", err)
	c.println(line)
	c.note(&cliError{class: errNoVerdict, err: errors.New(line), shown: true})
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
func (c *cli) verifyQuorum(args []string) {
	fs := c.flagSet("verify-quorum")
	name := fs.String("name", "", "the quorum's name, as passed to govern.Quorum")
	tallyPath := fs.String("tally", "", "path to the quorum tally ProofBundle JSON (the quorum/<name>/tally step)")
	var votePaths stringList
	fs.Var(&votePaths, "vote", "path to one vote ProofBundle JSON (a quorum/<name>/vote/<voter> step); repeat once per voter")
	commitPath := fs.String("commit", "", "path to the commit action ProofBundle JSON (optional)")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	k := fs.Int("k", 0, "the quorum threshold to assert: votes_for must be >= k")
	c.parse(fs, args)

	if *name == "" || *tallyPath == "" || len(votePaths) == 0 || *pubkey == "" || *k <= 0 {
		c.usageError("")
	}
	pub, err := readPubKey(*pubkey)
	okKey := c.note(err)

	var tally audit.ProofBundle
	okTally := c.note(c.readJSON(*tallyPath, &tally))
	votes := make([]audit.ProofBundle, len(votePaths))
	okVote := make([]bool, len(votePaths))
	for i, p := range votePaths {
		okVote[i] = c.note(c.readJSON(p, &votes[i]))
	}
	var commit audit.ProofBundle
	okCommit := *commitPath != "" && c.note(c.readJSON(*commitPath, &commit))

	// (1) authenticity; (2) same signed tree and same run as the tally. The signed head commits to
	// the run, so the same tree is also the same run.
	okTally = okTally && okKey && c.note(c.verifyBundle(tally, pub, "tally"))
	for i := range votes {
		okVote[i] = okVote[i] && okKey && c.note(c.verifyBundle(votes[i], pub, fmt.Sprintf("vote %d", i)))
		if okVote[i] && okTally && !votes[i].STH.SameTree(tally.STH.TreeHead) {
			c.fail("a vote is not committed in the same signed tree and run as the tally")
		}
	}
	okCommit = okCommit && okKey && c.note(c.verifyBundle(commit, pub, "commit"))
	if okCommit && okTally && !commit.STH.SameTree(tally.STH.TreeHead) {
		c.fail("the commit is not in the same signed tree and run as the quorum")
	}

	// (2) every bundle was recorded by this quorum: the tally under its tally step, each vote under
	// the vote step of the voter it records.
	var rec quorumTally
	if okTally {
		if want := "quorum/" + *name + "/tally"; tally.Record.Name != want {
			c.fail("the tally bundle is step %q, not %q: it is not the tally of quorum %q", tally.Record.Name, want, *name)
		}
		c.note(readLeaf(tally, &rec, "tally bundle is not a quorum tally"))
	}
	disclosed := map[string]string{} // voter -> decision
	counts := map[string]int{}
	for i := range votes {
		if !okVote[i] {
			continue
		}
		var v quorumVote
		if !c.note(readLeaf(votes[i], &v, fmt.Sprintf("vote bundle %d is not a vote leaf", i))) {
			continue
		}
		if want := "quorum/" + *name + "/vote/" + v.Voter; votes[i].Record.Name != want {
			c.fail("vote bundle %d is step %q, not %q: it is not %q's vote in quorum %q", i, votes[i].Record.Name, want, v.Voter, *name)
		}
		disclosed[v.Voter] = v.Decision
		counts[v.Decision]++
	}

	// (3) and (4) read the tally and every vote, so they run only when all of them are usable.
	if c.clean() {
		c.checkTally(rec, disclosed, counts, len(votes), *k)
		c.printf("OK: %d-of-%d agreement on decision %q (votes_for=%d >= k=%d), recomputed from %d disclosed votes, in run %q of a signed tree of size %d\n",
			rec.VotesFor, rec.Total, rec.Decision, rec.VotesFor, *k, len(votes), tally.RunID, tally.STH.Size)
	}

	// (optional) confirm a governed commit is anchored in the same tree and run.
	if okCommit {
		if !c.note(requireToolResult(commit, "commit")) {
			return
		}
		commitDigest, err := governedPolicyDigest(commit.Record.Result)
		if !c.note(err) {
			return
		}
		if commitDigest == "" {
			c.fail("commit bundle is not a governed-action leaf: it names no policy digest")
		}
		if c.clean() {
			c.printf("OK: a governed commit under policy %s is anchored in the same tree; the decision committed under k-of-n agreement\n", commitDigest)
		}
	}
}

// checkTally checks the disclosed votes are exactly the votes the tally records, that the tally
// recomputes from them, and that it meets k with a single most-supported decision.
func (c *cli) checkTally(rec quorumTally, disclosed map[string]string, counts map[string]int, nVotes, k int) {
	// The disclosed votes are exactly the votes the tally records (each recorded vote is disclosed
	// with the same decision here, and the count check below rules out extra or repeated bundles),
	// and the tally recomputes from them. A voter the tally records twice would let one disclosed
	// vote count twice, so each recorded voter must be distinct.
	recorded := map[string]bool{}
	for _, v := range rec.Votes {
		if recorded[v.Voter] {
			c.fail("the tally records a vote by %q more than once", v.Voter)
		}
		recorded[v.Voter] = true
		d, ok := disclosed[v.Voter]
		if !ok {
			c.fail("the tally records a vote by %q that is not disclosed; all votes must be disclosed to verify the tally", v.Voter)
		}
		if d != v.Decision {
			c.fail("the tally records %q voting %q, but the disclosed vote is %q", v.Voter, v.Decision, d)
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
	if nVotes != len(rec.Votes) || nVotes != rec.Total {
		c.fail("%d vote bundles disclosed but the tally records %d votes and total=%d; disclose exactly the votes the tally records", nVotes, len(rec.Votes), rec.Total)
	}
	if winner != rec.Decision || best != rec.VotesFor {
		c.fail("recorded tally (decision=%q votes_for=%d) does not match the disclosed votes (decision=%q votes_for=%d)",
			rec.Decision, rec.VotesFor, winner, best)
	}

	// The quorum threshold, and a single most-supported decision: in a tie for the most votes
	// every tied decision may reach k, but none has more support than another.
	for _, d := range decs {
		if d != winner && counts[d] == best {
			c.fail("no single decision has the most votes: %q and %q each have %d", winner, d, best)
		}
	}
	if rec.VotesFor < k {
		c.fail("quorum not met: votes_for=%d < k=%d for decision %q", rec.VotesFor, k, rec.Decision)
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
func (c *cli) verifyRun(args []string) {
	fs := c.flagSet("verify-run")
	certPath := fs.String("cert", "", "path to the RunCertificate JSON (from audit.CertifyRun)")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	var approved stringList
	fs.Var(&approved, "approved", "an approved policy digest; repeat once per allowed policy")
	approvedFile := fs.String("approved-file", "", "path to a file of approved policy digests, one per line")
	checker := fs.String("checker", "", "path to the external verified oracle (astchecker); if set, its verdict must agree with each policy's certificate")
	c.parse(fs, args)

	if *certPath == "" || *pubkey == "" || (len(approved) == 0 && *approvedFile == "") {
		c.usageError("")
	}

	var cert audit.RunCertificate
	okCert := c.note(c.readJSON(*certPath, &cert))
	pub, err := readPubKey(*pubkey)
	okKey := c.note(err)
	// The auditor's own allowlist governs the only-approved-policies check; the certificate carries
	// none, so a producer cannot pass by widening its own set.
	allow := append([]string(nil), approved...)
	okAllow := true
	if *approvedFile != "" {
		lines, err := readDigestLines(*approvedFile)
		okAllow = c.note(err)
		allow = append(allow, lines...)
	}
	// The used-policy head is projected from the journal head, so it cannot be the earlier one.
	if okCert {
		if err := audit.CheckTimestampOrder(cert.STH.TreeHead, cert.UsedPolicyAbsence.TreeHead); err != nil {
			c.fail("%v", err)
		}
	}
	if !okCert || !okKey {
		return
	}
	// With the allowlist unusable, every check that does not read it still runs: VerifyRun is given
	// the certificate's own used-policy set, so the membership check passes and any failure it
	// reports is one no allowlist could cure. The verb still does not verify.
	if !okAllow {
		allow = cert.UsedPolicies
	}

	res, err := audit.VerifyRun(cert, allow, pub)
	if err != nil {
		c.note(unusable(err))
		return
	}
	for _, r := range res.Reasons {
		c.printf("  - %s\n", r)
	}
	if !res.OK {
		c.fail("run %q certificate did not verify (only-approved-policies=%v, policies-convergence-certified=%v)",
			cert.RunID, res.OnlyApprovedPolicies, res.ConvergenceCertified)
	}
	if c.clean() {
		c.printf("OK: run %q: %d policies used, all in the approved set and bound to the run's signed used-policy set; each has an anchored convergence certificate in a signed tree of size %d\n",
			cert.RunID, len(cert.UsedPolicies), cert.STH.Size)
	}

	// The independent mathematical root: cross-check each used policy's certificate against the
	// oracle. It reads only the certificate, so it runs even when the allowlist was unusable, and a
	// verdict it finds outranks that.
	if *checker == "" {
		if c.clean() {
			c.println("OK: cryptographic root verified. Pass -checker <astchecker> to cross-check each policy's convergence claim against the oracle.")
		}
		return
	}
	// res.Policies is what VerifyRun read (strictly) from the leaves it verified, so the oracle
	// checks exactly those bytes. A policy the oracle gives no verdict on does not stop the others:
	// a verdict on another one outranks it.
	for _, polC := range res.Policies {
		var claim confluenceCert
		if err := audit.UnmarshalStrict(polC.Certificate, &claim); err != nil {
			c.note(unusable(fmt.Errorf("convergence certificate payload for %s: %w", polC.Digest, err)))
			continue
		}

		oracle, ok := c.checkPolicy(*checker, polC.Policy, "oracle ("+polC.Digest+")")
		if !ok {
			continue
		}
		if oracle.converges != claim.Converges {
			c.fail("policy %s certificate claims converges=%v but the verified oracle says converges=%v", polC.Digest, claim.Converges, oracle.converges)
		}
		if oracle.classified && oracle.compensationFree != claim.CompensationFree {
			c.fail("policy %s certificate claims compensation_free=%v but the verified oracle says compensation_free=%v", polC.Digest, claim.CompensationFree, oracle.compensationFree)
		}
		if !claim.Converges {
			c.fail("policy %s does NOT converge (certificate and oracle agree); do not deploy it", polC.Digest)
		}
	}
	if c.clean() {
		c.println("OK: the verified oracle agrees with every policy's certificate: the whole run ran under approved, provably convergent policies")
	}
}

// verifyEvidence verifies a portable evidence package (audit.Evidence) from public artifacts alone:
// the package JSON, an out-of-band public key, and (for a package with a run certificate) the
// auditor's approved-policy allowlist. It re-checks every field the package carries via
// audit.EvidencePackage.Verify (format, seal, key, the STH and its run, each action's inclusion proof
// and label, the grant chain, the run certificate and consistency proof if present) and prints a
// plain-English report, one line per proven item, then an overall verdict. It exits non-zero if the
// package did not verify.
func (c *cli) verifyEvidence(args []string) {
	fs := c.flagSet("verify-evidence")
	evidencePath := fs.String("evidence", "", "path to the EvidencePackage JSON (from audit.Evidence)")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	var approved stringList
	fs.Var(&approved, "approved", "an approved policy digest for the run certificate; repeat once per allowed policy")
	approvedFile := fs.String("approved-file", "", "path to a file of approved policy digests, one per line")
	c.parse(fs, args)

	if *evidencePath == "" || *pubkey == "" {
		c.usageError("")
	}

	var pkg audit.EvidencePackage
	okPkg := c.note(c.readJSON(*evidencePath, &pkg))
	pub, err := readPubKey(*pubkey)
	okKey := c.note(err)
	var opts []audit.EvidenceVerifyOption
	if len(approved) > 0 || *approvedFile != "" {
		allow := append([]string(nil), approved...)
		okAllow := true
		if *approvedFile != "" {
			lines, err := readDigestLines(*approvedFile)
			okAllow = c.note(err)
			allow = append(allow, lines...)
		}
		// With the allowlist unusable, every check that does not read it still runs: the run
		// certificate is checked against its own used-policy set, so any failure the package
		// reports is one no allowlist could cure. The verb still does not verify.
		if !okAllow && okPkg && pkg.RunCertificate != nil {
			allow = pkg.RunCertificate.UsedPolicies
		}
		opts = append(opts, audit.WithApprovedPolicies(allow...))
	}
	if !okPkg || !okKey {
		return
	}
	rep, err := pkg.Verify(pub, opts...)
	if err != nil {
		c.note(unusable(err))
		return
	}

	if pkg.Label != "" {
		c.printf("evidence for run %q: %s\n", rep.RunID, pkg.Label)
	} else {
		c.printf("evidence for run %q\n", rep.RunID)
	}
	if rep.STHVerified {
		c.printf("PASS  signed tree head authentic (size %d)\n", pkg.STH.Size)
	} else {
		c.println("FAIL  signed tree head is NOT authentic under this key")
	}
	for _, p := range rep.Problems {
		c.printf("FAIL  %s\n", p)
	}
	for _, it := range rep.Items {
		status := "FAIL"
		if it.Verified {
			status = "PASS"
		}
		c.printf("%s  %s\n", status, evidenceItemLine(it))
	}

	if !rep.OK {
		c.fail("evidence package did not verify under this key")
	}
	if !c.clean() {
		return
	}
	c.printf("PASS: run %q evidence verified (%d items) in a signed tree of size %d\n", rep.RunID, len(rep.Items), pkg.STH.Size)
}

// verifyApprovals checks an m-of-n approval gate from an evidence package with
// audit.VerifyApprovals, trusting only the log key, the approvers' keys, and the expected
// policy given on the command line.
func (c *cli) verifyApprovals(args []string) {
	fs := c.flagSet("verify-approvals")
	evidencePath := fs.String("evidence", "", "path to the EvidencePackage JSON carrying the approval evidence")
	pubkey := fs.String("pubkey", "", "the log's ed25519 public key as hex, or a path to a file containing it")
	call := fs.String("call", "", "the gated call's tool-use id")
	need := fs.Int("need", 0, "approvals the policy requires (k)")
	approvers := fs.String("approvers", "", "the policy's eligible approver ids, comma-separated, in policy order")
	keysPath := fs.String("approver-keys", "", `JSON object of approver id to ed25519 public key hex, e.g. {"ops":"ab12..."}`)
	c.parse(fs, args)

	if *evidencePath == "" || *pubkey == "" || *call == "" || *need == 0 || *approvers == "" || *keysPath == "" {
		c.usageError("")
	}
	// The expected policy comes from the command line, so an invalid one is a usage error, found
	// before any file is read.
	policy := agent.ApprovalPolicy{Need: *need, Approvers: strings.Split(*approvers, ",")}
	if err := policy.Validate(); err != nil {
		c.usageError(fmt.Sprintf("verify-approvals: -need and -approvers: %v", err))
	}
	var pkg audit.EvidencePackage
	okPkg := c.note(c.readJSON(*evidencePath, &pkg))
	logPub, err := readPubKey(*pubkey)
	c.note(err)
	var keyHex map[string]string
	okKeys := c.note(c.readJSON(*keysPath, &keyHex))
	keys := make(map[string][]byte, len(keyHex))
	for id, h := range keyHex {
		k, err := hex.DecodeString(strings.TrimSpace(h))
		switch {
		case err != nil:
			okKeys = c.note(unusable(fmt.Errorf("approver %q key must be hex: %w", id, err)))
		case len(k) != ed25519.PublicKeySize:
			okKeys = c.note(unusable(fmt.Errorf("approver %q key is %d bytes, want a %d-byte ed25519 public key", id, len(k), ed25519.PublicKeySize)))
		default:
			keys[id] = k
		}
	}
	if !okPkg || !okKeys || !c.clean() {
		return
	}
	verifierFor := func(id string) (agent.ApproverVerifier, bool) {
		k, ok := keys[id]
		if !ok {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: k}, true
	}

	// A package whose proofs cannot be checked is an unusable input, but the approval check still
	// runs: a gate that did not hold is a verdict, which outranks it.
	rep, err := pkg.Verify(logPub)
	if err != nil {
		err = unusable(err)
	}
	okRep := c.note(err)
	v, err := audit.VerifyApprovals(pkg.Actions, *call, policy, verifierFor, logPub)
	if err != nil {
		c.failLine(fmt.Sprintf("FAIL  %v", err))
	}
	var callArgs bytes.Buffer
	if err := json.Compact(&callArgs, v.Args); err != nil {
		callArgs.Reset()
		callArgs.Write(v.Args)
	}
	c.printf("approval gate on call %q: %s %s\n", *call, v.ToolName, callArgs.String())
	if okRep {
		proofs := "FAIL"
		if rep.OK {
			proofs = "PASS"
		}
		c.printf("%s  every proof in the package verifies under the log key\n", proofs)
	}
	for _, id := range v.Counted {
		c.printf("PASS  %s approved (signature verifies for this exact call)\n", id)
	}
	for _, id := range v.DeniedBy {
		c.printf("INFO  %s denied\n", id)
	}
	for _, d := range v.Ignored {
		c.printf("INFO  ignored %s: %s\n", d.Approver, d.Reason)
	}
	for _, p := range v.Problems {
		c.printf("FAIL  %s\n", p)
	}
	if (okRep && !rep.OK) || !v.OK {
		line := fmt.Sprintf("FAIL: %d of %d required approvals verified", len(v.Counted), v.Need)
		if len(v.Problems) > 0 || (okRep && !rep.OK) {
			line += ", and the evidence is not consistent"
		}
		c.failLine(line)
	}
	if c.clean() {
		c.printf("PASS: %d of %d required approvals verified, from complete evidence\n", len(v.Counted), v.Need)
	}
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
func readDigestLines(path string) ([]string, error) {
	b, err := readInput(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		s := trimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		out = append(out, s)
	}
	return out, nil
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

func (c *cli) proveAbsent(args []string) {
	fs := c.flagSet("prove-absent")
	journal := fs.String("journal", "", "path to the exported journal JSON ([]Record)")
	sthPath := fs.String("sth", "", "path to the signed absence tree head JSON (see audit.SignAbsenceRoot)")
	key := fs.String("key", "", "what to prove absent: tool:<id> or policy:<digest>")
	out := fs.String("out", "", "write the absence bundle here (default: stdout)")
	c.parse(fs, args)

	if *journal == "" || *sthPath == "" || *key == "" {
		c.usageError("")
	}
	set, absKey, err := absenceSelector(*key)
	if err != nil {
		c.usageError("prove-absent: -key: " + err.Error())
	}
	var recs []agent.Record
	c.note(c.readJSON(*journal, &recs))
	var sth audit.SignedTreeHead
	c.note(c.readJSON(*sthPath, &sth))
	if !c.clean() {
		return
	}

	bundle, err := audit.ProveAbsentBundle(recs, set, absKey, sth)
	if err != nil {
		c.note(unusable(err))
		return
	}
	c.emit(bundle, *out)
}

func (c *cli) verifyAbsent(args []string) {
	fs := c.flagSet("verify-absent")
	bundlePath := fs.String("bundle", "", "path to the AbsenceBundle JSON")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	c.parse(fs, args)

	if *bundlePath == "" || *pubkey == "" {
		c.usageError("")
	}
	var bundle audit.AbsenceBundle
	okBundle := c.note(c.readJSON(*bundlePath, &bundle))
	pub, err := readPubKey(*pubkey)
	c.note(err)

	// The key names the key set it can be absent from; the bundle's head must be of that set. A
	// key of no known set is an absence bundle this version cannot read.
	var set audit.KeySet
	if okBundle {
		var known bool
		if set, known = audit.KeySetForKey(bundle.Absence.Key); !known {
			c.note(unusable(fmt.Errorf("%q is not a tool-use or used-policy key", bundle.Absence.Key)))
		}
	}
	if !c.clean() {
		return
	}
	ok, err := bundle.Verify(pub, set)
	if err != nil {
		c.note(unusable(err))
		return
	}
	if !ok {
		c.fail("absence proof did not verify under this key")
	}
	c.printf("OK: %q is absent from run %q's first %d records (its signed %s key set of size %d)\n",
		bundle.Absence.Key, bundle.RunID, bundle.STH.Journal.Size, set.Kind, bundle.Absence.Size)
}

// readPubKey reads the verifier's trust root from a -pubkey value: an ed25519 public key in hex,
// or the path of a file holding one. A value that is a public key in hex is that key and is never
// opened as a file: a file of that name (planted in an evidence directory, say) could hold another
// key, and the CLI would then verify under it. The key must come from out-of-band; that is the
// whole point of the trust model. A value that is neither is an unusable input: it may be the
// path of a file that is missing.
func readPubKey(s string) ([]byte, error) {
	if key, err := hex.DecodeString(trimSpace(s)); err == nil && len(key) == ed25519.PublicKeySize {
		return key, nil
	}
	raw := s
	if _, err := os.Stat(s); err == nil {
		b, err := readInput(s)
		if err != nil {
			return nil, err
		}
		raw = string(b)
	}
	raw = trimSpace(raw)
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, unusable(fmt.Errorf("public key must be hex (or a file of hex): %w", err))
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, unusable(fmt.Errorf("public key is %d bytes, want a %d-byte ed25519 public key", len(key), ed25519.PublicKeySize))
	}
	return key, nil
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

// requireLeaf returns an unusable-input error unless b proves the StepValue record named name: the
// leaf the audit package writes for the role the bundle is read in. A record proves only what it
// is, so a record of any other name or kind (a tool result whose output has the leaf's shape, say)
// is an artifact of the wrong type for its flag.
func requireLeaf(b audit.ProofBundle, name, role string) error {
	if b.Record.Kind != agent.StepValue || b.Record.Name != name {
		return unusable(fmt.Errorf("the %s bundle proves record %q of kind %q, not the %s leaf %q", role, b.Record.Name, b.Record.Kind, role, name))
	}
	return nil
}

// requireToolResult returns an unusable-input error unless b proves a tool call's result: a
// governed action (or commit) is the result a governed tool journaled, not any record whose
// payload has that shape.
func requireToolResult(b audit.ProofBundle, role string) error {
	if b.Record.Kind != agent.StepToolResult {
		return unusable(fmt.Errorf("the %s bundle proves record %q of kind %q, not a tool call's result", role, b.Record.Name, b.Record.Kind))
	}
	return nil
}

// readLeaf decodes a proven record's Result into v with audit.UnmarshalStrict, as readJSON does a
// file: the leaf is committed as written, so it must read as written. A leaf that does not is not
// the leaf its flag names: an unusable input. what names the failure.
func readLeaf(b audit.ProofBundle, v any, what string) error {
	if err := audit.UnmarshalStrict(b.Record.Result, v); err != nil {
		return unusable(fmt.Errorf("%s: %w", what, err))
	}
	return nil
}

// governedPolicyDigest returns the policy digest a governed-action payload carries, read by the
// rule the used-policy set uses (see audit.GovernedPolicyDigest). A payload that is not a
// governed-action payload is an unusable input.
func governedPolicyDigest(result json.RawMessage) (string, error) {
	d, err := audit.GovernedPolicyDigest(result)
	if err != nil {
		return "", unusable(fmt.Errorf("the result is not a governed-action payload: %w", err))
	}
	return d, nil
}

// readJSON reads the file at path into v, strictly. A file that cannot be read or does not decode
// is an unusable input. A signed head in it that breaks the timestamp rule is a not-verified
// verdict: the file was read and understood, and it does not hold.
func (c *cli) readJSON(path string, v any) error {
	b, err := readInput(path)
	if err != nil {
		return err
	}
	// Strict: a duplicate or case-variant key, an unknown field, or invalid UTF-8 is an error, so the
	// file a person reads is exactly the data the verifier checks.
	if err := audit.UnmarshalStrict(b, v); err != nil {
		return unusable(fmt.Errorf("parse %s: %w", path, err))
	}
	// Every signed head in the input, however deeply it is carried, is held to the timestamp rule.
	if err := checkHeadTimes(reflect.ValueOf(v), time.Now()); err != nil {
		return &cliError{class: errNotVerified, err: fmt.Errorf("%s: %w", path, err)}
	}
	return nil
}

var signedHeadType = reflect.TypeFor[audit.SignedTreeHead]()

// checkHeadTimes applies audit.CheckTimestamp, against now and -max-clock-skew, to every signed
// tree head reachable from v.
func checkHeadTimes(v reflect.Value, now time.Time) error {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return checkHeadTimes(v.Elem(), now)
		}
	case reflect.Struct:
		if v.Type() == signedHeadType {
			return audit.CheckTimestamp(v.Interface().(audit.SignedTreeHead).TreeHead, now, maxClockSkew)
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				if err := checkHeadTimes(v.Field(i), now); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil // bytes and raw JSON carry no head
		}
		for i := range v.Len() {
			if err := checkHeadTimes(v.Index(i), now); err != nil {
				return err
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if err := checkHeadTimes(it.Value(), now); err != nil {
				return err
			}
		}
	}
	return nil
}

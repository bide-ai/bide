package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// EvidenceFormat is the format/version tag written into every EvidencePackage. It is
// domain-separated and versioned so a verifier rejects a package produced by an incompatible
// layout rather than misreading it. v2 binds tree heads to their run, seals the package, and
// carries the earlier signed head a consistency proof starts from. v3 is v2 over journals that
// key a tool call's records by its encoded ID (agent.ToolResultStep, agent.ApprovalTallyStep)
// and a sub-agent's run by agent.SubRunID, so the record names a v3 package proves differ from
// a v2 package's for the same run. v4 is v3 with snake_case names in every proof it carries (the
// inclusion and consistency proofs spelled "Index", "Size", "Path" and "First" before) and a
// "format" on each ProofBundle and on the RunCertificate. v5 names the signing key by scheme and
// key ({alg, public_key}) rather than by ed25519 hex, carries bide.audit.proof.v3 bundles (each
// record as its stored bytes) and bide.audit.sth.v5 heads, and labels its actions snake_case kinds.
const EvidenceFormat = "bide.audit.evidence.v5"

// evidenceSealTag domain-separates the package seal from every other message the log key signs.
const evidenceSealTag = EvidenceFormat + ".seal\x00"

// An EvidencePackage is the portable, offline-verifiable evidence artifact for one run: a single
// self-contained bundle that PACKAGES a run's already-produced cryptographic proofs (a signed tree
// head, per-action inclusion proofs, and optionally the run certificate, the authority grant chain,
// and an append-only consistency proof) into one JSON document you can store, email, or hand to an
// auditor. Verify re-checks each one against the out-of-band public key.
//
// Every field is verified or derived from verified data. Format must be EvidenceFormat. RunID must
// be the run the signed STH commits to. Each action's Kind, Label, and Ref must be what its proven
// record says (a tool result cannot be presented as an approval). The grant chain must be exactly
// the grants the anchored leaves prove. The run certificate must be for this run and tree. The
// consistency proof is checked from an earlier signed head of the same run to the STH.
// Alg and PublicKey must be the verifying key's. Label, the one field no proof covers, is covered by the
// seal (Signature): the log key's signature over the whole package, so nothing (Label included) can be edited, and
// no item added or dropped, after the producer sealed it.
//
// It is deliberately PURE DATA: no funcs, no keys, no unexported state, so it round-trips losslessly
// through JSON. The verifying key must be obtained out of band (the anchor / transparency log);
// Verify takes it as an argument rather than trusting Alg and PublicKey.
type EvidencePackage struct {
	// Format is the domain-separated format/version tag (EvidenceFormat).
	Format string `json:"format"`
	// RunID is the run this evidence is about; it must equal STH.RunID.
	RunID string `json:"run_id"`
	// Label is an optional producer-supplied description of what this evidence establishes (e.g.
	// "Q3 refund approvals"). No proof covers it; the seal attributes it to the log key holder.
	Label string `json:"label,omitempty"`
	// Alg and PublicKey name the key that signed the STH and the seal: its scheme and its encoded
	// public key (see NewVerifier), carried so a reader knows which key to obtain out of band.
	// Verify requires them to equal the verifier's; it never trusts them as the key.
	Alg       Alg    `json:"alg"`
	PublicKey []byte `json:"public_key"`

	// STH is the run's signed journal head: the commitment every action proof below is checked
	// against. One STH is built for the whole package so all proofs share one root. Its Timestamp is
	// when the package's commitment was made.
	STH SignedTreeHead `json:"sth"`

	// Actions are the material tool calls / steps proven included in the signed log. Each carries a
	// human label and the ProofBundle proving that one action is a leaf under the STH's root.
	Actions []EvidenceAction `json:"actions"`

	// Grants optionally carries the authority behind the run: the delegation chain (root-first) as
	// signed grants, plus a ProofBundle per grant proving the grant leaf is anchored in the signed
	// log. It is nil when the caller did not request grants (WithGrants).
	Grants *EvidenceGrants `json:"grants,omitempty"`

	// RunCertificate optionally carries the proof-carrying run certificate (only-approved-policies +
	// policies-convergence-certified) for the whole run. It is nil unless WithRunCertificate was set.
	RunCertificate *RunCertificate `json:"run_certificate,omitempty"`

	// Consistency optionally carries an append-only consistency proof from an earlier signed head of
	// this run's journal to STH. It is nil unless WithConsistencyFrom was set.
	Consistency *EvidenceConsistency `json:"consistency,omitempty"`

	// Signature is the seal: the log key's signature, under Alg, over the package with Signature
	// empty (see Seal). A package changed after sealing, including one with actions appended, fails to
	// verify until resealed.
	Signature []byte `json:"signature"`
}

// EvidenceConsistency proves the journal only grew between two signed heads of one run: From, an
// earlier head (typically taken from the anchor log), and the package STH.
type EvidenceConsistency struct {
	// From is an earlier signed journal head of the same run. Proof.First must equal From.Size.
	From SignedTreeHead `json:"from"`
	// Proof is the RFC 6962 consistency proof from From's tree to the package STH's tree.
	Proof Consistency `json:"proof"`
}

// EvidenceAction is one proven action inside an EvidencePackage: a ProofBundle plus the label, kind,
// and reference the report names it by. Verify derives all three from the proven record and fails
// the item if they differ, so they cannot misdescribe what was proven.
type EvidenceAction struct {
	// Label is a short human description of the action: the tool-use id for "tool", the step name
	// for "step", the called tool's name for "call", the approver id for "approval", "approval
	// tally" for "approval-tally", and "grant <issuer> -> <subject>" for "grant".
	Label string `json:"label"`
	// Kind is the action category, one of "tool", "step", "grant", "call", "approval", or
	// "approval-tally"; the proven record must be of that kind.
	Kind EvidenceKind `json:"kind"`
	// Ref is the identifier the action was selected by: the tool-use id ("tool", "call"), the record
	// name ("step", "approval", "approval-tally"), or the grant digest ("grant").
	Ref string `json:"ref,omitempty"`
	// Bundle is the inclusion proof that this action is a committed leaf under the package's STH.
	Bundle ProofBundle `json:"bundle"`
}

// EvidenceGrants carries the run's authority: the delegation chain as signed grants (root-first) and,
// per grant, a ProofBundle proving the grant leaf is anchored in the same signed log as the actions.
//
// Note the trust boundary: the anchoring bundles verify under the package's log key, and Verify
// checks that Chain is exactly the grants they prove. A grant's own issuer signature is made with
// the ISSUER's key, which is distinct from the log key and is not carried here; verifying the issuer
// signatures and the attenuation chain (VerifyDelegationChain) requires the issuers' public keys,
// which a caller supplies from their own PKI.
type EvidenceGrants struct {
	// Chain is the delegation chain, root-first to leaf-last, as signed grants (may be a single grant).
	Chain []SignedGrant `json:"chain"`
	// Anchored is one ProofBundle per grant in Chain, in the same order, proving that exact signed
	// grant is committed in the signed log.
	Anchored []EvidenceAction `json:"anchored"`
}

// evidenceOptions is the assembled configuration a set of EvidenceOption values produces.
type evidenceOptions struct {
	label          string
	toolCalls      []string // explicit tool-use ids to prove
	steps          []string // explicit step names to prove
	allToolCalls   bool     // enumerate and prove every completed tool call
	runCertificate bool
	runCertSpec    RunCertSpec
	grants         bool
	consistency    *SignedTreeHead
}

// An EvidenceOption selects what an EvidencePackage includes. With no action option (WithToolCall,
// WithStep, or WithAllToolCalls), Evidence defaults to proving every completed tool call, so the
// zero-configuration package is "prove every action this run took".
type EvidenceOption func(*evidenceOptions)

// WithLabel sets a human-readable description of what the evidence establishes.
func WithLabel(label string) EvidenceOption {
	return func(o *evidenceOptions) { o.label = label }
}

// WithToolCall adds a proof that the completed tool call with this tool-use id happened in the run.
func WithToolCall(toolUseID string) EvidenceOption {
	return func(o *evidenceOptions) { o.toolCalls = append(o.toolCalls, toolUseID) }
}

// WithStep adds a proof that the durable step with this name (a Step / Parallel task) happened.
func WithStep(name string) EvidenceOption {
	return func(o *evidenceOptions) { o.steps = append(o.steps, name) }
}

// WithAllToolCalls proves every completed tool call in the run, enumerated from its history. It is
// the default when no other action option is given.
func WithAllToolCalls() EvidenceOption {
	return func(o *evidenceOptions) { o.allToolCalls = true }
}

// WithRunCertificate includes the proof-carrying run certificate for the whole run, certified
// against spec.ApprovedPolicies. It fails assembly if the run used a policy outside the allowlist
// (CertifyRun refuses). Evidence signs the certificate with its own signer at its own timestamp;
// spec.Signer and spec.TimestampNanos are ignored. The verifier checks the certificate against its
// own allowlist (WithApprovedPolicies), not this one.
func WithRunCertificate(spec RunCertSpec) EvidenceOption {
	return func(o *evidenceOptions) {
		o.runCertificate = true
		o.runCertSpec = spec
	}
}

// WithGrants includes the run's authority: every anchored grant leaf, proven included in the signed
// log, plus the signed grants themselves as the delegation chain (in journal order, root-first for a
// well-formed attenuation chain).
func WithGrants() EvidenceOption {
	return func(o *evidenceOptions) { o.grants = true }
}

// WithConsistencyFrom includes an append-only consistency proof from earlier, a signed journal head
// of the same run taken before this package (for instance the last head an auditor saw in the anchor
// log), to the package's STH: nothing earlier than earlier.Size was rewritten or reordered.
func WithConsistencyFrom(earlier SignedTreeHead) EvidenceOption {
	return func(o *evidenceOptions) { o.consistency = &earlier }
}

// Evidence assembles and seals the portable, offline-verifiable EvidencePackage for runID. It reads
// the journal once, builds ONE signed tree head over it (signed with s at timestamp, Unix
// nanoseconds), then, per the options, gathers inclusion proofs for the material tool calls /
// steps and, optionally, the run certificate, the anchored authority grants, and an append-only
// consistency proof, all bound to that one STH and built for exactly the records it commits to.
// With no action option it proves every completed tool call in the run. It finishes with Seal(s).
//
// It mints no new proof types: every proof is an existing audit primitive, packaged into one
// document a verifier re-checks offline with EvidencePackage.Verify against the out-of-band key.
// The signer's scheme and public key are recorded as metadata (Verify requires them to match the
// verifier it is given; it does not trust the embedded ones).
func Evidence(ctx context.Context, store *agent.Journal, runID string, s Signer, timestamp int64, opts ...EvidenceOption) (EvidencePackage, error) {
	cfg := evidenceOptions{}
	for _, opt := range opts {
		opt(&cfg)
	}
	// Default: with no explicit action selection, prove every completed tool call.
	if !cfg.allToolCalls && len(cfg.toolCalls) == 0 && len(cfg.steps) == 0 {
		cfg.allToolCalls = true
	}
	if err := checkSigner(s); err != nil {
		return EvidencePackage{}, fmt.Errorf("audit: evidence: %w", err)
	}

	// One read of the journal: the STH and every proof below are built for these records.
	recs, err := store.History(ctx, runID)
	if err != nil {
		return EvidencePackage{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	th, err := journalHead(runID, recs, timestamp)
	if err != nil {
		return EvidencePackage{}, err
	}
	sth, err := SignTreeHead(th, s)
	if err != nil {
		return EvidencePackage{}, fmt.Errorf("audit: evidence: %w", err)
	}

	pkg := EvidencePackage{
		Format:    EvidenceFormat,
		RunID:     runID,
		Label:     cfg.label,
		Alg:       s.Alg(),
		PublicKey: s.PublicKey(),
		STH:       sth,
	}

	// Action proofs: explicit tool calls and steps, plus (default) every completed tool call.
	for _, id := range cfg.toolCalls {
		pb, err := ProveToolCall(ctx, store, runID, id, sth)
		if err != nil {
			return EvidencePackage{}, err
		}
		pkg.Actions = append(pkg.Actions, EvidenceAction{Label: id, Kind: KindTool, Ref: id, Bundle: pb})
	}
	for _, name := range cfg.steps {
		pb, err := ProveStep(ctx, store, runID, name, sth)
		if err != nil {
			return EvidencePackage{}, err
		}
		pkg.Actions = append(pkg.Actions, EvidenceAction{Label: name, Kind: KindStep, Ref: name, Bundle: pb})
	}
	if cfg.allToolCalls {
		seen := make(map[string]struct{}, len(cfg.toolCalls))
		for _, id := range cfg.toolCalls {
			seen[id] = struct{}{}
		}
		for _, r := range recs {
			if r.Kind != agent.StepToolResult || r.ToolUseID == "" {
				continue
			}
			if _, dup := seen[r.ToolUseID]; dup {
				continue
			}
			seen[r.ToolUseID] = struct{}{}
			pb, err := ProveToolCall(ctx, store, runID, r.ToolUseID, sth)
			if err != nil {
				return EvidencePackage{}, err
			}
			pkg.Actions = append(pkg.Actions, EvidenceAction{Label: r.ToolUseID, Kind: KindTool, Ref: r.ToolUseID, Bundle: pb})
		}
	}

	// Optional: authority grants, each anchored and proven included in the signed log.
	if cfg.grants {
		grants, err := gatherGrants(recs, runID, sth)
		if err != nil {
			return EvidencePackage{}, err
		}
		pkg.Grants = grants
	}

	// Optional: the proof-carrying run certificate for the whole run.
	if cfg.runCertificate {
		spec := cfg.runCertSpec
		spec.Signer, spec.TimestampNanos = s, timestamp
		cert, err := CertifyRun(ctx, store, runID, sth, spec)
		if err != nil {
			return EvidencePackage{}, err
		}
		pkg.RunCertificate = &cert
	}

	// Optional: an append-only consistency proof from an earlier head to this one, built for
	// exactly the two sizes the heads commit to.
	if cfg.consistency != nil {
		from := *cfg.consistency
		prefix, err := journalPrefix(runID, recs, from.TreeHead)
		if err != nil {
			return EvidencePackage{}, fmt.Errorf("audit: evidence: consistency: %w", err)
		}
		leaves, err := journalLeafHashes(recs)
		if err != nil {
			return EvidencePackage{}, err
		}
		pkg.Consistency = &EvidenceConsistency{
			From:  from,
			Proof: Consistency{First: len(prefix), Size: sth.Size, Path: consistencyProof(len(prefix), leaves)},
		}
	}

	if err := pkg.Seal(s); err != nil {
		return EvidencePackage{}, err
	}
	return pkg, nil
}

// Seal signs the package with s, the log key that signed its STH, over the package's JSON encoding
// with Signature empty under a dedicated domain tag. Evidence seals what it builds; a caller that
// adds actions afterwards (for instance ApprovalEvidence's output) seals again. It refuses a signer
// whose scheme and key are not the ones the package names (Alg, PublicKey), and a package with
// invalid UTF-8 in any string, which JSON would silently rewrite.
func (e *EvidencePackage) Seal(s Signer) error {
	if err := checkSigner(s); err != nil {
		return fmt.Errorf("audit: seal evidence: %w", err)
	}
	if s.Alg() != e.Alg || !bytes.Equal(s.PublicKey(), e.PublicKey) {
		return fmt.Errorf("audit: seal evidence: the signer's %s key is not the key the package names: %w", s.Alg(), agent.ErrConfig)
	}
	msg, err := e.sealMessage()
	if err != nil {
		return err
	}
	sig, err := s.Sign(msg)
	if err != nil {
		return fmt.Errorf("audit: seal evidence: %w", err)
	}
	e.Signature = sig
	return nil
}

func (e EvidencePackage) sealMessage() ([]byte, error) {
	e.Signature = nil
	if err := checkUTF8(e); err != nil {
		return nil, fmt.Errorf("audit: seal evidence: %w", err)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("audit: seal evidence: %w", err)
	}
	return append([]byte(evidenceSealTag), b...), nil
}

// gatherGrants enumerates the anchored grant leaves in the journal (audit:grant:<digest>), proves each
// is included in the signed log, and decodes the signed grant so the report can name the chain. The
// grants are returned in journal order, which is root-first for a well-formed attenuation chain.
func gatherGrants(recs []agent.Record, runID string, sth SignedTreeHead) (*EvidenceGrants, error) {
	out := &EvidenceGrants{}
	for i, r := range recs[:sth.Size] {
		if r.Kind != agent.StepValue || !strings.HasPrefix(r.Name, grantLeafPrefix) {
			continue
		}
		var sg SignedGrant
		if err := json.Unmarshal(r.Result, &sg); err != nil {
			return nil, fmt.Errorf("audit: evidence: decode grant leaf %q: %w", r.Name, err)
		}
		digest := strings.TrimPrefix(r.Name, grantLeafPrefix)
		pb, err := proveIn(runID, recs, i, sth)
		if err != nil {
			return nil, err
		}
		out.Chain = append(out.Chain, sg)
		out.Anchored = append(out.Anchored, EvidenceAction{
			Label:  grantLabel(sg.Grant),
			Kind:   KindGrant,
			Ref:    digest,
			Bundle: pb,
		})
	}
	if len(out.Chain) == 0 {
		return nil, fmt.Errorf("audit: evidence: WithGrants requested but run %s has no anchored grant leaves", runID)
	}
	return out, nil
}

func grantLabel(g Grant) string { return fmt.Sprintf("grant %s -> %s", g.Issuer, g.Subject) }

// EvidenceReport is the structured outcome of EvidencePackage.Verify: an overall verdict plus a
// per-item verdict list, so a caller (or the CLI) can print exactly which proven item held and which
// did not. It is the machine-readable form behind the plain-English CLI report.
type EvidenceReport struct {
	// OK is the overall verdict: true only when the package-level checks pass and every item verified.
	OK bool `json:"ok"`
	// RunID echoes the package's run id; when OK it is the run the signed STH commits to.
	RunID string `json:"run_id"`
	// Label echoes the package's optional label; when OK it is the label the log key sealed.
	Label string `json:"label,omitempty"`
	// STHVerified is true when the signed tree head is authentic under the verifying key and is a
	// journal head of RunID. Every action proof is checked against this same root.
	STHVerified bool `json:"sth_verified"`
	// Problems lists every package-level failure (format, seal, key, run binding), empty when none.
	Problems []string `json:"problems,omitempty"`
	// Items is one verdict per proven item (each action, each anchored grant, the run certificate, and
	// the consistency proof), in report order.
	Items []EvidenceItem `json:"items"`
}

// EvidenceItem is one line of an EvidenceReport: what was checked, whether it verified, and a short
// human note (the reason on failure, or a one-line description on success).
type EvidenceItem struct {
	Kind     EvidenceKind `json:"kind"`  // the item's kind: an action kind, KindRunCertificate, or KindConsistency
	Label    string       `json:"label"` // the human label for the item
	Ref      string       `json:"ref,omitempty"`
	Verified bool         `json:"verified"` // whether this item verified under the key
	Note     string       `json:"note"`     // plain-English detail, on success or failure
}

// EvidenceVerifyOption supplies what the auditor, not the package, decides.
type EvidenceVerifyOption func(*evidenceVerifyOptions)

type evidenceVerifyOptions struct {
	approved    []string
	hasApproved bool
	now         time.Time // WithVerifyTime
	hasNow      bool
	skew        time.Duration // WithClockSkew
	hasSkew     bool
}

// WithApprovedPolicies is the auditor's approved-policy allowlist, against which a package's run
// certificate is checked. A package that carries a run certificate fails verification without it.
func WithApprovedPolicies(digests ...string) EvidenceVerifyOption {
	return func(o *evidenceVerifyOptions) {
		o.approved = append(o.approved, digests...)
		o.hasApproved = true
	}
}

// Verify re-checks every proof in the package against v, a verifier for the log key obtained OUT
// OF BAND (the anchor / transparency log), entirely offline. It does not trust the package's
// embedded key: it requires it to be v's. It verifies: (0) the format tag, that Alg and PublicKey
// are v's scheme and key, the seal, and that the STH is an authentic journal head of RunID; (1) each
// action ProofBundle, confirming the action is a leaf under that signed root and that its Kind,
// Label, and Ref are what the proven record says; (2) each anchored grant bundle the same way, and
// that Chain is exactly the grants they prove; (3) the run certificate, if present, is for this run
// and tree and passes VerifyRun against the allowlist given with WithApprovedPolicies; and (4) the
// consistency proof, if present, from its authentic earlier head of this run to the STH. A package
// with none of (1) to (4) proves nothing and does not verify. Every signed head the package carries
// is held to the timestamp rule (see CheckTimestamp): against time.Now and DefaultClockSkew unless
// WithVerifyTime and WithClockSkew say otherwise.
//
// It returns a structured EvidenceReport with an overall verdict and a per-item verdict. The error is
// nil when the package verifies (OK), and wraps ErrNotVerified when it does not (the report says
// why). Any other error means the package could not be checked: it is not of the format this version
// reads (ErrFormat), or a proof it carries cannot be read (ErrFormat, ErrMalformed). Grant ISSUER
// signatures and the attenuation chain are NOT checked here (they need the issuers' keys, which are
// not in the package); a caller verifies those with VerifyDelegationChain and its own PKI.
func (e EvidencePackage) Verify(v Verifier, opts ...EvidenceVerifyOption) (EvidenceReport, error) {
	var vo evidenceVerifyOptions
	for _, o := range opts {
		o(&vo)
	}
	rep := EvidenceReport{RunID: e.RunID, Label: e.Label}
	if err := checkVerifier(v); err != nil {
		return rep, err
	}
	// The package and every artifact it carries, at any depth, are of the format this version
	// reads, whether or not a check below reaches them (a run certificate for another run is never
	// passed to VerifyRun, so the heads it carries would otherwise go unread).
	if err := checkFormats(e); err != nil {
		return rep, fmt.Errorf("audit: evidence package: %w", err)
	}
	problem := func(format string, args ...any) { rep.Problems = append(rep.Problems, fmt.Sprintf(format, args...)) }

	if e.Alg != v.Alg() || !bytes.Equal(e.PublicKey, v.PublicKey()) {
		problem("the package names the %s key %x, not the verifying %s key %x", e.Alg, e.PublicKey, v.Alg(), v.PublicKey())
	}
	if msg, err := e.sealMessage(); err != nil || !v.Verify(msg, e.Signature) {
		problem("the package seal does not verify under this key (the package was changed after it was sealed)")
	}
	switch err := e.STH.Verify(v); {
	case err != nil && !errors.Is(err, ErrNotVerified):
		return rep, fmt.Errorf("audit: evidence STH: %w", err)
	case err == nil && e.STH.Kind == TreeJournal && e.STH.RunID == e.RunID:
		rep.STHVerified = true
	default:
		problem("the signed tree head is not an authentic journal head of run %q under this key", e.RunID)
	}
	now, skew := vo.clock()
	if err := CheckTimestamp(e.STH.TreeHead, now, skew); err != nil {
		problem("%v", err)
	}
	allOK := len(rep.Problems) == 0

	verifyBundle := func(a EvidenceAction) (EvidenceItem, error) {
		item := EvidenceItem{Kind: a.Kind, Label: a.Label, Ref: a.Ref}
		err := a.Bundle.Verify(v)
		if err != nil && !errors.Is(err, ErrNotVerified) {
			return item, err
		}
		switch {
		case err != nil:
			item.Note = "proof did not verify under this key"
		case !a.Bundle.STH.SameTree(e.STH.TreeHead):
			// A bundle that verifies against a DIFFERENT signed tree must not count as evidence
			// for this run's committed log.
			item.Note = "verified against a different signed tree than the package STH"
		case CheckTimestamp(a.Bundle.STH.TreeHead, now, skew) != nil:
			item.Note = CheckTimestamp(a.Bundle.STH.TreeHead, now, skew).Error()
		default:
			why, err := actionMismatch(a)
			if err != nil {
				return item, err
			}
			if why != "" {
				item.Note = why
			} else {
				item.Verified = true
				item.Note = itemSuccessNote(item.Kind, item.Label)
			}
		}
		return item, nil
	}

	for _, a := range e.Actions {
		item, err := verifyBundle(a)
		if err != nil {
			return EvidenceReport{}, err
		}
		allOK = allOK && item.Verified
		rep.Items = append(rep.Items, item)
	}

	if e.Grants != nil {
		if len(e.Grants.Chain) == 0 || len(e.Grants.Anchored) != len(e.Grants.Chain) {
			problem("the grant chain has %d grants but %d anchoring proofs", len(e.Grants.Chain), len(e.Grants.Anchored))
			allOK = false
		}
		for i, g := range e.Grants.Anchored {
			item, err := verifyBundle(g)
			if err != nil {
				return EvidenceReport{}, err
			}
			if item.Verified && item.Kind != KindGrant {
				item.Verified, item.Note = false, fmt.Sprintf("an anchoring proof of kind %q, not %q", item.Kind, KindGrant)
			}
			if item.Verified {
				rec, err := g.Bundle.Record()
				if err != nil {
					return EvidenceReport{}, err
				}
				if i >= len(e.Grants.Chain) || !sameSignedGrant(e.Grants.Chain[i], rec) {
					item.Verified, item.Note = false, fmt.Sprintf("chain grant %d is not the grant this leaf anchors", i)
				}
			}
			allOK = allOK && item.Verified
			rep.Items = append(rep.Items, item)
		}
	}

	if c := e.RunCertificate; c != nil {
		item := EvidenceItem{Kind: KindRunCertificate, Label: "run certificate", Ref: c.RunID}
		switch {
		case c.RunID != e.RunID || !c.STH.SameTree(e.STH.TreeHead):
			item.Note = fmt.Sprintf("the run certificate is for run %q at a different tree, not this package's", c.RunID)
		case !vo.hasApproved:
			item.Note = "no approved-policy allowlist was supplied to check the run certificate against (WithApprovedPolicies)"
		case certTimeProblem(*c, now, skew) != nil:
			item.Note = certTimeProblem(*c, now, skew).Error()
		default:
			res, err := VerifyRun(*c, vo.approved, v)
			if err != nil && !errors.Is(err, ErrNotVerified) {
				return EvidenceReport{}, err
			}
			item.Verified = err == nil
			if item.Verified {
				item.Note = fmt.Sprintf("%d policies used, all approved and convergence-certified in the signed log", len(c.UsedPolicies))
			} else {
				item.Note = "run certificate did not verify: " + strings.Join(res.Reasons, "; ")
			}
		}
		allOK = allOK && item.Verified
		rep.Items = append(rep.Items, item)
	}

	if c := e.Consistency; c != nil {
		item := EvidenceItem{Kind: KindConsistency, Label: fmt.Sprintf("first %d records are an append-only prefix", c.Proof.First), Ref: fmt.Sprintf("%d..%d", c.Proof.First, c.Proof.Size)}
		fromErr := c.From.Verify(v)
		if fromErr != nil && !errors.Is(fromErr, ErrNotVerified) {
			return EvidenceReport{}, fmt.Errorf("audit: evidence consistency: %w", fromErr)
		}
		switch {
		case fromErr != nil || c.From.Kind != TreeJournal || c.From.RunID != e.RunID:
			item.Note = "the earlier tree head is not an authentic journal head of this run under this key"
		case CheckTimestamp(c.From.TreeHead, now, skew) != nil:
			item.Note = CheckTimestamp(c.From.TreeHead, now, skew).Error()
		case CheckTimestampOrder(c.From.TreeHead, e.STH.TreeHead) != nil:
			item.Note = CheckTimestampOrder(c.From.TreeHead, e.STH.TreeHead).Error()
		case c.Proof.First != c.From.Size || c.Proof.Size != e.STH.Size:
			item.Note = fmt.Sprintf("the proof is for sizes %d..%d, not the signed sizes %d..%d", c.Proof.First, c.Proof.Size, c.From.Size, e.STH.Size)
		case VerifyConsistency(c.From.Root, e.STH.Root, c.Proof) != nil:
			item.Note = "the consistency proof does not verify between the two signed roots"
		default:
			item.Verified = true
			item.Note = fmt.Sprintf("the signed tree of size %d is an append-only extension of the signed tree of size %d", e.STH.Size, c.From.Size)
		}
		allOK = allOK && item.Verified
		rep.Items = append(rep.Items, item)
	}

	// A package that proves nothing is not evidence of anything. Every item above is a proof, so no
	// items means no proof was offered, and an empty report must not read as a clean audit.
	if len(rep.Items) == 0 {
		problem("the package proves nothing: it has no action, grant, run certificate, or consistency proof")
	}

	rep.OK = allOK && len(rep.Problems) == 0
	if !rep.OK {
		failed := 0
		for _, it := range rep.Items {
			if !it.Verified {
				failed++
			}
		}
		return rep, notVerified("audit: evidence for run %q: %d problems, %d of %d items not verified", e.RunID, len(rep.Problems), failed, len(rep.Items))
	}
	return rep, nil
}

// certTimeProblem applies the timestamp rule to every signed head a run certificate carries: its
// journal head, its used-policy head (which must not be earlier than the journal head it was
// projected from), and the head of each convergence evidence bundle.
func certTimeProblem(c RunCertificate, now time.Time, skew time.Duration) error {
	heads := []TreeHead{c.STH.TreeHead, c.UsedPolicyAbsence.TreeHead}
	for _, pc := range c.Convergence {
		heads = append(heads, pc.PolicyLeaf.STH.TreeHead, pc.Certificate.STH.TreeHead)
	}
	for _, th := range heads {
		if err := CheckTimestamp(th, now, skew); err != nil {
			return err
		}
	}
	return CheckTimestampOrder(c.STH.TreeHead, c.UsedPolicyAbsence.TreeHead)
}

// actionMismatch returns why a's Kind, Label, and Ref are not what its proven record says, or ""
// if they are. It is the rule that keeps the report's description of an item tied to the proof. It
// errors (ErrMalformed) only if the proven record bytes do not decode.
func actionMismatch(a EvidenceAction) (string, error) {
	r, err := a.Bundle.Record()
	if err != nil {
		return "", err
	}
	want := func(kindOK bool, label, ref string) string {
		if !kindOK {
			return fmt.Sprintf("the proven record (kind %q, name %q) is not a %q action", r.Kind, r.Name, a.Kind)
		}
		if a.Label != label || a.Ref != ref {
			return fmt.Sprintf("labelled %q / %q, but the proven record says %q / %q", a.Label, a.Ref, label, ref)
		}
		return ""
	}
	switch a.Kind {
	case KindTool:
		return want(r.Kind == agent.StepToolResult, r.ToolUseID, r.ToolUseID), nil
	case KindStep:
		return want(r.Kind == agent.StepValue, r.Name, r.Name), nil
	case KindCall:
		_, call, ok := agent.FindToolCall([]agent.Record{r}, a.Ref)
		return want(ok, call.Name, a.Ref), nil
	case KindApproval:
		return want(r.Kind == agent.StepApproval && r.Approver() != "", r.Approver(), r.Name), nil
	case KindApprovalTally:
		return want(r.Kind == agent.StepValue && strings.HasPrefix(r.Name, agent.ApprovalTallyStep("")), "approval tally", r.Name), nil
	case KindGrant:
		var sg SignedGrant
		isGrant := r.Kind == agent.StepValue && strings.HasPrefix(r.Name, grantLeafPrefix) && json.Unmarshal(r.Result, &sg) == nil &&
			r.Name == grantLeafName(sg.Grant.Digest())
		return want(isGrant, grantLabel(sg.Grant), strings.TrimPrefix(r.Name, grantLeafPrefix)), nil
	default:
		return fmt.Sprintf("unknown action kind %q", a.Kind), nil
	}
}

// sameSignedGrant reports whether rec is the anchored leaf of exactly sg: the grant leaf named by
// sg's digest whose content is sg's canonical encoding.
func sameSignedGrant(sg SignedGrant, rec agent.Record) bool {
	want, err := json.Marshal(sg)
	if err != nil || rec.Name != grantLeafName(sg.Grant.Digest()) {
		return false
	}
	var got bytes.Buffer
	if err := json.Compact(&got, rec.Result); err != nil {
		return false
	}
	return bytes.Equal(got.Bytes(), want)
}

// itemSuccessNote phrases the plain-English success line for a verified item by kind.
func itemSuccessNote(kind EvidenceKind, label string) string {
	switch kind {
	case KindTool:
		return "tool call included in the signed log"
	case KindStep:
		return "step included in the signed log"
	case KindGrant:
		return "grant anchored in the signed log"
	case KindCall:
		return "the request for the gated call (its tool and arguments) included in the signed log"
	case KindApproval:
		return "approver decision included in the signed log (check the signatures and the count with VerifyApprovals)"
	case KindApprovalTally:
		return "the approval gate's recorded tally included in the signed log"
	default:
		return "included in the signed log"
	}
}

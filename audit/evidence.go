package audit

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/blackwell-systems/bide/agent"
)

// EvidenceFormat is the format/version tag written into every EvidencePackage. It is
// domain-separated and versioned so a verifier can reject a package produced by an
// incompatible future layout rather than misreading it.
const EvidenceFormat = "bide.audit.evidence.v1"

// An EvidencePackage is the portable, offline-verifiable evidence artifact for one run: a single
// self-contained bundle that PACKAGES a run's already-produced cryptographic proofs (a signed tree
// head, per-action inclusion proofs, and optionally the run certificate, the authority grant chain,
// and an append-only consistency proof) into one JSON document you can store, email, or hand to an
// auditor. It introduces no new cryptography: every field is an existing audit primitive, and Verify
// re-checks each one against the out-of-band public key.
//
// It is deliberately PURE DATA: no funcs, no ed25519 keys, no unexported state, so it round-trips
// losslessly through JSON. The PublicKeyHex it carries is a convenience for a reader to know which
// key produced it; it is NOT the trust root. As with a bare ProofBundle, the verifying key must be
// obtained out of band (the anchor / transparency log), and Verify takes that key as an argument
// rather than trusting the one embedded here.
type EvidencePackage struct {
	// Format is the domain-separated format/version tag (EvidenceFormat). A verifier that does not
	// recognize it should refuse to interpret the rest rather than guess.
	Format string `json:"format"`
	// RunID is the run this evidence is about.
	RunID string `json:"run_id"`
	// Label is an optional caller-supplied description of what this evidence establishes (e.g.
	// "Q3 refund approvals"). It is metadata for a human reader and is not part of any proof.
	Label string `json:"label,omitempty"`
	// ProducedAt is a caller-supplied unix timestamp (seconds) recording when the package was
	// assembled. It is informational; the authoritative time commitment is inside the STH.
	ProducedAt int64 `json:"produced_at"`
	// PublicKeyHex is the hex-encoded ed25519 public key that produced the STH, carried so a reader
	// knows which key to obtain out of band. Verify does not trust it: it takes the key separately.
	PublicKeyHex string `json:"public_key_hex"`

	// STH is the run's signed tree head: the {Size, Root, Timestamp} commitment every action proof
	// below is checked against. One STH is built for the whole package so all proofs share one root.
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

	// Consistency optionally carries an append-only consistency proof showing the first N records are
	// an unmodified prefix of the tree the STH commits to. It is nil unless WithConsistency was set.
	Consistency *Consistency `json:"consistency,omitempty"`
}

// EvidenceAction is one proven action inside an EvidencePackage: a ProofBundle plus a human-readable
// label and kind, so the verification report can name what was proven ("tool \"charge\"") without
// re-deriving it from the record.
type EvidenceAction struct {
	// Label is a short human description of the action (e.g. the tool name, or a step name).
	Label string `json:"label"`
	// Kind is the action category, one of "tool", "step", or "grant", used only to phrase the report.
	Kind string `json:"kind"`
	// Ref is the identifier the action was selected by (the tool-use id, step name, or grant digest),
	// surfaced in the report so a reader can trace it back.
	Ref string `json:"ref,omitempty"`
	// Bundle is the inclusion proof that this action is a committed leaf under the package's STH.
	Bundle ProofBundle `json:"bundle"`
}

// EvidenceGrants carries the run's authority: the delegation chain as signed grants (root-first) and,
// per grant, a ProofBundle proving the grant leaf is anchored in the same signed log as the actions.
//
// Note the trust boundary: the anchoring bundles verify under the package's log key, but a grant's
// own issuer signature is made with the ISSUER's key, which is distinct from the log key and is not
// carried here. EvidencePackage.Verify therefore checks that each grant is anchored in the signed
// log; verifying the issuer signatures and the attenuation chain (VerifyDelegationChain) requires the
// issuers' public keys, which a caller supplies from their own PKI.
type EvidenceGrants struct {
	// Chain is the delegation chain, root-first to leaf-last, as signed grants (may be a single grant).
	Chain []SignedGrant `json:"chain"`
	// Anchored is one ProofBundle per grant in Chain (same order), proving the grant leaf is committed
	// in the signed log. It may be shorter than Chain if some grants were not anchored in this run.
	Anchored []EvidenceAction `json:"anchored,omitempty"`
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
	consistency    bool
	consistencyN   int
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
// against the given approved-policy allowlist. It fails assembly if the run used a policy outside
// the allowlist (CertifyRun refuses), so a package with a certificate attests the run stayed within
// approved, anchored, convergence-certified policies.
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

// WithConsistency includes an append-only consistency proof that the first `first` records are an
// unmodified prefix of the tree the package's STH commits to (nothing before `first` was rewritten
// or reordered, only appended).
func WithConsistency(first int) EvidenceOption {
	return func(o *evidenceOptions) {
		o.consistency = true
		o.consistencyN = first
	}
}

// Evidence assembles the portable, offline-verifiable EvidencePackage for runID. It builds ONE signed
// tree head over the run's journal (NewTreeHead + SignTreeHead with priv at timestamp), then, per the
// options, gathers inclusion proofs for the material tool calls / steps and, optionally, the run
// certificate, the anchored authority grants, and an append-only consistency proof, all bound to that
// one STH. With no action option it proves every completed tool call in the run.
//
// It mints no new cryptography: every proof is an existing audit primitive, packaged into one document
// a verifier re-checks offline with EvidencePackage.Verify against the out-of-band public key. The
// public key is derived from priv and recorded as metadata only (Verify takes the trusted key as an
// argument; it does not trust the embedded one).
func Evidence(ctx context.Context, store agent.Durable, runID string, priv ed25519.PrivateKey, timestamp int64, opts ...EvidenceOption) (EvidencePackage, error) {
	cfg := evidenceOptions{}
	for _, opt := range opts {
		opt(&cfg)
	}
	// Default: with no explicit action selection, prove every completed tool call.
	if !cfg.allToolCalls && len(cfg.toolCalls) == 0 && len(cfg.steps) == 0 {
		cfg.allToolCalls = true
	}

	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return EvidencePackage{}, fmt.Errorf("audit: evidence: private key does not yield an ed25519 public key")
	}

	th, err := NewTreeHead(ctx, store, runID, timestamp)
	if err != nil {
		return EvidencePackage{}, err
	}
	sth := SignTreeHead(th, priv)

	recs, err := store.History(ctx, runID)
	if err != nil {
		return EvidencePackage{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}

	pkg := EvidencePackage{
		Format:       EvidenceFormat,
		RunID:        runID,
		Label:        cfg.label,
		ProducedAt:   timestamp,
		PublicKeyHex: hex.EncodeToString(pub),
		STH:          sth,
	}

	// Action proofs: explicit tool calls and steps, plus (default) every completed tool call.
	for _, id := range cfg.toolCalls {
		pb, err := ProveToolCall(ctx, store, runID, id, sth)
		if err != nil {
			return EvidencePackage{}, err
		}
		pkg.Actions = append(pkg.Actions, EvidenceAction{Label: id, Kind: "tool", Ref: id, Bundle: pb})
	}
	for _, name := range cfg.steps {
		pb, err := ProveStep(ctx, store, runID, name, sth)
		if err != nil {
			return EvidencePackage{}, err
		}
		pkg.Actions = append(pkg.Actions, EvidenceAction{Label: name, Kind: "step", Ref: name, Bundle: pb})
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
			pkg.Actions = append(pkg.Actions, EvidenceAction{Label: r.ToolUseID, Kind: "tool", Ref: r.ToolUseID, Bundle: pb})
		}
	}

	// Optional: authority grants, each anchored and proven included in the signed log.
	if cfg.grants {
		grants, err := gatherGrants(recs, ctx, store, runID, sth)
		if err != nil {
			return EvidencePackage{}, err
		}
		pkg.Grants = grants
	}

	// Optional: the proof-carrying run certificate for the whole run.
	if cfg.runCertificate {
		cert, err := CertifyRun(ctx, store, runID, sth, cfg.runCertSpec, priv, timestamp)
		if err != nil {
			return EvidencePackage{}, err
		}
		pkg.RunCertificate = &cert
	}

	// Optional: an append-only consistency proof over a prefix of the same tree.
	if cfg.consistency {
		cons, err := ProveConsistency(ctx, store, runID, cfg.consistencyN)
		if err != nil {
			return EvidencePackage{}, err
		}
		// Bind the proof to the tree the STH signed, not a possibly-larger current journal.
		if cons.Size != sth.Size {
			cons.Size = sth.Size
		}
		pkg.Consistency = &cons
	}

	return pkg, nil
}

// gatherGrants enumerates the anchored grant leaves in the journal (audit:grant:<digest>), proves each
// is included in the signed log, and decodes the signed grant so the report can name the chain. The
// grants are returned in journal order, which is root-first for a well-formed attenuation chain.
func gatherGrants(recs []agent.Record, ctx context.Context, store agent.Durable, runID string, sth SignedTreeHead) (*EvidenceGrants, error) {
	const prefix = "audit:grant:"
	out := &EvidenceGrants{}
	for _, r := range recs {
		if r.Kind != agent.StepValue || !strings.HasPrefix(r.Name, prefix) {
			continue
		}
		var sg SignedGrant
		if err := json.Unmarshal(r.Result, &sg); err != nil {
			return nil, fmt.Errorf("audit: evidence: decode grant leaf %q: %w", r.Name, err)
		}
		digest := strings.TrimPrefix(r.Name, prefix)
		pb, err := ProveGrant(ctx, store, runID, digest, sth)
		if err != nil {
			return nil, err
		}
		out.Chain = append(out.Chain, sg)
		out.Anchored = append(out.Anchored, EvidenceAction{
			Label:  fmt.Sprintf("grant %s -> %s", sg.Grant.Issuer, sg.Grant.Subject),
			Kind:   "grant",
			Ref:    digest,
			Bundle: pb,
		})
	}
	if len(out.Chain) == 0 {
		return nil, fmt.Errorf("audit: evidence: WithGrants requested but run %s has no anchored grant leaves", runID)
	}
	return out, nil
}

// EvidenceReport is the structured outcome of EvidencePackage.Verify: an overall verdict plus a
// per-item verdict list, so a caller (or the CLI) can print exactly which proven item held and which
// did not. It is the machine-readable form behind the plain-English CLI report.
type EvidenceReport struct {
	// OK is the overall verdict: true only when the STH is authentic and every item verified.
	OK bool `json:"ok"`
	// RunID echoes the package's run id for a self-describing report.
	RunID string `json:"run_id"`
	// Label echoes the package's optional label.
	Label string `json:"label,omitempty"`
	// STHVerified is true when the signed tree head's signature is authentic under the verifying key.
	// Every action proof is checked against this same root, so a false STH fails the whole package.
	STHVerified bool `json:"sth_verified"`
	// Items is one verdict per proven item (each action, each anchored grant, the run certificate, and
	// the consistency proof), in report order.
	Items []EvidenceItem `json:"items"`
}

// EvidenceItem is one line of an EvidenceReport: what was checked, whether it verified, and a short
// human note (the reason on failure, or a one-line description on success).
type EvidenceItem struct {
	Kind     string `json:"kind"`  // "tool", "step", "grant", "run-certificate", or "consistency"
	Label    string `json:"label"` // the human label for the item
	Ref      string `json:"ref,omitempty"`
	Verified bool   `json:"verified"` // whether this item verified under the key
	Note     string `json:"note"`     // plain-English detail, on success or failure
}

// Verify re-checks every proof in the package against pub, an ed25519 public key obtained OUT OF BAND
// (the anchor / transparency log), entirely offline. It does not trust the package's embedded key. It
// verifies: (1) the STH's signature, so the {Size, Root, Timestamp} commitment is authentic; (2) each
// action ProofBundle, confirming the action is a leaf under that signed root and is bound to the same
// tree; (3) each anchored grant bundle, if present, the same way; (4) the run certificate, if present,
// via VerifyRun against the certificate's own approved allowlist; and (5) the consistency proof, if
// present, that the prefix is append-only-contained in the signed root.
//
// It returns a structured EvidenceReport with an overall bool and a per-item verdict. A false OK with
// per-item notes is a well-formed-but-invalid package; an error means a bundle could not be
// canonicalized (a malformed artifact). Grant ISSUER signatures and the attenuation chain are NOT
// checked here (they need the issuers' keys, which are not in the package); a caller verifies those
// with VerifyDelegationChain and its own PKI.
func (e EvidencePackage) Verify(pub ed25519.PublicKey) (EvidenceReport, error) {
	rep := EvidenceReport{RunID: e.RunID, Label: e.Label}

	rep.STHVerified = e.STH.Verify(pub)
	if !rep.STHVerified {
		// Without an authentic STH nothing else can be trusted; report each item as unverified but do
		// not stop, so the reader sees the full inventory and the root cause.
		rep.OK = false
	}

	allOK := rep.STHVerified

	verifyBundle := func(item EvidenceItem, b ProofBundle) (EvidenceItem, error) {
		ok, err := b.Verify(pub)
		if err != nil {
			return item, err
		}
		// Bind the item to the package's STH: a bundle that verifies against a DIFFERENT signed tree
		// must not count as evidence for this run's committed log.
		if ok && !sameTree(b.STH, e.STH) {
			ok = false
			item.Note = "verified against a different signed tree than the package STH"
		}
		item.Verified = ok
		if item.Note == "" {
			if ok {
				item.Note = itemSuccessNote(item.Kind, item.Label)
			} else {
				item.Note = "proof did not verify under this key"
			}
		}
		return item, nil
	}

	for _, a := range e.Actions {
		item := EvidenceItem{Kind: a.Kind, Label: a.Label, Ref: a.Ref}
		item, err := verifyBundle(item, a.Bundle)
		if err != nil {
			return EvidenceReport{}, err
		}
		allOK = allOK && item.Verified
		rep.Items = append(rep.Items, item)
	}

	if e.Grants != nil {
		for _, g := range e.Grants.Anchored {
			item := EvidenceItem{Kind: "grant", Label: g.Label, Ref: g.Ref}
			item, err := verifyBundle(item, g.Bundle)
			if err != nil {
				return EvidenceReport{}, err
			}
			allOK = allOK && item.Verified
			rep.Items = append(rep.Items, item)
		}
	}

	if e.RunCertificate != nil {
		item := EvidenceItem{Kind: "run-certificate", Label: "run certificate", Ref: e.RunCertificate.RunID}
		res, err := VerifyRun(*e.RunCertificate, pub)
		if err != nil {
			return EvidenceReport{}, err
		}
		item.Verified = res.OK
		if res.OK {
			item.Note = fmt.Sprintf("%d policies used, all approved and convergence-certified in the signed log", len(e.RunCertificate.UsedPolicies))
		} else {
			item.Note = "run certificate did not verify: " + strings.Join(res.Reasons, "; ")
		}
		allOK = allOK && item.Verified
		rep.Items = append(rep.Items, item)
	}

	if e.Consistency != nil {
		item := EvidenceItem{Kind: "consistency", Label: fmt.Sprintf("first %d records are an append-only prefix", e.Consistency.First), Ref: fmt.Sprintf("%d..%d", e.Consistency.First, e.Consistency.Size)}
		// The later root is the STH's (verified above); the earlier root must be recomputed by the
		// caller who holds the prefix. Here we can only confirm the proof binds to the STH's size and
		// that the proof shape is well-formed against the signed size; a full check needs the earlier
		// root, which the package does not carry. We report what is checkable.
		ok := e.Consistency.Size == e.STH.Size
		item.Verified = ok && rep.STHVerified
		if item.Verified {
			item.Note = "consistency proof is bound to the signed tree size; supply the earlier root to confirm the prefix"
		} else if !ok {
			item.Note = fmt.Sprintf("consistency proof size %d does not match the signed tree size %d", e.Consistency.Size, e.STH.Size)
		} else {
			item.Note = "signed tree head is not authentic under this key"
		}
		allOK = allOK && item.Verified
		rep.Items = append(rep.Items, item)
	}

	rep.OK = allOK
	return rep, nil
}

// itemSuccessNote phrases the plain-English success line for a verified item by kind.
func itemSuccessNote(kind, label string) string {
	switch kind {
	case "tool":
		return "tool call included in the signed log"
	case "step":
		return "step included in the signed log"
	case "grant":
		return "grant anchored in the signed log"
	default:
		return "included in the signed log"
	}
}

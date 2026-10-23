package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// The taint policy and the under-taint status of a run (docs/design/untrusted-content-and-amount-
// caps.md, sections 5.3, 5.5 and 5.7, as amended by #489). A run is under taint if it started under a
// policy: its run:start records the policy's declarations digest. A run that started with no policy is
// never gated or refused for its marks, whatever policy a later process has. Under-taint status is sticky
// down the tree and along a session: a child run or session turn is under taint when its parent or its
// session is, even when it is first driven by a process with no policy, and such a process refuses its
// side-effecting calls rather than run them ungated.
//
// This file is PR 9a of the build: the policy value, the digest, the run:start record, the inheritance and
// the no-policy refusal. The policy gates nothing yet (the gates are PR 9b), and it has no exported
// option, so no host can reach a half-built control.

// taintPolicy is what an agent configured to enforce taint carries.
type taintPolicy struct {
	// Gate is the escalation gate a side-effecting call of a tainted turn needs, an ordinary approval
	// policy. nil only when Refuse is set.
	Gate *ApprovalPolicy
	// Refuse says a tainted turn's side-effecting calls are not run and not asked.
	Refuse bool
	// RequireProvenance makes a tool without a declared provenance a registration error and a run
	// without a stated input provenance an error at its first drive.
	RequireProvenance bool
}

// check refuses a policy that gates nothing, or gates with an invalid policy.
func (p taintPolicy) check() error {
	switch {
	case p.Gate == nil && !p.Refuse:
		return fmt.Errorf("agent: a taint policy needs an escalation Gate or Refuse: %w", ErrConfig)
	case p.Gate != nil && p.Refuse:
		return fmt.Errorf("agent: a taint policy has a Gate and Refuse, which are alternatives: %w", ErrConfig)
	case p.Gate != nil:
		if err := checkApproval(p.Gate); err != nil {
			return fmt.Errorf("agent: the taint policy's Gate: %w", err)
		}
	}
	return nil
}

// withTaintPolicy configures the agent to enforce p on the runs it starts. It is unexported until the
// gates exist (PR 9b), then exported as WithTaintPolicy.
func withTaintPolicy(p taintPolicy) Option {
	return agentOption(func(c *agentConfig) error {
		if err := p.check(); err != nil {
			return err
		}
		p.Gate = p.Gate.Clone()
		c.a.taint = &p
		return nil
	})
}

// taintDeclarations is what the declarations digest commits to: the policy and every registered tool's
// provenance and exemption, so a change to any of them is a different digest in the runs that follow.
type taintDeclarations struct {
	Format            string                   `json:"format"`
	Gate              *ApprovalPolicy          `json:"gate,omitempty"`
	Refuse            bool                     `json:"refuse,omitempty"`
	RequireProvenance bool                     `json:"require_provenance,omitempty"`
	Tools             map[string]toolTaintDecl `json:"tools,omitempty"`
}

type toolTaintDecl struct {
	Untrusted string `json:"untrusted,omitempty"`
	Exempt    string `json:"exempt,omitempty"`
}

const taintDeclarationsFormat = "bide.taint-declarations.v1"

// taintDigest is the SHA-256, in hex, of the declarations in force: the agent's policy and its tools'
// provenance and exemptions. "" for an agent with no policy. Equal declarations give equal digests;
// maps are encoded with sorted keys, so the digest is stable.
func (a *Agent) taintDigest() string {
	if a.taint == nil {
		return ""
	}
	d := taintDeclarations{Format: taintDeclarationsFormat, Gate: a.taint.Gate, Refuse: a.taint.Refuse, RequireProvenance: a.taint.RequireProvenance}
	for _, name := range slices.Sorted(maps.Keys(a.specs)) {
		s := a.specs[name]
		if src := s.untrustedSource(); src != "" || s.TaintExempt != "" {
			if d.Tools == nil {
				d.Tools = map[string]toolTaintDecl{}
			}
			d.Tools[name] = toolTaintDecl{Untrusted: src, Exempt: s.TaintExempt}
		}
	}
	b, err := json.Marshal(d)
	if err != nil { // a policy and strings always marshal
		panic(fmt.Sprintf("agent: encode the taint declarations: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// extTaintDigest is the key of run:start's ext member that holds the digest of the declarations a run
// started under. Its presence is what puts the run under taint.
const extTaintDigest = "taint_policy_digest"

// taintDigest is the digest run:start records, "" when the run started with no policy (and was not
// started under one).
func (s RunStart) taintDigest() string {
	raw, ok := s.Ext[extTaintDigest]
	if !ok {
		return ""
	}
	var d string
	if json.Unmarshal(raw, &d) != nil {
		return ""
	}
	return d
}

// withTaintDigest returns s recording digest d (a new Ext map: a RunStart copy shares its Ext).
func (s RunStart) withTaintDigest(d string) RunStart {
	if d == "" {
		return s
	}
	ext := maps.Clone(s.Ext)
	if ext == nil {
		ext = map[string]json.RawMessage{}
	}
	b, _ := json.Marshal(d)
	ext[extTaintDigest] = b
	s.Ext = ext
	return s
}

type underTaintKey struct{}

// withUnderTaint marks ctx as the context of a run under taint, started under the declarations with
// digest d: the runs a drive starts (a sub-agent's) inherit the status, and a call that reaches an agent
// with no policy is refused.
func withUnderTaint(ctx context.Context, d string) context.Context {
	if d == "" {
		return ctx
	}
	return context.WithValue(ctx, underTaintKey{}, d)
}

// underTaintFrom is the digest ctx carries, "" for a run that is not under taint.
func underTaintFrom(ctx context.Context) string {
	d, _ := ctx.Value(underTaintKey{}).(string)
	return d
}

// startTaintDigest is the digest a run being started under ctx records: the status it inherits (its
// parent's, its session's) first, else its own agent's policy's digest, else "" (not under taint).
func (a *Agent) startTaintDigest(ctx context.Context) string {
	if d := underTaintFrom(ctx); d != "" {
		return d
	}
	return a.taintDigest()
}

// errTaintUnenforceable is the refusal of a side-effecting call of a run under taint by an agent that has
// no taint policy and so cannot enforce it.
func errTaintUnenforceable(tool, digest string) error {
	return fmt.Errorf("agent: tool %q is a side effect of a run under taint (it started under declarations %.12s), and this agent has no taint policy to enforce it; drive the run with an agent configured as the one that started it: %w", tool, digest, ErrConfig)
}

// taintRefusal is non-nil when the call of the tool with spec, made under ctx, must not run because the
// run is under taint and this agent cannot enforce it. A read-only call, and a compensation (a saga's
// rollback, which is not gated by taint: docs/design/untrusted-content-and-amount-caps.md, section 5.5)
// are not refused.
func (a *Agent) taintRefusal(ctx context.Context, spec *ToolSpec) error {
	d := underTaintFrom(ctx)
	if d == "" || a.taint != nil || !tracked(spec.Safety) || isCompensating(ctx) {
		return nil
	}
	return errTaintUnenforceable(spec.Name, d)
}

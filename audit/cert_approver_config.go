package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Validate checks the status parameters when a gate is configured, so a bad value fails at startup and
// not at the first decision. Zero durations select the documented defaults, so the zero value is valid.
// Every failure wraps agent.ErrConfig.
func (p StatusParams) Validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("audit: StatusParams: %s: %w", fmt.Sprintf(format, args...), agent.ErrConfig)
	}
	for name, d := range map[string]time.Duration{"W": p.W, "MinW": p.MinW, "MaxStatusAge": p.MaxStatusAge,
		"RoutineGrace": p.RoutineGrace, "CAPeriod": p.CAPeriod, "CoverageTolerance": p.CoverageTolerance} {
		if d < 0 {
			return bad("%s is negative (%v)", name, d)
		}
	}
	w, minW := p.W, p.MinW
	if w == 0 {
		w = defaultW
	}
	if minW == 0 {
		minW = defaultMinW
	}
	if minW > w {
		return bad("MinW (%v) is above W (%v), so no decision could be final", minW, w)
	}
	for _, k := range slices.SortedFunc(maps.Keys(p.CAPeriodFor), func(a, b SPKIHash) int { return slices.Compare(a[:], b[:]) }) {
		if d := p.CAPeriodFor[k]; d <= 0 {
			return bad("CAPeriodFor an issuer is %v; a declared period is positive", d)
		}
	}
	if p.ThisUpdateSkew < 0 || p.ThisUpdateSkew > maxThisUpdateSkew {
		return bad("ThisUpdateSkew %v is outside 0 to %v", p.ThisUpdateSkew, maxThisUpdateSkew)
	}
	for _, k := range slices.SortedFunc(maps.Keys(p.ThisUpdateSkewFor), func(a, b SPKIHash) int { return slices.Compare(a[:], b[:]) }) {
		if d := p.ThisUpdateSkewFor[k]; d < 0 || d > maxThisUpdateSkew {
			return bad("ThisUpdateSkewFor an issuer is %v, outside 0 to %v", d, maxThisUpdateSkew)
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(p.MaxStatusAgeFor), func(a, b SPKIHash) int { return slices.Compare(a[:], b[:]) }) {
		if d := p.MaxStatusAgeFor[k]; d <= 0 || d > maxStatusAgeFor {
			return bad("MaxStatusAgeFor an issuer is %v, outside 0 to %v", d, maxStatusAgeFor)
		}
	}
	if p.CountLive && p.MaxStatusAge <= 0 {
		return bad("CountLive needs a positive MaxStatusAge: a freshness check without one accepts no status")
	}
	return nil
}

// ValidateConfig checks the verifier's configuration when the gate is configured (agent calls it
// from the approver seat check, at build and on every evaluation): the policy and approver id, the
// status parameters (StatusParams.Validate), and the rule that the pre-execution status check cannot
// be fresher than the CA publishes: every CA period, the default and each per-issuer one, is at most
// half of ExecStatusAge (for the leaf's issuing CA; the per-CA tiers follow with the execution-time check), so the newest CRL available at execution is always fresh enough. A bad value
// fails here, wrapping agent.ErrConfig, not at the first decision.
func (c *CertApprover) ValidateConfig() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("audit: CertApprover %q: %s: %w", c.ApproverID, fmt.Sprintf(format, args...), agent.ErrConfig)
	}
	if c.Policy == nil {
		return bad("no certificate policy")
	}
	if c.ApproverID == "" {
		return bad("no approver id")
	}
	if _, ok := c.Policy.Identity(c.ApproverID); !ok {
		return bad("the certificate policy's identity map does not name this approver")
	}
	if c.ClockTolerance < 0 {
		return bad("ClockTolerance is negative (%v)", c.ClockTolerance)
	}
	if err := c.StatusParams.Validate(); err != nil {
		return bad("%v", err)
	}
	if c.StatusArchive == nil && c.Status != nil && c.Status.maxBytes > agent.MaxStatusInline {
		return bad("the status snapshot accepts CRLs up to %d bytes, above the %d the journal stores, and there is no StatusArchive: a CRL that grew past the limit would stop the gate counting; set StatusArchive, or build the snapshot with WithLimits(%d, ...) so an oversize CRL is refused at intake", c.Status.maxBytes, agent.MaxStatusInline, agent.MaxStatusInline)
	}
	if c.PolicyEffectiveFrom.IsZero() {
		return bad("PolicyEffectiveFrom is not set: the identity record keeps the date the policy took effect")
	}
	if c.ExecStatusAge <= 0 {
		return bad("ExecStatusAge is not set: the status check before execution needs a positive age")
	}
	if p := c.StatusParams.CAPeriod; p > c.ExecStatusAge/2 {
		return bad("CAPeriod (%v) is above ExecStatusAge/2 (%v): the status check before execution could not be satisfied between two CRLs", p, c.ExecStatusAge/2)
	}
	for _, k := range slices.SortedFunc(maps.Keys(c.StatusParams.CAPeriodFor), func(a, b SPKIHash) int { return slices.Compare(a[:], b[:]) }) {
		if p := c.StatusParams.CAPeriodFor[k]; p > c.ExecStatusAge/2 {
			return bad("CAPeriodFor %x (%v) is above ExecStatusAge/2 (%v)", k[:4], p, c.ExecStatusAge/2)
		}
	}
	return nil
}

// DescribeIdentity implements agent.IdentityDescriber: the certificate policy and status values in
// force, every one that reaches a verdict, as the identity record keeps them (defaults filled in).
// Anchors and intermediates are named by the SHA-256 of their SubjectPublicKeyInfo; durations are
// time.Duration.String and times RFC 3339 with nanoseconds. TestIdentityPolicyRecordCoversEveryField
// fails when a field is added to IdentityPolicy, StatusParams or CertApprover and not to the record.
func (c *CertApprover) DescribeIdentity() agent.IdentityPolicyRecord {
	d := agent.IdentityPolicyRecord{EffectiveFrom: c.PolicyEffectiveFrom.UTC().Format(time.RFC3339Nano)}
	p := c.StatusParams
	w, minW, tol := p.W, p.MinW, p.CoverageTolerance
	if w <= 0 {
		w = defaultW
	}
	if minW <= 0 {
		minW = defaultMinW
	}
	if tol <= 0 {
		tol = defaultCoverageTolerance
	}
	ct := c.ClockTolerance
	if ct <= 0 {
		ct = time.Minute
	}
	d.W, d.MinW, d.MaxStatusAge, d.ExecStatusAge, d.CAPeriod = w.String(), minW.String(), p.MaxStatusAge.String(), c.ExecStatusAge.String(), p.CAPeriod.String()
	d.CoverageTolerance, d.ClockTolerance, d.RoutineGrace = tol.String(), ct.String(), p.RoutineGrace.String()
	d.ConsecutiveNumbers, d.CountLive, d.Retained, d.AllowHolds = p.ConsecutiveNumbers, p.CountLive, p.Retained, p.AllowHolds
	d.ThisUpdateSkew = p.ThisUpdateSkew.String()
	if len(p.ThisUpdateSkewFor) > 0 {
		d.ThisUpdateSkewFor = map[string]string{}
		for k, v := range p.ThisUpdateSkewFor {
			d.ThisUpdateSkewFor[hex.EncodeToString(k[:])] = v.String()
		}
	}
	if len(p.MaxStatusAgeFor) > 0 {
		d.MaxStatusAgeFor = map[string]string{}
		for k, v := range p.MaxStatusAgeFor {
			d.MaxStatusAgeFor[hex.EncodeToString(k[:])] = v.String()
		}
	}
	if len(p.CAPeriodFor) > 0 {
		d.CAPeriodFor = map[string]string{}
		for k, v := range p.CAPeriodFor {
			d.CAPeriodFor[hex.EncodeToString(k[:])] = v.String()
		}
	}
	d.Anchors, d.Intermediates = []string{}, []string{}
	if ci := c.Policy; ci != nil {
		for k := range ci.anchorBy {
			d.Anchors = append(d.Anchors, hex.EncodeToString(k[:]))
		}
		for k := range ci.inter {
			d.Intermediates = append(d.Intermediates, hex.EncodeToString(k[:]))
		}
		slices.Sort(d.Anchors)
		slices.Sort(d.Intermediates)
		d.EKU, d.IDKind = ci.eku.String(), string(ci.kind)
		d.PolicyOID = ci.policy.String()
		d.Canonical = fmt.Sprintf("%s v%d", ci.canon.Name(), ci.canon.Version())
		d.Skew, d.AnchorGrace, d.MaxLeafLifetime, d.MaxChain = ci.skew.String(), ci.grace.String(), ci.maxLife.String(), ci.maxChain
		d.NotBeforeBackdate = ci.backdate.String()
		d.AllowEmailProtectionEKU, d.RequireNameConstraints, d.RequirePolicyOnCAs = ci.emailEKU, ci.needNC, ci.needPol
		if !ci.rsa2048.IsZero() {
			d.RSA2048Until = ci.rsa2048.UTC().Format(time.RFC3339Nano)
		}
		dist := map[string]string{}
		for k, t := range ci.distrust {
			dist[hex.EncodeToString(k[:])] = t.UTC().Format(time.RFC3339Nano)
		}
		for k, t := range p.Distrust {
			dist[hex.EncodeToString(k[:])] = t.UTC().Format(time.RFC3339Nano)
		}
		if len(dist) > 0 {
			d.Distrust = dist
		}
	}
	return d
}

// PolicyDigest implements agent.IdentityDescriber: the SHA-256 (lowercase hex) of the JSON of
// DescribeIdentity, written with encoding/json without HTML escaping, fields in declaration order and
// map keys sorted, which the pause note and the identity record both carry.
func (c *CertApprover) PolicyDigest() string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(c.DescribeIdentity())
	sum := sha256.Sum256(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:])
}

// ArchiveStatus implements agent.StatusArchiver with the verifier's StatusArchive, if any.
func (c *CertApprover) ArchiveStatus(ctx context.Context, hash string, der []byte) error {
	if c.StatusArchive == nil {
		return fmt.Errorf("audit: CertApprover %q: no status archive configured: %w", c.ApproverID, agent.ErrConfig)
	}
	return c.StatusArchive.PutStatus(ctx, hash, der)
}

// StatusArchive stores the bytes of a CRL too large for the journal (agent.MaxStatusInline) under its
// SHA-256 (lowercase hex), durably, before the gate records its hash. Put must be idempotent.
type StatusArchive interface {
	PutStatus(ctx context.Context, hash string, der []byte) error
}

// VerifierInfo implements agent.IdentityDescriber: the build's vcs.revision, if the binary has one, the
// x509 GODEBUG settings the operator declares (CertApprover.GoDebug), and the build's baked-in
// DefaultGODEBUG (the toolchain's go line fixes many x509 behaviours), read from the build info and not
// from the environment.
func (c *CertApprover) VerifierInfo() (revision, goDebug, buildGoDebug string) {
	var settings []debug.BuildSetting
	if bi, ok := debug.ReadBuildInfo(); ok {
		settings = bi.Settings
	}
	revision, buildGoDebug = buildInfoValues(settings)
	return revision, c.GoDebug, buildGoDebug
}

// buildInfoValues picks vcs.revision and DefaultGODEBUG out of a build's settings.
func buildInfoValues(settings []debug.BuildSetting) (revision, defaultGoDebug string) {
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "DefaultGODEBUG":
			defaultGoDebug = s.Value
		}
	}
	return revision, defaultGoDebug
}

// x509Settings returns the x509 entries of a GODEBUG string, sorted.
func x509Settings(godebug string) []string {
	var out []string
	for _, kv := range strings.Split(godebug, ",") {
		if kv = strings.TrimSpace(kv); strings.HasPrefix(kv, "x509") {
			out = append(out, kv)
		}
	}
	slices.Sort(out)
	return out
}

// CheckDeclaredGoDebug compares the x509 GODEBUG settings an operator declares (CertApprover.GoDebug)
// with the process's real environment value, which the caller reads (os.Getenv("GODEBUG"): the engine
// itself reads no environment), and returns an error wrapping agent.ErrConfig when they differ. An
// application with a certificate-bound approver calls it at startup, and generates the declaration from
// the real settings, so the identity record's declared settings are the ones in force. The check is of
// the value at startup: a later os.Setenv("GODEBUG") is not caught, so do not change it after startup.
func CheckDeclaredGoDebug(declared, environment string) error {
	d, e := x509Settings(declared), x509Settings(environment)
	if !slices.Equal(d, e) {
		return fmt.Errorf("audit: the declared x509 GODEBUG settings %q differ from the process's %q: declare what the process runs under: %w", strings.Join(d, ","), strings.Join(e, ","), agent.ErrConfig)
	}
	return nil
}

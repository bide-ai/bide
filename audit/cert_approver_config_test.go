package audit_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// A bad status value fails when the gate is configured, not at the first decision: StatusParams.Validate,
// CertApprover.ValidateConfig (which adds the CAPeriod <= ExecStatusAge/2 rule), and the gate's
// startup check that calls it (approverSeats, so ValidateKeys and every evaluation).

func TestStatusParams_Validate(t *testing.T) {
	ok := audit.StatusParams{W: time.Hour, MinW: time.Minute, MaxStatusAge: time.Hour, CAPeriod: time.Hour}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a good value: %v", err)
	}
	if err := (audit.StatusParams{}).Validate(); err != nil {
		t.Fatalf("zero value (all defaults): %v", err)
	}
	for name, p := range map[string]audit.StatusParams{
		"negative W":              {W: -time.Second},
		"negative MinW":           {MinW: -time.Second},
		"MinW above W":            {W: time.Hour, MinW: 2 * time.Hour},
		"MinW above default W":    {MinW: 48 * time.Hour},
		"negative MaxStatusAge":   {MaxStatusAge: -time.Second},
		"negative RoutineGrace":   {RoutineGrace: -time.Second},
		"negative CAPeriod":       {CAPeriod: -time.Second},
		"negative CAPeriodFor":    {CAPeriodFor: map[audit.SPKIHash]time.Duration{{1}: -time.Second}},
		"zero CAPeriodFor":        {CAPeriodFor: map[audit.SPKIHash]time.Duration{{1}: 0}},
		"negative tolerance":      {CoverageTolerance: -time.Second},
		"negative skew":           {ThisUpdateSkew: -time.Second},
		"skew over 30 minutes":    {ThisUpdateSkew: 31 * time.Minute},
		"issuer skew over 30 min": {ThisUpdateSkewFor: map[audit.SPKIHash]time.Duration{{1}: time.Hour}},
		"CountLive without a max": {CountLive: true},
	} {
		err := p.Validate()
		if err == nil || !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestCertApprover_ValidateConfig(t *testing.T) {
	cg := newCertGate(t, 1, "alice")
	mk := func(mod func(*audit.CertApprover)) *audit.CertApprover {
		c := &audit.CertApprover{ApproverID: "alice", Policy: cg.ci, StatusParams: audit.StatusParams{CAPeriod: time.Hour, MaxStatusAge: time.Hour},
			ExecStatusAge: 2 * time.Hour, PolicyEffectiveFrom: testDecidedAt}
		mod(c)
		return c
	}
	if err := mk(func(*audit.CertApprover) {}).ValidateConfig(); err != nil {
		t.Fatalf("CAPeriod = ExecStatusAge/2: %v", err)
	}
	// The pre-execution status check cannot be fresher than the CA publishes.
	err := mk(func(c *audit.CertApprover) { c.ExecStatusAge = 2*time.Hour - time.Second }).ValidateConfig()
	if err == nil || !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "ExecStatusAge") {
		t.Fatalf("CAPeriod above ExecStatusAge/2: %v", err)
	}
	err = mk(func(c *audit.CertApprover) {
		c.StatusParams.CAPeriodFor = map[audit.SPKIHash]time.Duration{audit.SPKIHashOf(cg.inter.Cert): 4 * time.Hour}
	}).ValidateConfig()
	if err == nil || !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("a per-issuer CAPeriod above ExecStatusAge/2: %v", err)
	}
	for name, c := range map[string]*audit.CertApprover{
		"no policy":          mk(func(c *audit.CertApprover) { c.Policy = nil }),
		"no approver id":     mk(func(c *audit.CertApprover) { c.ApproverID = "" }),
		"unknown approver":   mk(func(c *audit.CertApprover) { c.ApproverID = "mallory" }),
		"no effective date":  mk(func(c *audit.CertApprover) { c.PolicyEffectiveFrom = time.Time{} }),
		"no ExecStatusAge":   mk(func(c *audit.CertApprover) { c.ExecStatusAge = 0 }),
		"bad StatusParams":   mk(func(c *audit.CertApprover) { c.StatusParams.W = -1 }),
		"negative tolerance": mk(func(c *audit.CertApprover) { c.ClockTolerance = -time.Second }),
	} {
		if err := c.ValidateConfig(); err == nil || !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// The gate calls the check on every evaluation and at build: a verifier whose configuration is bad
// stops the policy from validating, naming the approver.
func TestApprovalPolicy_ValidateKeysRunsTheVerifiersConfigCheck(t *testing.T) {
	cg := newCertGate(t, 1, "alice")
	bad := &audit.CertApprover{ApproverID: "alice", Policy: cg.ci, StatusParams: audit.StatusParams{CAPeriod: time.Hour}, ExecStatusAge: time.Hour, PolicyEffectiveFrom: testDecidedAt}
	pol := agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice"}}
	vf := func(id string) (agent.ApproverVerifier, bool) { return bad, id == "alice" }
	err := pol.ValidateKeys(vf)
	if err == nil || !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "alice") {
		t.Fatalf("ValidateKeys: %v", err)
	}
	bad.ExecStatusAge = 2 * time.Hour
	if err := pol.ValidateKeys(vf); err != nil {
		t.Fatalf("a good configuration: %v", err)
	}
}

// A counted decision's finding carries what the gate journals in ApprovalIdentity: the hashes of the
// accepted chain, the anchor, the size of the covering head, and the CRLs the status rests on (each
// once, byte for byte, so their hashes are the names the gate stores them under).
func TestCertApprover_FindingCarriesTheIdentityEvidence(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	c.pause()
	c.decideCert("alice", nil)
	h := c.anchor(certA)
	_, checks := c.tally()
	cc := checks[0].Cert
	if cc == nil || !checks[0].Counted {
		t.Fatalf("%+v", checks)
	}
	leaf := c.leaves["alice"]
	if len(cc.Chain) != 3 || cc.Chain[0] != hex.EncodeToString(sha256Of(leaf.DER)) {
		t.Fatalf("chain %v", cc.Chain)
	}
	if want := audit.SPKIHashOf(c.root.Cert); cc.Anchor != hex.EncodeToString(want[:]) {
		t.Fatalf("anchor %s", cc.Anchor)
	}
	if cc.HeadSize != h.Size {
		t.Fatalf("head size %d, want %d", cc.HeadSize, h.Size)
	}
	if hh := h.Hash(); cc.HeadHash != hex.EncodeToString(hh[:]) || cc.HeadRoot != hex.EncodeToString(h.Root) {
		t.Fatalf("head root/hash %s %s", cc.HeadRoot, cc.HeadHash)
	}
	if len(cc.Evidence) == 0 {
		t.Fatal("no status evidence")
	}
	seen := map[string]bool{}
	for _, ev := range cc.Evidence {
		sum := hex.EncodeToString(sha256Of(ev.DER))
		if ev.Kind != "crl" || ev.Format != agent.ApprovalStatusFormat || ev.ThisUpdate == "" || ev.Issuer == "" || seen[sum] {
			t.Fatalf("evidence %+v (duplicate %v)", ev, seen[sum])
		}
		seen[sum] = true
		if _, err := x509.ParseRevocationList(ev.DER); err != nil {
			t.Fatalf("evidence is not the CRL: %v", err)
		}
	}
}

func sha256Of(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

// Anchoring is not a free signing oracle: "no covering head" (which makes the gate anchor) is reported
// only for a decision whose chain, identifier, scheme and signature already verify, taking the signed
// time as the provable time. A junk decision gets its own reason and never causes a head to be signed.
func TestCertApprover_NoCoveringHeadOnlyForADecisionThatVerifies(t *testing.T) {
	good := newCertGate(t, 1, "alice")
	good.pause()
	good.decideCert("alice", nil)
	if _, checks := good.tally(); reasonOf(t, checks, "alice") != agent.ReasonNoCoveringHead {
		t.Fatalf("a valid decision, no head: %+v", checks)
	}
	for name, mut := range map[string]func(*testing.T, *certGate) func(*agent.Decision){
		"a foreign chain": func(t *testing.T, c *certGate) func(*agent.Decision) {
			other := testca.NewRoot(t, "other", testca.Opts{})
			l := other.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(x *x509.Certificate) {
				x.NotBefore, x.NotAfter = testDecidedAt.Add(-time.Hour), testDecidedAt.Add(time.Hour)
			}})
			return func(d *agent.Decision) {
				d.Cert = l.Chain()
				d.Signature = ed25519.Sign(l.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(c.subject(t), "alice", true, testDecidedAt, agent.CertLeafHash(l.DER)))
			}
		},
		"a bad signature": func(t *testing.T, c *certGate) func(*agent.Decision) {
			return func(d *agent.Decision) { d.Signature = append([]byte{}, d.Signature...); d.Signature[0] ^= 0xff }
		},
		"a v3 signature": func(t *testing.T, c *certGate) func(*agent.Decision) {
			l := c.leaves["alice"]
			return func(d *agent.Decision) {
				d.Signature = ed25519.Sign(l.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytes(c.subject(t), "alice", true, testDecidedAt))
			}
		},
	} {
		c := newCertGate(t, 1, "alice")
		c.pause()
		c.decideCert("alice", mut(t, c))
		_, checks := c.tally()
		if r := reasonOf(t, checks, "alice"); r == agent.ReasonNoCoveringHead || r == "" {
			t.Fatalf("%s: reason %q would make the gate anchor for a junk decision", name, r)
		}
	}
}

// The verifier describes the policy in force for the identity record: anchors, pinned intermediates,
// the usage and policy OID, the canonicalizer, the status values and the effective date; the digest
// of the description changes with each of them, and the roster digest with the identity map.
func TestCertApprover_DescribesThePolicyInForce(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	a := c.approver("alice")
	a.PolicyEffectiveFrom = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	a.StatusParams.Distrust = map[audit.SPKIHash]time.Time{audit.SPKIHashOf(c.inter.Cert): time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	d := a.DescribeIdentity()
	rootH, interH := audit.SPKIHashOf(c.root.Cert), audit.SPKIHashOf(c.inter.Cert)
	if d.EffectiveFrom != "2026-10-01T00:00:00Z" || len(d.Anchors) != 1 || d.Anchors[0] != hex.EncodeToString(rootH[:]) ||
		len(d.Intermediates) != 1 || d.Intermediates[0] != hex.EncodeToString(interH[:]) ||
		d.EKU != testca.EKU.String() || d.PolicyOID != testca.PolicyOID.String() || d.IDKind != "person-uri" ||
		!strings.HasPrefix(d.Canonical, "") || d.Canonical == "" || d.MaxStatusAge == "" || d.ExecStatusAge != "2h0m0s" || d.CAPeriod != "1h0m0s" ||
		d.Skew == "" || d.AnchorGrace == "" || d.W != "5s" || d.MinW != "1s" || d.Distrust[hex.EncodeToString(interH[:])] != "2027-01-01T00:00:00Z" || d.MaxChain != 4 || d.CoverageTolerance == "" || d.ClockTolerance != "1m0s" {
		t.Fatalf("%+v", d)
	}
	dig := a.PolicyDigest()
	if len(dig) != 64 || a.PolicyDigest() != dig {
		t.Fatalf("digest %q", dig)
	}
	changed := map[string]func(*audit.CertApprover){
		"effective date": func(x *audit.CertApprover) { x.PolicyEffectiveFrom = x.PolicyEffectiveFrom.Add(time.Second) },
		"ExecStatusAge":  func(x *audit.CertApprover) { x.ExecStatusAge += time.Second },
		"W":              func(x *audit.CertApprover) { x.StatusParams.W += time.Second },
		"CAPeriod":       func(x *audit.CertApprover) { x.StatusParams.CAPeriod += time.Second },
		"MaxStatusAge":   func(x *audit.CertApprover) { x.StatusParams.MaxStatusAge += time.Second },
		"distrust":       func(x *audit.CertApprover) { x.StatusParams.Distrust = nil },
	}
	for name, mod := range changed {
		x := *a
		mod(&x)
		if x.PolicyDigest() == dig {
			t.Fatalf("the digest does not cover %s", name)
		}
	}
	// The roster digest covers the whole identity map (every approver id, bare-key ones included).
	r := a.RosterDigest()
	if len(r) != 64 || r != c.approver("bob").RosterDigest() {
		t.Fatalf("roster digest %q is per approver, it is per gate", r)
	}
	ci2, err := audit.IdentityPolicy{
		Anchors: []*x509.Certificate{c.root.Cert}, Inter: []audit.SPKIHash{interH},
		Identity: map[string]string{"alice": testca.Person(1), "bob": testca.Person(2)},
		IDKind:   audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
	}.Compile()
	if err != nil {
		t.Fatal(err)
	}
	b := *a
	b.Policy = ci2
	if b.RosterDigest() == r {
		t.Fatal("the roster digest does not cover the identity map")
	}
}

// The policy record commits to every parameter that reaches a verdict. A field added to
// IdentityPolicy, StatusParams or CertApprover must be recorded (IdentityPolicyRecord and
// CertApprover.DescribeIdentity) or listed here with the reason it is not part of a verdict.
func TestIdentityPolicyRecordCoversEveryField(t *testing.T) {
	recorded := map[string]string{ // field -> the record member that holds it
		"IdentityPolicy.Anchors": "Anchors", "IdentityPolicy.Inter": "Intermediates", "IdentityPolicy.Distrust": "Distrust",
		"IdentityPolicy.IDKind": "IDKind", "IdentityPolicy.EKU": "EKU", "IdentityPolicy.AllowEmailProtectionEKU": "AllowEmailProtectionEKU",
		"IdentityPolicy.Policy": "PolicyOID", "IdentityPolicy.Canonical": "Canonical", "IdentityPolicy.MaxChain": "MaxChain",
		"IdentityPolicy.Skew": "Skew", "IdentityPolicy.AnchorGrace": "AnchorGrace", "IdentityPolicy.RSA2048Until": "RSA2048Until",
		"IdentityPolicy.MaxLeafLifetime": "MaxLeafLifetime", "IdentityPolicy.RequireNameConstraints": "RequireNameConstraints",
		"IdentityPolicy.RequirePolicyOnCAs": "RequirePolicyOnCAs",
		"StatusParams.W":                    "W", "StatusParams.MinW": "MinW", "StatusParams.RoutineGrace": "RoutineGrace", "StatusParams.MaxStatusAge": "MaxStatusAge",
		"StatusParams.CountLive": "CountLive", "StatusParams.Distrust": "Distrust", "StatusParams.Retained": "Retained", "StatusParams.CAPeriod": "CAPeriod",
		"StatusParams.CAPeriodFor": "CAPeriodFor", "StatusParams.ConsecutiveNumbers": "ConsecutiveNumbers", "StatusParams.CoverageTolerance": "CoverageTolerance",
		"StatusParams.ThisUpdateSkew": "ThisUpdateSkew", "StatusParams.ThisUpdateSkewFor": "ThisUpdateSkewFor", "StatusParams.AllowHolds": "AllowHolds",
		"CertApprover.ClockTolerance": "ClockTolerance", "CertApprover.ExecStatusAge": "ExecStatusAge", "CertApprover.PolicyEffectiveFrom": "EffectiveFrom",
	}
	notVerdict := map[string]string{
		"IdentityPolicy.Identity":        "the roster, committed by RosterDigest",
		"StatusParams.Now":               "the evaluation clock, recorded as ApprovalIdentity.EvaluatedAt",
		"CertApprover.ApproverID":        "per approver, in the decision list",
		"CertApprover.Policy":            "the compiled IdentityPolicy",
		"CertApprover.Heads":             "a source, not a parameter: the covering head is recorded",
		"CertApprover.LogKey":            "a key, not a parameter",
		"CertApprover.Status":            "the CRLs, recorded by hash",
		"CertApprover.StatusParams":      "recorded field by field",
		"CertApprover.Now":               "the evaluation clock",
		"CertApprover.StatusArchive":     "where large CRLs go, not a verdict parameter",
		"CertApprover.GoDebug":           "recorded in ApprovalIdentity.GoDebug, not a policy parameter",
		"CertApprover.RequesterIdentity": "a function mapping requesters to identifiers; its default is the roster, committed by RosterDigest",
	}
	rec := reflect.TypeOf(agent.IdentityPolicyRecord{})
	for _, typ := range []reflect.Type{reflect.TypeOf(audit.IdentityPolicy{}), reflect.TypeOf(audit.StatusParams{}), reflect.TypeOf(audit.CertApprover{})} {
		for i := range typ.NumField() {
			key := typ.Name() + "." + typ.Field(i).Name
			if why, ok := notVerdict[key]; ok {
				if why == "" {
					t.Errorf("%s is listed as not reaching a verdict without a reason", key)
				}
				continue
			}
			member, ok := recorded[key]
			if !ok {
				t.Errorf("%s is neither recorded in agent.IdentityPolicyRecord nor listed as not reaching a verdict: add it to DescribeIdentity and the record", key)
				continue
			}
			if _, ok := rec.FieldByName(member); !ok {
				t.Errorf("%s is said to be recorded as %s, which IdentityPolicyRecord does not have", key, member)
			}
		}
	}
}

// The policy digest is over the exact JSON bytes, pinned: it does not move between versions unless the
// record's content or encoding does (an HTML character in a value is not escaped), and the roster
// digest is length prefixed, so entries that differ only in where '=' falls do not collide.
func TestIdentityDigestsArePinned(t *testing.T) {
	r := agent.IdentityPolicyRecord{EffectiveFrom: "2026-10-01T00:00:00Z", Anchors: []string{"aa"}, Intermediates: []string{}, EKU: "1.2.3", IDKind: "person-uri", Canonical: "default v1<&>"}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(r)
	if !strings.Contains(b.String(), "v1<&>") {
		t.Fatal("HTML escaping on")
	}
	mk := func(identity map[string]string) string {
		cg := newCertGate(t, 1, "alice")
		ci, err := audit.IdentityPolicy{Anchors: []*x509.Certificate{cg.root.Cert}, Inter: []audit.SPKIHash{audit.SPKIHashOf(cg.inter.Cert)},
			Identity: identity, IDKind: audit.IDEmail, EKU: testca.EKU, Policy: testca.PolicyOID}.Compile()
		if err != nil {
			t.Fatal(err)
		}
		return (&audit.CertApprover{Policy: ci}).RosterDigest()
	}
	one := mk(map[string]string{"a=b": "c@x.example"})
	two := mk(map[string]string{"a": "b=c@x.example"})
	if one == two || len(one) != 64 {
		t.Fatalf("roster digests collide or are malformed: %s %s", one, two)
	}
	// A pinned vector for one entry: sha256(len32("alice") "alice" len32("alice@x.example") "alice@x.example").
	h := sha256.New()
	h.Write([]byte{0, 0, 0, 5})
	h.Write([]byte("alice"))
	h.Write([]byte{0, 0, 0, 15})
	h.Write([]byte("alice@x.example"))
	if got, want := mk(map[string]string{"alice": "alice@x.example"}), hex.EncodeToString(h.Sum(nil)); got != want {
		t.Fatalf("roster digest %s, want %s", got, want)
	}
}

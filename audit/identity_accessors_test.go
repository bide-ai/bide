package audit_test

import (
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// TestCertIdentityPolicyAccessors pins the read-only accessors for the certificate rules a request
// for a certificate has to ask for: each returns what the policy was compiled with, the defaults
// show as their effective value, and what a caller changes in a returned EKU does not reach the policy.
func TestCertIdentityPolicyAccessors(t *testing.T) {
	f := newFixture(t)

	p := f.policy()
	p.MaxLeafLifetime = 8 * time.Hour
	p.AllowEmailProtectionEKU = true
	c := mustCompile(t, p)
	if got := c.IDKind(); got != audit.IDPersonURI {
		t.Errorf("IDKind = %q, want %q", got, audit.IDPersonURI)
	}
	if got := c.EKU(); !got.Equal(testca.EKU) {
		t.Errorf("EKU = %v, want %v", got, testca.EKU)
	}
	if got := c.PolicyOID(); !got.Equal(testca.PolicyOID) {
		t.Errorf("PolicyOID = %v, want %v", got, testca.PolicyOID)
	}
	if got := c.MaxLeafLifetime(); got != 8*time.Hour {
		t.Errorf("MaxLeafLifetime = %s, want 8h", got)
	}
	if !c.AllowsEmailProtectionEKU() {
		t.Error("AllowsEmailProtectionEKU = false, want true")
	}

	// The zero MaxLeafLifetime selects the 24 hour default, and that is what the accessor reports.
	d := mustCompile(t, f.policy())
	if got := d.MaxLeafLifetime(); got != 24*time.Hour {
		t.Errorf("default MaxLeafLifetime = %s, want 24h", got)
	}
	if d.AllowsEmailProtectionEKU() {
		t.Error("AllowsEmailProtectionEKU = true on a policy that does not allow it")
	}
	if got := mustCompile(t, emailPolicy(f)).IDKind(); got != audit.IDEmail {
		t.Errorf("IDKind of an email policy = %q", got)
	}

	// Mutating the returned EKU changes neither the policy nor the next answer.
	e := d.EKU()
	e[len(e)-1] += 99
	if got := d.EKU(); !got.Equal(testca.EKU) {
		t.Errorf("EKU after the caller changed a copy = %v, want %v", got, testca.EKU)
	}
}

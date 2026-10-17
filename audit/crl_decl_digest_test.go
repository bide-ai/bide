package audit_test

import (
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// A complete -params file makes the stand-in approver's policy digest equal the gate's for the same
// declaration; every member of the file is in the digest.

type declRig struct {
	t      *testing.T
	ci     *audit.CertIdentity
	rootK  audit.SPKIHash
	interK audit.SPKIHash
}

func newDeclRig(t *testing.T) *declRig {
	root := testca.NewRoot(t, "root", testca.Opts{})
	inter := root.NewIntermediate(t, "inter", testca.Opts{})
	ci, err := audit.IdentityPolicy{
		Anchors: []*x509.Certificate{root.Cert}, Inter: []audit.SPKIHash{audit.SPKIHashOf(inter.Cert)},
		Identity: map[string]string{"alice": testca.Person(1)}, IDKind: audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
	}.Compile()
	if err != nil {
		t.Fatal(err)
	}
	return &declRig{t: t, ci: ci, rootK: audit.SPKIHashOf(root.Cert), interK: audit.SPKIHashOf(inter.Cert)}
}

func declHex(k audit.SPKIHash) string { return hex.EncodeToString(k[:]) }

var declEffective = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// gate is the CertApprover a gate declares, with every digest-covered value stated.
func (r *declRig) gate() *audit.CertApprover {
	return &audit.CertApprover{
		ApproverID: "alice", Policy: r.ci, ClockTolerance: 2 * time.Minute, PolicyEffectiveFrom: declEffective,
		ExecStatusAge:    2 * time.Hour,
		ExecStatusAgeFor: map[audit.SPKIHash]time.Duration{r.rootK: 48 * time.Hour, r.interK: 2 * time.Hour},
		CAOverlapFor:     map[audit.SPKIHash]time.Duration{r.rootK: time.Hour, r.interK: 0},
		StatusParams: audit.StatusParams{
			W: 12 * time.Hour, MinW: 20 * time.Minute, RoutineGrace: 2 * time.Minute, CAPeriod: time.Hour, MaxStatusAge: 3 * time.Hour,
			CoverageTolerance: 90 * time.Second, ThisUpdateSkew: 5 * time.Minute, AllowHolds: true, ConsecutiveNumbers: true, Retained: true, CountLive: true,
			CAPeriodFor:     map[audit.SPKIHash]time.Duration{r.rootK: 24 * time.Hour, r.interK: time.Hour},
			MaxStatusAgeFor: map[audit.SPKIHash]time.Duration{r.rootK: 96 * time.Hour, r.interK: 3 * time.Hour},
		},
	}
}

// file is the -params file of the same declaration, every member present.
func (r *declRig) file() map[string]any {
	return map[string]any{
		"ca_period": "1h0m0s", "max_status_age": "3h0m0s", "exec_status_age": "2h0m0s", "this_update_skew": "5m0s",
		"coverage_tolerance": "1m30s", "w": "12h0m0s", "min_w": "20m0s", "routine_grace": "2m0s", "clock_tolerance": "2m0s",
		"allow_holds": true, "consecutive_numbers": true, "retained": true, "effective_from": declEffective.Format(time.RFC3339),
		"issuers": map[string]any{
			declHex(r.rootK):  map[string]any{"ca_period": "24h0m0s", "max_status_age": "96h0m0s", "exec_status_age": "48h0m0s", "ca_overlap": "1h0m0s"},
			declHex(r.interK): map[string]any{"ca_period": "1h0m0s", "max_status_age": "3h0m0s", "exec_status_age": "2h0m0s", "ca_overlap": "0s"},
		},
	}
}

func (r *declRig) digest(f map[string]any) (digest string, missing []string) {
	r.t.Helper()
	b, _ := json.Marshal(f)
	d, err := audit.ParseStatusDecl("params", b)
	if err != nil {
		r.t.Fatal(err)
	}
	ap, err := d.Approver(r.ci, audit.NewStatusSnapshot(), func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		r.t.Fatal(err)
	}
	if err := ap.ValidateConfig(); err != nil {
		r.t.Fatalf("the gate's own configuration check refuses the declaration: %v", err)
	}
	return ap.PolicyDigest(), d.MissingForDigest()
}

func TestCompleteParamsFileGivesTheGatesDigest(t *testing.T) {
	r := newDeclRig(t)
	got, missing := r.digest(r.file())
	if len(missing) != 0 {
		t.Fatalf("a file with every member reports %v missing", missing)
	}
	if want := r.gate().PolicyDigest(); got != want {
		t.Fatalf("digest %s, the gate's is %s", got, want)
	}
}

// Changing any single member of the file breaks the equality: every member reaches the digest.
func TestEveryParamsMemberReachesTheDigest(t *testing.T) {
	r := newDeclRig(t)
	want := r.gate().PolicyDigest()
	change := map[string]any{
		"ca_period": "55m0s", "max_status_age": "2h30m0s", "exec_status_age": "2h10m0s", "this_update_skew": "4m0s",
		"coverage_tolerance": "1m0s", "w": "13h0m0s", "min_w": "25m0s", "routine_grace": "3m0s", "clock_tolerance": "3m0s",
		"allow_holds": false, "consecutive_numbers": false, "retained": false, "effective_from": declEffective.Add(time.Hour).Format(time.RFC3339),
	}
	for k, v := range change {
		t.Run(k, func(t *testing.T) {
			f := r.file()
			f[k] = v
			if got, _ := r.digest(f); got == want {
				t.Fatalf("changing %s did not change the digest", k)
			}
		})
	}
	for _, m := range []string{"ca_period", "max_status_age", "exec_status_age", "this_update_skew", "ca_overlap"} {
		t.Run("issuer "+m, func(t *testing.T) {
			f := r.file()
			iss := f["issuers"].(map[string]any)[declHex(r.interK)].(map[string]any)
			old, _ := iss[m].(string)
			iss[m] = map[string]string{"ca_period": "50m0s", "max_status_age": "2h45m0s", "exec_status_age": "2h5m0s", "this_update_skew": "1m0s", "ca_overlap": "1m0s"}[m]
			if iss[m] == old {
				t.Fatal("test bug")
			}
			if got, _ := r.digest(f); got == want {
				t.Fatalf("changing the issuer's %s did not change the digest", m)
			}
		})
	}
}

// The new members are optional: an older file parses and builds the approver it always built, and the
// digest says which members it lacks.
func TestParamsFileWithoutTheNewMembersIsUnchanged(t *testing.T) {
	r := newDeclRig(t)
	f := r.file()
	for _, k := range []string{"w", "min_w", "routine_grace", "clock_tolerance", "allow_holds", "consecutive_numbers", "retained", "effective_from", "coverage_tolerance"} {
		delete(f, k)
	}
	_, missing := r.digest(f)
	want := "allow_holds, clock_tolerance, consecutive_numbers, coverage_tolerance, effective_from, min_w, retained, routine_grace, w"
	if got := strings.Join(missing, ", "); got != want {
		t.Fatalf("missing %q, want %q", got, want)
	}
	b, _ := json.Marshal(f)
	d, err := audit.ParseStatusDecl("params", b)
	if err != nil {
		t.Fatal(err)
	}
	ap, _ := d.Approver(r.ci, audit.NewStatusSnapshot(), func() time.Time { return declEffective })
	if ap.ClockTolerance != 0 || ap.StatusParams.W != 0 || ap.StatusParams.AllowHolds || ap.StatusParams.Retained || !ap.PolicyEffectiveFrom.Equal(declEffective) {
		t.Fatalf("an absent member changed the approver: %+v", ap)
	}
}

func TestParamsNewMembersAreValidated(t *testing.T) {
	r := newDeclRig(t)
	for name, mut := range map[string]func(map[string]any){
		"w not a duration":      func(f map[string]any) { f["w"] = "a day" },
		"effective_from format": func(f map[string]any) { f["effective_from"] = "1 September" },
		"allow_holds not bool":  func(f map[string]any) { f["allow_holds"] = "yes" },
		"unknown member":        func(f map[string]any) { f["count_live"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := r.file()
			mut(f)
			b, _ := json.Marshal(f)
			if _, err := audit.ParseStatusDecl("params", b); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// Values the gate's own rules refuse are refused by ValidateConfig, with no second rule here.
	f := r.file()
	f["min_w"] = "13h0m0s" // above w: no decision could be final
	b, _ := json.Marshal(f)
	d, err := audit.ParseStatusDecl("params", b)
	if err != nil {
		t.Fatal(err)
	}
	ap, _ := d.Approver(r.ci, audit.NewStatusSnapshot(), time.Now)
	if err := ap.ValidateConfig(); err == nil || !strings.Contains(err.Error(), "MinW") {
		t.Fatalf("ValidateConfig: %v", err)
	}
	f = r.file()
	f["clock_tolerance"] = "-1m"
	b, _ = json.Marshal(f)
	d, _ = audit.ParseStatusDecl("params", b)
	ap, _ = d.Approver(r.ci, audit.NewStatusSnapshot(), time.Now)
	if err := ap.ValidateConfig(); err == nil || !strings.Contains(err.Error(), "ClockTolerance") {
		t.Fatalf("ValidateConfig: %v", err)
	}
}

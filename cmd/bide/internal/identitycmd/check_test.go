package identitycmd

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/policyfile"
	"github.com/bide-ai/bide/internal/testca"
)

// expect writes the policy record a gate running the env's files would journal, for -expect.
func (e *env) expect() string {
	e.t.Helper()
	pol, err := policyfile.Read(e.policy)
	if err != nil {
		e.t.Fatal(err)
	}
	pb, err := os.ReadFile(e.params)
	if err != nil {
		e.t.Fatal(err)
	}
	decl, err := audit.ParseStatusDecl(e.params, pb)
	if err != nil {
		e.t.Fatal(err)
	}
	ap, err := decl.Approver(pol, audit.NewStatusSnapshot(), time.Now)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := json.Marshal(ap.DescribeIdentity())
	p := filepath.Join(e.dir, "expect.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) checkArgs(extra ...string) []string {
	return append([]string{"check", "-policy", e.policy, "-params", e.params}, extra...)
}

func TestCheckOnlineReady(t *testing.T) {
	e := newEnv(t)
	e.publish(1, 1)
	st, out, errs := e.run(e.checkArgs("-expect", e.expect(), "-fetch")...)
	if st != 0 {
		t.Fatalf("status %d\n%s\n%s", st, out, errs)
	}
	for _, want := range []string{"OK    policy.load", "OK    crl.fresh.", "ready:", "not checkable ahead of time"} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatal("color on a pipe")
	}
}

func TestCheckJSONAndExitCodes(t *testing.T) {
	e := newEnv(t)
	e.publish(1, 1)
	exp := e.expect()
	t.Run("json ready", func(t *testing.T) {
		st, out, _ := e.run(e.checkArgs("-expect", exp, "-fetch", "-json")...)
		var j struct {
			Ready, Partial bool
			Checks         []struct{ ID, Status string }
			NotCheckable   []string `json:"not_checkable"`
		}
		if err := json.Unmarshal([]byte(out), &j); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if st != 0 || !j.Ready || j.Partial || len(j.Checks) == 0 || len(j.NotCheckable) == 0 {
			t.Fatalf("status %d: %+v", st, j)
		}
	})
	t.Run("a failure exits 1", func(t *testing.T) {
		e.mu.Lock()
		delete(e.routes, "/inter.crl")
		e.mu.Unlock()
		defer e.publish(1, 1)
		st, out, _ := e.run(e.checkArgs("-expect", exp, "-fetch")...)
		if st != 1 || !strings.Contains(out, "FAIL  crl.fetch") || !strings.Contains(out, "fix: ") || !strings.Contains(out, "not ready") {
			t.Fatalf("status %d:\n%s", st, out)
		}
	})
	t.Run("partial exits 4", func(t *testing.T) {
		st, out, _ := e.run(e.checkArgs("-no-expect", "-no-status")...)
		if st != 4 || !strings.Contains(out, "partial:") || strings.Contains(out, "ready: every") {
			t.Fatalf("status %d:\n%s", st, out)
		}
	})
	t.Run("usage exits 2", func(t *testing.T) {
		for _, args := range [][]string{
			e.checkArgs("-expect", exp),         // no status stance
			e.checkArgs("-fetch"),               // no expect stance
			e.checkArgs("-fetch", "-no-status"), // contradictory
			e.checkArgs("-expect", exp, "-fetch", "-at", "yesterday"),
			append(e.checkArgs("-expect", exp, "-fetch"), "extra"),
		} {
			if st, _, errs := e.run(args...); st != 2 {
				t.Fatalf("%v: status %d: %s", args, st, errs)
			}
		}
	})
	t.Run("an unreadable file exits 3", func(t *testing.T) {
		st, _, errs := e.run("check", "-policy", filepath.Join(e.dir, "nope.json"), "-params", e.params, "-no-expect", "-no-status")
		if st != 3 || !strings.Contains(errs, "-policy") {
			t.Fatalf("status %d: %s", st, errs)
		}
	})
}

// A certificate chain through the command: accepted, then refused for the reason the gate gives.
func TestCheckCertificate(t *testing.T) {
	e := newEnv(t)
	e.publish(1, 1)
	now := time.Now()
	good := e.inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(c *x509Certificate) { c.NotBefore, c.NotAfter = now.Add(-time.Hour), now.Add(time.Hour) }})
	gp := filepath.Join(e.dir, "good.pem")
	if err := os.WriteFile(gp, pemChain(good.Chain()...), 0o600); err != nil {
		t.Fatal(err)
	}
	args := e.checkArgs("-expect", e.expect(), "-fetch", "-cert", gp)
	if st, out, _ := e.run(args...); st != 0 || !strings.Contains(out, "OK    cert.status.exec") {
		t.Fatalf("status %d:\n%s", st, out)
	}
	expired := e.inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(c *x509Certificate) { c.NotBefore, c.NotAfter = now.Add(-3*time.Hour), now.Add(-2*time.Hour) }})
	ep := filepath.Join(e.dir, "expired.pem")
	if err := os.WriteFile(ep, pemChain(expired.Chain()...), 0o600); err != nil {
		t.Fatal(err)
	}
	st, out, _ := e.run(e.checkArgs("-expect", e.expect(), "-fetch", "-cert", ep)...)
	if st != 1 || !strings.Contains(out, "FAIL  cert.chain") || !strings.Contains(out, "outside the certificate's validity") {
		t.Fatalf("status %d:\n%s", st, out)
	}
}

type x509Certificate = x509.Certificate

func pemChain(der ...[]byte) []byte {
	var out []byte
	for _, d := range der {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	return out
}

// The digest, end to end: fetch-crl prints the digest and whether it is comparable; check -expect-digest is
// an equality once -params states every member.
func TestDigestComparabilityThroughTheCommands(t *testing.T) {
	e := newEnv(t)
	e.publish(1, 1)
	startup := func() string {
		st, out, errs := e.run(append([]string{"fetch-crl", "-once"}, e.common()...)...)
		if st != 0 {
			t.Fatalf("status %d: %s %s", st, out, errs)
		}
		return out
	}
	out := startup()
	if !strings.Contains(out, "cannot be compared with the gate's because the file lacks allow_holds, clock_tolerance, consecutive_numbers, coverage_tolerance, effective_from, min_w, retained, routine_grace, w") {
		t.Fatalf("an old file is not reported as incomplete:\n%s", out)
	}
	// An old file: -expect-digest is refused, naming the members.
	st, out, _ := e.run(e.checkArgs("-expect-digest", strings.Repeat("a", 64), "-fetch")...)
	if st != 1 || !strings.Contains(out, "FAIL  params.digest") || !strings.Contains(out, "lacks allow_holds,") {
		t.Fatalf("status %d:\n%s", st, out)
	}
	// A complete file: the printed digest is the one check compares.
	b, err := os.ReadFile(e.params)
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	p["coverage_tolerance"], p["w"], p["min_w"], p["routine_grace"], p["clock_tolerance"] = "1m0s", "24h0m0s", "30m0s", "0s", "1m0s"
	p["allow_holds"], p["consecutive_numbers"], p["retained"], p["effective_from"] = false, false, false, "2026-09-01T00:00:00Z"
	b, _ = json.Marshal(p)
	if err := os.WriteFile(e.params, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out = startup()
	i := strings.Index(out, "policy digest ")
	if i < 0 || !strings.Contains(out, "every member the digest covers is stated, so it must equal the digest the gate records") {
		t.Fatalf("a complete file is not reported as comparable:\n%s", out)
	}
	digest := out[i+len("policy digest ") : i+len("policy digest ")+64]
	if st, out, _ := e.run(e.checkArgs("-expect-digest", digest, "-fetch")...); st != 0 || !strings.Contains(out, "OK    params.digest") {
		t.Fatalf("status %d:\n%s", st, out)
	}
	if st, out, _ := e.run(e.checkArgs("-expect-digest", strings.Repeat("b", 64), "-fetch")...); st != 1 || !strings.Contains(out, "FAIL  params.digest") {
		t.Fatalf("status %d:\n%s", st, out)
	}
}

// idpOf is a critical issuing distribution point holding only the fullName of uri (what step-ca stamps).
func idpOf(t *testing.T, uri string) pkix.Extension {
	t.Helper()
	enc := func(v asn1.RawValue) []byte {
		b, err := asn1.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	name := enc(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 6, Bytes: []byte(uri)})
	full := enc(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: name})
	point := enc(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: full})
	return pkix.Extension{Id: asn1.ObjectIdentifier{2, 5, 29, 28}, Critical: true,
		Value: enc(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: point})}
}

// publishScoped serves a CRL of the intermediate whose issuing distribution point names uri.
func (e *env) publishScoped(uri string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.routes["/root.crl"] = e.root.NewCRL(e.t, testca.CRLOpts{Number: 1, ThisUpdate: e.base.Add(-time.Hour), NextUpdate: e.base.Add(48 * time.Hour)})
	e.routes["/inter.crl"] = e.inter.NewCRL(e.t, testca.CRLOpts{Number: 1, ThisUpdate: e.base.Add(-time.Minute), NextUpdate: e.base.Add(time.Hour),
		Extra: []pkix.Extension{idpOf(e.t, uri)}})
}

// A step-ca style CRL (an issuing distribution point naming the URL it is served at) is read by the command,
// and the leaf must name that URL: one that does not is reported with the cause.
func TestCheckCertificateAndAScopedCRL(t *testing.T) {
	e := newEnv(t)
	url := e.srv.URL + "/inter.crl"
	e.publishScoped(url)
	now := time.Now()
	leaf := func(name string, points ...string) string {
		l := e.inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(c *x509Certificate) {
			c.NotBefore, c.NotAfter, c.CRLDistributionPoints = now.Add(-time.Hour), now.Add(time.Hour), points
		}})
		p := filepath.Join(e.dir, name+".pem")
		if err := os.WriteFile(p, pemChain(l.Chain()...), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	exp := e.expect()
	st, out, errs := e.run(e.checkArgs("-expect", exp, "-fetch", "-cert", leaf("names", url))...)
	if st != 0 || !strings.Contains(out, "OK    cert.cdp") || !strings.Contains(out, "OK    cert.status.count") {
		t.Fatalf("a leaf that names the CRL's point: status %d\n%s\n%s", st, out, errs)
	}
	st, out, _ = e.run(e.checkArgs("-expect", exp, "-fetch", "-cert", leaf("none"))...)
	if st != 1 || !strings.Contains(out, "FAIL  cert.cdp") || !strings.Contains(out, "FAIL  cert.status.count") {
		t.Fatalf("a leaf without the point: status %d\n%s", st, out)
	}
	st, out, _ = e.run(e.checkArgs("-expect", exp, "-fetch", "-cert", leaf("two", url, "http://mirror.bank.example/other.crl"))...)
	if st != 0 || !strings.Contains(out, "WARN  cert.cdp") {
		t.Fatalf("a leaf with two different points: status %d\n%s", st, out)
	}
}

// fetch-crl refuses a CRL whose issuing distribution point is not the URL it was fetched from.
func TestFetchRefusesAnIDPThatIsNotItsSource(t *testing.T) {
	e := newEnv(t)
	e.publishScoped("http://elsewhere.bank.example/other.crl")
	st, out, _ := e.run(append([]string{"fetch-crl", "-once"}, e.common()...)...)
	if st != 1 || !strings.Contains(out, "unsupported-crl") || !strings.Contains(out, "elsewhere.bank.example") {
		t.Fatalf("status %d:\n%s", st, out)
	}
}

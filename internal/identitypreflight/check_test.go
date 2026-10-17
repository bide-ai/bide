package identitypreflight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/internal/testca"
)

// A full green setup, offline: a flat directory of CRLs, then a crlrefresh archive, then the certificate.
func TestGreenOfflineFlatAndArchive(t *testing.T) {
	g := newRig(t)
	rc, ic := g.freshCRLs()
	for name, dir := range map[string]string{"flat": g.flatDir(rc, ic), "archive": g.archiveDir(rc, ic)} {
		t.Run(name, func(t *testing.T) {
			o := g.options(dir)
			o.Cert = filepath.Join(g.dir, "chain.pem")
			r := g.run(o)
			r.mustBeReady(t)
			for _, id := range []string{"policy.load", "policy.anchor", "policy.intermediate", "params.config", "params.expect", "crl.setup", "crl.load",
				"cert.parse", "cert.chain", "cert.status.count", "cert.status.exec"} {
				if r.get(id) == nil {
					t.Fatalf("no check %s:\n%s", id, r.dump())
				}
			}
			if !strings.Contains(r.get("cert.chain").Message, testca.Person(1)) {
				t.Fatalf("the verified identifier is not shown: %s", r.get("cert.chain").Message)
			}
			if len(r.NotCheckable) == 0 {
				t.Fatal("a ready report must still list what is not checkable")
			}
		})
	}
}

// The archive is read without a write: a pre-flight must not change a fetch-crl job's rollback memory.
func TestArchiveIsNotWritten(t *testing.T) {
	g := newRig(t)
	rc, ic := g.freshCRLs()
	dir := g.archiveDir(rc, ic)
	snap := func() map[string]time.Time {
		m := map[string]time.Time{}
		_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err == nil {
				m[p] = fi.ModTime()
			}
			return nil
		})
		return m
	}
	before := snap()
	g.run(g.options(dir)).mustBeReady(t)
	after := snap()
	if len(before) != len(after) {
		t.Fatalf("files changed: %d before, %d after", len(before), len(after))
	}
	for p, m := range before {
		if !after[p].Equal(m) {
			t.Fatalf("%s was modified", p)
		}
	}
}

// Online: the CRLs come from the policy's crl sources over HTTP, nothing is stored.
func TestGreenOnline(t *testing.T) {
	g := newRig(t)
	srv := newCRLServer(t)
	now := time.Now()
	srv.set("/root.crl", g.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: now.Add(-time.Hour), NextUpdate: now.Add(48 * time.Hour)}))
	srv.set("/inter.crl", g.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: now.Add(-10 * time.Minute), NextUpdate: now.Add(time.Hour)}))
	g.policy["crl"] = map[string]any{
		g.rootK:  map[string]any{"urls": []string{srv.URL + "/root.crl"}},
		g.interK: map[string]any{"urls": []string{srv.URL + "/inter.crl"}},
	}
	leaf := g.inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(c *x509Cert) { c.NotBefore, c.NotAfter = now.Add(-time.Hour), now.Add(time.Hour) }})
	g.write("online.pem", pemOf(leaf.Chain()...))
	g.at = now
	o := g.options("")
	o.At, o.Fetch, o.Cert = time.Time{}, true, filepath.Join(g.dir, "online.pem")
	r := g.run(o)
	r.mustBeReady(t)
	if r.get("crl.fetch") == nil {
		t.Fatalf("no fetch line:\n%s", r.dump())
	}
	if _, err := os.Stat(filepath.Join(g.dir, "crl")); err == nil {
		t.Fatal("the pre-flight stored CRLs")
	}
}

func TestOnlineUnreachableAndNotACRL(t *testing.T) {
	g := newRig(t)
	srv := newCRLServer(t)
	now := time.Now()
	srv.set("/inter.crl", []byte("not a crl"))
	// The token in the query must never appear in a report.
	g.policy["crl"] = map[string]any{
		g.rootK:  map[string]any{"urls": []string{srv.URL + "/root.crl?token=SECRET123"}},
		g.interK: map[string]any{"urls": []string{srv.URL + "/inter.crl"}},
	}
	g.at = now
	o := g.options("")
	o.At, o.Fetch = time.Time{}, true
	r := g.run(o)
	found404 := false
	for _, c := range r.Checks {
		found404 = found404 || (c.ID == "crl.fetch" && c.Status == Fail && strings.Contains(c.Message, "404 Not Found") && c.Fix != "")
	}
	if !found404 {
		t.Fatalf("the 404 is not reported with its URL:\n%s", r.dump())
	}
	r.want(t, "crl.event.bad-signature", Warn, "inter")
	var buf bytes.Buffer
	r.Text(&buf, term.New(false))
	_ = r.JSON(&buf)
	if strings.Contains(buf.String(), "SECRET123") {
		t.Fatalf("a URL token reached the report:\n%s", buf.String())
	}
	if r.ExitCode() != ExitFail || r.Ready {
		t.Fatal("an unreachable CRL source must not be ready")
	}
	n := 0
	for _, c := range r.Checks {
		if c.ID == "crl.fetch" && c.Status == Fail {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("want a failure per issuer, got %d:\n%s", n, r.dump())
	}
}

func TestPolicyDoesNotLoad(t *testing.T) {
	g := newRig(t)
	g.policy["bogus_member"] = true
	o := g.options(g.flatDir(g.freshCRLs()))
	r := g.run(o)
	r.want(t, "policy.load", Fail, "bogus_member")
	if r.Ready || r.ExitCode() != ExitFail {
		t.Fatal("a policy that does not load is not ready")
	}
}

func TestMissingFilesAreIOErrors(t *testing.T) {
	g := newRig(t)
	o := g.options(g.flatDir(g.freshCRLs()))
	for name, mut := range map[string]func(*Options){
		"policy":  func(o *Options) { o.Policy = filepath.Join(g.dir, "absent.json") },
		"params":  func(o *Options) { o.Params = filepath.Join(g.dir, "absent.json") },
		"expect":  func(o *Options) { o.Expect = filepath.Join(g.dir, "absent.json") },
		"crl-dir": func(o *Options) { o.CRLDir = filepath.Join(g.dir, "absent") },
		"cert":    func(o *Options) { o.Cert = filepath.Join(g.dir, "absent.pem") },
	} {
		t.Run(name, func(t *testing.T) {
			x := o
			mut(&x)
			_, err := Run(context.Background(), x)
			var ie *IOError
			if !errors.As(err, &ie) {
				t.Fatalf("want an IOError, got %v", err)
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	g := newRig(t)
	good := g.options(g.flatDir(g.freshCRLs()))
	for name, mut := range map[string]func(*Options){
		"no policy":            func(o *Options) { o.Policy = "" },
		"no params":            func(o *Options) { o.Params = "" },
		"no status stance":     func(o *Options) { o.CRLDir = "" },
		"no-status and dir":    func(o *Options) { o.NoStatus = true },
		"no expect stance":     func(o *Options) { o.Expect = "" },
		"expect and no":        func(o *Options) { o.NoExpect = true },
		"at with fetch":        func(o *Options) { o.Fetch = true },
		"chain without cert":   func(o *Options) { o.Chain = "x" },
		"as without cert":      func(o *Options) { o.As = "alice" },
		"as not in the roster": func(o *Options) { o.Cert, o.As = filepath.Join(g.dir, "chain.pem"), "mallory" },
	} {
		t.Run(name, func(t *testing.T) {
			x := good
			mut(&x)
			_, err := Run(context.Background(), x)
			var ue *UsageError
			if !errors.As(err, &ue) {
				t.Fatalf("want a UsageError, got %v", err)
			}
		})
	}
}

func TestAnchorExpiredAndExpiring(t *testing.T) {
	g := newRig(t)
	rc, ic := g.freshCRLs()
	o := g.options(g.flatDir(rc, ic))
	o.At = testca.T0.Add(366 * 24 * time.Hour)
	g.run(o).want(t, "policy.anchor", Fail, "expired")
	o.At = testca.T0.Add(300 * 24 * time.Hour)
	g.run(o).want(t, "policy.anchor", Warn, "expires")
}

func TestIntermediateMismatches(t *testing.T) {
	g := newRig(t)
	rc, ic := g.freshCRLs()
	t.Run("pinned key without a certificate", func(t *testing.T) {
		g.policy["intermediate_certs"] = []string{}
		r := g.run(g.options(g.flatDir(rc, ic)))
		r.want(t, "policy.intermediates", Fail, "holds no certificate for it")
	})
	t.Run("certificate not pinned", func(t *testing.T) {
		g := newRig(t)
		other := g.root.NewIntermediate(t, "other", testca.Opts{})
		g.write("other.pem", pemOf(other.DER))
		g.policy["intermediate_certs"] = []string{"inter.pem", "other.pem"}
		r := g.run(g.options(g.flatDir(g.freshCRLs())))
		r.want(t, "policy.intermediates", Fail, "does not pin")
	})
	t.Run("pins a key that is not the certificate", func(t *testing.T) {
		g := newRig(t)
		other := g.root.NewIntermediate(t, "other", testca.Opts{})
		g.policy["intermediates"] = []string{hexKey(other.Cert)}
		r := g.run(g.options(g.flatDir(g.freshCRLs())))
		if len(r.failing()) == 0 || r.get("policy.intermediates").Status != Fail {
			t.Fatalf("want a failure:\n%s", r.dump())
		}
	})
}

func TestMissingCRLSourceFailsSetup(t *testing.T) {
	g := newRig(t)
	g.policy["crl"] = map[string]any{g.rootK: map[string]any{"urls": []string{"http://crl.bank.example/root.crl"}}}
	r := g.run(g.options(g.flatDir(g.freshCRLs())))
	r.want(t, "crl.setup", Fail, "no CRL source")
}

func TestParamsProblems(t *testing.T) {
	t.Run("does not load", func(t *testing.T) {
		g := newRig(t)
		g.params["no_such"] = 1
		g.run(g.options(g.flatDir(g.freshCRLs()))).want(t, "params.load", Fail, "no_such")
	})
	t.Run("fails the gate's own configuration check", func(t *testing.T) {
		g := newRig(t)
		g.params["ca_period"] = "2h"
		g.params["issuers"] = map[string]any{}
		r := g.run(g.options(g.flatDir(g.freshCRLs())))
		r.want(t, "params.config", Fail, "ExecStatusAge/2")
	})
}

func TestExpectComparesFieldByField(t *testing.T) {
	g := newRig(t)
	rc, ic := g.freshCRLs()
	dir := g.flatDir(rc, ic)
	t.Run("agrees, and lists what cannot be compared", func(t *testing.T) {
		r := g.run(g.options(dir))
		r.want(t, "params.expect", OK, "agree")
		r.want(t, "params.expect.unverified", OK, "effective_from")
		if m := r.get("params.expect.unverified").Message; !strings.Contains(m, "routine_grace") || !strings.Contains(m, "retained") {
			t.Fatalf("the unverified members are not named with their recorded values: %s", m)
		}
	})
	for name, mut := range map[string]func(map[string]any){
		"max_status_age":  func(m map[string]any) { m["max_status_age"] = "2h0m0s" },
		"eku":             func(m map[string]any) { m["eku"] = "1.2.3.4" },
		"anchors":         func(m map[string]any) { m["anchors"] = []any{"00"} },
		"exec_status_age": func(m map[string]any) { m["exec_status_age"] = "30m0s" },
		"ca_period_for":   func(m map[string]any) { delete(m, "ca_period_for") },
	} {
		t.Run("differs in "+name, func(t *testing.T) {
			rec := g.recordCopy()
			mut(rec)
			o := g.options(dir)
			o.Expect = g.writeJSON("expect.json", rec)
			r := g.run(o)
			found := false
			for _, c := range r.Checks {
				if c.ID == "params.expect" && c.Status == Fail && strings.HasPrefix(c.Message, name+":") {
					found = true
				}
			}
			if !found {
				t.Fatalf("no failure naming %s:\n%s", name, r.dump())
			}
		})
	}
	t.Run("the whole identity record is accepted", func(t *testing.T) {
		o := g.options(dir)
		o.Expect = g.writeJSON("expect.json", map[string]any{"format": "x", "policy": g.recordCopy(), "policy_digest": "00"})
		g.run(o).want(t, "params.expect", OK, "agree")
	})
	t.Run("not a record", func(t *testing.T) {
		o := g.options(dir)
		o.Expect = g.writeJSON("expect.json", map[string]any{"hello": 1})
		g.run(o).want(t, "params.expect", Fail, "hello")
	})
	t.Run("a member of another version", func(t *testing.T) {
		rec := g.recordCopy()
		rec["new_member"] = true
		o := g.options(dir)
		o.Expect = g.writeJSON("expect.json", rec)
		g.run(o).want(t, "params.expect", Fail, "new_member")
	})
	t.Run("not compared is partial", func(t *testing.T) {
		o := g.options(dir)
		o.Expect, o.NoExpect = "", true
		r := g.run(o)
		r.want(t, "params.expect", Skip, "not compared")
		if r.Ready || !r.Partial || r.ExitCode() != ExitPartial {
			t.Fatalf("a skipped comparison must be partial, not ready: exit %d", r.ExitCode())
		}
	})
}

func TestFreshness(t *testing.T) {
	fresh := func(t *testing.T) (*rig, []byte, []byte) {
		g := newRig(t)
		rc, ic := g.freshCRLs()
		return g, rc, ic
	}
	t.Run("no CRL for an issuer", func(t *testing.T) {
		g, root, _ := fresh(t)
		r := g.run(g.options(g.flatDir(root, nil)))
		r.want(t, "crl.fresh."+g.interK[:8], Fail, "no CRL is held")
		r.want(t, "crl.fresh."+g.rootK[:8], OK, "CRL number 1")
	})
	t.Run("too old for the declared ages", func(t *testing.T) {
		g, root, _ := fresh(t)
		old := g.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: g.at.Add(-5 * time.Hour), NextUpdate: g.at.Add(-4 * time.Hour)})
		r := g.run(g.options(g.flatDir(root, old)))
		r.want(t, "crl.fresh."+g.interK[:8], Fail, "a gate would not accept it")
	})
	t.Run("past nextUpdate", func(t *testing.T) {
		g, root, _ := fresh(t)
		old := g.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: g.at.Add(-50 * time.Minute), NextUpdate: g.at.Add(-10 * time.Minute)})
		r := g.run(g.options(g.flatDir(root, old)))
		r.want(t, "crl.fresh."+g.interK[:8], Fail, "a gate would not accept it")
	})
	t.Run("a CRL signed by another CA is not used", func(t *testing.T) {
		g, root, _ := fresh(t)
		stranger := testca.NewRoot(t, "stranger", testca.Opts{})
		bad := stranger.NewCRL(t, testca.CRLOpts{Number: 9, ThisUpdate: g.at.Add(-time.Minute), NextUpdate: g.at.Add(time.Hour)})
		g.write("crls/bad.crl", bad)
		r := g.run(g.options(g.flatDir(root, nil)))
		if c := r.get("crl.file"); c == nil || c.Status != Warn || !strings.Contains(c.Message, "bad.crl") {
			t.Fatalf("a rejected file is not reported:\n%s", r.dump())
		}
		r.want(t, "crl.fresh."+g.interK[:8], Fail, "no CRL is held")
	})
	t.Run("healthy", func(t *testing.T) {
		g, root, inter := fresh(t)
		g.run(g.options(g.flatDir(root, inter))).mustBeReady(t)
	})
	t.Run("PEM CRLs are read", func(t *testing.T) {
		g, rc, ic := fresh(t)
		g.write("crls/root.pem", pemCRL(rc))
		g.write("crls/inter.pem", pemCRL(ic))
		g.run(g.options(filepath.Join(g.dir, "crls"))).mustBeReady(t)
	})
}

func TestNoStatusIsPartial(t *testing.T) {
	g := newRig(t)
	o := g.options("")
	o.NoStatus = true
	o.Cert = filepath.Join(g.dir, "chain.pem")
	r := g.run(o)
	if r.Ready || !r.Partial || r.Failed() || r.ExitCode() != ExitPartial {
		t.Fatalf("exit %d ready %v:\n%s", r.ExitCode(), r.Ready, r.dump())
	}
	r.want(t, "crl.status", Skip, "not checked")
	r.want(t, "cert.chain", OK, "accepted")
	r.want(t, "cert.status.count", Skip, "not checked")
	var buf bytes.Buffer
	if err := r.JSON(&buf); err != nil {
		t.Fatal(err)
	}
	var j map[string]any
	if err := json.Unmarshal(buf.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	if j["ready"] != false || j["partial"] != true {
		t.Fatalf("json: ready %v partial %v", j["ready"], j["partial"])
	}
	if len(j["not_checkable"].([]any)) == 0 {
		t.Fatal("not_checkable missing from the json")
	}
}

func TestPartialNeverHidesAFailure(t *testing.T) {
	g := newRig(t)
	g.policy["bogus_member"] = true
	o := g.options("")
	o.NoStatus = true
	r := g.run(o)
	if r.ExitCode() != ExitFail {
		t.Fatalf("a failure must outrank partial: exit %d", r.ExitCode())
	}
}

func pemCRL(der []byte) []byte {
	return pemBlock("X509 CRL", der)
}

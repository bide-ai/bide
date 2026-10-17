package identitycmd

import (
	"crypto/x509"
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

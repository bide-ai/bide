package identitypreflight

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/testca"
)

// The check against the captures of real CAs (EJBCA Community and Microsoft AD CS) that audit/testdata holds:
// the same certificates and CRLs the status-rule conformance tests use, here through the whole pre-flight.

func capturePEM(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "audit", "testdata", dir, name+".pem"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatalf("%s/%s: no PEM block", dir, name)
	}
	return pem.EncodeToMemory(blk)
}

func crlTime(t *testing.T, dir, name string) time.Time {
	t.Helper()
	blk, _ := pem.Decode(capturePEM(t, dir, name))
	rl, err := x509.ParseRevocationList(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return rl.ThisUpdate
}

type captureRig struct {
	*rig
	dir, crls string
	last      time.Time // thisUpdate of the last delta
	after     time.Duration
}

func newCaptureRig(t *testing.T, dir string, skew string, maxLeaf string, after time.Duration) *captureRig {
	t.Helper()
	g := &rig{t: t, dir: t.TempDir()}
	c := &captureRig{rig: g, dir: dir}
	root, ca := capturePEM(t, dir, "root"), capturePEM(t, dir, "ca")
	g.write("root.pem", root)
	g.write("ca.pem", ca)
	rb, _ := pem.Decode(root)
	cb, _ := pem.Decode(ca)
	rc, err := x509.ParseCertificate(rb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	cc, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	g.rootK, g.interK = hexKey(rc), hexKey(cc)
	ids := map[string]string{}
	for n := 1; n <= 4; n++ {
		ids[fmt.Sprintf("u%d", n)] = testca.Person(n)
	}
	g.policy = map[string]any{
		"anchors": []string{"root.pem"}, "intermediates": []string{g.interK}, "intermediate_certs": []string{"ca.pem"},
		"identity": ids, "id_kind": "person-uri", "eku": testca.EKU.String(), "policy_oid": testca.PolicyOID.String(),
		"canonical": map[string]string{"host": "idp.bank.example"}, "require_policy_on_cas": true, "max_leaf_lifetime": maxLeaf,
		"crl": map[string]any{
			g.rootK:  map[string]any{"urls": []string{"http://crl.bank.example/root.crl"}},
			g.interK: map[string]any{"urls": []string{"http://crl.bank.example/ca.crl"}},
		},
	}
	g.params = map[string]any{
		"ca_period": "1h", "max_status_age": "2h", "exec_status_age": "2h",
		"issuers": map[string]any{
			g.rootK: map[string]any{"ca_period": "24h", "max_status_age": "96h", "exec_status_age": "96h", "ca_overlap": "0s"},
			g.interK: map[string]any{"ca_period": "1h", "max_status_age": "2h", "exec_status_age": "2h", "ca_overlap": "0s",
				"this_update_skew": skew},
		},
	}
	g.record = g.gateRecord()
	c.crls = filepath.Join(g.dir, "crls")
	for _, n := range []string{"rootcrl", "base1", "delta1", "base2", "delta2", "base3", "delta3"} {
		g.write("crls/"+n+".pem", capturePEM(t, dir, n))
	}
	c.last = crlTime(t, dir, "delta3")
	g.at = c.last.Add(after)
	c.after = after
	return c
}

// judge checks leaf u (with the issuing CA behind it) over the captured CRLs at the rig's time.
func (c *captureRig) judge(u string) *Report {
	c.t.Helper()
	o := c.options(c.crls)
	o.Cert = c.write("cert-"+u+".pem", append(capturePEM(c.t, c.dir, u), capturePEM(c.t, c.dir, "ca")...))
	o.As = u
	return c.run(o)
}

func TestRealCAs(t *testing.T) {
	for _, tc := range []struct {
		dir, skew, maxLeaf string
		after              time.Duration
	}{{"ejbca", "0s", "24h", 5 * time.Minute}, {"adcs", "10m", "25h", 15 * time.Minute}} {
		t.Run(tc.dir, func(t *testing.T) {
			c := newCaptureRig(t, tc.dir, tc.skew, tc.maxLeaf, tc.after)
			t.Run("never revoked", func(t *testing.T) {
				r := c.judge("u4")
				r.want(t, "cert.chain", OK, "accepted")
				r.want(t, "cert.status.count", OK, "would count")
				r.want(t, "cert.status.exec", OK, "would pass")
				r.want(t, "crl.fresh."+c.interK[:8], OK, "CRL number")
				r.want(t, "crl.fresh."+c.rootK[:8], OK, "CRL number")
			})
			t.Run("key compromise", func(t *testing.T) {
				r := c.judge("u1")
				r.want(t, "cert.chain", OK, "accepted")
				r.want(t, "cert.status.count", Fail, "revoked")
				r.want(t, "cert.status.exec", Fail, "revoked")
			})
			t.Run("superseded", func(t *testing.T) {
				c.judge("u3").want(t, "cert.status.count", Fail, "revoked")
			})
			t.Run("status gone stale", func(t *testing.T) {
				c.at = c.last.Add(5 * time.Hour)
				defer func() { c.at = c.last.Add(c.after) }()
				r := c.judge("u4")
				r.want(t, "crl.fresh."+c.interK[:8], Fail, "would not accept")
				r.want(t, "cert.status.count", Fail, "older than the policy allows")
			})
		})
	}
}

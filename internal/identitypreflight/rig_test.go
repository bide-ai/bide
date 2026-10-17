package identitypreflight

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/crlrefresh"
	"github.com/bide-ai/bide/internal/policyfile"
	"github.com/bide-ai/bide/internal/testca"
)

// rig is a two-tier PKI, a policy directory for it, and a leaf for alice. Times are fixed around testca.T0
// for the offline tests (at); the online tests make their own CRLs and leaf around the clock.
type rig struct {
	t             *testing.T
	dir           string
	root, inter   *testca.CA
	rootK, interK string
	leaf          *testca.Leaf
	at            time.Time
	policy        map[string]any
	params        map[string]any
	record        map[string]any // the gate's recorded policy for the files as built
}

func hexKey(c *x509.Certificate) string { h := audit.SPKIHashOf(c); return hex.EncodeToString(h[:]) }

func pemOf(der ...[]byte) []byte {
	var out []byte
	for _, d := range der {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	return out
}

func newRig(t *testing.T) *rig {
	t.Helper()
	g := &rig{t: t, dir: t.TempDir(), at: testca.T0}
	g.root = testca.NewRoot(t, "root", testca.Opts{})
	g.inter = g.root.NewIntermediate(t, "inter", testca.Opts{})
	g.rootK, g.interK = hexKey(g.root.Cert), hexKey(g.inter.Cert)
	g.leaf = g.inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519})
	g.write("root.pem", pemOf(g.root.DER))
	g.write("inter.pem", pemOf(g.inter.DER))
	g.write("chain.pem", pemOf(g.leaf.Chain()...))
	g.policy = map[string]any{
		"anchors": []string{"root.pem"}, "intermediates": []string{g.interK}, "intermediate_certs": []string{"inter.pem"},
		"identity": map[string]string{"alice": testca.Person(1)}, "id_kind": "person-uri",
		"eku": testca.EKU.String(), "policy_oid": testca.PolicyOID.String(), "canonical": map[string]string{"host": "idp.bank.example"},
		"crl": map[string]any{
			g.rootK:  map[string]any{"urls": []string{"http://crl.bank.example/root.crl"}},
			g.interK: map[string]any{"urls": []string{"http://crl.bank.example/inter.crl"}},
		},
	}
	g.params = map[string]any{
		"ca_period": "30m", "max_status_age": "3h", "exec_status_age": "1h",
		"issuers": map[string]any{
			g.rootK:  map[string]any{"ca_period": "24h", "max_status_age": "96h", "exec_status_age": "48h", "ca_overlap": "0s"},
			g.interK: map[string]any{"ca_period": "30m", "max_status_age": "3h", "exec_status_age": "1h", "ca_overlap": "0s"},
		},
	}
	g.record = g.gateRecord()
	return g
}

func (g *rig) write(name string, b []byte) string {
	g.t.Helper()
	p := filepath.Join(g.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		g.t.Fatal(err)
	}
	return p
}

func (g *rig) writeJSON(name string, v any) string {
	g.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		g.t.Fatal(err)
	}
	return g.write(name, b)
}

// fresh publishes the CRLs a healthy CA has at g.at: the root's 1h old, the intermediate's 10 minutes old.
func (g *rig) freshCRLs() (rootCRL, interCRL []byte) {
	rootCRL = g.root.NewCRL(g.t, testca.CRLOpts{Number: 1, ThisUpdate: g.at.Add(-time.Hour), NextUpdate: g.at.Add(48 * time.Hour)})
	interCRL = g.inter.NewCRL(g.t, testca.CRLOpts{Number: 1, ThisUpdate: g.at.Add(-10 * time.Minute), NextUpdate: g.at.Add(time.Hour)})
	return
}

// flatDir writes the CRLs into a flat directory and returns it.
func (g *rig) flatDir(rootCRL, interCRL []byte) string {
	dir := filepath.Join(g.dir, "crls")
	if rootCRL != nil {
		g.write("crls/root.crl", rootCRL)
	}
	if interCRL != nil {
		g.write("crls/inter.crl", interCRL)
	}
	return dir
}

// archiveDir writes the CRLs in the layout fetch-crl keeps.
func (g *rig) archiveDir(rootCRL, interCRL []byte) string {
	dir := filepath.Join(g.dir, "archive")
	a := crlrefresh.DirArchive(dir)
	for _, x := range []struct {
		ca  *testca.CA
		der []byte
	}{{g.root, rootCRL}, {g.inter, interCRL}} {
		if x.der == nil {
			continue
		}
		rl, err := x509.ParseRevocationList(x.der)
		if err != nil {
			g.t.Fatal(err)
		}
		if err := a.PutCRL(context.Background(), audit.SPKIHashOf(x.ca.Cert), crlrefresh.ArchivedCRL{Number: rl.Number, ThisUpdate: rl.ThisUpdate}, x.der); err != nil {
			g.t.Fatal(err)
		}
	}
	return dir
}

// options writes the policy and params files and returns offline options over crlDir with the gate's
// record (which agrees with the files) as -expect.
func (g *rig) options(crlDir string) Options {
	g.t.Helper()
	o := Options{Policy: g.writeJSON("policy.json", g.policy), Params: g.writeJSON("params.json", g.params), CRLDir: crlDir, At: g.at}
	o.Expect = g.writeJSON("expect.json", g.record)
	return o
}

// gateRecord is the policy record a gate running these files would journal: the stand-in's record, plus
// the members -params cannot declare (a gate sets them) and a real effective_from.
func (g *rig) gateRecord() map[string]any {
	g.t.Helper()
	pol, err := policyfile.Read(g.writeJSON("policy.json", g.policy))
	if err != nil {
		g.t.Fatal(err)
	}
	pb, _ := json.Marshal(g.params)
	decl, err := audit.ParseStatusDecl("params", pb)
	if err != nil {
		g.t.Fatal(err)
	}
	ap, err := decl.Approver(pol, audit.NewStatusSnapshot(), func() time.Time { return g.at })
	if err != nil {
		g.t.Fatal(err)
	}
	b, _ := json.Marshal(ap.DescribeIdentity())
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["effective_from"] = "2026-09-01T00:00:00Z"
	m["routine_grace"] = "2m0s"
	m["retained"] = true
	return m
}

func (g *rig) run(o Options) *Report {
	g.t.Helper()
	r, err := Run(context.Background(), o)
	if err != nil {
		g.t.Fatalf("Run: %v", err)
	}
	return r
}

func (r *Report) get(id string) *Check {
	for i := range r.Checks {
		if r.Checks[i].ID == id {
			return &r.Checks[i]
		}
	}
	return nil
}

func (r *Report) dump() string {
	var sb strings.Builder
	for _, c := range r.Checks {
		sb.WriteString(string(c.Status) + " " + c.ID + " " + c.Message + "\n")
	}
	return sb.String()
}

// want fails the test unless the check exists with the status and its message contains sub.
func (r *Report) want(t *testing.T, id string, s Status, sub string) {
	t.Helper()
	c := r.get(id)
	if c == nil {
		t.Fatalf("no check %q in:\n%s", id, r.dump())
	}
	if c.Status != s || !strings.Contains(c.Message, sub) {
		t.Fatalf("check %s is %s %q, want %s containing %q\nreport:\n%s", id, c.Status, c.Message, s, sub, r.dump())
	}
	if (s == Fail || s == Warn) && c.Fix == "" {
		t.Fatalf("check %s has no fix", id)
	}
}

// failing returns the ids of failed checks.
func (r *Report) failing() []string {
	var out []string
	for _, c := range r.Checks {
		if c.Status == Fail {
			out = append(out, c.ID)
		}
	}
	return out
}

func (r *Report) mustBeReady(t *testing.T) {
	t.Helper()
	if !r.Ready || r.Failed() || r.Partial || r.ExitCode() != ExitReady {
		t.Fatalf("not ready (exit %d):\n%s", r.ExitCode(), r.dump())
	}
	for _, c := range r.Checks {
		if c.Status == Skip || c.Status == Warn {
			t.Fatalf("a ready report holds %s %s: %s", c.Status, c.ID, c.Message)
		}
	}
}

// crlServer serves CRLs by path and counts requests.
type crlServer struct {
	*httptest.Server
	mu     sync.Mutex
	routes map[string][]byte
	hits   []string
}

func newCRLServer(t *testing.T) *crlServer {
	s := &crlServer{routes: map[string][]byte{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		b, ok := s.routes[r.URL.Path]
		s.hits = append(s.hits, r.URL.RequestURI())
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *crlServer) set(path string, b []byte) { s.mu.Lock(); s.routes[path] = b; s.mu.Unlock() }

func bigN(n int64) *big.Int { return big.NewInt(n) }

type x509Cert = x509.Certificate

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// recordCopy is a copy of the gate's record a test may change.
func (g *rig) recordCopy() map[string]any {
	b, _ := json.Marshal(g.record)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

package approvecmd

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/approvals"
	"github.com/bide-ai/bide/audit"
)

// intake serves the fixture's journal through approvals.Handler, as an application would, and
// returns the server and the handler's URL. wrap, if given, sits in front of the handler.
func (f *fixture) intake(wrap func(http.Handler) http.Handler, opts ...approvals.Option) *httptest.Server {
	f.t.Helper()
	j, closeJ := f.store()
	f.t.Cleanup(closeJ)
	verify := func(id string) (agent.ApproverVerifier, bool) {
		v, ok := f.pubs[id]
		return v, ok
	}
	h, err := approvals.New(j, verify, append([]approvals.Option{approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0)}, opts...)...)
	if err != nil {
		f.t.Fatal(err)
	}
	var handler http.Handler = h
	if wrap != nil {
		handler = wrap(h)
	}
	srv := httptest.NewServer(handler)
	f.t.Cleanup(srv.Close)
	return srv
}

// to is the flags of a decision by approver id through the endpoint at url.
func (f *fixture) to(url, id string, extra ...string) []string {
	return append([]string{"-to", url, "-run", f.run, "-call", f.call, "-as", id, "-key", f.keys[id], "-approver-keys", f.keysJSON}, extra...)
}

func (f *fixture) runTo(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = Main(append([]string{"-color", "never"}, args...), strings.NewReader(stdin), &out, &errb, stores)
	return code, out.String(), errb.String()
}

// Two approvers decide through the endpoint; the call runs once; each keeps a receipt that verifies.
func TestRemote_EndToEnd(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	srv := f.intake(nil)

	code, out, errs := f.runTo("ops\n", f.to(srv.URL, "ops")...)
	if code != 0 {
		t.Fatalf("ops: %d\n%s\n%s", code, out, errs)
	}
	contains(t, "ops output", out, "Approve this call?", "read from the endpoint", `"amount": 120`, "refund <ok>",
		"args SHA-256", "computed here", "signed and recorded by the application", "1 of 2 needed approved", "receipt ")
	if len(f.decisions()) != 1 || f.charges.Load() != 0 {
		t.Fatalf("%d decisions, %d charges after ops", len(f.decisions()), f.charges.Load())
	}
	rcpt := filepath.Join(f.dir, "r.json")
	code, out, errs = f.runTo("", f.to(srv.URL, "finance", "-yes", "-receipt", rcpt)...)
	if code != 0 {
		t.Fatalf("finance: %d\n%s\n%s", code, out, errs)
	}
	contains(t, "finance output", out, "the gate has its approvals")
	if _, err := f.drive(twoOfThree, false); err != nil || f.charges.Load() != 1 {
		t.Fatalf("resume: %v (charged %d)", err, f.charges.Load())
	}
	// The receipt the endpoint returned verifies on its own, under the approver's key.
	code, out, errs = f.runTo("", "-verify-receipt", rcpt, "-approver-keys", f.keysJSON)
	if code != 0 {
		t.Fatalf("-verify-receipt: %d\n%s\n%s", code, out, errs)
	}
	if fi, err := os.Stat(rcpt); err != nil || fi.Mode().Perm()&0o077 != 0 && runtime.GOOS != "windows" {
		t.Fatalf("receipt file: %v %v", fi, err)
	}
	// The default receipt, for ops, sits next to the key.
	matches, _ := filepath.Glob(filepath.Join(f.dir, "*-ops-*.receipt.json"))
	if len(matches) != 1 {
		t.Fatalf("ops receipts: %v", matches)
	}
}

// An endpoint whose digest is not the digest of the arguments it shows is refused before anything
// is signed.
func TestRemote_InconsistentDigestIsRefused(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	posts := new(atomic.Int32)
	srv := f.intake(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				posts.Add(1)
			}
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			body := strings.Replace(rec.Body.String(), `"amount":120`, `"amount":12`, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.Code)
			w.Write([]byte(body))
		})
	})
	code, out, errs := f.runTo("ops\n", f.to(srv.URL, "ops")...)
	if code != 1 || posts.Load() != 0 || len(f.decisions()) != 0 {
		t.Fatalf("exit %d, %d posts, %d decisions\n%s\n%s", code, posts.Load(), len(f.decisions()), out, errs)
	}
	contains(t, "stderr", errs, "inconsistent", "nothing was signed")
}

// An endpoint that shows other arguments than the journal holds, with a digest that matches what it
// shows, gets a signature over the shown arguments, which the application's journal refuses: nothing
// is recorded.
func TestRemote_ShownArgumentsThatAreNotTheJournalsRecordNothing(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	srv := f.intake(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			var p approvals.Pending
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Error(err)
			}
			p.Args = json.RawMessage(`{"order":1049,"amount":1}`)
			p.ArgsSHA256 = audit.ApprovalArgsDigest(agent.ApprovalSubject{RunID: p.Run, ToolUseID: p.Call, ToolName: p.Tool, Args: p.Args})
			writeTestJSON(w, p)
		})
	})
	code, out, errs := f.runTo("ops\n", f.to(srv.URL, "ops")...)
	if code != 1 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	contains(t, "stderr", errs, "invalid_approval")
	if len(f.decisions()) != 0 {
		t.Fatal("a decision over arguments the journal does not hold was recorded")
	}
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// The command refuses, with nothing sent, a call that is not waiting, an id that is not an
// approver, and a decision that already counts.
func TestRemote_Preflight(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	posts := new(atomic.Int32)
	srv := f.intake(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				posts.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	if code, _, errs := f.runTo("", f.to(srv.URL, "ops", "-yes")...); code != 0 {
		t.Fatalf("ops: %d %s", code, errs)
	}
	before := posts.Load()
	code, _, errs := f.runTo("", f.to(srv.URL, "ops", "-yes")...)
	if code != 1 || !strings.Contains(errs, "already decided") {
		t.Fatalf("ops again: %d %s", code, errs)
	}
	code, _, errs = f.runTo("", "-to", srv.URL, "-run", f.run, "-call", f.call, "-as", "mallory", "-key", f.keys["ops"], "-unverified", "-yes")
	if code != 1 || !strings.Contains(errs, "not an approver") {
		t.Fatalf("mallory: %d %s", code, errs)
	}
	if code, _, errs = f.runTo("", f.to(srv.URL, "finance", "-yes")...); code != 0 {
		t.Fatalf("finance: %d %s", code, errs)
	}
	if posts.Load() != before+1 {
		t.Fatalf("%d posts, want %d", posts.Load(), before+1)
	}
	// The gate has its approvals: a third approver is told so and sends nothing.
	code, _, errs = f.runTo("", f.to(srv.URL, "risk", "-yes")...)
	if code != 1 || posts.Load() != before+1 {
		t.Fatalf("risk after the gate passed: %d, %d posts\n%s", code, posts.Load(), errs)
	}
}

// -reject signs a rejection, and the confirmation says so.
func TestRemote_Reject(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	srv := f.intake(nil)
	code, out, errs := f.runTo("ops\n", f.to(srv.URL, "ops", "-reject")...)
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	contains(t, "output", out, "REJECT this call?", "Rejected")
	if d := f.decisions(); len(d) != 1 || d[0].Approved {
		t.Fatalf("decisions: %+v", d)
	}
}

// The endpoint's refusals are shown with its code, and a hook-refused request is a refusal.
func TestRemote_EndpointRefusals(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	srv := f.intake(nil, approvals.WithAuth(func(r *http.Request, a approvals.Access) error {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			return approvals.ErrUnauthenticated
		}
		return nil
	}))
	code, _, errs := f.runTo("", f.to(srv.URL, "ops", "-yes")...)
	if code != 1 || !strings.Contains(errs, "unauthenticated") {
		t.Fatalf("no token: %d %s", code, errs)
	}
	t.Setenv("INTAKE_TOKEN", "s3cret")
	if code, _, errs = f.runTo("", f.to(srv.URL, "ops", "-yes", "-token-env", "INTAKE_TOKEN")...); code != 0 {
		t.Fatalf("token: %d %s", code, errs)
	}
	if code, _, errs = f.runTo("", f.to(srv.URL, "finance", "-yes", "-token-env", "NO_SUCH_VAR_X")...); code != 1 || !strings.Contains(errs, "NO_SUCH_VAR_X") {
		t.Fatalf("unset token variable: %d %s", code, errs)
	}
}

// http is for this machine only; a redirect is not followed.
func TestRemote_URLRules(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	for _, u := range []string{"http://example.com", "ftp://x", "https://user:pw@example.com", "https://example.com/?a=b", "example.com", "https://"} {
		if code, _, errs := f.runTo("", f.to(u, "ops", "-yes")...); code != 1 {
			t.Errorf("%s: exit %d %s", u, code, errs)
		}
	}
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("a redirect was followed") }))
	defer other.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer redir.Close()
	if code, _, errs := f.runTo("", f.to(redir.URL, "ops", "-yes")...); code != 1 {
		t.Errorf("redirect: exit %d %s", code, errs)
	}
}

// Flags that do not go together are usage errors.
func TestRemote_Flags(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	base := f.to("https://example.com", "ops")
	for name, extra := range map[string][]string{
		"-db":          {"-db", f.db},
		"-list":        {"-list"},
		"-json":        {"-json"},
		"-client-cert": {"-client-cert", "c.pem"},
		"-unverified":  {"-unverified"},
	} {
		if code, _, _ := f.runTo("", append(base, extra...)...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
	for _, args := range [][]string{
		{"-to", "https://example.com", "-run", "r", "-call", "c", "-as", "ops", "-key", "k"},
		{"-to", "https://example.com", "-run", "r", "-call", "c", "-approver-keys", "k"},
		{"-db", f.db, "-run", f.run, "-call", f.call, "-as", "ops", "-key", f.keys["ops"], "-approver-keys", f.keysJSON, "-tls-ca", "ca.pem"},
		{"-db", f.db, "-run", f.run, "-call", f.call, "-as", "ops", "-key", f.keys["ops"], "-approver-keys", f.keysJSON, "-token-env", "X"},
	} {
		if code, _, _ := f.runTo("", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// Over TLS the command trusts only -tls-ca, and presents -client-cert; an endpoint that requires a
// client certificate refuses a command without one.
func TestRemote_TLSAndClientCertificates(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	j, closeJ := f.store()
	defer closeJ()
	verify := func(id string) (agent.ApproverVerifier, bool) { v, ok := f.pubs[id]; return v, ok }
	h, err := approvals.New(j, verify, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0),
		approvals.WithAuth(func(r *http.Request, a approvals.Access) error {
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				return approvals.ErrUnauthenticated
			}
			return nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	srv.StartTLS()
	defer srv.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	ca := filepath.Join(f.dir, "ca.pem")
	os.WriteFile(ca, caPEM, 0o600)
	certPath, keyPath := writeSelfSigned(t, f.dir)

	if code, _, errs := f.runTo("", f.to(srv.URL, "ops", "-yes")...); code != 1 || !strings.Contains(strings.ToLower(errs), "certificate") {
		t.Fatalf("no -tls-ca: %d %s", code, errs)
	}
	if code, _, errs := f.runTo("", f.to(srv.URL, "ops", "-yes", "-tls-ca", ca)...); code != 1 || !strings.Contains(errs, "unauthenticated") {
		t.Fatalf("no client certificate: %d %s", code, errs)
	}
	if code, out, errs := f.runTo("", f.to(srv.URL, "ops", "-yes", "-tls-ca", ca, "-client-cert", certPath, "-client-key", keyPath)...); code != 0 {
		t.Fatalf("mutual TLS: %d\n%s\n%s", code, out, errs)
	}
	if len(f.decisions()) != 1 {
		t.Fatalf("%d decisions", len(f.decisions()))
	}
}

// writeSelfSigned writes a self-signed ECDSA certificate and key (PEM) in dir.
func writeSelfSigned(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ops"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certPath, keyPath = filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.key")
	os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return certPath, keyPath
}

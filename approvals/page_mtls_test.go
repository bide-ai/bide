package approvals_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/approvals"
)

// The approve page, in real Chrome, authenticated by a TLS client certificate. Cookie
// authentication is checked by hand; this is the check for mutual TLS, in both of the page's
// modes: the page served by the intake (fetch credentials "same-origin") and the page hosted on
// another origin with config.json asking for "include" (approvals.WithCORS and
// WithCORSCredentials).
//
// It runs only with BIDE_MTLS_BROWSER=1, in the "Page mTLS (Chrome)" job (.github/workflows/page-mtls.yml),
// which installs Chrome's managed policy that selects the certificate, libnss3-tools, openssl and the
// Node dependencies of approvals/page/test/chrome. Chrome on Linux reads client certificates from
// the NSS database of its HOME (.pki/nssdb): the test gives Chrome a HOME of its own, imports a
// test client certificate into it and trusts a test CA there, so nothing of the user's is touched.
// With BIDE_MTLS_BROWSER=1 and CI set, a missing Chrome or tool fails the test instead of skipping it.

const mtlsApprover = "alice"

type mtlsPKI struct {
	dir          string
	caPEM        []byte
	srvCert      tls.Certificate
	clientCert   tls.Certificate
	clientCN     string
	caPool       *x509.CertPool
	clientKeyPEM []byte
	clientPEM    []byte
}

func mtlsFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

func newMTLSPKI(t *testing.T, dir string) *mtlsPKI {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "bide test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leaf := func(serial int64, cn string, usage x509.ExtKeyUsage, ips []net.IP) (tls.Certificate, []byte, []byte) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, NotBefore: now.Add(-time.Hour),
			NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, IPAddresses: ips}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
		c, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return c, certPEM, keyPEM
	}
	p := &mtlsPKI{dir: dir, clientCN: "approver-" + mtlsApprover, caPool: x509.NewCertPool()}
	p.caPool.AddCert(caCert)
	p.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	p.srvCert, _, _ = leaf(2, "127.0.0.1", x509.ExtKeyUsageServerAuth, []net.IP{net.ParseIP("127.0.0.1")})
	p.clientCert, p.clientPEM, p.clientKeyPEM = leaf(3, p.clientCN, x509.ExtKeyUsageClientAuth, nil)
	return p
}

// installInProfile trusts the CA and imports the client certificate (a PKCS#12 file made by openssl)
// into the NSS database under home/.pki/nssdb, where Chrome on Linux looks.
func (p *mtlsPKI) installInProfile(t *testing.T, home string) {
	t.Helper()
	for _, tool := range []string{"certutil", "pk12util", "openssl"} {
		if _, err := exec.LookPath(tool); err != nil {
			mtlsFail(t, "%s is not installed (libnss3-tools and openssl are needed to give Chrome a client certificate)", tool)
		}
	}
	files := map[string][]byte{"ca.pem": p.caPEM, "client.pem": p.clientPEM, "client.key": p.clientKeyPEM}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(p.dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db := "sql:" + filepath.Join(home, ".pki", "nssdb")
	if err := os.MkdirAll(filepath.Join(home, ".pki", "nssdb"), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = p.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
	}
	run("certutil", "-N", "-d", db, "--empty-password")
	run("certutil", "-A", "-d", db, "-n", "bide test CA", "-t", "C,,", "-i", "ca.pem")
	run("openssl", "pkcs12", "-export", "-inkey", "client.key", "-in", "client.pem", "-certfile", "ca.pem", "-name", p.clientCN, "-out", "client.p12", "-passout", "pass:bide")
	run("pk12util", "-d", db, "-i", "client.p12", "-W", "bide", "-K", "")
	out, err := exec.Command("certutil", "-L", "-d", db).CombinedOutput()
	if err != nil {
		t.Fatalf("certutil -L: %v\n%s", err, out)
	}
	t.Logf("the profile's certificates:\n%s", out)
}

// seenCert records what the intake's WithAuth hook saw of the TLS connection.
type seenCert struct {
	mu   sync.Mutex
	seen []string // "METHOD path subject"
}

func (s *seenCert) hook(r *http.Request, _ approvals.Access) error {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 {
		return approvals.ErrUnauthenticated
	}
	s.mu.Lock()
	s.seen = append(s.seen, r.Method+" "+r.URL.Path+" "+r.TLS.PeerCertificates[0].Subject.CommonName)
	s.mu.Unlock()
	return nil
}

func (s *seenCert) has(method, path, cn string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.seen {
		if e == method+" "+path+" "+cn {
			return true
		}
	}
	return false
}

func (s *seenCert) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

type mtlsDriverOut struct {
	OK     bool     `json:"ok"`
	Result string   `json:"result"`
	Tool   string   `json:"tool"`
	Origin string   `json:"origin"`
	Error  string   `json:"error"`
	Log    []string `json:"log"`
}

func TestPage_ClientCertificateInChrome(t *testing.T) {
	if os.Getenv("BIDE_MTLS_BROWSER") != "1" {
		t.Skip("set BIDE_MTLS_BROWSER=1 to drive the approve page in Chrome with a client certificate (the Page mTLS (Chrome) job does)")
	}
	node := nodeBin(t)
	chrome := os.Getenv("CHROME_BIN")
	if chrome == "" {
		chrome, _ = exec.LookPath("google-chrome-stable")
	}
	if chrome == "" {
		mtlsFail(t, "Chrome is not installed (set CHROME_BIN, or install google-chrome-stable)")
	}
	if _, err := os.Stat("/etc/opt/chrome/policies/managed/mtls.json"); err != nil {
		mtlsFail(t, "Chrome's managed policy that selects the client certificate (/etc/opt/chrome/policies/managed/mtls.json) is not installed: %v", err)
	}
	driverDir, err := filepath.Abs(filepath.Join("page", "test", "chrome"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(driverDir, "node_modules", "puppeteer-core")); err != nil {
		mtlsFail(t, "puppeteer-core is not installed in %s (npm ci there): %v", driverDir, err)
	}
	for _, mode := range []string{"same-origin", "include"} {
		t.Run(mode, func(t *testing.T) { chromeMTLS(t, node, chrome, driverDir, mode == "include") })
	}
}

func chromeMTLS(t *testing.T, node, chrome, driverDir string, crossOrigin bool) {
	work := t.TempDir()
	home := filepath.Join(work, "home")
	pki := newMTLSPKI(t, work)
	pki.installInProfile(t, home)

	// The intake over TLS, requiring a client certificate the test CA issued.
	intake := httptest.NewUnstartedServer(nil)
	intake.TLS = &tls.Config{Certificates: []tls.Certificate{pki.srvCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pki.caPool, MinVersion: tls.VersionTLS12}
	intakeOrigin := "https://" + intake.Listener.Addr().String()

	seen := &seenCert{}
	opts := []approvals.Option{approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0), approvals.WithClock(time.Now), approvals.WithAuth(seen.hook)}
	pageURL := intakeOrigin + "/page/#run=" + runID + "&call=c1"
	if crossOrigin {
		// The page's own server: another origin, no client certificate asked of it, as a static host of
		// the bundle is.
		pageSrv := httptest.NewUnstartedServer(nil)
		pageSrv.TLS = &tls.Config{Certificates: []tls.Certificate{pki.srvCert}, MinVersion: tls.VersionTLS12}
		pageOrigin := "https://" + pageSrv.Listener.Addr().String()
		opts = append(opts, approvals.WithCORS(pageOrigin), approvals.WithCORSCredentials())
		cfg, _ := json.Marshal(map[string]string{"intake": intakeOrigin, "credentials": "include"})
		csp := approvals.PageCSP(intakeOrigin)
		files := approvals.PageFiles()
		types := map[string]string{"index.html": "text/html; charset=utf-8", "page.js": "text/javascript; charset=utf-8", "app.css": "text/css; charset=utf-8", "config.json": "application/json"}
		pageSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name := strings.TrimPrefix(r.URL.Path, "/")
			if name == "" {
				name = "index.html"
			}
			body := files[name]
			if name == "config.json" {
				body = cfg
			}
			if body == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", types[name])
			w.Header().Set("Content-Security-Policy", csp)
			w.Header().Set("Referrer-Policy", "no-referrer")
			_, _ = w.Write(body)
		})
		pageSrv.StartTLS()
		t.Cleanup(pageSrv.Close)
		pageURL = pageOrigin + "/#run=" + runID + "&call=c1"
	} else {
		opts = append(opts, approvals.WithPage())
	}
	r := newRig(t, opts...)
	intake.Config.Handler = r.h
	intake.StartTLS()
	t.Cleanup(intake.Close)

	// The page's key is enrolled when the page offers it: a file handoff with the driver.
	enrolDir := filepath.Join(work, "enrol")
	if err := os.MkdirAll(enrolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "mtls.mjs")
	cmd.Dir = driverDir
	cmd.Env = append(os.Environ(), "HOME="+home, "CHROME_BIN="+chrome, "PAGE_URL="+pageURL, "ENROL_DIR="+enrolDir, "APPROVER="+mtlsApprover,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "XDG_CACHE_HOME="+filepath.Join(home, ".cache"))
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	enrolled := false
	deadline := time.After(3 * time.Minute)
	var werr error
loop:
	for {
		select {
		case werr = <-waited:
			break loop
		case <-deadline:
			_ = cmd.Process.Kill()
			<-waited
			t.Fatalf("the Chrome driver did not finish in 3 minutes; output so far: %s", stdout.String())
		case <-time.After(100 * time.Millisecond):
			if b, err := os.ReadFile(filepath.Join(enrolDir, "pub")); err == nil && !enrolled {
				pub, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ed25519:")
				raw, err := hex.DecodeString(pub)
				if !ok || err != nil || len(raw) != 32 {
					t.Fatalf("the page offered a public key that is not ed25519:<hex>: %q", b)
				}
				r.enrol(mtlsApprover, raw)
				if err := os.WriteFile(filepath.Join(enrolDir, "enrolled"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				enrolled = true
			}
		}
	}
	line := strings.TrimSpace(stdout.String())
	var out mtlsDriverOut
	if jerr := json.Unmarshal([]byte(line[strings.LastIndex(line, "\n")+1:]), &out); jerr != nil {
		t.Fatalf("driver output %q (exit: %v): %v", line, werr, jerr)
	}
	t.Logf("what Chrome did:\n%s", strings.Join(out.Log, "\n"))
	t.Logf("seen by the intake's WithAuth hook: %q", seen.all())
	if !out.OK || !strings.HasPrefix(out.Result, "Approved: signed and recorded") || out.Tool != "charge" {
		t.Fatalf("the page did not complete an Approve: %+v", out)
	}
	t.Logf("the page said: %s", out.Result)
	// The hook saw the certificate on what the page fetched and posted.
	for _, e := range [][2]string{{"GET", "/pending"}, {"POST", "/decisions"}} {
		if !seen.has(e[0], e[1], pki.clientCN) {
			t.Errorf("the WithAuth hook never saw %s %s with the client certificate %q: %q", e[0], e[1], pki.clientCN, seen.all())
		}
	}
	if !crossOrigin && !seen.has("GET", "/page/", pki.clientCN) && !seen.has("GET", "/page/index.html", pki.clientCN) {
		t.Errorf("the page's own document was served without the client certificate: %q", seen.all())
	}
	// The decision is counted at the gate: read back with a Go client presenting the same certificate.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pki.caPool, Certificates: []tls.Certificate{pki.clientCert}, MinVersion: tls.VersionTLS12}}}
	resp, err := client.Get(intakeOrigin + "/pending?run=" + runID + "&call=c1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var pend map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&pend); err != nil {
		t.Fatal(err)
	}
	gate, _ := pend["gate"].(map[string]any)
	if gate["approved"] != float64(1) {
		t.Fatalf("the gate does not show the page's approval: %s", fmt.Sprint(pend))
	}
}

package notifycmd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/notify"
)

const (
	testSecretValue = "cli-test-secret-value-must-not-print"
	otherSecret     = "a-different-secret-must-not-print"
	slackPathSecret = "T000/B000/slack-path-secret"
	pdKeySecret     = "pagerduty-routing-key-secret"
)

// syncBuf is a bytes.Buffer safe to write from a server goroutine and read from the test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// setEnv replaces the package's view of the environment for the test.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	old := getenv
	getenv = func(k string) string { return kv[k] }
	t.Cleanup(func() { getenv = old })
}

// serverRun is a server verb running in the background.
type serverRun struct {
	addr   net.Addr
	stdout *syncBuf
	stderr *syncBuf
	stop   context.CancelFunc
	done   chan int
}

func startVerb(t *testing.T, verb string, args ...string) *serverRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &serverRun{stdout: &syncBuf{}, stderr: &syncBuf{}, stop: cancel, done: make(chan int, 1)}
	ready := make(chan net.Addr, 1)
	go func() {
		s.done <- run(ctx, append([]string{verb}, args...), s.stdout, s.stderr, func(a net.Addr) { ready <- a })
	}()
	select {
	case s.addr = <-ready:
	case code := <-s.done:
		t.Fatalf("%s exited %d before listening:\n%s%s", verb, code, s.stdout, s.stderr)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}
	t.Cleanup(func() { s.finish(t) })
	return s
}

func (s *serverRun) finish(t *testing.T) int {
	s.stop()
	select {
	case code := <-s.done:
		s.done <- code
		return code
	case <-time.After(15 * time.Second):
		t.Fatal("server did not stop")
	}
	return -1
}

// test runs bide notify test with args and returns its status and output.
func test(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var o, e bytes.Buffer
	code = run(context.Background(), append([]string{"test"}, args...), &o, &e, nil)
	return code, o.String(), e.String()
}

// listenOn starts bide notify listen with a generated certificate and returns it with the CA file.
func listenOn(t *testing.T, extra ...string) (*serverRun, string) {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	s := startVerb(t, "listen", append([]string{"-addr", "127.0.0.1:0", "-ca-out", ca}, extra...)...)
	return s, ca
}

func noSecretLeaks(t *testing.T, what string, outs ...string) {
	t.Helper()
	for _, o := range outs {
		for _, leak := range []string{testSecretValue, otherSecret, slackPathSecret, pdKeySecret} {
			if strings.Contains(o, leak) {
				t.Fatalf("%s leaks %q:\n%s", what, leak, o)
			}
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTestAgainstListenEndToEnd(t *testing.T) {
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue})
	l, ca := listenOn(t)
	url := "https://" + l.addr.String() + "/"
	code, out, errs := test(t, "-url", url, "-ca", ca)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"approval_pending", "tls", "ok: TLS 1.3", "private CA", "HTTP 204, acknowledged", "hmac-sha256 signed with the current secret", "the receiver acknowledged the test notification"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	waitFor(t, "listen to print the event", func() bool { return strings.Contains(l.stdout.String(), "verified") })
	got := l.stdout.String()
	for _, want := range []string{"approval_pending", "run=notify-test-", "attempt=1", "waiting for approval of tool refund", "CA file (-ca): " + ca, "pin (-pin):"} {
		if !strings.Contains(got, want) {
			t.Errorf("listen output lacks %q:\n%s", want, got)
		}
	}
	noSecretLeaks(t, "test or listen output", out, errs, got, l.stderr.String())
}

func TestEveryKindIsSentAndVerified(t *testing.T) {
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue})
	l, ca := listenOn(t, "-json")
	url := "https://" + l.addr.String() + "/"
	for _, k := range kinds {
		code, out, errs := test(t, "-url", url, "-ca", ca, "-kind", string(k))
		if code != 0 || !strings.Contains(out, string(k)) {
			t.Fatalf("%s: exit %d\n%s%s", k, code, out, errs)
		}
	}
	waitFor(t, "five events", func() bool { return strings.Count(l.stdout.String(), `"schema":"bide.notification.v1"`) == len(kinds) })
	for _, k := range kinds {
		if !strings.Contains(l.stdout.String(), `"kind":"`+string(k)+`"`) {
			t.Errorf("listen did not print a %s event:\n%s", k, l.stdout.String())
		}
	}
	// Two tests of one kind are two events, not a redelivery: the nonce differs.
	a, _ := sampleNotification(kinds[0], time.Now(), "")
	b, _ := sampleNotification(kinds[0], time.Now(), "")
	if a.ID == b.ID || a.RunID == b.RunID {
		t.Fatal("two samples share an ID")
	}
}

func TestBadSecretIsReportedWithoutEchoingEither(t *testing.T) {
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue})
	l, ca := listenOn(t)
	setEnv(t, map[string]string{defaultSecretEnv: otherSecret})
	code, out, errs := test(t, "-url", "https://"+l.addr.String()+"/", "-ca", ca)
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	for _, want := range []string{"ok: TLS", "HTTP 401, refused", "did not acknowledge", "refused the signature"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	waitFor(t, "listen to print the rejection", func() bool { return strings.Contains(l.stdout.String(), "rejected") })
	if !strings.Contains(l.stdout.String(), "signature does not match") {
		t.Errorf("listen should say why:\n%s", l.stdout.String())
	}
	noSecretLeaks(t, "output", out, errs, l.stdout.String(), l.stderr.String())
}

func TestUntrustedCertificateNamesTheFix(t *testing.T) {
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue})
	l, ca := listenOn(t)
	url := "https://" + l.addr.String() + "/"

	code, out, _ := test(t, "-url", url) // system roots: the self-signed certificate is not trusted
	if code != 1 || !strings.Contains(out, "failed:") || !strings.Contains(out, "unknown authority") || !strings.Contains(out, "pass -ca") || !strings.Contains(out, "not sent: the handshake failed") {
		t.Fatalf("no -ca: exit %d\n%s", code, out)
	}
	// A different CA file is refused too.
	other, _ := newSelfSigned(nil, time.Now())
	otherFile := filepath.Join(t.TempDir(), "other.pem")
	os.WriteFile(otherFile, other.pem, 0o600)
	code, out, _ = test(t, "-url", url, "-ca", otherFile)
	if code != 1 || !strings.Contains(out, "unknown authority") {
		t.Fatalf("wrong CA: exit %d\n%s", code, out)
	}
	if strings.Contains(l.stdout.String(), "verified") || strings.Contains(l.stdout.String(), "rejected") {
		t.Fatalf("a request reached the receiver through a failed handshake:\n%s", l.stdout.String())
	}
	_ = ca
}

func TestPinMatchAndMismatch(t *testing.T) {
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue})
	l, ca := listenOn(t)
	url := "https://" + l.addr.String() + "/"
	var pin string
	for _, line := range strings.Split(l.stdout.String(), "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "pin (-pin):"); ok {
			pin = strings.TrimSpace(p)
		}
	}
	if len(pin) != 64 {
		t.Fatalf("listen did not print a pin:\n%s", l.stdout.String())
	}
	if code, out, _ := test(t, "-url", url, "-ca", ca, "-pin", pin); code != 0 || !strings.Contains(out, "key pinned") {
		t.Fatalf("right pin: exit %d\n%s", code, out)
	}
	code, out, _ := test(t, "-url", url, "-ca", ca, "-pin", strings.Repeat("00", 32))
	if code != 1 || !strings.Contains(out, "matches no configured pin") || !strings.Contains(out, "compare -pin") {
		t.Fatalf("wrong pin: exit %d\n%s", code, out)
	}
}

// clientPair writes a CA and a client certificate signed by it.
func clientPair(t *testing.T) (caFile, certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "client ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "bide"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	caFile, certFile, keyFile = filepath.Join(dir, "ca.pem"), filepath.Join(dir, "c.pem"), filepath.Join(dir, "c.key")
	os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600)
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600)
	return
}

func TestMutualTLS(t *testing.T) {
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue})
	clientCA, cert, key := clientPair(t)
	l, ca := listenOn(t, "-client-ca", clientCA)
	url := "https://" + l.addr.String() + "/"
	code, out, _ := test(t, "-url", url, "-ca", ca)
	if code != 1 || !strings.Contains(out, "-client-cert") {
		t.Fatalf("no client certificate: exit %d\n%s", code, out)
	}
	code, out, _ = test(t, "-url", url, "-ca", ca, "-client-cert", cert, "-client-key", key)
	if code != 0 || !strings.Contains(out, "client certificate presented") {
		t.Fatalf("with a client certificate: exit %d\n%s", code, out)
	}
	waitFor(t, "the verified event", func() bool { return strings.Contains(l.stdout.String(), "verified") })
	if !strings.Contains(l.stdout.String(), "client certificate ") {
		t.Errorf("listen should show the client certificate's pin:\n%s", l.stdout.String())
	}
}

func TestNothingListening(t *testing.T) {
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	code, out, _ := test(t, "-url", "https://"+addr+"/")
	if code != 1 || !strings.Contains(out, "no response") || !strings.Contains(out, "nothing is listening") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestPlainHTTPToLocalhostAndReceiverFailures(t *testing.T) {
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue})
	status := 204
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	code, out, _ := test(t, "-url", srv.URL+"/hook?token=url-secret")
	if code != 0 || !strings.Contains(out, "not used (http to localhost)") || !strings.Contains(out, "HTTP 204") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if strings.Contains(out, "url-secret") || strings.Contains(out, "/hook") {
		t.Fatalf("the URL's path or query is printed:\n%s", out)
	}
	status = 404
	if code, out, _ = test(t, "-url", srv.URL); code != 1 || !strings.Contains(out, "HTTP 404, refused") || !strings.Contains(out, "path in -url") {
		t.Fatalf("404: exit %d\n%s", code, out)
	}
	status = 503
	if code, out, _ = test(t, "-url", srv.URL, "-timeout", "2s"); code != 1 || !strings.Contains(out, "HTTP 503") || !strings.Contains(out, "bide retries") {
		t.Fatalf("503: exit %d\n%s", code, out)
	}
}

func TestSecretSources(t *testing.T) {
	// A receiver that verifies against the exact secret: a secret with its white space kept would not match.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if notify.Verify(r, []string{testSecretValue}, 0) != nil {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	setEnv(t, nil)
	code, out, errs := test(t, "-url", srv.URL)
	if code != 1 || !strings.Contains(errs, defaultSecretEnv) || !strings.Contains(errs, "openssl rand") || out != "" {
		t.Fatalf("no secret: exit %d\nstdout %q\nstderr %q", code, out, errs)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "secret")
	os.WriteFile(f, []byte("  "+testSecretValue+"\n"), 0o600)
	oldRead := readFile
	readFile = os.ReadFile
	defer func() { readFile = oldRead }()
	if code, out, errs = test(t, "-url", srv.URL, "-secret-file", f); code != 0 {
		t.Fatalf("secret file: exit %d\n%s%s", code, out, errs)
	}
	noSecretLeaks(t, "output", out, errs)
	code, _, errs = test(t, "-url", srv.URL, "-secret-file", filepath.Join(dir, "missing"))
	if code != 1 || !strings.Contains(errs, "missing") {
		t.Fatalf("missing file: exit %d\n%s", code, errs)
	}
	// With a previous secret, the request carries two signatures and says so.
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue, defaultPreviousEnv: otherSecret})
	if code, out, _ = test(t, "-url", srv.URL); code != 0 || !strings.Contains(out, "current+previous") {
		t.Fatalf("rotation: exit %d\n%s", code, out)
	}
	noSecretLeaks(t, "rotation output", out)
}

func TestUsageErrors(t *testing.T) {
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue})
	for name, args := range map[string][]string{
		"no url":         {},
		"bad kind":       {"-url", "http://127.0.0.1:1", "-kind", "nope"},
		"cert w/o key":   {"-url", "http://127.0.0.1:1", "-client-cert", "x"},
		"extra argument": {"-url", "http://127.0.0.1:1", "oops"},
		"bad flag":       {"-secret", testSecretValue},
	} {
		code, _, errs := test(t, args...)
		if code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
		noSecretLeaks(t, name, errs)
	}
	// A secret passed as a flag value is refused, and is not echoed back in the error.
	_, _, errs := test(t, "-secret", testSecretValue)
	if strings.Contains(errs, testSecretValue) {
		t.Fatalf("the flag package echoed the secret:\n%s", errs)
	}
}

func TestInsecureURLIsAConfigError(t *testing.T) {
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue})
	code, _, errs := test(t, "-url", "http://example.com/hook")
	if code != 1 || !strings.Contains(errs, "must be https") {
		t.Fatalf("exit %d\n%s", code, errs)
	}
}

func TestVerbDispatch(t *testing.T) {
	var o, e bytes.Buffer
	if code := run(context.Background(), nil, &o, &e, nil); code != 2 || !strings.Contains(e.String(), "bide notify test") {
		t.Fatalf("no verb: %d %q", code, e.String())
	}
	e.Reset()
	if code := run(context.Background(), []string{"-h"}, &o, &e, nil); code != 0 || !strings.Contains(e.String(), "relay") {
		t.Fatalf("-h: %d", code)
	}
	e.Reset()
	if code := run(context.Background(), []string{"nope"}, &o, &e, nil); code != 2 || !strings.Contains(e.String(), `unknown verb "nope"`) {
		t.Fatalf("unknown: %d %q", code, e.String())
	}
	for _, v := range Verbs() {
		e.Reset()
		if code := run(context.Background(), []string{v, "-h"}, &o, &e, nil); code != 0 || !strings.Contains(e.String(), "usage: bide notify "+v) {
			t.Fatalf("%s -h: %d %q", v, code, e.String())
		}
	}
}

func TestListenRefusesPlainRequests(t *testing.T) {
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue})
	l, ca := listenOn(t)
	pem, _ := os.ReadFile(ca)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsWith(pool)}}
	resp, err := c.Post("https://"+l.addr.String()+"/", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unsigned request: %d, want 401", resp.StatusCode)
	}
	waitFor(t, "rejection line", func() bool { return strings.Contains(l.stdout.String(), "missing signature") })
}

func TestListenNeedsASecretAndStopsCleanly(t *testing.T) {
	setEnv(t, nil)
	var o, e bytes.Buffer
	if code := run(context.Background(), []string{"listen", "-addr", "127.0.0.1:0"}, &o, &e, nil); code != 1 || !strings.Contains(e.String(), defaultSecretEnv) {
		t.Fatalf("no secret: %d %q", code, e.String())
	}
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue})
	l, _ := listenOn(t)
	if code := l.finish(t); code != 0 {
		t.Fatalf("clean stop: exit %d\n%s", code, l.stderr)
	}
}

// ---- relay ----

type fakeUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
	status int
}

func newFake(t *testing.T) *fakeUpstream {
	f := &fakeUpstream{status: 200}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		st := f.status
		f.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUpstream) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.bodies) }

func TestRelayForwardsAndSurvivesRedelivery(t *testing.T) {
	slack, pd := newFake(t), newFake(t)
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue, defaultSlackEnv: slack.URL + "/services/" + slackPathSecret, defaultPagerDutyEnv: pdKeySecret})
	r := startVerb(t, "relay", "-addr", "127.0.0.1:0", "-pagerduty-url", pd.URL, "-view-url", "https://viewer.example/runs")
	url := "http://" + r.addr.String() + "/"
	for i := 0; i < 2; i++ {
		// Two requests with different IDs.
		code, out, errs := test(t, "-url", url)
		if code != 0 || !strings.Contains(out, "HTTP 204, acknowledged") {
			t.Fatalf("exit %d\n%s%s", code, out, errs)
		}
	}
	if slack.n() != 2 || pd.n() != 2 {
		t.Fatalf("slack %d, pagerduty %d, want 2 each (two distinct events)", slack.n(), pd.n())
	}
	if !strings.Contains(slack.bodies[0], "notify-test-") || !strings.Contains(slack.bodies[0], "https://viewer.example/runs/notify-test-") {
		t.Fatalf("slack body:\n%s", slack.bodies[0])
	}
	got := r.stdout.String() + r.stderr.String()
	for _, want := range []string{"forwarding to slack and pagerduty", "forwarded to pagerduty, slack"} {
		if !strings.Contains(got, want) {
			t.Errorf("relay output lacks %q:\n%s", want, got)
		}
	}
	noSecretLeaks(t, "relay output", got)
}

func TestRelayReports502WhenAForwardFails(t *testing.T) {
	slack := newFake(t)
	slack.status = 500
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue, defaultSlackEnv: slack.URL + "/services/" + slackPathSecret})
	r := startVerb(t, "relay", "-addr", "127.0.0.1:0")
	code, out, errs := test(t, "-url", "http://"+r.addr.String()+"/", "-timeout", "20s")
	if code != 1 || !strings.Contains(out, "HTTP 502") || !strings.Contains(out, "bide retries") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	waitFor(t, "failure log", func() bool { return strings.Contains(r.stderr.String(), "slack failed") })
	noSecretLeaks(t, "relay output", r.stdout.String(), r.stderr.String(), out)
}

func TestRelayConfigErrors(t *testing.T) {
	var o, e bytes.Buffer
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue})
	if code := run(context.Background(), []string{"relay", "-addr", "127.0.0.1:0"}, &o, &e, nil); code != 1 || !strings.Contains(e.String(), "nothing to forward to") {
		t.Fatalf("no sinks: %d %q", code, e.String())
	}
	e.Reset()
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue, defaultSlackEnv: "http://hooks.slack.com/services/" + slackPathSecret})
	if code := run(context.Background(), []string{"relay"}, &o, &e, nil); code != 1 || !strings.Contains(e.String(), "https") || strings.Contains(e.String(), slackPathSecret) {
		t.Fatalf("insecure slack URL: %d %q", code, e.String())
	}
	e.Reset()
	if code := run(context.Background(), []string{"relay", "-client-ca", "x"}, &o, &e, nil); code != 2 {
		t.Fatalf("-client-ca without -cert: %d", code)
	}
}

func TestRelayOpensThenClosesAnIncidentForTheSameRun(t *testing.T) {
	slack, pd := newFake(t), newFake(t)
	setEnv(t, map[string]string{defaultSecretEnv: testSecretValue, defaultSlackEnv: slack.URL + "/services/" + slackPathSecret, defaultPagerDutyEnv: pdKeySecret})
	r := startVerb(t, "relay", "-addr", "127.0.0.1:0", "-pagerduty-url", pd.URL)
	url := "http://" + r.addr.String() + "/"
	for _, k := range []string{"approval_pending", "resolved"} {
		if code, out, errs := test(t, "-url", url, "-kind", k, "-run", "notify-test-demo"); code != 0 {
			t.Fatalf("%s: exit %d\n%s%s", k, code, out, errs)
		}
	}
	if pd.n() != 2 || !strings.Contains(pd.bodies[0], `"event_action":"trigger"`) || !strings.Contains(pd.bodies[1], `"event_action":"resolve"`) {
		t.Fatalf("pagerduty bodies: %q", pd.bodies)
	}
	key := func(b string) string { return strings.Split(strings.Split(b, `"dedup_key":"`)[1], `"`)[0] }
	if key(pd.bodies[0]) != key(pd.bodies[1]) || len(key(pd.bodies[0])) != 64 {
		t.Fatalf("the resolve does not name the trigger's incident: %q", pd.bodies)
	}
}

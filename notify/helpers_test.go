package notify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

var serial int64

func nextSerial() *big.Int { serial++; return big.NewInt(serial + 1000) }

func newCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          nextSerial(),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

type leaf struct {
	tls     tls.Certificate
	x509    *x509.Certificate
	certPEM []byte
	keyPEM  []byte
}

// issue makes a leaf for a server (127.0.0.1, localhost) or a client.
func (ca *testCA) issue(t *testing.T, cn string, server bool) leaf {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
		tmpl.DNSNames = []string{"localhost"}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	l := leaf{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}),
	}
	l.x509, _ = x509.ParseCertificate(der)
	l.tls = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: l.x509}
	return l
}

func (l leaf) write(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	certFile, keyFile = filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	writeFile(t, certFile, l.certPEM)
	writeFile(t, keyFile, l.keyPEM)
	return
}

// writeFile writes and moves the mtime forward, so a rewrite within one timestamp tick is seen.
var mtimeTick int

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	mtimeTick++
	mt := time.Now().Add(time.Duration(mtimeTick) * time.Hour)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// rec is one request a test server saw.
type rec struct {
	header http.Header
	body   []byte
	peer   []*x509.Certificate // client certificates presented
	tls    uint16
}

type testServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []rec
	// handle, if set, picks the response for request number i (from 0); it returns the status.
	handle func(i int, w http.ResponseWriter, r *http.Request) int
}

func (s *testServer) seen() []rec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rec(nil), s.reqs...)
}

type srvOpt func(*tls.Config)

// newTLSServer starts an HTTPS server with cert; opts adjust its TLS config.
func newTLSServer(t *testing.T, cert tls.Certificate, opts ...srvOpt) *testServer {
	t.Helper()
	ts := &testServer{}
	ts.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ts.mu.Lock()
		i := len(ts.reqs)
		var peer []*x509.Certificate
		var ver uint16
		if r.TLS != nil {
			peer, ver = r.TLS.PeerCertificates, r.TLS.Version
		}
		ts.reqs = append(ts.reqs, rec{header: r.Header.Clone(), body: b, peer: peer, tls: ver})
		ts.mu.Unlock()
		status := 200
		if ts.handle != nil {
			status = ts.handle(i, w, r)
		}
		w.WriteHeader(status)
	}))
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	for _, o := range opts {
		o(cfg)
	}
	ts.Server.TLS = cfg
	ts.Server.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func requireClientCert(ca *testCA) srvOpt {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return func(c *tls.Config) { c.ClientAuth = tls.RequireAndVerifyClientCert; c.ClientCAs = pool }
}

// testSecret is a signing secret that must never appear in an error, a payload or a header value.
const testSecret = "s3cr3t-signing-key-do-not-leak"

// fastWebhook makes a Webhook whose backoff does not sleep: sleeps are recorded.
func fastWebhook(t *testing.T, rawURL string, opts ...Option) (*Webhook, *[]time.Duration) {
	t.Helper()
	opts = append([]Option{WithSecrets(testSecret, "")}, opts...)
	w, err := NewWebhook(rawURL, opts...)
	if err != nil {
		t.Fatal(err)
	}
	var sleeps []time.Duration
	w.sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return ctx.Err()
	}
	w.jitter = func(time.Duration) time.Duration { return 0 }
	return w, &sleeps
}

func sampleApproval() agent.Notification {
	return agent.Notification{
		ID:        agent.NotifyID("approval:orders-1042:call_01:0"),
		Key:       "approval:orders-1042:call_01:0",
		Kind:      agent.NotificationApprovalPending,
		RunID:     "orders-1042",
		EventAt:   time.Date(2026, 10, 13, 9, 14, 5, 0, time.UTC),
		Observed:  time.Date(2026, 10, 13, 9, 14, 7, 0, time.UTC),
		Deadline:  time.Date(2026, 10, 13, 9, 19, 7, 0, time.UTC),
		Attempt:   1,
		Approval:  &agent.NotifyApproval{ToolUseID: "call_01", ToolName: "refund", Need: 2, Of: 3, Approvers: []string{"alice", "bob", "carol"}},
		Principal: &agent.Principal{OnBehalfOf: "desk-7"},
	}
}

// hexMAC is an independent computation of the documented signature: HMAC-SHA256(secret, msg).
func hexMAC(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func encodeB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

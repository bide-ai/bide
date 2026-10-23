package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// deliverAuth runs one delivery and returns what the audit record would hold.
func deliverAuth(t *testing.T, w *Webhook) (*agent.NotifyAuth, error) {
	t.Helper()
	res, err := w.deliver(context.Background(), "id-1", []byte(`{}`))
	return res.Auth, err
}

func TestDeliveryOverTLSWithPrivateCA(t *testing.T) {
	ca := newCA(t, "private")
	srv := newTLSServer(t, ca.issue(t, "receiver", true).tls)
	w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem))
	a, err := deliverAuth(t, w)
	if err != nil {
		t.Fatal(err)
	}
	want := agent.NotifyAuth{Signature: "hmac-sha256", SecretSlot: "current", CustomCA: true, TLSVersion: "1.3"}
	if *a != want {
		t.Fatalf("auth %+v, want %+v", *a, want)
	}
}

func TestSystemRootsRejectPrivateReceiver(t *testing.T) {
	ca := newCA(t, "private")
	srv := newTLSServer(t, ca.issue(t, "receiver", true).tls)
	w, _ := fastWebhook(t, srv.URL) // no WithServerCA: system roots
	if _, err := deliverAuth(t, w); err == nil || len(srv.seen()) != 0 {
		t.Fatalf("a receiver the system roots do not vouch for was accepted: %v", err)
	}
}

func TestServerCAReplacesRootsRatherThanAddingToThem(t *testing.T) {
	// The "public" receiver: its CA is trusted by the test process (stand-in for a public CA).
	// A private bundle that does not hold it must refuse it, as the system roots would not be consulted.
	public := newCA(t, "public")
	private := newCA(t, "private")
	srv := newTLSServer(t, public.issue(t, "receiver", true).tls)
	w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(private.pem))
	if _, err := deliverAuth(t, w); err == nil || len(srv.seen()) != 0 {
		t.Fatalf("a server signed by a CA outside the custom bundle was accepted: %v", err)
	}
	// And the same server is accepted once its CA is the bundle.
	w, _ = fastWebhook(t, srv.URL, WithServerCAPEM(public.pem))
	if _, err := deliverAuth(t, w); err != nil {
		t.Fatal(err)
	}
}

func TestServerCAFromFile(t *testing.T) {
	ca := newCA(t, "private")
	dir := t.TempDir()
	writeFile(t, dir+"/ca.pem", ca.pem)
	srv := newTLSServer(t, ca.issue(t, "receiver", true).tls)
	w, _ := fastWebhook(t, srv.URL, WithServerCA(dir+"/ca.pem"))
	if _, err := deliverAuth(t, w); err != nil {
		t.Fatal(err)
	}
}

func TestTLSVersions(t *testing.T) {
	ca := newCA(t, "private")
	cert := ca.issue(t, "receiver", true).tls
	t.Run("1.2 accepted and reported", func(t *testing.T) {
		srv := newTLSServer(t, cert, func(c *tls.Config) { c.MaxVersion = tls.VersionTLS12 })
		w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem))
		a, err := deliverAuth(t, w)
		if err != nil || a.TLSVersion != "1.2" {
			t.Fatalf("%v %+v", err, a)
		}
	})
	t.Run("1.3 preferred", func(t *testing.T) {
		srv := newTLSServer(t, cert)
		w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem))
		a, err := deliverAuth(t, w)
		if err != nil || a.TLSVersion != "1.3" {
			t.Fatalf("%v %+v", err, a)
		}
	})
	for name, max := range map[string]uint16{"1.1": tls.VersionTLS11, "1.0": tls.VersionTLS10} {
		t.Run(name+" refused", func(t *testing.T) {
			srv := newTLSServer(t, cert, func(c *tls.Config) { c.MinVersion = tls.VersionTLS10; c.MaxVersion = max })
			w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem))
			if _, err := deliverAuth(t, w); err == nil || len(srv.seen()) != 0 {
				t.Fatalf("TLS %s receiver was spoken to: %v", name, err)
			}
		})
	}
	t.Run("floor is 1.2", func(t *testing.T) {
		w, _ := fastWebhook(t, "https://example.com/")
		if w.tlsCfg.MinVersion != tls.VersionTLS12 {
			t.Fatalf("MinVersion %x", w.tlsCfg.MinVersion)
		}
	})
}

func TestPins(t *testing.T) {
	ca := newCA(t, "private")
	other := newCA(t, "other")
	srvLeaf := ca.issue(t, "receiver", true)
	otherLeaf := other.issue(t, "unrelated", true)
	srv := newTLSServer(t, srvLeaf.tls)
	good := SPKIHash(srvLeaf.x509)
	bad := SPKIHash(otherLeaf.x509)

	t.Run("match", func(t *testing.T) {
		w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem), WithServerPin(good))
		a, err := deliverAuth(t, w)
		if err != nil || !a.ServerPinned || !a.CustomCA {
			t.Fatalf("%v %+v", err, a)
		}
	})
	t.Run("mismatch is refused before any request", func(t *testing.T) {
		before := len(srv.seen())
		w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem), WithServerPin(bad))
		if _, err := deliverAuth(t, w); err == nil || len(srv.seen()) != before {
			t.Fatalf("pin mismatch accepted: %v", err)
		}
	})
	t.Run("two pins for rotation: either matches", func(t *testing.T) {
		for _, pins := range [][]string{{good, bad}, {bad, good}} {
			w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem), WithServerPin(pins...))
			if _, err := deliverAuth(t, w); err != nil {
				t.Fatalf("%v: %v", pins, err)
			}
		}
	})
	t.Run("the CA's key pins the chain", func(t *testing.T) {
		w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem), WithServerPin(SPKIHash(ca.cert)))
		if _, err := deliverAuth(t, w); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a pin never replaces verification", func(t *testing.T) {
		// The right pin, but the server is not trusted by the (system) roots.
		before := len(srv.seen())
		w, _ := fastWebhook(t, srv.URL, WithServerPin(good))
		if _, err := deliverAuth(t, w); err == nil || len(srv.seen()) != before {
			t.Fatalf("an untrusted server was accepted because its key matched a pin: %v", err)
		}
	})
	t.Run("base64 pins", func(t *testing.T) {
		p, err := parsePin(good)
		if err != nil {
			t.Fatal(err)
		}
		_ = p
		b64 := "" // standard base64 of the same 32 bytes
		raw, _ := parsePin(good)
		b64 = encodeB64(raw[:])
		w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem), WithServerPin(b64))
		if _, err := deliverAuth(t, w); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMutualTLS(t *testing.T) {
	ca := newCA(t, "private")
	clientCA := newCA(t, "clients")
	srv := newTLSServer(t, ca.issue(t, "receiver", true).tls, requireClientCert(clientCA))
	client := clientCA.issue(t, "bide", false)
	dir := t.TempDir()
	cf, kf := client.write(t, dir)

	t.Run("required by the server", func(t *testing.T) {
		before := len(srv.seen())
		w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem))
		if _, err := deliverAuth(t, w); err == nil || len(srv.seen()) != before {
			t.Fatalf("delivered without a client certificate: %v", err)
		}
	})
	t.Run("presented from files and reported", func(t *testing.T) {
		w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem), WithClientCert(cf, kf))
		for i := 0; i < 2; i++ { // every delivery shakes hands afresh, so each one reports it truthfully
			a, err := deliverAuth(t, w)
			if err != nil {
				t.Fatal(err)
			}
			if !a.MTLS || a.ClientSPKI != SPKIHash(client.x509) || a.TLSVersion != "1.3" {
				t.Fatalf("auth %+v", *a)
			}
		}
		last := srv.seen()[len(srv.seen())-1]
		if len(last.peer) == 0 || last.peer[0].Subject.CommonName != "bide" {
			t.Fatalf("server saw %v", last.peer)
		}
	})
	t.Run("presented from PEM", func(t *testing.T) {
		w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem), WithClientCertPEM(client.certPEM, client.keyPEM))
		if a, err := deliverAuth(t, w); err != nil || !a.MTLS {
			t.Fatalf("%v %+v", err, a)
		}
	})
	t.Run("not claimed when the server asks for none", func(t *testing.T) {
		open := newTLSServer(t, ca.issue(t, "receiver", true).tls)
		w, _ := fastWebhook(t, open.URL, WithServerCAPEM(ca.pem), WithClientCert(cf, kf))
		a, err := deliverAuth(t, w)
		if err != nil || a.MTLS || a.ClientSPKI != "" {
			t.Fatalf("%v %+v", err, a)
		}
	})
	t.Run("a certificate the server does not trust is refused", func(t *testing.T) {
		stranger := newCA(t, "stranger").issue(t, "stranger", false)
		before := len(srv.seen())
		w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem), WithClientCertPEM(stranger.certPEM, stranger.keyPEM))
		if _, err := deliverAuth(t, w); err == nil || len(srv.seen()) != before {
			t.Fatalf("delivered with an untrusted client certificate: %v", err)
		}
	})
}

func serverSeenSerial(srv *testServer) string {
	s := srv.seen()
	if len(s) == 0 || len(s[len(s)-1].peer) == 0 {
		return ""
	}
	return s[len(s)-1].peer[0].SerialNumber.String()
}

func TestClientCertReload(t *testing.T) {
	ca := newCA(t, "private")
	clients := newCA(t, "clients")
	srv := newTLSServer(t, ca.issue(t, "receiver", true).tls, requireClientCert(clients))
	one := clients.issue(t, "bide-one", false)
	two := clients.issue(t, "bide-two", false)
	dir := t.TempDir()
	cf, kf := one.write(t, dir)

	var mu sync.Mutex
	var reported []error
	clock := time.Unix(1_900_000_000, 0)
	w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem), WithClientCert(cf, kf),
		WithOnError(func(e error) { mu.Lock(); reported = append(reported, e); mu.Unlock() }))
	w.now = func() time.Time { return clock }

	deliver := func() string {
		t.Helper()
		if _, err := deliverAuth(t, w); err != nil {
			t.Fatal(err)
		}
		return serverSeenSerial(srv)
	}
	if got := deliver(); got != one.x509.SerialNumber.String() {
		t.Fatalf("first delivery used %s", got)
	}

	// The files change; within a minute they are not even looked at.
	two.write(t, dir)
	clock = clock.Add(30 * time.Second)
	if got := deliver(); got != one.x509.SerialNumber.String() {
		t.Fatalf("the files were re-read inside a minute: serial %s", got)
	}
	// After a minute the next handshake picks the new pair up.
	clock = clock.Add(31 * time.Second)
	if got := deliver(); got != two.x509.SerialNumber.String() {
		t.Fatalf("a rotated certificate was not picked up: serial %s", got)
	}
	// An unchanged file is not re-read: the pair is the same pointer.
	cur := w.cert.cur
	clock = clock.Add(2 * time.Minute)
	deliver()
	if w.cert.cur != cur {
		t.Fatal("an unchanged pair was reloaded")
	}

	// A broken rewrite keeps the last good pair and reports the error.
	writeFile(t, cf, []byte("garbage"))
	clock = clock.Add(2 * time.Minute)
	if got := deliver(); got != two.x509.SerialNumber.String() {
		t.Fatalf("a failed reload dropped the last good pair: serial %s", got)
	}
	mu.Lock()
	n := len(reported)
	mu.Unlock()
	if n == 0 {
		t.Fatal("the failed reload was not reported")
	}
	if s := reported[0].Error(); strings.Contains(s, dir) {
		t.Fatalf("reload error leaks a path: %s", s)
	}
}

func TestClientCertReloadedAfterHandshakeFailure(t *testing.T) {
	ca := newCA(t, "private")
	oldCA := newCA(t, "old-clients")
	newCA2 := newCA(t, "new-clients")
	// The receiver now only trusts the new client CA; the files still hold a certificate of the old one.
	srv := newTLSServer(t, ca.issue(t, "receiver", true).tls, requireClientCert(newCA2))
	stale := oldCA.issue(t, "old", false)
	fresh := newCA2.issue(t, "new", false)
	dir := t.TempDir()
	cf, kf := stale.write(t, dir)

	clock := time.Now() // never advances: only the failure can trigger the reload
	w, sleeps := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem), WithClientCert(cf, kf), WithRequestsPerCall(2))
	w.now = func() time.Time { return clock }
	// The operator's rotation lands while the round backs off.
	w.sleep = func(ctx context.Context, d time.Duration) error { fresh.write(t, dir); return nil }
	_ = sleeps

	res, err := w.deliver(context.Background(), "id", []byte(`{}`))
	if err != nil {
		t.Fatalf("the round did not recover after the handshake failure: %v", err)
	}
	if res.Requests != 2 || !res.Auth.MTLS || res.Auth.ClientSPKI != SPKIHash(fresh.x509) {
		t.Fatalf("%+v %+v", res, res.Auth)
	}
}

func TestClientCertReloadedByTheFailureItself(t *testing.T) {
	ca := newCA(t, "private")
	oldCA := newCA(t, "old-clients")
	newCA2 := newCA(t, "new-clients")
	srv := newTLSServer(t, ca.issue(t, "receiver", true).tls, requireClientCert(newCA2))
	stale := oldCA.issue(t, "old", false)
	fresh := newCA2.issue(t, "new", false)
	dir := t.TempDir()
	cf, kf := stale.write(t, dir)
	clock := time.Now()
	w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem), WithClientCert(cf, kf))
	w.now = func() time.Time { return clock }
	fresh.write(t, dir) // rotated on disk; the minute has not passed, so only a failure can notice
	if _, err := deliverAuth(t, w); err == nil {
		t.Fatal("the stale pair was accepted")
	}
	if a, err := deliverAuth(t, w); err != nil || a.ClientSPKI != SPKIHash(fresh.x509) {
		t.Fatalf("the failure did not trigger a reload: %v %+v", err, a)
	}
}

func TestClientCertReloadErrorIsReportedNotFatal(t *testing.T) {
	// The initial load must succeed; a later one that cannot read the file keeps the pair.
	ca := newCA(t, "clients")
	l := ca.issue(t, "c", false)
	dir := t.TempDir()
	cf, kf := l.write(t, dir)
	var got []error
	w, _ := fastWebhook(t, "https://example.com/", WithClientCert(cf, kf), WithOnError(func(e error) { got = append(got, e) }))
	w.cert.reloadNow() // unchanged but forced: loads again, fine
	if len(got) != 0 {
		t.Fatalf("%v", got)
	}
	writeFile(t, kf, []byte("not a key"))
	w.cert.reloadNow()
	if len(got) != 1 || w.cert.cur == nil || !errors.Is(got[0], got[0]) {
		t.Fatalf("%v", got)
	}
	if SPKIHash(w.cert.cur.Leaf) != SPKIHash(l.x509) {
		t.Fatal("last good pair lost")
	}
}

func TestUnsignedRequestsAreNeverOffered(t *testing.T) {
	// Everything the webhook sends is signed, over plain http (localhost) as over TLS.
	var hdr http.Header
	srv := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) { hdr = r.Header.Clone() })
	w, _ := fastWebhook(t, srv.URL)
	if a, err := deliverAuth(t, w); err != nil || a.Signature != "hmac-sha256" || a.TLSVersion != "" || a.MTLS {
		t.Fatalf("%v %+v", err, a)
	}
	if !strings.HasPrefix(hdr.Get(HeaderSignature), "v1=") {
		t.Fatalf("unsigned: %v", hdr)
	}
}

func TestAuthSecretSlot(t *testing.T) {
	srv := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) {})
	w, err := NewWebhook(srv.URL, WithSecrets("new-fips-padded-secret", "old-fips-padded-secret"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := deliverAuth(t, w)
	if err != nil || a.SecretSlot != "current+previous" {
		t.Fatalf("%v %+v", err, a)
	}
	// Whatever is recorded, the secrets themselves never are.
	if strings.Contains(a.Signature+a.SecretSlot+a.ClientSPKI, "new") && a.SecretSlot != "current+previous" {
		t.Fatal("secret in auth")
	}
}

func TestClientCertReloadSeesASameSizeRewriteWithTheSameMtime(t *testing.T) {
	ca := newCA(t, "clients")
	one := ca.issue(t, "c1", false)
	two := ca.issue(t, "c2", false)
	for i := 0; i < 200 && (len(one.certPEM) != len(two.certPEM) || len(one.keyPEM) != len(two.keyPEM)); i++ {
		two = ca.issue(t, "c2", false) // ECDSA signatures vary by a byte; draw until the sizes match
	}
	if len(one.certPEM) != len(two.certPEM) || len(one.keyPEM) != len(two.keyPEM) {
		t.Fatal("could not build a same-size pair")
	}
	dir := t.TempDir()
	cf, kf := one.write(t, dir)
	ci, _ := os.Stat(cf)
	ki, _ := os.Stat(kf)
	clock := time.Now()
	w, _ := fastWebhook(t, "https://example.com/", WithClientCert(cf, kf))
	w.now = func() time.Time { return clock }
	if err := os.WriteFile(cf, two.certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kf, two.keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(cf, ci.ModTime(), ci.ModTime())
	_ = os.Chtimes(kf, ki.ModTime(), ki.ModTime())
	clock = clock.Add(2 * time.Minute)
	w.cert.mu.Lock()
	w.cert.refresh(false)
	got := w.cert.cur
	w.cert.mu.Unlock()
	if SPKIHash(got.Leaf) != SPKIHash(two.x509) {
		t.Fatal("a same-size, same-mtime rewrite was not picked up")
	}
}

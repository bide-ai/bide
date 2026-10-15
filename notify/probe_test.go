package notify

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestProbeReportsHandshakeStatusAndTiming(t *testing.T) {
	ca := newCA(t, "private")
	srv := newTLSServer(t, ca.issue(t, "receiver", true).tls)
	w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem))
	res, err := w.Probe(context.Background(), sampleApproval())
	if err != nil {
		t.Fatal(err)
	}
	if !res.TLS || !res.Handshake || res.HandshakeErr != nil || res.TLSVersion != "1.3" || res.Status != 200 || res.Requests != 1 || res.Auth == nil || !res.Auth.CustomCA {
		t.Fatalf("probe = %+v", res)
	}
	if res.HandshakeTime <= 0 || res.Elapsed < res.HandshakeTime {
		t.Fatalf("timing: handshake %v, elapsed %v", res.HandshakeTime, res.Elapsed)
	}
	// It sent what Notify sends: a signed request carrying the notification's ID.
	got := srv.seen()
	if len(got) != 1 || got[0].header.Get(HeaderDelivery) != sampleApproval().ID || got[0].header.Get(HeaderSignature) == "" {
		t.Fatalf("request = %+v", got)
	}
}

func TestProbeHandshakeFailureNamesTheReason(t *testing.T) {
	good, other := newCA(t, "good"), newCA(t, "other")
	srv := newTLSServer(t, good.issue(t, "receiver", true).tls)
	w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(other.pem))
	res, err := w.Probe(context.Background(), sampleApproval())
	if err == nil {
		t.Fatal("a receiver outside the CA bundle was accepted")
	}
	if !res.TLS || res.Handshake || res.HandshakeErr == nil || res.Status != 0 {
		t.Fatalf("probe = %+v", res)
	}
	if !strings.Contains(res.HandshakeErr.Error(), "certificate") {
		t.Fatalf("reason %q does not name the certificate", res.HandshakeErr)
	}
	if len(srv.seen()) != 0 {
		t.Fatal("a request reached a receiver whose certificate failed")
	}
}

func TestProbePinMismatchIsAHandshakeFailure(t *testing.T) {
	ca := newCA(t, "private")
	srv := newTLSServer(t, ca.issue(t, "receiver", true).tls)
	wrong := strings.Repeat("ab", 32)
	w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem), WithServerPin(wrong))
	res, err := w.Probe(context.Background(), sampleApproval())
	if err == nil || res.Handshake || res.HandshakeErr == nil || !strings.Contains(res.HandshakeErr.Error(), "pin") {
		t.Fatalf("pin mismatch: err %v, probe %+v", err, res)
	}
}

func TestProbeRefusedStatusIsNotAHandshakeFailure(t *testing.T) {
	ca := newCA(t, "private")
	srv := newTLSServer(t, ca.issue(t, "receiver", true).tls)
	srv.handle = func(int, http.ResponseWriter, *http.Request) int { return 401 }
	w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem))
	res, err := w.Probe(context.Background(), sampleApproval())
	var pe *PermanentError
	if !errors.As(err, &pe) || pe.Status != 401 || res.Status != 401 || !res.Handshake || res.Auth != nil {
		t.Fatalf("401: err %v, probe %+v", err, res)
	}
}

func TestProbeKeepsTheSecretOutOfTheResult(t *testing.T) {
	ca := newCA(t, "private")
	srv := newTLSServer(t, ca.issue(t, "receiver", true).tls)
	w, _ := fastWebhook(t, srv.URL, WithServerCAPEM(ca.pem))
	res, _ := w.Probe(context.Background(), sampleApproval())
	if strings.Contains(strings.ToLower(printable(res)), strings.ToLower(testSecret)) {
		t.Fatal("the secret is in the probe result")
	}
}

func printable(r ProbeResult) string {
	s := ""
	if r.HandshakeErr != nil {
		s = r.HandshakeErr.Error()
	}
	if r.Auth != nil {
		s += r.Auth.Signature + r.Auth.SecretSlot + r.Auth.ClientSPKI
	}
	return s
}

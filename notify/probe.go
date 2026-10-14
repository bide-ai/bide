package notify

import (
	"context"
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
)

// ProbeResult is what one delivery looked like from the sender's side, for a tool that tests a
// receiver (bide notify test). It carries no secret and no URL.
type ProbeResult struct {
	// Status is the receiver's HTTP status, 0 if there was no response.
	Status int
	// TLS is whether the request went over TLS. Handshake, HandshakeErr, HandshakeTime and
	// TLSVersion are meaningful only then.
	TLS bool
	// Handshake is true when the TLS handshake completed; with TLS true and Handshake false,
	// HandshakeErr says why (an unknown CA, a pin mismatch, a rejected client certificate).
	Handshake     bool
	HandshakeErr  error
	HandshakeTime time.Duration
	TLSVersion    string
	// Auth is how the delivery was authenticated, set when the receiver answered 2xx.
	Auth *agent.NotifyAuth
	// Requests is how many requests the delivery made, and Elapsed the time all of them took.
	Requests int
	Elapsed  time.Duration
}

type probeKey struct{}

// probeTrace collects the TLS handshake of the request in flight. A retried delivery overwrites it,
// so it describes the last request.
type probeTrace struct {
	mu       sync.Mutex
	started  time.Time
	tls      bool
	done     bool
	err      error
	duration time.Duration
	version  uint16
}

func (p *probeTrace) reset() {
	p.mu.Lock()
	p.started, p.tls, p.done, p.err, p.duration, p.version = time.Time{}, false, false, nil, 0, 0
	p.mu.Unlock()
}

func (p *probeTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		TLSHandshakeStart: func() {
			p.mu.Lock()
			p.tls, p.started = true, time.Now()
			p.mu.Unlock()
		},
		TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
			p.mu.Lock()
			p.duration, p.err, p.done = time.Since(p.started), err, err == nil
			p.version = cs.Version
			p.mu.Unlock()
		},
	}
}

// Probe delivers n exactly as Notify does (the same payload, signature, TLS options and request
// rules) and reports how the delivery went, without the audit acknowledgement Notify makes. The
// error is the one Notify would return. It is for tools that test a receiver; the notifier
// itself uses Notify.
func (w *Webhook) Probe(ctx context.Context, n agent.Notification) (ProbeResult, error) {
	body, err := jsonMarshal(w.payload(n))
	if err != nil {
		return ProbeResult{}, err
	}
	tr := &probeTrace{}
	start := w.now()
	res, derr := w.deliver(context.WithValue(ctx, probeKey{}, tr), n.ID, body)
	out := ProbeResult{Status: res.Status, Auth: res.Auth, Requests: res.Requests, Elapsed: w.now().Sub(start)}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out.TLS = tr.tls || w.url.Scheme == "https"
	out.Handshake, out.HandshakeErr, out.HandshakeTime = tr.done, tr.err, tr.duration
	if tr.done {
		out.TLSVersion = tlsVersionName(tr.version)
	}
	return out, derr
}

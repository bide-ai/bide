package crlrefresh_test

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/crlrefresh"
	"github.com/bide-ai/bide/internal/testca"
)

// crlRoute is one path of the test CRL server.
type crlRoute struct {
	mu       sync.Mutex
	body     []byte
	status   int
	delay    time.Duration
	hdr      http.Header
	redirect string
	chunked  bool
	etag     string
	hits     int
	inm      []string
}

type crlServer struct {
	*httptest.Server
	mu     sync.Mutex
	routes map[string]*crlRoute
}

func newCRLServer(t *testing.T) *crlServer {
	t.Helper()
	s := &crlServer{routes: map[string]*crlRoute{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		rt := s.routes[r.URL.Path]
		s.mu.Unlock()
		if rt == nil {
			http.NotFound(w, r)
			return
		}
		rt.mu.Lock()
		rt.hits++
		rt.inm = append(rt.inm, r.Header.Get("If-None-Match"))
		body, status, delay, hdr, redirect, etag, chunked := rt.body, rt.status, rt.delay, rt.hdr, rt.redirect, rt.etag, rt.chunked
		rt.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		for k, v := range hdr {
			w.Header()[k] = v
		}
		if redirect != "" {
			http.Redirect(w, r, redirect, http.StatusFound)
			return
		}
		if etag != "" {
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		if status != 0 {
			w.WriteHeader(status)
		}
		if chunked {
			for len(body) > 0 {
				n := len(body)
				if n > 512 {
					n = 512
				}
				_, _ = w.Write(body[:n])
				w.(http.Flusher).Flush()
				body = body[n:]
			}
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *crlServer) set(path string, body []byte) *crlRoute {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt := s.routes[path]
	if rt == nil {
		rt = &crlRoute{}
		s.routes[path] = rt
	}
	rt.mu.Lock()
	rt.body = body
	rt.mu.Unlock()
	return rt
}

func (s *crlServer) url(path string) string { return s.URL + path }

type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept []time.Duration
	real  time.Duration // a real pause per Sleep, so a loop test does not spin
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.slept = append(c.slept, d)
	c.t = c.t.Add(d)
	real := c.real
	c.mu.Unlock()
	if real > 0 {
		select {
		case <-time.After(real):
		case <-ctx.Done():
		}
	}
	return ctx.Err()
}

// rig is a refresher over the test PKI, a CRL server and a fake clock.
type crig struct {
	t            *testing.T
	root, inter  *testca.CA
	rootK, intK  audit.SPKIHash
	ci           *audit.CertIdentity
	srv          *crlServer
	clk          *fakeClock
	snap         *audit.StatusSnapshot
	ap           *audit.CertApprover
	r            *crlrefresh.Refresher
	arch         crlrefresh.Archive
	delta        bool
	mu           sync.Mutex
	events       []crlrefresh.Event
	alarms       []crlrefresh.Alarm
	sources      audit.CRLSources
	opts         crlrefresh.Options
	leaf         *testca.Leaf
	snapOpts     []audit.SnapshotOption
	soFull       []audit.SnapshotOption
	interMutate  func(*x509.Certificate)
	rootMutate   func(*x509.Certificate)
	skew         time.Duration
	policyMutate func(*audit.IdentityPolicy)
}

type crigOpt func(*crig)

func withArchive(a crlrefresh.Archive) crigOpt { return func(g *crig) { g.arch = a } }
func withDelta() crigOpt                       { return func(g *crig) { g.delta = true } }
func withSkew(d time.Duration) crigOpt         { return func(g *crig) { g.skew = d } }
func withSnapOpts(o ...audit.SnapshotOption) crigOpt {
	return func(g *crig) { g.snapOpts = o }
}

const (
	interPeriod = 30 * time.Minute
	rootPeriod  = 24 * time.Hour
)

// newRig builds the PKI, the approver and the refresher. The clock starts five minutes after T0.
func newCRig(t *testing.T, opts ...crigOpt) *crig {
	t.Helper()
	g := &crig{t: t, srv: newCRLServer(t), clk: &fakeClock{t: testca.T0.Add(5 * time.Minute)}}
	for _, o := range opts {
		o(g)
	}
	g.root = testca.NewRoot(t, "root", testca.Opts{Mutate: g.rootMutate})
	g.inter = g.root.NewIntermediate(t, "inter", testca.Opts{Mutate: g.interMutate})
	g.rootK, g.intK = audit.SPKIHashOf(g.root.Cert), audit.SPKIHashOf(g.inter.Cert)
	pol := audit.IdentityPolicy{
		Anchors:   []*x509.Certificate{g.root.Cert},
		Inter:     []audit.SPKIHash{g.intK},
		Identity:  map[string]string{"alice": testca.Person(1), "bob": testca.Person(2)},
		IDKind:    audit.IDPersonURI,
		EKU:       testca.EKU,
		Policy:    testca.PolicyOID,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
		MaxChain:  4,
	}
	if g.policyMutate != nil {
		g.policyMutate(&pol)
	}
	g.ci = mustCompile(t, pol)
	g.leaf = g.inter.NewLeaf(t, testca.Opts{})
	so := append([]audit.SnapshotOption{audit.WithIssuers(g.root.Cert, g.inter.Cert), audit.WithLimits(agent.MaxStatusInline, 1000)}, g.snapOpts...)
	g.soFull = so
	g.snap = audit.NewStatusSnapshot(so...)
	g.ap = &audit.CertApprover{
		ApproverID: "alice", Policy: g.ci, Status: g.snap, Now: g.clk.Now, PolicyEffectiveFrom: testDecidedAt,
		StatusParams: audit.StatusParams{
			CAPeriod: interPeriod, MaxStatusAge: time.Hour, ThisUpdateSkew: g.skew, CountLive: true,
			CAPeriodFor:     map[audit.SPKIHash]time.Duration{g.rootK: rootPeriod, g.intK: interPeriod},
			MaxStatusAgeFor: map[audit.SPKIHash]time.Duration{g.rootK: 4 * 24 * time.Hour, g.intK: time.Hour},
		},
		ExecStatusAge:    time.Hour,
		ExecStatusAgeFor: map[audit.SPKIHash]time.Duration{g.rootK: 48 * time.Hour, g.intK: time.Hour},
		CAOverlapFor:     map[audit.SPKIHash]time.Duration{g.rootK: 0, g.intK: 0},
	}
	if g.sources == nil {
		is := audit.CRLSource{URLs: []string{g.srv.url("/inter.crl")}}
		if g.delta {
			is.DeltaURLs = []string{g.srv.url("/inter-delta.crl")}
		}
		g.sources = audit.CRLSources{g.rootK: {URLs: []string{g.srv.url("/root.crl")}}, g.intK: is}
	}
	g.opts.Issuers = []*x509.Certificate{g.inter.Cert}
	g.opts.Archive = g.arch
	g.opts.Sleep = g.clk.Sleep
	g.opts.Rand = func() float64 { return 0.5 }
	if g.delta {
		g.opts.DeltaPeriodFor = map[audit.SPKIHash]time.Duration{g.intK: 5 * time.Minute}
	}
	g.opts.Hooks = crlrefresh.Hooks{
		OnEvent: func(e crlrefresh.Event) { g.mu.Lock(); g.events = append(g.events, e); g.mu.Unlock() },
		OnAlarm: func(a crlrefresh.Alarm) { g.mu.Lock(); g.alarms = append(g.alarms, a); g.mu.Unlock() },
	}
	return g
}

// start builds the refresher (separate so a test can change sources or options first).
func (g *crig) start() *crlrefresh.Refresher {
	g.t.Helper()
	r, err := crlrefresh.New(g.ap, g.sources, g.opts)
	if err != nil {
		g.t.Fatalf("NewCRLRefresher: %v", err)
	}
	g.r = r
	return r
}

func (g *crig) iCRL(num int64, this time.Time, valid time.Duration, e ...x509.RevocationListEntry) []byte {
	g.t.Helper()
	return g.inter.NewCRL(g.t, testca.CRLOpts{Number: num, ThisUpdate: this, NextUpdate: this.Add(valid), Entries: e})
}

func (g *crig) rCRL(num int64, this time.Time, valid time.Duration) []byte {
	g.t.Helper()
	return g.root.NewCRL(g.t, testca.CRLOpts{Number: num, ThisUpdate: this, NextUpdate: this.Add(valid)})
}

func (g *crig) iDelta(num, base int64, this time.Time, valid time.Duration, e ...x509.RevocationListEntry) []byte {
	g.t.Helper()
	ind, err := asn1.Marshal(big.NewInt(base))
	if err != nil {
		g.t.Fatal(err)
	}
	return g.inter.NewCRL(g.t, testca.CRLOpts{Number: num, ThisUpdate: this, NextUpdate: this.Add(valid), Entries: e,
		Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 27}, true, ind)}})
}

// publishFresh puts a fresh root CRL and inter CRL (numbers n) on the server, dated now.
func (g *crig) publishFresh(n int64) (inter []byte) {
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(n, now.Add(-time.Hour), 48*time.Hour))
	inter = g.iCRL(n, now.Add(-time.Minute), time.Hour)
	g.srv.set("/inter.crl", inter)
	return inter
}

func (g *crig) kinds(k crlrefresh.EventKind) []crlrefresh.Event {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []crlrefresh.Event
	for _, e := range g.events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func (g *crig) health(k audit.SPKIHash) crlrefresh.IssuerHealth {
	for _, h := range g.r.Health() {
		if h.Issuer == k {
			return h
		}
	}
	g.t.Fatalf("no health for %x", k[:4])
	return crlrefresh.IssuerHealth{}
}

func (g *crig) refresh() map[audit.SPKIHash]error {
	g.t.Helper()
	out := map[audit.SPKIHash]error{}
	for _, r := range g.r.RefreshOnce(context.Background()) {
		out[r.Issuer] = r.Err
	}
	return out
}

func (g *crig) wantOK() {
	g.t.Helper()
	for k, err := range g.refresh() {
		if err != nil {
			g.t.Fatalf("issuer %x: %v", k[:4], err)
		}
	}
}

// dump is the events as kind:feed:detail, for a failure message.
func (g *crig) dump() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var b []string
	for _, e := range g.events {
		if e.Kind == crlrefresh.EventSourcesChanged || e.Kind == crlrefresh.EventStartup {
			continue
		}
		b = append(b, string(e.Kind)+":"+e.Feed+":"+e.Detail)
	}
	return strings.Join(b, " | ")
}

// restart replaces the snapshot and the refresher, as a process restart does; the archive, the server
// and the clock stay.
func (g *crig) restart() *crlrefresh.Refresher {
	g.t.Helper()
	g.snap = audit.NewStatusSnapshot(g.soFull...)
	g.ap.Status = g.snap
	g.mu.Lock()
	g.events = nil
	g.mu.Unlock()
	return g.start()
}

func bigInt(n int64) *big.Int { return big.NewInt(n) }

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// rigRefresherFor builds a second refresher over the same PKI and archive with its own snapshot: another
// process.
func rigRefresherFor(t *testing.T, g *crig, arch crlrefresh.Archive) *crlrefresh.Refresher {
	t.Helper()
	snap := audit.NewStatusSnapshot(g.soFull...)
	ap := *g.ap
	ap.Status = snap
	opts := g.opts
	opts.Archive = arch
	opts.Hooks = crlrefresh.Hooks{}
	r, err := crlrefresh.New(&ap, g.sources, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ext(oid asn1.ObjectIdentifier, critical bool, v []byte) pkix.Extension {
	return pkix.Extension{Id: oid, Critical: critical, Value: v}
}

var testDecidedAt = time.Date(2026, 10, 13, 12, 14, 3, 123_000_000, time.UTC)

func mustCompile(t *testing.T, p audit.IdentityPolicy) *audit.CertIdentity {
	t.Helper()
	c, err := p.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return c
}

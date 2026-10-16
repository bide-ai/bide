package audit_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// A refused CRL must not be hidden behind a 304: the validator is kept only for an accepted CRL.
func TestRefresherRefusedCRLIsNotHiddenBehindA304(t *testing.T) {
	g := newCRig(t)
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	rt := g.srv.set("/inter.crl", g.iCRL(1, now.Add(10*time.Minute), 2*time.Hour))
	rt.etag = `"v1"`
	g.start()
	if g.refresh()[g.intK] == nil || len(g.kinds(audit.EventFutureDated)) != 1 {
		t.Fatalf("a future-dated CRL was not refused: %s", g.dump())
	}
	g.clk.advance(15 * time.Minute) // the same CRL is now acceptable
	g.wantOK()
	if h := g.health(g.intK); h.NewestNumber == nil || h.NewestNumber.Int64() != 1 || h.ConsecutiveFailures != 0 {
		t.Fatalf("the CRL stayed wedged behind a 304: %+v", h)
	}
	// A 304 with nothing accepted from that URL is not "obtained".
	g2 := newCRig(t)
	n2 := g2.clk.Now()
	g2.srv.set("/root.crl", g2.rCRL(1, n2.Add(-time.Hour), 48*time.Hour))
	other := g2.inter
	_ = other
	rt2 := g2.srv.set("/inter.crl", g2.rCRL(9, n2.Add(-time.Minute), time.Hour)) // wrong issuer
	rt2.etag = `"w"`
	g2.start()
	for range 2 {
		if g2.refresh()[g2.intK] == nil {
			t.Fatalf("a refused CRL turned into a success: %s", g2.dump())
		}
	}
	if rt2.inm[len(rt2.inm)-1] != "" {
		t.Fatalf("a refused CRL's validator was kept: %v", rt2.inm)
	}
}

// After a reset, a source that keeps serving the poisoned CRL does not bring the mark back, and a reset
// made by another handle on the archive reaches a running refresher.
func TestResetHoldsAgainstTheSourceAndAcrossProcesses(t *testing.T) {
	arch := audit.DirCRLArchive(t.TempDir())
	g := newCRig(t, withArchive(arch))
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	poison := g.iCRL(1_000_000, now.Add(-2*time.Minute), time.Hour)
	g.srv.set("/inter.crl", poison)
	g.start()
	g.wantOK()
	good := g.iCRL(6, now.Add(-time.Minute), time.Hour)

	// "Another process": a second refresher on the same archive and a fresh snapshot makes the reset.
	r2 := rigRefresherFor(t, g, arch)
	if err := r2.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r2.ResetHighWater(context.Background(), g.intK, audit.ResetRequest{Reason: "mistake", Operator: "ops", AcceptLostListings: true}); err != nil {
		t.Fatal(err)
	}

	// The running refresher applies it at its next attempt; the source still serves the poison.
	g.srv.set("/inter.crl", poison)
	g.refresh()
	if len(g.kinds(audit.EventSetAsideServed)) != 1 {
		t.Fatalf("a served set-aside CRL was not refused: %s", g.dump())
	}
	if h := g.health(g.intK); h.NewestNumber != nil && h.NewestNumber.Int64() == 1_000_000 {
		t.Fatalf("the poisoned CRL came back: %+v", h)
	}
	g.srv.set("/inter.crl", good)
	g.wantOK()
	if h := g.health(g.intK); h.NewestNumber.Int64() != 6 {
		t.Fatalf("after the reset the real CRL was not accepted: %+v", h)
	}
	ents, _ := arch.List(context.Background(), g.intK)
	marked := 0
	for _, e := range ents {
		if e.Marker == audit.MarkerSetAside {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("the served set-aside CRL was not archived with a marker: %d", marked)
	}
}

// A reset that would drop a revocation nobody else lists is refused without an explicit acceptance.
func TestResetRefusesToUnrevokeSilently(t *testing.T) {
	arch := audit.DirCRLArchive(t.TempDir())
	g := newCRig(t, withArchive(arch))
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	g.srv.set("/inter.crl", g.iCRL(1, now.Add(-20*time.Minute), time.Hour))
	g.start()
	g.wantOK()
	g.clk.advance(10 * time.Minute)
	g.srv.set("/inter.crl", g.iCRL(2, g.clk.Now().Add(-time.Minute), time.Hour, testca.Revoked(g.leaf.Cert, g.clk.Now().Add(-5*time.Minute), 1)))
	g.wantOK()
	ctx := context.Background()
	err := g.r.ResetHighWater(ctx, g.intK, audit.ResetRequest{Reason: "r", Operator: "o"})
	if err == nil {
		t.Fatal("a reset that drops a unique revocation was accepted")
	}
	if g.health(g.intK).NewestNumber.Int64() != 2 {
		t.Fatal("a refused reset changed the snapshot")
	}
	if rs, _ := arch.Resets(ctx, g.intK); len(rs) != 0 {
		t.Fatal("a refused reset wrote a record")
	}
	if err := g.r.ResetHighWater(ctx, g.intK, audit.ResetRequest{Reason: "r", Operator: "o", AcceptLostListings: true}); err != nil {
		t.Fatal(err)
	}
	rs, _ := arch.Resets(ctx, g.intK)
	if len(rs) != 1 || len(rs[0].LostSerials) != 1 || rs[0].LostSerials[0] != g.leaf.Cert.SerialNumber.String() {
		t.Fatalf("the record does not list what was dropped: %+v", rs)
	}
}

// A reset during delta attempts whose base is missing must not deadlock.
func TestResetDuringDeltaAttemptsDoesNotDeadlock(t *testing.T) {
	arch := audit.DirCRLArchive(t.TempDir())
	g := newCRig(t, withArchive(arch), withDelta())
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	g.srv.set("/inter-delta.crl", g.iDelta(31, 30, now.Add(-time.Minute), 20*time.Minute))
	g.srv.set("/inter.crl", g.iCRL(40, now.Add(-time.Minute), time.Hour))
	g.start()
	g.refresh()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(2)
			go func() { defer wg.Done(); g.refresh() }()
			go func() {
				defer wg.Done()
				_ = g.r.ResetHighWater(context.Background(), g.intK, audit.ResetRequest{Reason: "r", Operator: "o", AcceptLostListings: true})
			}()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock between a reset and a delta attempt")
	}
}

func TestRefresherFollowsAnHTTPToHTTPSUpgradeOnlyOnDefaultPorts(t *testing.T) {
	// The rule is unit-tested in the internal tests; here the observable end: a redirect to another
	// port of the same host is still refused.
	g := newCRig(t)
	g.publishFresh(1)
	other := newCRLServer(t)
	other.set("/x.crl", g.iCRL(7, g.clk.Now().Add(-time.Minute), time.Hour))
	g.srv.set("/inter.crl", nil).redirect = other.url("/x.crl")
	g.start()
	if g.refresh()[g.intK] == nil || len(g.kinds(audit.EventRedirectRefused)) != 1 {
		t.Fatalf("%s", g.dump())
	}
}

func TestDeltaOutageDoesNotFailARefreshOnce(t *testing.T) {
	g := newCRig(t, withDelta())
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	g.srv.set("/inter.crl", g.iCRL(10, now.Add(-time.Minute), time.Hour))
	g.start() // the delta URL is not served: 404
	g.wantOK()
	if len(g.kinds(audit.EventFetchFailed)) == 0 {
		t.Fatalf("the delta failure was not reported: %s", g.dump())
	}
}

func TestStartupReportsThePolicyDigest(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	g.start()
	found := false
	for _, e := range g.kinds(audit.EventStartup) {
		found = found || bytes.Contains([]byte(e.Detail), []byte("policy digest "+g.ap.PolicyDigest()))
	}
	if !found {
		t.Fatalf("no policy digest in the start-up events: %s", g.dump())
	}
}

func TestArchiveAcceptsAResignedSeriesFile(t *testing.T) {
	g := newCRig(t)
	arch := audit.DirCRLArchive(t.TempDir())
	this := testca.T0
	a := g.iCRL(3, this, time.Hour)
	b := g.iCRL(3, this, time.Hour) // same TBS, re-signed (ECDSA is randomised)
	if bytes.Equal(a, b) {
		t.Skip("deterministic signatures")
	}
	rec := audit.ArchivedCRL{Number: bigInt(3), ThisUpdate: this}
	ctx := context.Background()
	if err := arch.PutCRL(ctx, g.intK, rec, a); err != nil {
		t.Fatal(err)
	}
	if err := arch.PutCRL(ctx, g.intK, rec, b); err != nil {
		t.Fatalf("a re-signed copy of an archived CRL failed forever: %v", err)
	}
	if err := arch.PutCRL(ctx, g.intK, rec, g.iCRL(3, this, 2*time.Hour)); err == nil {
		t.Fatal("another TBS under the same series name was accepted")
	}
}

// Survivors of the first mutation pass.
func TestRefresherRefusesACRLWithAnotherIssuerName(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	// Same key, different issuer name: signed by the intermediate's key but naming another subject.
	other := testca.NewRoot(t, "elsewhere", testca.Opts{KeyWith: g.inter.Key})
	now := g.clk.Now()
	g.srv.set("/inter.crl", other.NewCRL(t, testca.CRLOpts{Number: 5, ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour)}))
	g.start()
	if g.refresh()[g.intK] == nil || len(g.kinds(audit.EventBadSignature)) != 1 {
		t.Fatalf("a CRL naming another issuer was accepted: %s", g.dump())
	}
}

func TestRefresherBodyLimitIsTheSnapshotsOwn(t *testing.T) {
	g := newCRig(t, withSnapOpts(audit.WithLimits(2000, 1000)))
	g.publishFresh(1)
	// A valid CRL padded past the limit by many entries, chunked with no Content-Length.
	now := g.clk.Now()
	var entries []x509.RevocationListEntry
	for i := range 200 {
		entries = append(entries, testca.Revoked(g.leaf.Cert, now, 0))
		entries[i].SerialNumber = bigInt(int64(1000 + i))
	}
	big := g.iCRL(2, now.Add(-time.Minute), time.Hour, entries...)
	if len(big) <= 2000 {
		t.Fatalf("test CRL is only %d bytes", len(big))
	}
	rt := g.srv.set("/inter.crl", big)
	rt.hdr = http.Header{"Content-Type": {"application/pkix-crl"}}
	g.start()
	if g.refresh()[g.intK] == nil || len(g.kinds(audit.EventOversizeResponse)) != 1 {
		t.Fatalf("an oversize CRL was accepted: %s", g.dump())
	}
	_ = agent.MaxStatusInline
}

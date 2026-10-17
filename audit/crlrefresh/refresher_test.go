package crlrefresh_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/crlrefresh"
	"github.com/bide-ai/bide/internal/testca"
)

func TestRefresherAddsAndRotates(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	g.start()
	g.wantOK()
	if g.snap.Len() != 2 {
		t.Fatalf("snapshot holds %d CRLs, want the root's and the intermediate's", g.snap.Len())
	}
	h := g.health(g.intK)
	if h.NewestNumber.Int64() != 1 || h.Critical || h.Alarm || h.Age != time.Minute {
		t.Fatalf("health %+v", h)
	}
	g.clk.advance(25 * time.Minute)
	g.publishFresh(2)
	g.wantOK()
	if g.snap.Len() != 4 {
		t.Fatalf("after rotation the snapshot holds %d, want 4", g.snap.Len())
	}
	if h := g.health(g.intK); h.NewestNumber.Int64() != 2 || h.Age != time.Minute {
		t.Fatalf("rotated health %+v", h)
	}
	select {
	case <-g.r.Ready():
	default:
		t.Fatal("Ready is not closed with every issuer fresh")
	}
}

func TestRefresherStaleOnArrivalIsArchivedNotFresh(t *testing.T) {
	g := newCRig(t, withArchive(crlrefresh.DirArchive(t.TempDir())))
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	g.srv.set("/inter.crl", g.iCRL(1, now.Add(-3*time.Hour), time.Hour)) // nextUpdate two hours ago
	g.start()
	errs := g.refresh()
	if errs[g.intK] == nil {
		t.Fatal("a CRL past its nextUpdate was reported as a fresh refresh")
	}
	if len(g.kinds(crlrefresh.EventStaleOnArrival)) != 1 {
		t.Fatalf("events %+v", g.dump())
	}
	// It was archived and added, so the URL is not reported as refused.
	if msg := errs[g.intK].Error(); !strings.Contains(msg, ": stale: ") || strings.Contains(msg, "refused") {
		t.Fatalf("a stale CRL is not labelled stale: %v", errs[g.intK])
	}
	if g.snap.Len() != 2 {
		t.Fatalf("an expired CRL still fills the series: snapshot holds %d", g.snap.Len())
	}
	if h := g.health(g.intK); !h.Critical || !h.LastSuccess.IsZero() {
		t.Fatalf("an expired CRL counted as success: %+v", h)
	}
	ents, _ := g.opts.Archive.List(context.Background(), g.intK)
	if len(ents) != 1 {
		t.Fatalf("archive holds %d files", len(ents))
	}
}

func TestRefresherOversizeIsRefusedNotTruncated(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	g.srv.set("/inter.crl", bytes.Repeat([]byte{0x30}, agent.MaxStatusInline+10))
	g.start()
	if g.refresh()[g.intK] == nil {
		t.Fatal("an oversize body was accepted")
	}
	if len(g.kinds(crlrefresh.EventOversizeResponse)) == 0 {
		t.Fatalf("no oversize event: %+v", g.dump())
	}
	// Chunked, with no Content-Length: the limit applies to the body read.
	rt := g.srv.set("/inter.crl", bytes.Repeat([]byte{0x30}, agent.MaxStatusInline+10))
	rt.hdr = http.Header{"Content-Type": {"application/pkix-crl"}}
	if g.refresh()[g.intK] == nil {
		t.Fatal("an oversize body was accepted")
	}
}

func TestRefresherGzipBombIsNotDecompressed(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(make([]byte, 64<<20))
	_ = zw.Close()
	rt := g.srv.set("/inter.crl", buf.Bytes())
	rt.hdr = http.Header{"Content-Encoding": {"gzip"}}
	g.start()
	if g.refresh()[g.intK] == nil {
		t.Fatal("a gzip body was accepted as a CRL")
	}
	if len(g.kinds(crlrefresh.EventOversizeResponse)) != 0 {
		t.Fatalf("the body was decompressed before the size limit applied: %s", g.dump())
	}
	if len(g.kinds(crlrefresh.EventBadSignature)) == 0 {
		t.Fatalf("the gzip body produced no refusal event: %s", g.dump())
	}
}

func TestRefresherSlowServerHitsTheDeadline(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	rt := g.srv.set("/inter.crl", g.iCRL(1, g.clk.Now().Add(-time.Minute), time.Hour))
	rt.delay = 5 * time.Second
	g.start()
	crlrefresh.SetAttemptTimeout(g.r, 200*time.Millisecond)
	start := time.Now()
	if g.refresh()[g.intK] == nil {
		t.Fatal("a slow server produced a CRL")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the attempt took %v, past its deadline", d)
	}
	if g.snap.Len() != 1 {
		t.Fatalf("snapshot holds %d (the root's only)", g.snap.Len())
	}
}

func TestRefresherChecksTheSignatureAgainstTheIssuersOwnCertificate(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	g.start()
	g.wantOK()
	now := g.clk.Now()
	// The root's CRL served at the intermediate's URL: it verifies under an issuer of the snapshot, but
	// not under this issuer.
	g.srv.set("/inter.crl", g.rCRL(9, now.Add(-time.Minute), time.Hour))
	if g.refresh()[g.intK] == nil {
		t.Fatal("another issuer's CRL was accepted at this issuer's URL")
	}
	if len(g.kinds(crlrefresh.EventBadSignature)) == 0 {
		t.Fatalf("no bad-signature event: %+v", g.dump())
	}
	// A CRL with the issuer's own name signed by another key.
	rogue := testca.NewRoot(t, "inter", testca.Opts{})
	g.srv.set("/inter.crl", rogue.NewCRL(t, testca.CRLOpts{Number: 10, ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour)}))
	err := g.refresh()[g.intK]
	if err == nil {
		t.Fatal("a CRL signed by another key was accepted")
	}
	if strings.Contains(err.Error(), ": ok") {
		t.Fatalf("a CRL that failed verification is reported as ok: %v", err)
	}
	if h := g.health(g.intK); h.NewestNumber.Int64() != 1 {
		t.Fatalf("a refused CRL changed what is held: %+v", h)
	}
}

func TestRefresherRollbackConflictAndResignedCRL(t *testing.T) {
	g := newCRig(t, withArchive(crlrefresh.DirArchive(t.TempDir())))
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	five := g.iCRL(5, now.Add(-2*time.Minute), time.Hour)
	g.srv.set("/inter.crl", five)
	g.start()
	g.wantOK()

	// A re-signed copy of the same CRL (same TBS, other signature bytes) is the same CRL.
	again := g.iCRL(5, now.Add(-2*time.Minute), time.Hour)
	if bytes.Equal(again, five) {
		t.Skip("this key type signs deterministically; the re-sign case needs a randomised signature")
	}
	g.srv.set("/inter.crl", again)
	g.wantOK()
	if len(g.kinds(crlrefresh.EventConflict)) != 0 || g.snap.Len() != 2 {
		t.Fatalf("a re-signed CRL was treated as a conflict or added: %+v, %d held", g.events, g.snap.Len())
	}

	// Same number, different content: equivocation.
	g.srv.set("/inter.crl", g.iCRL(5, now.Add(-time.Minute), time.Hour, testca.Revoked(g.leaf.Cert, now, 1)))
	if g.refresh()[g.intK] == nil {
		t.Fatal("an equivocating CRL counted as success")
	}
	if len(g.kinds(crlrefresh.EventConflict)) != 1 || g.snap.Len() != 2 {
		t.Fatalf("conflict not reported or added: %+v", g.dump())
	}

	// A lower number is a rollback, even when it is still inside its validity.
	g.srv.set("/inter.crl", g.iCRL(4, now.Add(-time.Minute), time.Hour))
	if g.refresh()[g.intK] == nil {
		t.Fatal("a rollback counted as success")
	}
	rb := g.kinds(crlrefresh.EventRollback)
	if len(rb) != 1 || rb[0].Number.Int64() != 4 || rb[0].Held.Int64() != 5 {
		t.Fatalf("rollback event %+v", rb)
	}
	if h := g.health(g.intK); h.NewestNumber.Int64() != 5 {
		t.Fatalf("rollback changed the newest CRL: %+v", h)
	}
	// The refused CRLs are in the archive, marked, so an auditor can tell them from the series.
	ents, _ := g.opts.Archive.List(context.Background(), g.intK)
	marks := map[string]int{}
	for _, e := range ents {
		marks[e.Marker]++
	}
	if marks[""] != 1 || marks[crlrefresh.MarkerRollback] != 1 || marks[crlrefresh.MarkerConflict] != 1 {
		t.Fatalf("archive markers %v", marks)
	}
	// A restart never loads a marked file as the series: the genuine CRL 5 is still the current one.
	g.restart()
	g.srv.set("/inter.crl", five)
	g.wantOK()
	if len(g.kinds(crlrefresh.EventConflict)) != 0 || len(g.kinds(crlrefresh.EventRollback)) != 0 {
		t.Fatalf("a marked archive file was taken as the series after a restart: %s", g.dump())
	}
}

func TestRefresherAllURLsAreAskedAndTheHighestWins(t *testing.T) {
	g := newCRig(t)
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	// Two mirrors: the first serves a pre-revocation CRL still inside its validity.
	g.srv.set("/stale.crl", g.iCRL(3, now.Add(-20*time.Minute), time.Hour))
	g.srv.set("/inter.crl", g.iCRL(4, now.Add(-time.Minute), time.Hour, testca.Revoked(g.leaf.Cert, now.Add(-30*time.Second), 1)))
	g.sources = audit.CRLSources{g.rootK: {URLs: []string{g.srv.url("/root.crl")}},
		g.intK: {URLs: []string{g.srv.url("/stale.crl"), g.srv.url("/inter.crl")}}}
	g.start()
	g.wantOK()
	if h := g.health(g.intK); h.NewestNumber.Int64() != 4 {
		t.Fatalf("the stale mirror won: %+v", h)
	}
}

func TestRefresherDeltaPairing(t *testing.T) {
	g := newCRig(t, withDelta())
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	g.srv.set("/inter.crl", g.iCRL(10, now.Add(-10*time.Minute), time.Hour))
	g.srv.set("/inter-delta.crl", g.iDelta(11, 10, now.Add(-time.Minute), 20*time.Minute))
	g.start()
	g.wantOK()
	h := g.health(g.intK)
	if !h.Paired || h.NewestNumber.Int64() != 11 || !h.NewestThisUpdate.Equal(now.Add(-time.Minute)) {
		t.Fatalf("paired delta health %+v", h)
	}
	// Freshness is the earlier nextUpdate of the pair: the delta's.
	if !h.NewestNextUpdate.Equal(now.Add(19 * time.Minute)) {
		t.Fatalf("merged nextUpdate %v", h.NewestNextUpdate)
	}
	// The CA stops publishing deltas: the stale delta must not look current, and the base alone is not fresher.
	g.clk.advance(25 * time.Minute)
	if h := g.health(g.intK); !h.Critical {
		t.Fatalf("a delta past its nextUpdate left the issuer looking fresh: %+v", h)
	}
}

func TestRefresherFetchesTheBaseOfAnUnpairedDeltaAtOnce(t *testing.T) {
	g := newCRig(t, withDelta())
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	g.srv.set("/inter.crl", g.iCRL(20, now.Add(-10*time.Minute), time.Hour))
	g.srv.set("/inter-delta.crl", g.iDelta(21, 20, now.Add(-time.Minute), 20*time.Minute))
	g.start()
	// Only the delta feed runs: the refresher itself must fetch the base it names.
	r := g.r
	_ = r
	g.wantOK()
	if rt := g.srv.routes["/inter.crl"]; rt.hits < 1 {
		t.Fatal("the base was never fetched")
	}
	if !g.health(g.intK).Paired {
		t.Fatal("delta and base are not paired")
	}
	if len(g.kinds(crlrefresh.EventUnpairedDelta)) != 0 {
		t.Fatalf("the delta was added before its base was fetched: %s", g.dump())
	}

	// A delta whose base cannot be obtained is still archived and added, reported unpaired.
	g2 := newCRig(t, withDelta())
	g2.srv.set("/root.crl", g2.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	g2.srv.set("/inter-delta.crl", g2.iDelta(31, 30, now.Add(-time.Minute), 20*time.Minute))
	g2.start()
	g2.refresh()
	if len(g2.kinds(crlrefresh.EventUnpairedDelta)) != 1 || g2.health(g2.intK).Paired {
		t.Fatalf("unpaired delta: %+v", g2.dump())
	}
	if g2.snap.Len() != 2 {
		t.Fatalf("the unpaired delta was not added: %d held", g2.snap.Len())
	}
}

func TestRefresherObsoleteDeltaIsNotPaired(t *testing.T) {
	g := newCRig(t, withDelta())
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	g.srv.set("/inter-delta.crl", g.iDelta(11, 10, now.Add(-20*time.Minute), 20*time.Minute))
	g.srv.set("/inter.crl", g.iCRL(12, now.Add(-time.Minute), time.Hour))
	g.start()
	g.refresh()
	h := g.health(g.intK)
	if h.Paired || h.NewestNumber.Int64() != 12 || h.Critical {
		t.Fatalf("a delta older than the base was paired or made the issuer stale: %+v", h)
	}
}

func TestRefresherFutureDatedAndSkew(t *testing.T) {
	g := newCRig(t)
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	g.srv.set("/inter.crl", g.iCRL(1, now.Add(10*time.Minute), time.Hour))
	g.start()
	if g.refresh()[g.intK] == nil || len(g.kinds(crlrefresh.EventFutureDated)) != 1 || g.snap.Len() != 1 {
		t.Fatalf("a CRL dated 10 minutes ahead was not refused: %+v", g.dump())
	}
	// With a declared 10 minute skew a thisUpdate 9 minutes ahead is accepted (skew plus a minute).
	g2 := newCRig(t, withSkew(10*time.Minute))
	n2 := g2.clk.Now()
	g2.srv.set("/root.crl", g2.rCRL(1, n2.Add(-time.Hour), 48*time.Hour))
	g2.srv.set("/inter.crl", g2.iCRL(1, n2.Add(9*time.Minute), time.Hour))
	g2.start()
	g2.refresh() // added; the gate itself calls a CRL more than a minute ahead not yet fresh
	if len(g2.kinds(crlrefresh.EventFutureDated)) != 0 || g2.snap.Len() != 2 {
		t.Fatalf("a CRL within the declared skew was refused: %+v", g2.dump())
	}
}

func TestRefresherNotModifiedNeverRefreshesAnything(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	g.srv.set("/inter.crl", g.iCRL(1, g.clk.Now().Add(-time.Minute), time.Hour)).etag = `"v1"`
	g.start()
	g.wantOK()
	succeeded := g.health(g.intK).LastSuccess
	g.clk.advance(2 * time.Hour) // the held CRL is now long stale
	g.wantAttempt()
	rt := g.srv.routes["/inter.crl"]
	if rt.inm[len(rt.inm)-1] != `"v1"` {
		t.Fatalf("no conditional request: %v", rt.inm)
	}
	if h := g.health(g.intK); !h.Critical || !h.LastSuccess.Equal(succeeded) {
		t.Fatalf("a 304 made a two-hour-old CRL look fresh: %+v", h)
	}
}

func (g *crig) wantAttempt() {
	g.t.Helper()
	g.r.RefreshOnce(context.Background())
}

func TestRefresherAcceptsPEM(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	der := g.iCRL(2, g.clk.Now().Add(-time.Minute), time.Hour)
	g.srv.set("/inter.crl", pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der}))
	g.start()
	g.wantOK()
	if g.health(g.intK).NewestNumber.Int64() != 2 {
		t.Fatal("a PEM CRL was not read")
	}
}

func TestRefresherRedirects(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	g.srv.set("/real.crl", g.iCRL(1, g.clk.Now().Add(-time.Minute), time.Hour))
	g.srv.set("/inter.crl", nil).redirect = "/real.crl"
	g.start()
	g.wantOK() // same host and port: followed

	other := newCRLServer(t)
	other.set("/x.crl", g.iCRL(7, g.clk.Now().Add(-time.Minute), time.Hour))
	g.srv.set("/inter.crl", nil).redirect = other.url("/x.crl")
	if g.refresh()[g.intK] == nil || len(g.kinds(crlrefresh.EventRedirectRefused)) != 1 {
		t.Fatalf("a redirect to another host was followed: %+v", g.dump())
	}
	if other.routes["/x.crl"].hits != 0 {
		t.Fatal("the other host was contacted")
	}
}

func TestRefresherDateHeaderSkewIsOnlyAWarning(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	rt := g.srv.set("/inter.crl", g.iCRL(1, g.clk.Now().Add(-time.Minute), time.Hour))
	rt.hdr = http.Header{"Date": {time.Now().Add(-3 * time.Hour).UTC().Format(http.TimeFormat)}}
	g.start()
	// The fake clock is in 2026 while the real Date is "now": the difference is large either way.
	g.wantOK()
	if len(g.kinds(crlrefresh.EventClockSkew)) == 0 {
		t.Fatalf("no clock-skew warning: %+v", g.dump())
	}
}

func TestRefresherDistrustedKey(t *testing.T) {
	var inter *x509.Certificate
	g := newCRig(t, func(g *crig) {
		g.policyMutate = func(p *audit.IdentityPolicy) {
			p.Distrust = map[audit.SPKIHash]time.Time{audit.SPKIHashOf(g.inter.Cert): testca.T0}
		}
	})
	_ = inter
	g.publishFresh(1)
	g.start()
	if g.refresh()[g.intK] == nil || len(g.kinds(crlrefresh.EventDistrusted)) != 1 {
		t.Fatalf("a CRL from a distrusted key was added: %+v", g.dump())
	}
}

func TestRefresherUnsupportedCRLIsReportedNotFetched(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	now := g.clk.Now()
	// A partitioned CRL (issuing distribution point) is refused by the snapshot; the refresher says so.
	g.srv.set("/inter.crl", g.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour),
		Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 28}, true, []byte{0x30, 0x00})}}))
	g.start()
	if g.refresh()[g.intK] == nil || len(g.kinds(crlrefresh.EventUnsupportedCRL)) == 0 {
		t.Fatalf("an IDP CRL was not refused as unsupported: %+v", g.dump())
	}
}

// idpFor is the value of a critical IDP extension holding only the fullName of one URI.
func idpFor(t *testing.T, uri string) []byte {
	t.Helper()
	name, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 6, Bytes: []byte(uri)})
	if err != nil {
		t.Fatal(err)
	}
	point, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: name})
	if err != nil {
		t.Fatal(err)
	}
	dp, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: point})
	if err != nil {
		t.Fatal(err)
	}
	out, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: dp})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A CRL whose issuing distribution point names the URL it was fetched from is added. One that names
// another URL is a misconfiguration (the source is not the point the CA says the CRL is for): it is
// refused with an unsupported-crl event naming both, and not added.
func TestRefresherIDPMustNameTheSourceURL(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	now := g.clk.Now()
	url := g.srv.URL + "/inter.crl"
	g.srv.set("/inter.crl", g.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour),
		Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 28}, true, idpFor(t, url))}}))
	g.start()
	g.wantOK()
	if h := g.health(g.intK); h.NewestNumber.Int64() != 2 || len(g.kinds(crlrefresh.EventUnsupportedCRL)) != 0 {
		t.Fatalf("a CRL whose IDP names its source was not added: %+v", g.dump())
	}

	g = newCRig(t)
	g.publishFresh(1)
	now = g.clk.Now()
	url = g.srv.URL + "/inter.crl"
	g.srv.set("/inter.crl", g.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour),
		Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 28}, true, idpFor(t, "http://elsewhere.example/other.crl"))}}))
	g.start()
	if g.refresh()[g.intK] == nil {
		t.Fatalf("a CRL whose IDP names another URL was added: %+v", g.dump())
	}
	evs := g.kinds(crlrefresh.EventUnsupportedCRL)
	if len(evs) == 0 || !strings.Contains(evs[0].Detail, "elsewhere.example") || !strings.Contains(evs[0].Detail, url) {
		t.Fatalf("the event does not name both URLs: %+v", evs)
	}
	if h := g.health(g.intK); h.NewestNumber != nil {
		t.Fatalf("the refused CRL became the newest: %+v", h)
	}
}

func TestRefresherArchiveFailureStillAddsAndRetries(t *testing.T) {
	fa := &flakyArchive{DirArchive: crlrefresh.DirArchive(t.TempDir()), fail: true}
	g := newCRig(t, withArchive(fa))
	g.publishFresh(1)
	g.start()
	g.wantOK()
	if g.snap.Len() != 2 {
		t.Fatal("a CRL was withheld because the archive failed")
	}
	if len(g.kinds(crlrefresh.EventArchiveError)) == 0 || g.health(g.intK).ArchiveBacklog == 0 {
		t.Fatalf("archive failure not reported: %+v", g.dump())
	}
	fa.fail = false
	g.wantOK()
	if g.health(g.intK).ArchiveBacklog != 0 {
		t.Fatal("the retry queue was not flushed")
	}
	ents, _ := fa.List(context.Background(), g.intK)
	if len(ents) != 1 {
		t.Fatalf("archive holds %d", len(ents))
	}
}

func TestRefresherArchiveQueueDropsOldestAndSaysSo(t *testing.T) {
	fa := &flakyArchive{DirArchive: crlrefresh.DirArchive(t.TempDir()), fail: true}
	g := newCRig(t, withArchive(fa))
	g.opts.MaxArchiveQueueBytes = 1
	g.publishFresh(1)
	g.start()
	g.refresh()
	if len(g.kinds(crlrefresh.EventArchiveDropped)) == 0 {
		t.Fatalf("a dropped archive entry was silent: %+v", g.dump())
	}
}

type flakyArchive struct {
	crlrefresh.DirArchive
	fail bool
}

func (f *flakyArchive) PutCRL(ctx context.Context, i audit.SPKIHash, a crlrefresh.ArchivedCRL, der []byte) error {
	if f.fail {
		return errors.New("disk full")
	}
	return f.DirArchive.PutCRL(ctx, i, a, der)
}

func TestNewCRLRefresherConfigErrors(t *testing.T) {
	base := func() *crig {
		g := newCRig(t)
		g.publishFresh(1)
		return g
	}
	for name, mod := range map[string]func(g *crig){
		"no source for an issuer": func(g *crig) {
			g.sources = audit.CRLSources{g.intK: {URLs: []string{g.srv.url("/inter.crl")}}}
		},
		"source for an unknown key": func(g *crig) {
			g.sources = audit.CRLSources{g.rootK: {URLs: []string{g.srv.url("/r")}}, g.intK: {URLs: []string{g.srv.url("/i")}}, {9}: {URLs: []string{g.srv.url("/x")}}}
		},
		"ldap url": func(g *crig) {
			g.sources = audit.CRLSources{g.rootK: {URLs: []string{"ldap://ldap.bank.example/cn=root"}}, g.intK: {URLs: []string{g.srv.url("/i")}}}
		},
		"credentials in a url": func(g *crig) {
			g.sources = audit.CRLSources{g.rootK: {URLs: []string{"http://u:pw@host.example/r.crl"}}, g.intK: {URLs: []string{g.srv.url("/i")}}}
		},
		"fragment": func(g *crig) {
			g.sources = audit.CRLSources{g.rootK: {URLs: []string{"http://h.example/r.crl#x"}}, g.intK: {URLs: []string{g.srv.url("/i")}}}
		},
		"no intermediate": func(g *crig) { g.opts.Issuers = nil },
		"unpinned issuer cert": func(g *crig) {
			g.opts.Issuers = append(g.opts.Issuers, g.root.NewIntermediate(t, "other", testca.Opts{}).Cert)
		},
		"cert not chaining to an anchor": func(g *crig) {
			g.opts.Issuers = []*x509.Certificate{testca.NewRoot(t, "stranger", testca.Opts{}).Cert}
		},
		"no clock":    func(g *crig) { g.ap.Now = nil },
		"no snapshot": func(g *crig) { g.ap.Status = nil },
		"delta no period": func(g *crig) {
			g.sources[g.intK] = audit.CRLSource{URLs: []string{g.srv.url("/i")}, DeltaURLs: []string{g.srv.url("/d")}}
		},
		"snapshot too small": func(g *crig) {
			g.ap.Status = audit.NewStatusSnapshot(audit.WithMaxCRLs(2), audit.WithLimits(agent.MaxStatusInline, 10))
		},
		"no period": func(g *crig) {
			g.ap.StatusParams.CAPeriod = 0
			delete(g.ap.StatusParams.CAPeriodFor, g.intK)
		},
		"no margin for a tier": func(g *crig) {
			// the root's limit equals its period: an alarm could not come before the pause
			g.ap.ExecStatusAgeFor[g.rootK] = rootPeriod
		},
	} {
		g := base()
		mod(g)
		_, err := crlrefresh.New(g.ap, g.sources, g.opts)
		if err == nil || !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRefresherNeverLogsAURLQuery(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	g.srv.set("/inter.crl", g.iCRL(1, g.clk.Now().Add(-time.Minute), time.Hour))
	g.sources[g.intK] = audit.CRLSource{URLs: []string{g.srv.url("/inter.crl") + "?token=SECRET"}}
	g.start()
	g.wantOK()
	g.srv.set("/inter.crl", []byte("garbage"))
	g.refresh()
	for _, h := range g.r.Health() {
		for _, u := range h.URLs {
			if strings.Contains(u, "SECRET") {
				t.Fatalf("Health leaks the query: %q", u)
			}
		}
		if strings.Contains(h.LastError, "SECRET") {
			t.Fatalf("LastError leaks the query: %q", h.LastError)
		}
	}
	for _, e := range g.events {
		if strings.Contains(e.URL+e.Detail, "SECRET") || (e.Err != nil && strings.Contains(e.Err.Error(), "SECRET")) {
			t.Fatalf("event leaks the query: %+v", e)
		}
	}
}

func TestRefresherFromPinnedCDP(t *testing.T) {
	srv := newCRLServer(t)
	g := newCRigWith(t, func(g *crig) {
		g.srv = srv
		g.interMutate = func(c *x509.Certificate) {
			c.CRLDistributionPoints = []string{srv.url("/root.crl"), "ldap://ldap.bank.example/cn=root"}
		}
	})
	g.sources = audit.CRLSources{g.rootK: {FromPinnedCDP: true}, g.intK: {URLs: []string{srv.url("/inter.crl")}}}
	g.publishFresh(1)
	g.start()
	g.wantOK()
	if h := g.health(g.rootK); len(h.URLs) != 1 || !strings.HasSuffix(h.URLs[0], "/root.crl") {
		t.Fatalf("effective root URLs %v (the ldap point must be skipped)", h.URLs)
	}
	// A source with neither urls nor a CDP to read is refused.
	g.sources = audit.CRLSources{g.rootK: {URLs: []string{srv.url("/root.crl")}}, g.intK: {FromPinnedCDP: true}}
	if _, err := crlrefresh.New(g.ap, g.sources, g.opts); err == nil {
		t.Fatal("the leaf CA's CRL was taken from a CDP: only leaves name it, and leaves are never a source")
	}
}

func newCRigWith(t *testing.T, pre func(*crig)) *crig {
	t.Helper()
	return newCRig(t, func(g *crig) { pre(g) })
}

// A CRL that was fetched fine but refused afterwards must be reported with the reason, not as a URL that
// is "ok" in a failed attempt.
func TestRefresherErrorNamesWhyAFetchedCRLWasNotAccepted(t *testing.T) {
	g := newCRig(t, withArchive(crlrefresh.DirArchive(t.TempDir())))
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	g.srv.set("/inter.crl", g.iCRL(5, now.Add(-2*time.Minute), time.Hour))
	g.start()
	g.wantOK()

	for name, tc := range map[string]struct {
		crl  []byte
		want string
	}{
		"conflict": {g.iCRL(5, now.Add(-time.Minute), time.Hour, testca.Revoked(g.leaf.Cert, now, 1)), "equivocating"},
		"rollback": {g.iCRL(4, now.Add(-time.Minute), time.Hour), "below the high-water mark"},
		"future":   {g.iCRL(6, now.Add(24*time.Hour), 48*time.Hour), "ahead of the local clock"},
	} {
		g.srv.set("/inter.crl", tc.crl)
		err := g.refresh()[g.intK]
		if err == nil {
			t.Fatalf("%s: counted as success", name)
		}
		if strings.Contains(err.Error(), ": ok") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the error does not say why the fetched CRL was refused: %v", name, err)
		}
		if h := g.health(g.intK); !strings.Contains(h.LastError, tc.want) {
			t.Errorf("%s: LastError %q", name, h.LastError)
		}
	}
}

func TestRefresherDeltaServedAtTheBaseURLIsReported(t *testing.T) {
	g := newCRig(t, withDelta())
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	// A delta at the base URL (the base feed's error is the one refresh returns).
	g.srv.set("/inter.crl", g.iDelta(11, 10, now.Add(-time.Minute), 20*time.Minute))
	g.start()
	err := g.refresh()[g.intK]
	if err == nil {
		t.Fatal("the wrong kind of CRL counted as success")
	}
	if want := "refused: a delta CRL was served at a base URL"; !strings.Contains(err.Error(), want) {
		t.Errorf("the error lacks %q: %v", want, err)
	}
}

package crlrefresh_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/crlrefresh"
	"github.com/bide-ai/bide/internal/testca"
)

func TestRestartKeepsRollbackMemoryAndSeedsTheSnapshot(t *testing.T) {
	g := newCRig(t, withArchive(crlrefresh.DirArchive(t.TempDir())))
	g.publishFresh(5)
	g.start()
	g.wantOK()

	g.restart()
	if g.snap.Len() != 0 {
		t.Fatal("a fresh snapshot is not empty")
	}
	// The server now serves an older CRL that is still inside its validity: with an empty snapshot and no
	// memory it would be accepted.
	now := g.clk.Now()
	g.srv.set("/inter.crl", g.iCRL(3, now.Add(-time.Minute), time.Hour))
	errs := g.refresh()
	if errs[g.intK] == nil {
		t.Fatal("a rollback after a restart counted as success")
	}
	rb := g.kinds(crlrefresh.EventRollback)
	if len(rb) != 1 || rb[0].Held.Int64() != 5 {
		t.Fatalf("the archive's mark was forgotten: %s", g.dump())
	}
	if h := g.health(g.intK); h.NewestNumber == nil || h.NewestNumber.Int64() != 5 {
		t.Fatalf("seeding did not load the archived CRL: %+v", h)
	}
}

func TestPoisonedHighWaterMarkResetSurvivesARestart(t *testing.T) {
	arch := crlrefresh.DirArchive(t.TempDir())
	g := newCRig(t, withArchive(arch))
	now := g.clk.Now()
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	g.srv.set("/inter.crl", g.iCRL(1_000_000, now.Add(-2*time.Minute), time.Hour))
	g.start()
	g.wantOK()

	g.srv.set("/inter.crl", g.iCRL(6, now.Add(-time.Minute), time.Hour))
	if g.refresh()[g.intK] == nil {
		t.Fatal("a real CRL after a poisoned number was accepted without a reset")
	}
	ctx := context.Background()
	if err := g.r.ResetHighWater(ctx, g.intK, crlrefresh.ResetRequest{Operator: "ops"}); err == nil {
		t.Fatal("a reset without a reason")
	}
	if err := g.r.ResetHighWater(ctx, g.intK, crlrefresh.ResetRequest{Reason: "CA issued number 1000000 by mistake", Operator: "dayna"}); err != nil {
		t.Fatal(err)
	}
	if len(g.kinds(crlrefresh.EventHighWaterReset)) != 1 {
		t.Fatalf("no reset event: %s", g.dump())
	}
	if h := g.health(g.intK); h.NewestNumber != nil {
		t.Fatalf("the poisoned CRL is still in the snapshot: %+v", h)
	}
	g.wantOK()
	if h := g.health(g.intK); h.NewestNumber.Int64() != 6 {
		t.Fatalf("after the reset the real CRL was not accepted: %+v", h)
	}

	// The record is durable and names what it sets aside; the file stays in the archive.
	resets, err := arch.Resets(ctx, g.intK)
	if err != nil || len(resets) != 1 || resets[0].Operator != "dayna" || len(resets[0].SetAside) != 1 || resets[0].SetAside[0].Number != "1000000" {
		t.Fatalf("resets %+v %v", resets, err)
	}
	ents, _ := arch.List(ctx, g.intK)
	listed := false
	for _, e := range ents {
		listed = listed || e.Number.Int64() == 1_000_000
	}
	if !listed {
		t.Fatal("the set-aside CRL left the archive")
	}

	// A restart honours it: the mark is computed without the poisoned CRL, and the operator hears of the
	// reset at every start until it is acknowledged.
	g.restart()
	g.srv.set("/inter.crl", g.iCRL(7, now, time.Hour))
	g.wantOK()
	if h := g.health(g.intK); h.NewestNumber.Int64() != 7 || h.UnackedResets != 1 {
		t.Fatalf("after a restart: %+v", h)
	}
	if len(g.kinds(crlrefresh.EventHighWaterReset)) != 1 {
		t.Fatalf("no start-up reset event: %s", g.dump())
	}
	if err := g.r.AckReset(ctx, g.intK, resets[0].Name); err != nil {
		t.Fatal(err)
	}
	g.restart()
	g.refresh()
	if len(g.kinds(crlrefresh.EventHighWaterReset)) != 0 || g.health(g.intK).UnackedResets != 0 {
		t.Fatalf("an acknowledged reset is still reported: %s", g.dump())
	}
}

func TestAMarkNeverComesFromAFileNameAlone(t *testing.T) {
	arch := crlrefresh.DirArchive(t.TempDir())
	g := newCRig(t, withArchive(arch))
	g.publishFresh(2)
	g.start()
	g.wantOK()
	dir := filepath.Join(string(arch), "crl", hex.EncodeToString(g.intK[:]))
	if err := os.WriteFile(filepath.Join(dir, "999999999-20261001T120000Z.crl"), []byte("not a crl"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.restart()
	g.srv.set("/inter.crl", g.iCRL(3, g.clk.Now().Add(-time.Minute), time.Hour))
	g.wantOK()
	if len(g.kinds(crlrefresh.EventArchiveCorrupt)) != 1 {
		t.Fatalf("a planted file was not reported: %s", g.dump())
	}
	if g.health(g.intK).NewestNumber.Int64() != 3 {
		t.Fatal("a planted file poisoned the mark")
	}
}

func TestSeedingDoesNotRaiseTheEvictionBookkeeping(t *testing.T) {
	arch := crlrefresh.DirArchive(t.TempDir())
	g := newCRig(t, withArchive(arch), withSnapOpts(audit.WithMaxCRLs(8)))
	now := g.clk.Now()
	// Twelve archived CRLs, all inside the status window: more than the snapshot keeps.
	g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
	g.start()
	for n := int64(1); n <= 11; n++ {
		g.srv.set("/inter.crl", g.iCRL(n, now.Add(time.Duration(n-12)*time.Minute), time.Hour))
		g.refresh()
	}
	g.restart()
	g.refresh()
	if g.snap.Evicted() != 0 {
		t.Fatalf("seeding evicted %d CRLs: live status would turn non-final for nothing", g.snap.Evicted())
	}
}

func TestDirArchive(t *testing.T) {
	g := newCRig(t)
	dir := t.TempDir()
	arch := crlrefresh.DirArchive(filepath.Join(dir, "a"))
	ctx := context.Background()
	der := g.iCRL(3, testca.T0, time.Hour)
	rec := crlrefresh.ArchivedCRL{Number: bigInt(3), ThisUpdate: testca.T0, Source: "http://x/y.crl", Fetched: testca.T0}
	if err := arch.PutCRL(ctx, g.intK, rec, der); err != nil {
		t.Fatal(err)
	}
	if err := arch.PutCRL(ctx, g.intK, rec, der); err != nil {
		t.Fatalf("a second put of the same CRL: %v", err)
	}
	if err := arch.PutCRL(ctx, g.intK, rec, g.iCRL(3, testca.T0, 2*time.Hour)); err == nil {
		t.Fatal("two different CRLs under one series name")
	}
	mk := rec
	mk.Marker = crlrefresh.MarkerConflict
	if err := arch.PutCRL(ctx, g.intK, mk, g.iCRL(3, testca.T0, 2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	ents, err := arch.List(ctx, g.intK)
	if err != nil || len(ents) != 2 {
		t.Fatalf("%v %v", ents, err)
	}
	var series crlrefresh.ArchivedCRL
	for _, e := range ents {
		if e.Marker == "" {
			series = e
		}
	}
	if series.Source != "http://x/y.crl" || !series.Fetched.Equal(testca.T0) {
		t.Fatalf("provenance lost: %+v", series)
	}
	got, err := arch.Read(ctx, g.intK, series)
	if err != nil || string(got) != string(der) {
		t.Fatalf("read back: %v", err)
	}
	if _, err := arch.Read(ctx, g.intK, crlrefresh.ArchivedCRL{Name: "../../etc/passwd"}); err == nil {
		t.Fatal("a path outside the archive was read")
	}
	r := crlrefresh.ResetRecord{Issuer: "x", Reason: "r", Operator: "o", Time: testca.T0}
	if err := arch.PutReset(ctx, g.intK, r); err != nil {
		t.Fatal(err)
	}
	if err := arch.PutReset(ctx, g.intK, r); err == nil {
		t.Fatal("a reset record was overwritten: they are append-only")
	}
	rs, _ := arch.Resets(ctx, g.intK)
	if len(rs) != 1 || rs[0].Acked {
		t.Fatalf("%+v", rs)
	}
	if err := arch.AckReset(ctx, g.intK, rs[0]); err != nil {
		t.Fatal(err)
	}
	if rs, _ = arch.Resets(ctx, g.intK); !rs[0].Acked {
		t.Fatal("ack did not stick")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, "a"))
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("archive directory mode %v %v", fi.Mode(), err)
		}
		bad := filepath.Join(dir, "open")
		if err := os.Mkdir(bad, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(bad, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := crlrefresh.DirArchive(bad).PutCRL(ctx, g.intK, rec, der); err == nil {
			t.Fatal("a world-writable archive directory was accepted")
		}
	}
}

func TestRunRefreshesUntilCancelledAndLeavesNothingRunning(t *testing.T) {
	g := newCRig(t)
	g.clk.real = time.Millisecond
	g.publishFresh(1)
	g.start()
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = g.r.Run(ctx); close(done) }()
	select {
	case <-g.r.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("Ready never closed")
	}
	// Virtual time runs fast: the loop must have asked again, with sleeps no longer than the alarm step.
	deadline := time.Now().Add(5 * time.Second)
	for g.srv.routes["/inter.crl"].hitCount() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if g.srv.routes["/inter.crl"].hitCount() < 3 {
		t.Fatal("the loop did not fetch again")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	g.clk.mu.Lock()
	for _, d := range g.clk.slept {
		if d > 30*time.Second {
			t.Fatalf("a sleep of %v: the alarm would not be evaluated while waiting", d)
		}
	}
	g.clk.mu.Unlock()
	deadline = time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+1 {
		t.Fatalf("%d goroutines after Run returned, %d before", n, before)
	}
}

func (r *crlRoute) hitCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits
}

func TestRunAlarmsBeforeTheGatesPauseAndReportsBoth(t *testing.T) {
	g := newCRig(t)
	g.clk.real = time.Millisecond
	g.publishFresh(1)
	g.start()
	// The CA then stops publishing: the server keeps serving CRL 1.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = g.r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	var seen []crlrefresh.Alarm
	for time.Now().Before(deadline) {
		g.mu.Lock()
		seen = append([]crlrefresh.Alarm(nil), g.alarms...)
		g.mu.Unlock()
		crit := false
		for _, a := range seen {
			crit = crit || (a.Critical && a.Issuer == g.intK)
		}
		if crit {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	var first, crit *crlrefresh.Alarm
	for i := range seen {
		a := &seen[i]
		if a.Issuer != g.intK {
			continue
		}
		if a.Active && !a.Critical && first == nil {
			first = a
		}
		if a.Critical && crit == nil {
			crit = a
		}
	}
	if first == nil || crit == nil {
		t.Fatalf("alarms %+v", seen)
	}
	if first.At.After(crit.At) || first.Age <= first.Threshold-time.Minute {
		t.Fatalf("the alarm did not come first: %+v then %+v", first, crit)
	}
	if first.Threshold != 31*time.Minute || first.Limit != time.Hour {
		t.Fatalf("threshold %v limit %v, want max(limit/2, period+tolerance)=31m under 1h", first.Threshold, first.Limit)
	}
}

func TestCriticalIsExactlyTheGatesFreshnessRule(t *testing.T) {
	for _, skew := range []time.Duration{0, 10 * time.Minute} {
		g := newCRig(t, withSkew(skew))
		now := g.clk.Now()
		this := now.Add(-time.Minute)
		g.srv.set("/root.crl", g.rCRL(1, now.Add(-time.Hour), 48*time.Hour))
		g.srv.set("/inter.crl", g.iCRL(1, this, 3*time.Hour))
		g.start()
		g.wantOK()
		ac, err := g.ci.VerifyChain("alice", g.leaf.Chain(), testca.T0, testca.T0.Add(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		limit := time.Hour + skew // the gate accepts age <= MaxStatusAge + skew
		for _, d := range []time.Duration{limit - time.Second, limit, limit + time.Second} {
			at := this.Add(d)
			g.clk.mu.Lock()
			g.clk.t = at
			g.clk.mu.Unlock()
			p := g.ap.StatusParams
			p.Now = at
			st := g.ci.EvaluateStatus(ac, testca.T0.Add(5*time.Second), g.snap, p)
			gateFresh := st.Certs[0].Tier != audit.TierNoInformation
			if got := !g.health(g.intK).Critical; got != gateFresh {
				t.Errorf("skew %v age %v: refresher says fresh=%v, the gate says %v", skew, d, got, gateFresh)
			}
		}
	}
}

func TestRacesBetweenGateEvaluationAndRotation(t *testing.T) {
	g := newCRig(t)
	g.publishFresh(1)
	g.start()
	g.wantOK()
	ac, err := g.ci.VerifyChain("alice", g.leaf.Chain(), testca.T0, testca.T0.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				p := g.ap.StatusParams
				p.Now = g.clk.Now()
				_ = g.ci.EvaluateStatus(ac, testca.T0.Add(5*time.Second), g.snap, p)
				_ = g.r.Health()
				_ = g.snap.Len()
			}
		}()
	}
	for n := int64(2); n < 12; n++ {
		g.clk.advance(time.Minute)
		g.publishFresh(n)
		g.refresh()
	}
	close(stop)
	wg.Wait()
	if h := g.health(g.intK); h.NewestNumber.Int64() != 11 {
		t.Fatalf("%+v", h)
	}
}

func TestCRLSourcesFromThePolicyFile(t *testing.T) {
	g := newCRig(t)
	files := map[string][]byte{
		"root.pem":  pemCert(g.root.Cert.Raw),
		"inter.pem": pemCert(g.inter.Cert.Raw),
	}
	read := func(rel string) ([]byte, error) {
		b, ok := files[rel]
		if !ok {
			return nil, os.ErrNotExist
		}
		return b, nil
	}
	doc := func(urls string) []byte {
		return []byte(`{"anchors":["root.pem"],"intermediates":["` + hex.EncodeToString(g.intK[:]) + `"],
		"identity":{"alice":"` + testca.Person(1) + `"},"id_kind":"person-uri","eku":"` + testca.EKU.String() + `",
		"policy_oid":"` + testca.PolicyOID.String() + `","canonical":{"host":"idp.bank.example"},
		"intermediate_certs":["inter.pem"],
		"crl":{"` + hex.EncodeToString(g.intK[:]) + `":{"urls":[` + urls + `]},"` + hex.EncodeToString(g.rootK[:]) + `":{"urls":["http://pki.bank.example/root.crl"]}}}`)
	}
	a, b := doc(`"http://pki.bank.example/a.crl"`), doc(`"http://moved.bank.example/b.crl","https://x.example/c.crl"`)
	ca, err := audit.ParseIdentityPolicy("p.json", a, read)
	if err != nil {
		t.Fatalf("the policy parser must ignore the refresher members: %v", err)
	}
	cb, err := audit.ParseIdentityPolicy("p.json", b, read)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(c *audit.CertIdentity) string {
		return (&audit.CertApprover{ApproverID: "alice", Policy: c}).PolicyDigest()
	}
	if mk(ca) != mk(cb) {
		t.Fatal("moving a CRL URL changed the policy digest")
	}
	src, certs, err := audit.ParseCRLSources("p.json", b, read)
	if err != nil || len(certs) != 1 || len(src[g.intK].URLs) != 2 || len(src[g.rootK].URLs) != 1 {
		t.Fatalf("%v %v %v", src, certs, err)
	}
	for name, bad := range map[string]string{
		"ldap":           strings.Replace(string(a), "http://pki.bank.example/a.crl", "ldap://pki.bank.example/cn=x", 1),
		"credentials":    strings.Replace(string(a), "http://pki.bank.example/a.crl", "http://u:p@pki.bank.example/a.crl", 1),
		"uppercase key":  strings.Replace(string(a), hex.EncodeToString(g.rootK[:]), strings.ToUpper(hex.EncodeToString(g.rootK[:])), 1),
		"missing cert":   strings.Replace(string(a), "inter.pem", "nope.pem", 1),
		"path escape":    strings.Replace(string(a), `"inter.pem"`, `"../inter.pem"`, 1),
		"unknown member": strings.Replace(string(a), `"crl"`, `"crls"`, 1),
	} {
		if _, _, err := audit.ParseCRLSources("p.json", []byte(bad), read); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var m map[string]any
	_ = json.Unmarshal(a, &m)
}

// A seed that stopped after taking the mark but before adding the CRL must not make the fetched CRL
// "unchanged" without the gate holding it.
func TestUnchangedCRLIsStillHeldByTheSnapshot(t *testing.T) {
	arch := crlrefresh.DirArchive(t.TempDir())
	g := newCRig(t, withArchive(arch))
	g.publishFresh(3)
	g.start()
	g.wantOK()
	g.restart()
	if err := g.r.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The seed took the mark (3) but the snapshot lacks the intermediate's CRL, as after a seed that stopped
	// early: the root's CRL stays, so the refresher does not re-seed.
	for _, c := range g.snap.HeldCRLs(g.inter.Cert) {
		g.snap.RemoveCRL(c.Hash)
	}
	g.wantOK()
	if len(g.snap.HeldCRLs(g.inter.Cert)) == 0 {
		t.Fatal("an unchanged CRL was reported while the snapshot lacked it")
	}
}

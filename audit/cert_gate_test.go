package audit_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// The gate of a policy with a certificate-bound approver: anchor first, say why it waits, and journal
// the identity environment of the terminal count (docs/design/approver-identity.md, "Journal and
// format changes").

// reanchoringStore is a store that can publish a head over a run on demand, as audit.AuditedStore
// does: here it signs one through the certGate and counts the calls.
type reanchoringStore struct {
	agent.Store
	calls  int
	anchor func(ctx context.Context, runID string) error
}

func (s *reanchoringStore) Reanchor(ctx context.Context, runID string) error {
	s.calls++
	if s.anchor == nil {
		return nil
	}
	return s.anchor(ctx, runID)
}

// withReanchoring replaces c's journal by one over a reanchoring store whose Reanchor signs a head at
// certA (when sign is set), and returns the store.
func (c *certGate) withReanchoring(sign bool) *reanchoringStore {
	rs := &reanchoringStore{Store: agent.NewMemStore()}
	j, err := agent.NewJournal(rs)
	if err != nil {
		c.t.Fatal(err)
	}
	c.store = j
	if sign {
		rs.anchor = func(context.Context, string) error { c.anchor(certA); return nil }
	}
	return rs
}

func (c *certGate) recordByName(name string) (agent.Record, bool) {
	c.t.Helper()
	recs, err := c.store.History(context.Background(), gateRun)
	if err != nil {
		c.t.Fatal(err)
	}
	for _, r := range recs {
		if r.Name == name {
			return r, true
		}
	}
	return agent.Record{}, false
}

func (c *certGate) pauseNote() (agent.ApprovalWait, bool) {
	c.t.Helper()
	recs, err := c.store.History(context.Background(), gateRun)
	if err != nil {
		c.t.Fatal(err)
	}
	return agent.LatestPausePhase(recs, "c1")
}

// A decision journaled directly (bide approve) has no covering head; the gate asks the store to anchor
// and counts it, in one drive, so a run that waits on the gate is not stuck.
func TestCertGate_AnchorsFirst(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	rs := c.withReanchoring(true)
	c.pause()
	c.decideCert("alice", nil)
	if err := c.run(); err != nil || c.charged != 1 {
		t.Fatalf("run: %v, charged %d", err, c.charged)
	}
	if rs.calls != 1 {
		t.Fatalf("Reanchor called %d times", rs.calls)
	}
	// Counted only once a head covered it: the tally exists and its decision has a head.
	if _, ok := c.recordByName(agent.ApprovalTallyStep("c1")); !ok {
		t.Fatal("no tally")
	}
}

// Without a store that can anchor, the gate waits and says so; the decision is not lost, and a head
// published later (by the application's next write) lets the next drive count it.
func TestCertGate_WaitsForAnAnchorItCannotMake(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	c.pause()
	c.decideCert("alice", nil)
	if err := c.run(); err == nil || c.charged != 0 {
		t.Fatalf("the gate ran on an unanchored decision: %v, charged %d", err, c.charged)
	}
	w, ok := c.pauseNote()
	if !ok || w.Pause != agent.PauseAwaitingAnchor || w.PauseDetail == "" || !w.IdentityBound {
		t.Fatalf("pause note %+v (%v)", w, ok)
	}
	c.anchor(certA)
	if err := c.run(); err != nil || c.charged != 1 {
		t.Fatalf("after the head: %v, charged %d", err, c.charged)
	}
}

// Anchoring is not a free signing oracle: a junk decision never makes the gate sign a head.
func TestCertGate_JunkDecisionDoesNotCauseAHead(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	rs := c.withReanchoring(true)
	c.pause()
	c.decideCert("alice", func(d *agent.Decision) { d.Signature = append([]byte{}, d.Signature...); d.Signature[0] ^= 0xff })
	if err := c.run(); err == nil || c.charged != 0 {
		t.Fatalf("a junk decision: %v, charged %d", err, c.charged)
	}
	if rs.calls != 0 {
		t.Fatalf("a junk decision made the gate anchor %d times", rs.calls)
	}
}

// The gate asks to anchor at most once per ReanchorMinInterval per run, however often it is driven.
func TestCertGate_ReanchorIsRateLimited(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	now := certA
	c.clock = func() time.Time { return now }
	rs := c.withReanchoring(false) // the anchor does not produce a head: the decision stays unanchored
	c.pause()
	c.decideCert("alice", nil)
	for range 3 {
		_ = c.run()
	}
	if rs.calls != 1 {
		t.Fatalf("%d anchors within the interval", rs.calls)
	}
	now = now.Add(agent.ReanchorMinInterval)
	_ = c.run()
	if rs.calls != 2 {
		t.Fatalf("%d anchors after the interval", rs.calls)
	}
}

// Missing status is a pause with its own phase, never an uncount.
func TestCertGate_PausePhaseAwaitingStatus(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	c.status = audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000)) // no CRLs
	c.pause()
	c.decideCert("alice", nil)
	c.anchor(certA)
	if err := c.run(); err == nil || c.charged != 0 {
		t.Fatalf("%v, charged %d", err, c.charged)
	}
	w, ok := c.pauseNote()
	if !ok || w.Pause != agent.PauseAwaitingStatus {
		t.Fatalf("pause note %+v (%v)", w, ok)
	}
	// Driving again changes nothing: no second note for the same phase.
	_ = c.run()
	recs, _ := c.store.History(context.Background(), gateRun)
	n := 0
	for _, r := range recs {
		if strings.HasPrefix(r.Name, "approval-pause:") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d phase notes for one phase", n)
	}
}

// The first note a gate writes says it has a certificate-bound approver, which ones, and the policy digest.
func TestCertGate_PendingNoteCarriesTheIdentityFields(t *testing.T) {
	c := newCertGate(t, 2, "alice", "dave")
	c.pub("dave")
	c.pause()
	r, ok := c.recordByName(agent.ApprovalPendingStep("c1"))
	if !ok {
		t.Fatal("no pending note")
	}
	var w agent.ApprovalWait
	if err := json.Unmarshal(r.Result, &w); err != nil {
		t.Fatal(err)
	}
	if !w.IdentityBound || len(w.IdentityApprovers) != 1 || w.IdentityApprovers[0] != "alice" || len(w.IdentityKinds) != 64 ||
		w.IdentityPolicyDigest != c.approver("alice").PolicyDigest() {
		t.Fatalf("%+v", w)
	}
	// A bare-key gate's note has none.
	g := newGate(1, []string{"alice"}, []string{"alice"})
	wantPaused(t, g.run(), 0)
	r, _ = func() (agent.Record, bool) {
		for _, x := range journalOf(t, g) {
			if x.Name == agent.ApprovalPendingStep("c1") {
				return x, true
			}
		}
		return agent.Record{}, false
	}()
	if strings.Contains(string(r.Result), "identity") {
		t.Fatalf("a bare-key note: %s", r.Result)
	}
}

// A terminal count of an identity-bound gate journals the identity environment beside the tally: the
// policy in force, each counted decision's chain, anchor, time, tier, basis and status hashes, and the
// CRLs by hash.
func TestCertGate_RecordsTheIdentityEnvironment(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	c.goDebug = "x509sha1=1"
	c.pause()
	c.decideCert("alice", nil)
	c.anchor(certA)
	if err := c.run(); err != nil || c.charged != 1 {
		t.Fatalf("%v %d", err, c.charged)
	}
	r, ok := c.recordByName(agent.ApprovalIdentityStep("c1", 0))
	if !ok {
		t.Fatal("no identity record")
	}
	var id agent.ApprovalIdentity
	if err := json.Unmarshal(r.Result, &id); err != nil {
		t.Fatal(err)
	}
	a := c.approver("alice")
	tr, _ := c.recordByName(agent.ApprovalTallyStep("c1"))
	if tsum := sha256.Sum256(tr.Result); id.TallyDigest != hex.EncodeToString(tsum[:]) || id.Tally != agent.ApprovalTallyStep("c1") || id.Generation != 0 ||
		id.Evaluated != agent.EvaluatedLive || id.EvaluatedAt == "" {
		t.Fatalf("tally binding: %+v (tally bytes %s)", id, tr.Result)
	}
	if id.Format != agent.ApprovalIdentityFormat || id.ToolUseID != "c1" || id.PolicyDigest != a.PolicyDigest() || id.RosterDigest != a.RosterDigest() ||
		id.Rules != agent.ApprovalIdentityRulesVer || !strings.HasPrefix(id.Verifier, "go") || len(id.Decisions) != 1 {
		t.Fatalf("%+v", id)
	}
	d := id.Decisions[0]
	if d.Approver != "alice" || d.Identifier == "" || d.LeafHash != d.Chain[0] || len(d.Chain) != 3 || d.Anchor == "" || d.HeadSize == 0 ||
		d.TimeSource != agent.TimeSourceLogSignerClock || d.BasisKind != agent.BasisAtOrAfterDecision || d.HeadRoot == "" || d.HeadHash == "" || d.Tier != agent.TierLogclockFinal || d.Basis == "" || d.StatusTime == "" || d.Window == "" || len(d.Status) == 0 {
		t.Fatalf("%+v", d)
	}
	for _, h := range d.Status {
		sr, ok := c.recordByName(agent.ApprovalStatusStep(h))
		if !ok {
			t.Fatalf("no status record %s", h)
		}
		var sb agent.ApprovalStatusBytes
		if err := json.Unmarshal(sr.Result, &sb); err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(sb.DER); hex.EncodeToString(sum[:]) != h || sb.Kind != "crl" {
			t.Fatalf("status record %s holds %+v", h, sb)
		}
	}
	// Tier follows the finding: a live count (status issued before the decision) is provisional.
	prov := newCertGate(t, 1, "alice")
	prov.params.CountLive, prov.params.MaxStatusAge = true, time.Hour
	prov.now = func() time.Time { return certA.Add(time.Minute) }
	prov.status = audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
	if _, err := prov.status.AddCRL(prov.inter.NewCRL(t, crlBefore(prov))); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.status.AddCRL(prov.root.NewCRL(t, crlBefore(prov))); err != nil {
		t.Fatal(err)
	}
	prov.pause()
	prov.decideCert("alice", nil)
	prov.anchor(certA)
	if err := prov.run(); err != nil {
		t.Fatalf("live count: %v", err)
	}
	pr, _ := prov.recordByName(agent.ApprovalIdentityStep("c1", 0))
	var pid agent.ApprovalIdentity
	_ = json.Unmarshal(pr.Result, &pid)
	if len(pid.Decisions) != 1 || pid.Decisions[0].Tier != agent.TierProvisional || pid.Decisions[0].BasisKind != agent.BasisLiveBeforeDecision {
		t.Fatalf("%+v", pid)
	}
}

// A gate that is not terminal writes no identity record, and a bare-key gate never does.
func TestCertGate_NoIdentityRecordWithoutATerminalCountOrACertificateApprover(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	c.pause()
	if _, ok := c.recordByName(agent.ApprovalIdentityStep("c1", 0)); ok {
		t.Fatal("identity record before a count")
	}
	g := newGate(1, []string{"alice"}, []string{"alice"})
	wantPaused(t, g.run(), 0)
	g.decide(t, "alice", true, false)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	for _, r := range journalOf(t, g) {
		if strings.HasPrefix(r.Name, "approval-identity:") || strings.HasPrefix(r.Name, "approval-status:") || strings.HasPrefix(r.Name, "approval-pause:") {
			t.Fatalf("a bare-key gate wrote %s", r.Name)
		}
	}
}

// crlBefore is a CRL issued shortly before the decision and fresh at the live count.
func crlBefore(c *certGate) testca.CRLOpts {
	return testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt.Add(-time.Minute), NextUpdate: testDecidedAt.Add(time.Hour)}
}

// failingTallyStore fails the first insert of the tally, once, as a crash between the identity record
// and the tally would.
type failingTallyStore struct {
	agent.Store
	failed bool
}

func (s *failingTallyStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if !s.failed && name == agent.ApprovalTallyStep("c1") {
		s.failed = true
		return agent.Entry{}, false, errors.New("injected: the process died before the tally")
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// The identity record is written before the tally and names it by the digest of its exact bytes. If the
// process dies between the two and the next count differs, the stale record under the plain name is not
// reused: the new record sits under a digest name, and the recorded tally's digest matches exactly one.
func TestCertGate_IdentityRecordFollowsTheTallyAfterACrash(t *testing.T) {
	c := newCertGate(t, 1, "alice", "bob")
	fs := &failingTallyStore{Store: agent.NewMemStore()}
	j, err := agent.NewJournal(fs)
	if err != nil {
		t.Fatal(err)
	}
	c.store = j
	c.pause()
	c.decideCert("alice", nil)
	c.anchor(certA)
	if err := c.run(); err == nil || c.charged != 0 {
		t.Fatalf("the first drive should have failed at the tally: %v, charged %d", err, c.charged)
	}
	if _, ok := c.recordByName(agent.ApprovalTallyStep("c1")); ok {
		t.Fatal("the tally was written")
	}
	first, ok := c.recordByName(agent.ApprovalIdentityStep("c1", 0))
	if !ok {
		t.Fatal("no identity record from the first evaluation")
	}
	// Another decision arrives before the next evaluation: the count (and so the tally) differs.
	c.decideCert("bob", nil)
	c.anchor(certA.Add(time.Second))
	if err := c.run(); err != nil || c.charged != 1 {
		t.Fatalf("second drive: %v, charged %d", err, c.charged)
	}
	tr, _ := c.recordByName(agent.ApprovalTallyStep("c1"))
	sum := sha256.Sum256(tr.Result)
	digest := hex.EncodeToString(sum[:])
	var fid agent.ApprovalIdentity
	_ = json.Unmarshal(first.Result, &fid)
	if fid.TallyDigest == digest {
		t.Skip("the two counts happened to be identical")
	}
	r, ok := c.recordByName(agent.ApprovalIdentityDigestStep("c1", 0, digest))
	if !ok {
		t.Fatal("no identity record under the digest name")
	}
	var id agent.ApprovalIdentity
	if err := json.Unmarshal(r.Result, &id); err != nil || id.TallyDigest != digest || id.Tally != agent.ApprovalTallyStep("c1") {
		t.Fatalf("%v %+v", err, id)
	}
}

type memArchive struct{ got map[string][]byte }

func (a *memArchive) PutStatus(_ context.Context, hash string, der []byte) error {
	if a.got == nil {
		a.got = map[string][]byte{}
	}
	a.got[hash] = append([]byte(nil), der...)
	return nil
}

// A CRL above agent.MaxStatusInline is not stored in the journal. A policy with no status archive must
// bound its snapshot at configuration (ValidateConfig), so an oversize CRL is refused at intake and the
// gate pauses for status instead of failing at the moment of counting; with an archive the bytes are
// archived first and the journal records the hash and size only.
func TestCertGate_LargeStatusGoesToTheArchive(t *testing.T) {
	var entries []x509.RevocationListEntry
	for i := range 12000 {
		entries = append(entries, x509.RevocationListEntry{SerialNumber: big.NewInt(int64(1_000_000 + i)), RevocationTime: testDecidedAt.Add(-24 * time.Hour)})
	}
	// An unbounded snapshot and no archive is a configuration error at startup, naming the fix.
	c := newCertGate(t, 1, "alice")
	c.status = audit.NewStatusSnapshot()
	err := c.policy.ValidateKeys(c.resolve)
	if err == nil || !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "StatusArchive") {
		t.Fatalf("unbounded snapshot, no archive: %v", err)
	}
	// A bounded snapshot refuses the oversize CRL at intake; the gate waits for status.
	c = newCertGate(t, 1, "alice")
	big := c.inter.NewCRL(t, testca.CRLOpts{Number: 3, ThisUpdate: c.crlsAt, NextUpdate: c.crlsAt.Add(time.Hour), Entries: entries})
	if len(big) <= agent.MaxStatusInline {
		t.Fatalf("test CRL is only %d bytes", len(big))
	}
	if _, err := c.status.AddCRL(big); err == nil {
		t.Fatal("a bounded snapshot took an oversize CRL")
	}
	mk := func() *certGate {
		c := newCertGate(t, 1, "alice")
		c.archive = &memArchive{}
		c.status = audit.NewStatusSnapshot()
		for _, der := range [][]byte{
			c.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(time.Hour), Entries: entries}),
			c.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: c.crlsAt, NextUpdate: c.crlsAt.Add(time.Hour), Entries: entries}),
			c.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(time.Hour)}),
			c.root.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: c.crlsAt, NextUpdate: c.crlsAt.Add(time.Hour)}),
		} {
			if _, err := c.status.AddCRL(der); err != nil {
				t.Fatal(err)
			}
		}
		c.pause()
		c.decideCert("alice", nil)
		c.anchor(certA)
		return c
	}
	c = mk()
	arch := c.archive.(*memArchive)
	if err := c.run(); err != nil || c.charged != 1 {
		t.Fatalf("with an archive: %v, charged %d", err, c.charged)
	}
	if len(arch.got) == 0 {
		t.Fatal("nothing archived")
	}
	for h, der := range arch.got {
		if sum := sha256.Sum256(der); hex.EncodeToString(sum[:]) != h || len(der) <= agent.MaxStatusInline {
			t.Fatalf("archived %s: %d bytes", h, len(der))
		}
		r, ok := c.recordByName(agent.ApprovalStatusStep(h))
		if !ok {
			t.Fatal("no status record for an archived CRL")
		}
		var sb agent.ApprovalStatusBytes
		if err := json.Unmarshal(r.Result, &sb); err != nil || len(sb.DER) != 0 || sb.Size != len(der) {
			t.Fatalf("%v %+v", err, sb)
		}
	}
}

// The identity record lists the decisions that counted, not every certificate-bound decision read: a
// decision that failed (here a bad signature) is not in it.
func TestCertGate_IdentityRecordListsOnlyCountedDecisions(t *testing.T) {
	c := newCertGate(t, 1, "alice", "bob")
	c.pause()
	c.decideCert("bob", func(d *agent.Decision) { d.Signature = append([]byte{}, d.Signature...); d.Signature[0] ^= 0xff })
	c.decideCert("alice", nil)
	c.anchor(certA)
	if err := c.run(); err != nil || c.charged != 1 {
		t.Fatalf("%v %d", err, c.charged)
	}
	r, _ := c.recordByName(agent.ApprovalIdentityStep("c1", 0))
	var id agent.ApprovalIdentity
	if err := json.Unmarshal(r.Result, &id); err != nil || len(id.Decisions) != 1 || id.Decisions[0].Approver != "alice" {
		t.Fatalf("%v %+v", err, id.Decisions)
	}
}

// htmlCanon is a canonicalizer whose name carries HTML characters, to show the policy digest is over
// unescaped JSON.
type htmlCanon struct{ audit.Canonicalizer }

func (htmlCanon) Name() string { return "a&b<c>" }

func TestCertApprover_PolicyDigestIsOverUnescapedJSON(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	ci, err := audit.IdentityPolicy{
		Anchors: []*x509.Certificate{c.root.Cert}, Inter: []audit.SPKIHash{audit.SPKIHashOf(c.inter.Cert)},
		Identity: map[string]string{"alice": testca.Person(1)}, IDKind: audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical: htmlCanon{audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"})},
	}.Compile()
	if err != nil {
		t.Fatal(err)
	}
	a := c.approver("alice")
	a.Policy = ci
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(a.DescribeIdentity())
	sum := sha256.Sum256(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	if !strings.Contains(b.String(), "a&b<c>") || a.PolicyDigest() != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest %s over %s", a.PolicyDigest(), b.String())
	}
}

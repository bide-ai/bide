package audit_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

type stopRig struct {
	t      *testing.T
	events *audit.MemEventStore
	trail  *audit.StopTrail
	store  agent.Store
	j      *agent.Journal
	ctl    *agent.StopControl
	policy agent.StopPolicy
	keys   map[string]ed25519.PrivateKey
	head   ed25519.PrivateKey
	pub    ed25519.PublicKey
}

func newStopRig(t *testing.T) *stopRig {
	t.Helper()
	r := &stopRig{t: t, events: audit.NewMemEventStore(), store: agent.NewMemStore(), keys: map[string]ed25519.PrivateKey{}}
	pubs := map[string]ed25519.PublicKey{}
	for _, id := range []string{"ops1", "ops2", "risk"} {
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		pubs[id], r.keys[id] = pub, priv
	}
	r.pub, r.head, _ = func() (ed25519.PublicKey, ed25519.PrivateKey, error) { return ed25519.GenerateKey(nil) }()
	r.policy = agent.StopPolicy{DeploymentID: "prod-91c2", Setters: []string{"ops1", "ops2"}, Lifters: []string{"ops1", "ops2", "risk"}, LiftQuorum: 2, LiftNeedsOther: true,
		Verifiers: func(id string) (agent.ApproverVerifier, bool) {
			p, ok := pubs[id]
			return audit.Ed25519Verifier{Pub: p}, ok
		}}
	r.trail = audit.NewStopTrail(r.events)
	j, err := agent.NewJournal(r.store)
	if err != nil {
		t.Fatal(err)
	}
	r.j = j
	r.ctl, err = agent.NewStopControl(j, agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(r.policy), agent.WithStopVerifier(audit.StopVerifierFromAudit), agent.WithStopTrail(r.trail))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *stopRig) signer(id string) audit.Ed25519Signer { return audit.Ed25519Signer{Priv: r.keys[id]} }

func (r *stopRig) set(tool, by string) agent.StopEntry {
	r.t.Helper()
	e, err := r.ctl.SetStop(context.Background(), agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopTool, Name: tool}, By: by, Reason: "incident", Signer: r.signer(by)})
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

func (r *stopRig) lift(e agent.StopEntry, by string) {
	r.t.Helper()
	if _, err := r.ctl.LiftStop(context.Background(), e.ID, agent.LiftRequest{By: by, Signer: r.signer(by)}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *stopRig) export() audit.StopTrailExport {
	r.t.Helper()
	x, err := audit.ExportStopTrail(context.Background(), r.events, agent.StopTrailName)
	if err != nil {
		r.t.Fatal(err)
	}
	return x
}

func (r *stopRig) sth(now time.Time) audit.SignedTreeHead {
	r.t.Helper()
	h, err := audit.SignStopTrail(context.Background(), r.events, agent.StopTrailName, audit.Ed25519Signer{Priv: r.head}, now)
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

func (r *stopRig) input(x audit.StopVerifyInput) audit.StopVerifyInput {
	if x.Trail.Leaves == nil {
		x.Trail = r.export()
	}
	if x.Verifier == nil {
		x.Verifier = audit.Ed25519Verifier{Pub: r.pub}
	}
	if x.Options == nil {
		x.Options = []agent.StopOption{agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(r.policy), agent.WithStopVerifier(audit.StopVerifierFromAudit)}
	}
	if x.Now.IsZero() {
		x.Now = time.Now()
	}
	return x
}

func problems(rep audit.StopReport) string { return strings.Join(rep.Problems, "; ") }

// A trail a control wrote verifies against its signed head and the policy, shows the stops as the
// control run does, and agrees with the control run record for record.
func TestVerifyStops_HappyPath(t *testing.T) {
	r := newStopRig(t)
	a := r.set("refund", "ops1")
	r.set("charge", "ops2")
	r.lift(a, "ops1")
	r.lift(a, "risk")
	ctlRecs, err := r.ctl.StopRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Heads: []audit.SignedTreeHead{r.sth(time.Now())}, Control: ctlRecs, HaveControl: true}))
	if err != nil {
		t.Fatalf("%v: %s", err, problems(rep))
	}
	if !rep.Anchored || rep.Tail != 0 || len(rep.Stops) != 2 || !rep.Stops[0].Lifted || rep.Stops[1].Lifted || rep.Mode != "audited" || rep.Records != 5 { // root, 2 sets, 2 lifts
		t.Fatalf("%+v", rep)
	}
	// The same trail with no head is refused unless allowed, and then it is read unattested.
	if rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{})); !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(problems(rep), "no signed head") {
		t.Fatalf("no head: %v %s", err, problems(rep))
	}
	if rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Unanchored: true})); err != nil || rep.Anchored {
		t.Fatalf("unanchored: %v %+v", err, rep)
	}
}

// A record deleted, changed or reordered after a signed head covered it is found; a head of another trail
// or key does not verify.
func TestVerifyStops_TamperingAfterAHead(t *testing.T) {
	r := newStopRig(t)
	a := r.set("refund", "ops1")
	r.set("charge", "ops2")
	r.lift(a, "ops1")
	head := r.sth(time.Now())
	good := r.export()
	for name, mod := range map[string]func(x *audit.StopTrailExport){
		"deleted":   func(x *audit.StopTrailExport) { x.Leaves = slices.Delete(slices.Clone(x.Leaves), 1, 2) },
		"truncated": func(x *audit.StopTrailExport) { x.Leaves = x.Leaves[:2] },
		"reordered": func(x *audit.StopTrailExport) {
			x.Leaves = slices.Clone(x.Leaves)
			x.Leaves[1], x.Leaves[2] = x.Leaves[2], x.Leaves[1]
		},
		"changed": func(x *audit.StopTrailExport) {
			x.Leaves = slices.Clone(x.Leaves)
			x.Leaves[1] = append(slices.Clone(x.Leaves[1]), ' ')
		},
	} {
		x := good
		mod(&x)
		if rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Trail: x, Heads: []audit.SignedTreeHead{head}})); !errors.Is(err, audit.ErrNotVerified) {
			t.Errorf("%s: %v %s", name, err, problems(rep))
		}
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Heads: []audit.SignedTreeHead{head}, Verifier: audit.Ed25519Verifier{Pub: other}})); !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(problems(rep), "does not verify") {
		t.Errorf("another key: %v", err)
	}
	// Records after the newest head are the unattested tail; too old a tail fails.
	r.set("late", "ops1")
	rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Heads: []audit.SignedTreeHead{head}, Now: time.Now().Add(time.Hour), MaxClockSkew: 2 * time.Hour, MaxTailAge: time.Minute}))
	if !errors.Is(err, audit.ErrNotVerified) || rep.Tail != 1 || !strings.Contains(problems(rep), "unattested") {
		t.Errorf("tail: %v %+v", err, rep)
	}
	// With no MaxTailAge any unattested tail fails; with one it is accepted while it is younger.
	rep, err = audit.VerifyStops(r.input(audit.StopVerifyInput{Heads: []audit.SignedTreeHead{head}}))
	if !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(problems(rep), "are not attested") {
		t.Errorf("default tail: %v %s", err, problems(rep))
	}
	if rep, err = audit.VerifyStops(r.input(audit.StopVerifyInput{Heads: []audit.SignedTreeHead{head}, MaxTailAge: time.Hour})); err != nil {
		t.Errorf("a young tail: %v %s", err, problems(rep))
	}
}

// A record that does not verify under the pinned policy, written into the trail, is a problem, and so is
// a trail that holds a pin the chain does not reach.
func TestVerifyStops_ForgedRecordsAndAWrongPin(t *testing.T) {
	r := newStopRig(t)
	r.set("refund", "ops1")
	// A forged set, appended to the trail directly (a store writer, not the control).
	forged := agent.StopRecord{Format: agent.StopFormat, Kind: "set", Mode: agent.StopAudited, Scope: &agent.StopScope{Kind: agent.StopGlobal}, Compensation: agent.CompensationAllow, Deployment: "prod-91c2", At: time.Now().UTC().Format(time.RFC3339Nano), By: "ops1", Alg: audit.AlgEd25519, Signature: "00"}
	forged.StopID = agent.StopIDOf(forged)
	have, _ := r.trail.Load(context.Background(), agent.StopTrailName)
	if err := r.trail.Append(context.Background(), agent.StopTrailName, len(have), forged); err != nil {
		t.Fatal(err)
	}
	rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Unanchored: true}))
	if !errors.Is(err, audit.ErrNotVerified) || rep.RejectedTotal != 1 || len(rep.Stops) != 1 || !strings.Contains(problems(rep), "do not verify") {
		t.Fatalf("a forged record: %v %+v", err, rep)
	}
	// Another deployment's policy as the pin: the root is not this chain.
	other := r.policy
	other.DeploymentID = "staging-7f3a"
	rep, err = audit.VerifyStops(r.input(audit.StopVerifyInput{Unanchored: true, Options: []agent.StopOption{agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(other)}}))
	if !errors.Is(err, audit.ErrNotVerified) || rep.RejectedTotal == 0 {
		t.Fatalf("the wrong deployment: %v %+v", err, rep)
	}
}

// Against the control run: a record in force without evidence, and evidence of a record that is not in
// force, are both found.
func TestVerifyStops_ComparesWithTheControlRun(t *testing.T) {
	r := newStopRig(t)
	a := r.set("refund", "ops1")
	ctlRecs, _ := r.ctl.StopRecords(context.Background())
	// A stop written around the trail: the control run holds it, the trail does not.
	bare, err := agent.NewStopControl(r.j, agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(r.policy), agent.WithStopVerifier(audit.StopVerifierFromAudit))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.SetStop(context.Background(), agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopTool, Name: "hidden"}, By: "ops2", Signer: r.signer("ops2")}); err != nil {
		t.Fatal(err)
	}
	ctlRecs2, _ := r.ctl.StopRecords(context.Background())
	rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Unanchored: true, Control: ctlRecs2, HaveControl: true}))
	if !errors.Is(err, audit.ErrNotVerified) || len(rep.StoreOnly) != 1 || len(rep.TrailOnly) != 0 {
		t.Fatalf("store-only: %v %+v", err, rep)
	}
	// Reaudit closes the gap.
	if n, err := r.ctl.Reaudit(context.Background()); err != nil || n != 1 {
		t.Fatalf("Reaudit: %d %v", n, err)
	}
	if _, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Unanchored: true, Control: ctlRecs2, HaveControl: true})); err != nil {
		t.Fatal(err)
	}
	// Evidence of a lift that is not in the control run (the row was deleted): compare with the earlier control.
	r.lift(a, "ops1")
	rep, err = audit.VerifyStops(r.input(audit.StopVerifyInput{Unanchored: true, Control: ctlRecs, HaveControl: true}))
	if !errors.Is(err, audit.ErrNotVerified) || len(rep.TrailOnly) == 0 {
		t.Fatalf("trail-only: %v %+v", err, rep)
	}
}

func TestStopTrail_RejectsAForeignLeaf(t *testing.T) {
	ev := audit.NewMemEventStore()
	if err := ev.Append(context.Background(), agent.StopTrailName, 0, []byte("not a leaf")); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.NewStopTrail(ev).Load(context.Background(), agent.StopTrailName); err == nil {
		t.Fatal("a foreign leaf was read as a stop record")
	}
	if rep, err := audit.VerifyStops(audit.StopVerifyInput{Trail: audit.StopTrailExport{Format: audit.StopTrailExportFormat, Leaves: [][]byte{[]byte("x")}}, Unanchored: true}); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("%v %+v", err, rep)
	}
}

// A head over another trail, and a trusted root the trail does not hold, are problems.
func TestVerifyStops_HeadOfAnotherTrailAndAMissingRoot(t *testing.T) {
	ctx := context.Background()
	r := newStopRig(t)
	r.set("refund", "ops1")
	log, err := audit.LoadEventLog(ctx, r.events, agent.StopTrailName)
	if err != nil {
		t.Fatal(err)
	}
	other, err := audit.SignTreeHead(log.TreeHead("notify:r1", time.Now().UnixNano()), audit.Ed25519Signer{Priv: r.head})
	if err != nil {
		t.Fatal(err)
	}
	if rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Heads: []audit.SignedTreeHead{other}})); !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(problems(rep), "not \"stop\"") {
		t.Fatalf("a head of another trail: %v %s", err, problems(rep))
	}
	root := strings.Repeat("ab", 32)
	rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Unanchored: true, Options: []agent.StopOption{agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(r.policy), agent.WithStopRoot(root), agent.WithStopVerifier(audit.StopVerifierFromAudit)}}))
	if !errors.Is(err, audit.ErrNotVerified) || rep.ChainErr == "" || rep.RejectedTotal != 0 || !strings.Contains(problems(rep), "policy chain cannot be trusted") {
		t.Fatalf("a trusted root the trail does not hold: %v %+v", err, rep)
	}
}

// A leaf that is not exactly the encoding the control writes is refused, and reported as a problem, never
// dropped: a repeated or case-variant member, an unknown kind, a short salt, a record in another spelling.
func TestVerifyStops_StrictLeaves(t *testing.T) {
	r := newStopRig(t)
	r.set("refund", "ops1")
	good := r.export()
	last := len(good.Leaves) - 1
	for name, edit := range map[string][2]string{
		"repeated member":   {`"event":{`, `"event":{"by":"x",`},
		"case-variant":      {`"event":{`, `"event":{"By":"x",`},
		"unknown kind":      {`"kind":"stop_set"`, `"kind":"stop_zzz"`},
		"short salt":        {`"salt":"`, `"salt":"AAAA`},
		"another spelling":  {`"event":{`, `"event":{ `},
		"another leaf kind": {`"kind":"stop_set"`, `"kind":"turn_started"`},
	} {
		x := good
		x.Leaves = slices.Clone(good.Leaves)
		leaf := string(x.Leaves[last])
		if !strings.Contains(leaf, edit[0]) {
			t.Fatalf("%s: the leaf has no %q: %s", name, edit[0], leaf)
		}
		x.Leaves[last] = []byte(strings.Replace(leaf, edit[0], edit[1], 1))
		rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Trail: x, Unanchored: true}))
		if !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(problems(rep), "record") {
			t.Errorf("%s: %v %s", name, err, problems(rep))
		}
	}
	// A record whose kind the leaf names truthfully but that is not well formed (a set with no scope).
	bad := agent.StopRecord{Format: agent.StopFormat, Kind: "set", Mode: agent.StopAudited, Compensation: agent.CompensationAllow, At: time.Now().UTC().Format(time.RFC3339Nano), By: "ops1", Alg: audit.AlgEd25519, Signature: "00"}
	ev := audit.NewMemEventStore()
	if err := audit.NewStopTrail(ev).Append(context.Background(), agent.StopTrailName, 0, bad); err != nil {
		t.Fatal(err)
	}
	x, _ := audit.ExportStopTrail(context.Background(), ev, agent.StopTrailName)
	if rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Trail: x, Unanchored: true})); !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(problems(rep), "not a valid stop record") {
		t.Errorf("a malformed record: %v %s", err, problems(rep))
	}
}

// A flood of records in the control run is a finding, not a hang.
func TestVerifyStops_ControlRunFloodIsAFinding(t *testing.T) {
	r := newStopRig(t)
	r.set("refund", "ops1")
	flood := make([]agent.StopRecord, audit.MaxStopRecords+1)
	for i := range flood {
		flood[i] = agent.StopRecord{Format: agent.StopFormat, Kind: "lift", Mode: agent.StopBasic, StopID: "x", SetDigest: "y", At: time.Now().UTC().Format(time.RFC3339Nano), By: "eve", Reason: strings.Repeat("a", i%50)}
	}
	start := time.Now()
	rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Unanchored: true, Control: flood, HaveControl: true}))
	if !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(problems(rep), "the control run holds") || time.Since(start) > 10*time.Second {
		t.Fatalf("%v %s (%v)", err, problems(rep), time.Since(start))
	}
}

// A lift written to the trail whose control-run write never finished is unapplied, not a forgery: the
// verb lists it and passes; ApplyPending makes it effective; a trail-only set, and a forged trail-only
// record, are still problems.
func TestVerifyStops_UnappliedRecordsAreNotForgeries(t *testing.T) {
	ctx := context.Background()
	r := newStopRig(t)
	e := r.set("refund", "ops1")
	r.lift(e, "ops1") // quorum 2: this one alone does not lift it
	// The second lift reaches the trail, and the control-run write fails.
	flaky := &failInsert{Store: r.store}
	j, _ := agent.NewJournal(flaky)
	c2, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(r.policy), agent.WithStopVerifier(audit.StopVerifierFromAudit), agent.WithStopTrail(r.trail))
	if err != nil {
		t.Fatal(err)
	}
	flaky.on.Store(true)
	if _, err := c2.LiftStop(ctx, e.ID, agent.LiftRequest{By: "risk", Signer: r.signer("risk")}); err == nil {
		t.Fatal("expected the control-run write to fail")
	}
	flaky.on.Store(false)
	ctlRecs, _ := c2.StopRecords(ctx)
	rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Unanchored: true, Control: ctlRecs, HaveControl: true}))
	if err != nil || len(rep.Unapplied) != 1 || rep.Unapplied[0].Kind != "lift" || len(rep.TrailOnly) != 0 {
		t.Fatalf("an unapplied lift: %v %+v %s", err, rep, problems(rep))
	}
	if st, _ := c2.State(ctx); len(st.Active()) != 1 {
		t.Fatalf("the unapplied lift must not be in force: %+v", st.Active())
	}
	pend, err := c2.PendingRecords(ctx)
	if err != nil || len(pend) != 1 || pend[0].Record.By != "risk" || pend[0].Superseded || pend[0].Age < 0 {
		t.Fatalf("PendingRecords: %+v %v", pend, err)
	}
	// An applier who is not enrolled is refused before anything is applied; an unsigned request too.
	eve := audit.Ed25519Signer{Priv: r.keys["ops1"]}
	if _, _, err := c2.ApplyPending(ctx, agent.ApplyRequest{By: "eve", Signer: eve}); !errors.Is(err, agent.ErrStopRejected) {
		t.Fatalf("an applier who is not enrolled: %v", err)
	}
	if _, _, err := c2.ApplyPending(ctx, agent.ApplyRequest{By: "ops1"}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("an unsigned apply: %v", err)
	}
	if st, _ := c2.State(ctx); len(st.Active()) != 1 {
		t.Fatalf("nothing may have been applied: %+v", st.Active())
	}
	n, sup, err := c2.ApplyPending(ctx, agent.ApplyRequest{By: "ops2", Reason: "the crash was ours", Signer: r.signer("ops2")})
	if err != nil || n != 1 || sup != 0 {
		t.Fatalf("ApplyPending: %d applied, %d superseded, %v", n, sup, err)
	}
	// Who applied what is a signed record on the trail and in the control run.
	have, _ := r.trail.Load(ctx, agent.StopTrailName)
	var applyRec *agent.StopRecord
	for i := range have {
		if have[i].Kind == "apply" && have[i].By == "ops2" {
			applyRec = &have[i]
		}
	}
	if applyRec == nil || len(applyRec.Applied) != 1 || applyRec.Applied[0] != agent.StopDigest(pend[0].Record) || applyRec.Reason != "the crash was ours" {
		t.Fatalf("the apply record: %+v", applyRec)
	}
	if st, _ := c2.State(ctx); len(st.Active()) != 0 {
		t.Fatalf("the lift must be in force after ApplyPending: %+v", st.Active())
	}
	ctlRecs, _ = c2.StopRecords(ctx)
	if rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Unanchored: true, Control: ctlRecs, HaveControl: true})); err != nil || len(rep.Unapplied) != 0 {
		t.Fatalf("after ApplyPending: %v %+v", err, rep)
	}
	if n, sup, err := c2.ApplyPending(ctx, agent.ApplyRequest{By: "ops2", Signer: r.signer("ops2")}); err != nil || n != 0 || sup != 0 {
		t.Fatalf("a second ApplyPending: %d %d %v", n, sup, err)
	}
	// A trail-only record the fold rejects (a forgery written into the trail) is a problem, not unapplied.
	forged := agent.StopRecord{Format: agent.StopFormat, Kind: "lift", Mode: agent.StopAudited, StopID: e.ID, SetDigest: "00", Deployment: "prod-91c2", At: time.Now().UTC().Format(time.RFC3339Nano), By: "ops1", Alg: audit.AlgEd25519, Signature: "00"}
	have, _ = r.trail.Load(ctx, agent.StopTrailName)
	if err := r.trail.Append(ctx, agent.StopTrailName, len(have), forged); err != nil {
		t.Fatal(err)
	}
	rep, err = audit.VerifyStops(r.input(audit.StopVerifyInput{Unanchored: true, Control: ctlRecs, HaveControl: true}))
	if !errors.Is(err, audit.ErrNotVerified) || len(rep.Unapplied) != 0 || len(rep.TrailOnly) != 1 {
		t.Fatalf("a forged trail-only record: %v %+v", err, rep)
	}
}

// failInsert fails every Insert of a control-run lift while on is set.
type failInsert struct {
	agent.Store
	on atomic.Bool
}

func (f *failInsert) Unwrap() agent.Store { return f.Store }
func (f *failInsert) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if f.on.Load() && runID == agent.StopControlRun && strings.HasPrefix(name, "@stop/lift/") {
		return agent.Entry{}, false, errors.New("injected store fault")
	}
	return f.Store.Insert(ctx, runID, name, data)
}

// A pending record that a stored record follows on the trail is superseded: reported as such, not a
// failure, left out of the stops shown, and not applied (applying it would reorder the control run).
func TestVerifyStops_SupersededRecords(t *testing.T) {
	ctx := context.Background()
	r := newStopRig(t)
	e := r.set("refund", "ops1")
	flaky := &failInsert{Store: r.store}
	j, _ := agent.NewJournal(flaky)
	c2, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(r.policy), agent.WithStopVerifier(audit.StopVerifierFromAudit), agent.WithStopTrail(r.trail))
	if err != nil {
		t.Fatal(err)
	}
	flaky.on.Store(true)
	if _, err := c2.LiftStop(ctx, e.ID, agent.LiftRequest{By: "ops1", Signer: r.signer("ops1")}); err == nil {
		t.Fatal("expected the control-run write to fail")
	}
	flaky.on.Store(false)
	// Another lifter signs: a record that reaches both. The first is now behind it on the trail.
	if _, err := c2.LiftStop(ctx, e.ID, agent.LiftRequest{By: "risk", Signer: r.signer("risk")}); err != nil {
		t.Fatal(err)
	}
	ctlRecs, _ := c2.StopRecords(ctx)
	rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Unanchored: true, Control: ctlRecs, HaveControl: true}))
	if err != nil || len(rep.Superseded) != 1 || len(rep.Unapplied) != 0 || len(rep.TrailOnly) != 0 {
		t.Fatalf("%v %+v %s", err, rep, problems(rep))
	}
	if len(rep.Stops) != 1 || rep.Stops[0].LiftsHeld != 1 || rep.Stops[0].Lifted {
		t.Fatalf("the stops shown are the control run's (one lift held: risk's), not the trail's (which would show both lifts and the stop lifted): %+v", rep.Stops)
	}
	if n, sup, err := c2.ApplyPending(ctx, agent.ApplyRequest{By: "ops2", Signer: r.signer("ops2")}); err != nil || n != 0 || sup != 1 {
		t.Fatalf("ApplyPending: %d applied, %d superseded, %v", n, sup, err)
	}
}

// SignStopTrailExport signs exactly the leaves it is given: the head verifies against them, equals the head a
// store read would give, and does not follow a trail that grew after the export.
func TestSignStopTrailExportSignsExactlyTheExportedLeaves(t *testing.T) {
	r := newStopRig(t)
	e := r.set("refund", "ops1")
	x := r.export()
	now := time.Now()
	h, err := audit.SignStopTrailExport(x, audit.Ed25519Signer{Priv: r.head}, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := r.sth(now); h.Size != want.Size || string(h.Root) != string(want.Root) {
		t.Fatalf("export head %d/%x differs from the store head %d/%x", h.Size, h.Root, want.Size, want.Root)
	}
	r.lift(e, "ops1") // the trail grows after the export
	r.lift(e, "risk")
	if h.Size != len(x.Leaves) {
		t.Fatalf("head size %d, exported %d", h.Size, len(x.Leaves))
	}
	rep, err := audit.VerifyStops(r.input(audit.StopVerifyInput{Heads: []audit.SignedTreeHead{h}, MaxTailAge: time.Hour}))
	if err != nil || rep.Tail != len(r.export().Leaves)-len(x.Leaves) || rep.Tail == 0 {
		t.Fatalf("the head must cover the exported prefix only: tail %d, %v", rep.Tail, err)
	}
}

func TestSignStopTrailExportRefusesNothingAndForeignTrails(t *testing.T) {
	r := newStopRig(t)
	s := audit.Ed25519Signer{Priv: r.head}
	if _, err := audit.SignStopTrailExport(r.export(), s, time.Now()); !errors.Is(err, audit.ErrMalformed) {
		t.Fatalf("an empty trail: %v", err)
	}
	r.set("refund", "ops1")
	x := r.export()
	x.Trail = "notify"
	if _, err := audit.SignStopTrailExport(x, s, time.Now()); !errors.Is(err, audit.ErrMalformed) {
		t.Fatalf("another trail: %v", err)
	}
}

package audit_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"strings"
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

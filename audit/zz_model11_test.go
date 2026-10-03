package audit

// Findings of model 11 (spec/tla/delegation): each test reproduces a counterexample and fails on
// the code as it stands.

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

func m11Root(t *testing.T, signer Signer, id string) SignedGrant {
	t.Helper()
	sg, err := SignGrant(Grant{ID: id, Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"},
		NotAfterUnix: time.Now().Unix() + 3600}, signer)
	if err != nil {
		t.Fatal(err)
	}
	return sg
}

// D1 (findings/d1-rotation): a saga whose delegations were minted under two bound grants (the
// first expired between them, and the refusal of the second mint asks for a live one; here the
// root grant is rotated between drives) can never finish its rollback: BindRollback verifies each
// journaled grant against the one grant bound now, and the walk stops at the first that does not
// match, whichever of the two is bound.
func TestModel11_D1_RollbackAcrossRotatedGrants(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	signer := rev117eSigner(t)
	p1, p2 := m11Root(t, signer, "p1"), m11Root(t, signer, "p2")
	var refunds atomic.Int32
	charge := func(name string) agent.Tool {
		return agent.CompensatedFunc(name, "a write", agent.Safety{},
			func(context.Context, struct{}) (string, error) { return "charged", nil },
			func(context.Context, struct{}, string) error { refunds.Add(1); return nil })
	}
	boom := agent.Func("boom", "fails", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("sold out")
	})
	cfg := AttenuationConfig{Store: store, Narrow: narrowLimitBy(1), Rules: rev117eRules}
	tools := func() []agent.Tool {
		sub1 := agenttest.MustNew(
			agent.NewScriptedModel(agent.ToolTurn("s1", "charge1", `{}`), agent.TextTurn("d1 done")),
			store,
			agent.WithTools(charge("charge1")),
		)
		sub2 := agenttest.MustNew(
			agent.NewScriptedModel(agent.ToolTurn("s2", "charge2", `{}`), agent.ToolTurn("s3", "boom", `{}`), agent.TextTurn("x")),
			store,
			agent.WithTools(charge("charge2"), boom),
		)
		return []agent.Tool{AttenuatingSubAgent("d1", "first", sub1, cfg), AttenuatingSubAgent("d2", "second", sub2, cfg)}
	}
	// Drive 1, under p1: d1 delegates and finishes; the next model turn fails (the run stops).
	parent1 := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("c1", "d1", `{"task":"a"}`), agent.ErrorTurn(errors.New("provider down"))),
		store,
		agent.WithTools(tools()...),
	)
	if _, err := parent1.RunSaga(WithGrant(ctx, p1, signer), "trip", "go"); err == nil {
		t.Fatal("drive 1: want the provider error")
	}
	// Drive 2, under p2 (the rotated grant): d2 mints from p2, its sub-run fails, the saga rolls back.
	script := func() agent.Model {
		return agent.NewScriptedModel(agent.ToolTurn("c1", "d1", `{"task":"a"}`), agent.ToolTurn("c2", "d2", `{"task":"b"}`), agent.TextTurn("x"))
	}
	drive := func(ctx context.Context) *agent.SagaAborted {
		_, err := agenttest.MustNew(script(), store, agent.WithTools(tools()...)).RunSaga(ctx, "trip", "go")
		var ab *agent.SagaAborted
		if !errors.As(err, &ab) {
			t.Fatalf("RunSaga = %v, want *SagaAborted", err)
		}
		return ab
	}
	// Under p2 alone, d1's grant links to no bound grant: refused, with a message that says what
	// to bind. (Before the fix, either grant alone left the rollback stuck for good.)
	ab := drive(WithGrant(ctx, p2, signer))
	if !errors.Is(ab.CompensateErr, ErrNotVerified) || !strings.Contains(ab.CompensateErr.Error(), "WithRollbackGrants") {
		t.Fatalf("under p2 alone: CompensateErr = %v, want ErrNotVerified naming WithRollbackGrants", ab.CompensateErr)
	}
	// With both bound (p2 acting, p1 for the rollback), each delegation verifies against its own
	// parent and the rollback finishes.
	ab = drive(WithRollbackGrants(WithGrant(ctx, p2, signer), signer, p1))
	if ab.CompensateErr != nil || refunds.Load() != 2 || len(ab.Uncompensated) != 0 {
		t.Fatalf("the rollback did not finish with both grants bound: compensated %v, uncompensated %v, refunds %d, error %v",
			ab.Compensated, ab.Uncompensated, refunds.Load(), ab.CompensateErr)
	}
}

// D2 (findings/d2-rerun-expired): BindRollback rebinds the journaled grant without the delegated
// mark, so CallGuard does not apply to the rollback's re-run of a retry-safe write in the
// sub-run: the tool is called after the delegation's grant expired. The sub-run's own rollback,
// under the delegated grant, is refused by CallGuard for the same call.
func TestModel11_D2_RollbackRerunAfterGrantExpiry(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	signer := rev117eSigner(t)
	notAfter := time.Now().Unix() + 1
	narrow := func(parent Grant, sub string) Grant {
		g := narrowLimitBy(1)(parent, sub)
		g.NotAfterUnix = notAfter
		return g
	}
	var calls, undone atomic.Int32
	idem := agent.CompensatedFunc("idem", "a retry-safe write", agent.Safety{Idempotent: true},
		func(context.Context, struct{}) (string, error) { calls.Add(1); return "set", nil },
		func(context.Context, struct{}, string) error { undone.Add(1); return nil })
	// boom fails once the grant has expired, so the sub-run's own saga rolls back after expiry.
	boom := agent.Func("boom", "fails", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		for time.Now().Unix() <= notAfter {
			time.Sleep(20 * time.Millisecond)
		}
		return "", errors.New("sold out")
	})
	sub := agenttest.MustNew(
		rev117eMultiModel{calls: [][2]string{{"s1", "boom"}, {"s2", "idem"}}},
		store,
		agent.WithTools(boom, idem),
		agent.WithMaxConcurrency(1),
	)
	deleg := AttenuatingSubAgent("deleg", "d", sub, AttenuationConfig{Store: store, Narrow: narrow, Rules: rev117eRules})
	parent := agenttest.MustNew(rev117eMultiModel{calls: [][2]string{{"c1", "deleg"}}}, store, agent.WithTools(deleg))
	_, err := parent.RunSaga(WithGrant(ctx, m11Root(t, signer, "p"), signer), "trip", "go")
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga = %v, want *SagaAborted", err)
	}
	t.Logf("compensated %v, unknown %v, err %v; idem called %d times, undone %d", ab.Compensated, ab.UnknownOutcome, ab.CompensateErr, calls.Load(), undone.Load())
	if calls.Load() != 0 {
		t.Fatalf("idem's tool was called %d time(s) by the rollback after the delegation's grant expired at %d", calls.Load(), notAfter)
	}
	if ab.CompensateErr != nil || !slices.Contains(ab.UnknownOutcome, "idem") {
		t.Fatalf("the refused re-run must be listed as an unknown outcome and the rollback go on: unknown %v, err %v", ab.UnknownOutcome, ab.CompensateErr)
	}
}

// D3 (findings/d3-plain-deleg): a saga's call starts a plain programmatic sub-run (Run, not
// RunSaga); in it, a sub-agent's run makes a compensable write and then fails, so the plain run
// records the sub-agent call as an error result and goes on. The saga's rollback walks the
// sub-run, but rollbackRun skips a call with an error result before its sub-agent recursion, so
// the write is neither compensated nor listed.
func TestModel11_D3_FailedSubAgentInPlainSubRunSkipped(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	var undone atomic.Int32
	charge := agent.CompensatedFunc("charge", "a write", agent.Safety{},
		func(context.Context, struct{}) (string, error) { return "charged", nil },
		func(context.Context, struct{}, string) error { undone.Add(1); return nil })
	build := func(m agent.Model, tools ...agent.Tool) *agent.Agent {
		a, err := agent.New(m, store, agent.WithTools(tools...))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	grand := build(agent.NewScriptedModel(agent.ToolTurn("g1", "charge", `{}`), agent.ErrorTurn(errors.New("provider rejected the request"))), charge)
	child := build(agent.NewScriptedModel(agent.ToolTurn("k1", "worker", `{"task":"w"}`), agent.TextTurn("child done")),
		agent.SubAgent("worker", "w", grand))
	starter := agent.Func("starter", "", agent.Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := agent.RunInfoFrom(ctx)
		msg, err := child.Run(ctx, info.SubRunFor("child"), "work")
		return msg.Text(), err
	}, agent.WithSubRuns(func(name string) *agent.Agent {
		if name == "child" {
			return child
		}
		return nil
	}))
	boom := agent.Func("boom", "", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("boom")
	})
	parent := build(agent.NewScriptedModel(agent.ToolTurn("c1", "starter", `{}`), agent.ToolTurn("c2", "boom", `{}`), agent.TextTurn("x")), starter, boom)
	_, err := parent.RunSaga(ctx, "p", "go")
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga = %v, want *SagaAborted", err)
	}
	t.Logf("compensated %v, uncompensated %v, unknown %v, err %v", ab.Compensated, ab.Uncompensated, ab.UnknownOutcome, ab.CompensateErr)
	listed := slices.Contains(ab.Uncompensated, "charge") || slices.Contains(ab.Uncompensated, "worker")
	if ab.CompensateErr == nil && undone.Load() == 0 && !listed {
		t.Fatalf("the sub-agent's write was neither compensated nor listed (compensated %v, uncompensated %v)", ab.Compensated, ab.Uncompensated)
	}
	// The fix compensates it (the walk reaches the sub-agent's run with its tools), once.
	if ab.CompensateErr != nil || undone.Load() != 1 || !slices.Contains(ab.Compensated, "charge") || listed {
		t.Fatalf("the sub-agent's write must be compensated once: undone %d, compensated %v, uncompensated %v, err %v",
			undone.Load(), ab.Compensated, ab.Uncompensated, ab.CompensateErr)
	}
}

// D1, key rotation: the root grant and its signing key were both rotated between drives. Each
// rollback grant carries its own signer, so the delegation minted under the old key verifies
// under the old key, and the one minted under the new key under the new one; a grant bound with
// the wrong signer verifies nothing.
func TestModel11_D1_RollbackAcrossRotatedKeys(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	oldSigner, newSigner := rev117eSigner(t), rev117eSigner(t)
	p1, p2 := m11Root(t, oldSigner, "p1"), m11Root(t, newSigner, "p2")
	var refunds atomic.Int32
	charge := func(name string) agent.Tool {
		return agent.CompensatedFunc(name, "a write", agent.Safety{},
			func(context.Context, struct{}) (string, error) { return "charged", nil },
			func(context.Context, struct{}, string) error { refunds.Add(1); return nil })
	}
	boom := agent.Func("boom", "fails", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("sold out")
	})
	cfg := AttenuationConfig{Store: store, Narrow: narrowLimitBy(1), Rules: rev117eRules}
	tools := func() []agent.Tool {
		sub1 := agenttest.MustNew(
			agent.NewScriptedModel(agent.ToolTurn("s1", "charge1", `{}`), agent.TextTurn("d1 done")),
			store,
			agent.WithTools(charge("charge1")),
		)
		sub2 := agenttest.MustNew(
			agent.NewScriptedModel(agent.ToolTurn("s2", "charge2", `{}`), agent.ToolTurn("s3", "boom", `{}`), agent.TextTurn("x")),
			store,
			agent.WithTools(charge("charge2"), boom),
		)
		return []agent.Tool{AttenuatingSubAgent("d1", "first", sub1, cfg), AttenuatingSubAgent("d2", "second", sub2, cfg)}
	}
	parent1 := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("c1", "d1", `{"task":"a"}`), agent.ErrorTurn(errors.New("provider down"))),
		store,
		agent.WithTools(tools()...),
	)
	if _, err := parent1.RunSaga(WithGrant(ctx, p1, oldSigner), "trip", "go"); err == nil {
		t.Fatal("drive 1: want the provider error")
	}
	drive := func(ctx context.Context) *agent.SagaAborted {
		m := agent.NewScriptedModel(agent.ToolTurn("c1", "d1", `{"task":"a"}`), agent.ToolTurn("c2", "d2", `{"task":"b"}`), agent.TextTurn("x"))
		_, err := agenttest.MustNew(m, store, agent.WithTools(tools()...)).RunSaga(ctx, "trip", "go")
		var ab *agent.SagaAborted
		if !errors.As(err, &ab) {
			t.Fatalf("RunSaga = %v, want *SagaAborted", err)
		}
		return ab
	}
	// p1 bound under the new key: d1's grant does not verify, and the rollback stops.
	ab := drive(WithRollbackGrants(WithGrant(ctx, p2, newSigner), newSigner, p1))
	if !errors.Is(ab.CompensateErr, ErrNotVerified) || !strings.Contains(ab.CompensateErr.Error(), "does not verify under the bound signer's key") {
		t.Fatalf("p1 under the wrong signer: CompensateErr = %v, want the signature refusal (ErrNotVerified)", ab.CompensateErr)
	}
	ab = drive(WithRollbackGrants(WithGrant(ctx, p2, newSigner), oldSigner, p1))
	if ab.CompensateErr != nil || refunds.Load() != 2 {
		t.Fatalf("rollback across the key rotation: compensated %v, uncompensated %v, refunds %d, err %v",
			ab.Compensated, ab.Uncompensated, refunds.Load(), ab.CompensateErr)
	}
}

// D1 and D2, the bound context: BindRollback accepts a parent bound only with WithRollbackGrants
// (the acting grant is another), rebinds the child marked delegated with its parent's signer, and
// binds no rollback grants in the sub-run, so a grandchild verifies against the child alone.
func TestModel11_BindRollbackScope(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	s1, s2 := rev117eSigner(t), rev117eSigner(t)
	old, live := m11Root(t, s1, "old"), m11Root(t, s2, "live")
	child, err := SignGrant(Grant{ID: "c", Issuer: "desk", Subject: "exec", Scope: map[string]string{"limit": "6"},
		NotAfterUnix: old.Grant.NotAfterUnix, ParentRef: old.Grant.Digest()}, s1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordGrant(ctx, store, "sub", child); err != nil {
		t.Fatal(err)
	}
	sub := agenttest.MustNew(agent.NewScriptedModel(agent.TextTurn("x")), store)
	b := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(1), Rules: rev117eRules}).(interface {
		BindRollback(context.Context, string) (context.Context, error)
	})
	bound, err := b.BindRollback(WithRollbackGrants(WithGrant(ctx, live, s2), s1, old), "sub")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := bound.Value(grantCtxKey{}).(grantCarrier)
	if c.sg.Grant.ID != "c" || !c.delegated || c.signer == nil || !bytes.Equal(c.signer.PublicKey(), s1.PublicKey()) {
		t.Fatalf("bound carrier: grant %q, delegated %v; want the child, delegated, under its parent's signer", c.sg.Grant.ID, c.delegated)
	}
	if ps := rollbackParents(bound); len(ps) != 1 || ps[0].sg.Grant.ID != "c" {
		t.Fatalf("the sub-run's rollback parents = %d, want the child alone", len(ps))
	}
	// Bound only with WithRollbackGrants and no acting grant: still accepted.
	if _, err := b.BindRollback(WithRollbackGrants(ctx, s1, old), "sub"); err != nil {
		t.Fatalf("parent bound only for the rollback: %v", err)
	}
	// Each WithRollbackGrants adds to what an outer one bound: old, bound first, still verifies.
	if _, err := b.BindRollback(WithRollbackGrants(WithRollbackGrants(ctx, s1, old), s2, live), "sub"); err != nil {
		t.Fatalf("parent bound by an outer WithRollbackGrants: %v", err)
	}
	// An acting grant bound with no signer is no parent: nothing to verify against.
	if _, err := b.BindRollback(WithGrant(ctx, old, nil), "sub"); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("acting grant with no signer: %v, want ErrConfig", err)
	}
	// A nil signer binds nothing: no grant to verify against.
	if _, err := b.BindRollback(WithRollbackGrants(ctx, nil, old), "sub"); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("nil signer: %v, want ErrConfig", err)
	}
}

// D2, the documented over-report: the re-run the guard refuses is listed as an unknown outcome
// even when the sub-run's journal holds no "may have begun" record (@saga/args/) for the step and
// its tool was never called. Journals of v0.9.0 and earlier hold no such record, so its absence
// cannot prove the step never began; over-reporting is the safe side (see rollbackRun).
func TestModel11_D2_UnknownEvenWithoutABeganRecord(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	signer := rev117eSigner(t)
	notAfter := time.Now().Unix() + 1
	narrow := func(parent Grant, sub string) Grant {
		g := narrowLimitBy(1)(parent, sub)
		g.NotAfterUnix = notAfter
		return g
	}
	var calls atomic.Int32
	idem := agent.CompensatedFunc("idem", "a retry-safe write", agent.Safety{Idempotent: true},
		func(context.Context, struct{}) (string, error) { calls.Add(1); return "set", nil },
		func(context.Context, struct{}, string) error { return nil })
	boom := agent.Func("boom", "fails", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		for time.Now().Unix() <= notAfter {
			time.Sleep(20 * time.Millisecond)
		}
		return "", errors.New("sold out")
	})
	sub := agenttest.MustNew(
		rev117eMultiModel{calls: [][2]string{{"s1", "boom"}, {"s2", "idem"}}},
		store,
		agent.WithTools(boom, idem),
		agent.WithMaxConcurrency(1),
	)
	deleg := AttenuatingSubAgent("deleg", "d", sub, AttenuationConfig{Store: store, Narrow: narrow, Rules: rev117eRules})
	parent := agenttest.MustNew(rev117eMultiModel{calls: [][2]string{{"c1", "deleg"}}}, store, agent.WithTools(deleg))
	_, err := parent.RunSaga(WithGrant(ctx, m11Root(t, signer, "p"), signer), "trip", "go")
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga = %v, want *SagaAborted", err)
	}
	recs, err := store.History(ctx, agent.SubRunID("trip", "c1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if strings.HasPrefix(r.Name, "@saga/args/") {
			t.Fatalf("the sub-run holds a may-have-begun record %q; the test needs a step with none", r.Name)
		}
	}
	if calls.Load() != 0 || !slices.Contains(ab.UnknownOutcome, "idem") || ab.CompensateErr != nil {
		t.Fatalf("want idem never called and listed as an unknown outcome: called %d, unknown %v, err %v", calls.Load(), ab.UnknownOutcome, ab.CompensateErr)
	}
}

// nilSigner is a pointer Signer whose methods panic on a nil receiver.
type nilSigner struct{ Ed25519Signer }

// A typed-nil signer (a nil pointer in the Signer interface) is refused: WithGrant binds the grant
// with no signer, so a delegation under it is refused with ErrConfig and records nothing, and
// WithRollbackGrants binds nothing. Neither panics.
func TestModel11_TypedNilSignerRefused(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	signer := rev117eSigner(t)
	root := m11Root(t, signer, "p")
	var typedNil *nilSigner
	if _, s, ok := GrantFrom(WithGrant(ctx, root, typedNil)); !ok || s != nil {
		t.Fatalf("GrantFrom after WithGrant with a typed-nil signer: signer %v, ok %v; want the grant with no signer", s, ok)
	}
	if ps := rollbackParents(WithRollbackGrants(ctx, typedNil, root)); len(ps) != 0 {
		t.Fatalf("WithRollbackGrants with a typed-nil signer bound %d grant(s)", len(ps))
	}
	sub := agenttest.MustNew(agent.NewScriptedModel(agent.TextTurn("done")), store)
	deleg := AttenuatingSubAgent("deleg", "d", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(1), Rules: rev117eRules})
	parent := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("c1", "deleg", `{"task":"a"}`), agent.TextTurn("x")),
		store,
		agent.WithTools(deleg),
	)
	if _, err := parent.Run(WithGrant(ctx, root, typedNil), "r", "go"); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Run under a grant with a typed-nil signer = %v, want ErrConfig", err)
	}
	recs, err := store.History(ctx, agent.SubRunID("r", "c1"))
	if err != nil || len(recs) != 0 {
		t.Fatalf("the refused delegation journaled %d record(s) (err %v), want none", len(recs), err)
	}
}

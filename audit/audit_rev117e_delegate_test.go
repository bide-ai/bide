package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// rev117eMultiModel emits several tool calls in its first turn, then a final answer.
type rev117eMultiModel struct{ calls [][2]string } // {id, name}

func (m rev117eMultiModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	n := 0
	for _, msg := range req.Messages {
		if msg.Role == agent.RoleAssistant {
			n++
		}
	}
	ch := make(chan agent.Emit, len(m.calls)+2)
	if n == 0 {
		for i, c := range m.calls {
			args := `{}`
			if c[1] == "deleg" || c[1] == "inner" || c[1] == "deep" {
				args = `{"task":"go"}`
			}
			ch <- agent.Emit{Event: agent.ToolCallDelta{Index: i, ID: c[0], Name: c[1], ArgsFragment: json.RawMessage(args)}}
		}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "done"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// rev117eCountModel answers in one turn and counts its calls.
type rev117eCountModel struct{ n *atomic.Int32 }

func (m rev117eCountModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	m.n.Add(1)
	return answerModel{text: "ok"}.Stream(ctx, req)
}

func rev117eSigner(t *testing.T) Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Ed25519Signer{Priv: priv}
}

func rev117eRoot(t *testing.T, signer Signer, notAfter int64) SignedGrant {
	t.Helper()
	sg, err := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}, NotAfterUnix: notAfter}, signer)
	if err != nil {
		t.Fatal(err)
	}
	return sg
}

var rev117eRules = ScopeRules{"limit": NumericAtMost}

// A sub-run turn that holds both a lost outcome (a call whose answer is unknown) and an unrecorded
// refusal returns them joined. The parent's group sees the Unrecorded first and holds it as a
// refusal, so in a saga it never sets halted: a sibling step not yet started then starts, which
// the halt rule (agent/loop.go: "the saga must not run a step a run that never crashed would not
// have reached") forbids. The control (no refusal in the sub-run) does not start the sibling.
func TestRev117e_UnrecordedHidesDeepHaltInSaga(t *testing.T) {
	run := func(t *testing.T, notAfter int64) (int32, error) {
		ctx := context.Background()
		store := agenttest.MemJournal()
		signer := rev117eSigner(t)
		ctx = WithGrant(ctx, rev117eRoot(t, signer, notAfter), signer)
		deleg := AttenuatingSubAgent("deleg", "d", agenttest.MustNew(answerModel{text: "ok"}, store),
			AttenuationConfig{Store: store, Narrow: narrowLimitBy(1), Rules: rev117eRules})
		lost := agent.Func("lost", "outcome lost", agent.Safety{}, func(context.Context, struct{}) (string, error) {
			return "", fmt.Errorf("connection dropped after send: %w", agent.ErrToolOutcomeUnknown)
		})
		inner := agenttest.MustNew(
			rev117eMultiModel{calls: [][2]string{{"i1", "deleg"}, {"i2", "lost"}}},
			store,
			agent.WithTools(deleg, lost),
			agent.WithMaxConcurrency(1),
		) // deleg refuses first, then lost halts
		var side atomic.Int32
		sideTool := agent.Func("side", "a later step", agent.Safety{}, func(context.Context, struct{}) (string, error) {
			side.Add(1)
			return "ok", nil
		})
		parent := agenttest.MustNew(
			rev117eMultiModel{calls: [][2]string{{"c1", "inner"}, {"c2", "side"}}},
			store,
			agent.WithTools(agent.SubAgent("inner", "i", inner), sideTool),
			agent.WithMaxConcurrency(1),
		)
		_, err := parent.RunSaga(ctx, "p", "go")
		return side.Load(), err
	}
	t.Run("control: live grant, halt only", func(t *testing.T) {
		side, err := run(t, 0)
		if side != 0 {
			t.Fatalf("control: sibling started %d times after a deep halt (err %v)", side, err)
		}
	})
	t.Run("expired bound grant: refusal joined with halt", func(t *testing.T) {
		side, err := run(t, time.Now().Unix()-3600)
		t.Logf("side=%d err=%v", side, err)
		if side != 0 {
			t.Fatalf("sibling step started %d time(s) after a deep halt that was joined with an unrecorded refusal (err %v)", side, err)
		}
	})
}

// rev117eFlakyStore fails a load of one run once with a storage error.
type rev117eFlakyStore struct {
	agent.Store
	run   string
	fails atomic.Int32
}

func (s *rev117eFlakyStore) Unwrap() agent.Store { return s.Store }

func (s *rev117eFlakyStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	if runID == s.run && s.fails.Add(-1) >= 0 {
		return func(yield func(agent.Entry, error) bool) {
			yield(agent.Entry{}, fmt.Errorf("transient read failure: %w", agent.ErrStorage))
		}
	}
	return s.Store.Load(ctx, runID, after)
}

// A transient storage failure reading the sub-run's journaled authority is recorded as the
// delegation's failure, for good: a resume never delegates. A plain SubAgent whose sub-run cannot
// read its journal records nothing (subRunUnfinished), so its resume re-enters the sub-run.
func TestRev117e_StorageReadFailureRecordedAsDelegationFailure(t *testing.T) {
	for _, withGrant := range []bool{false, true} {
		t.Run(fmt.Sprintf("grant=%v", withGrant), func(t *testing.T) {
			ctx := context.Background()
			store := agent.NewMemStore()
			j := agenttest.MustJournal(store)
			flaky := &rev117eFlakyStore{Store: store, run: agent.SubRunID("p", "c1")}
			flaky.fails.Store(1)
			if withGrant {
				signer := rev117eSigner(t)
				ctx = WithGrant(ctx, rev117eRoot(t, signer, 0), signer)
			}
			var subCalls atomic.Int32
			deleg := AttenuatingSubAgent("deleg", "d", agenttest.MustNew(rev117eCountModel{n: &subCalls}, j),
				AttenuationConfig{Store: agenttest.MustJournal(flaky), Narrow: narrowLimitBy(1), Rules: rev117eRules})
			parent := agenttest.MustNew(
				agent.NewScriptedModel(agent.ToolTurn("c1", "deleg", `{"task":"go"}`), agent.TextTurn("done")),
				j,
				agent.WithTools(deleg),
			)
			_, err1 := parent.Run(ctx, "p", "go")
			_, err2 := parent.Run(ctx, "p", "go") // resume once the store is healthy
			if subCalls.Load() == 0 {
				recs, _ := j.History(ctx, "p")
				var res string
				for _, r := range recs {
					if r.Kind == agent.StepToolResult && r.ToolUseID == "c1" {
						res = fmt.Sprintf("IsError=%v %s", r.IsError, r.Result)
					}
				}
				t.Fatalf("delegation never ran: first Run err=%v, resume err=%v; parent journaled c1 as: %s", err1, err2, res)
			}
		})
	}
}

// The doc says a delegation resumed under other authority than it began with, including "the
// reverse" (a grant after no grant), is refused. A sub-run with records but no journaled authority
// (BindRollback refuses it as ErrProtocol) is instead given a freshly minted grant when a grant is
// bound, so steps that ran with no grant and later steps share a sub-run the rollback then binds
// to the new grant.
func TestRev117e_GrantMintedOntoSubRunWithoutAuthority(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	subRun := agent.SubRunID("p", "c1")
	if _, err := journaltest.Do(ctx, store, subRun, "legacy-step", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"x"`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	signer := rev117eSigner(t)
	ctx = WithGrant(ctx, rev117eRoot(t, signer, 0), signer)
	tool := AttenuatingSubAgent("deleg", "d", agenttest.MustNew(answerModel{text: "ok"}, store),
		AttenuationConfig{Store: store, Narrow: narrowLimitBy(1), Rules: rev117eRules}).(*attenuatingSubAgent)
	if _, err := tool.BindRollback(ctx, subRun); !errors.Is(err, agent.ErrProtocol) {
		t.Fatalf("precondition: BindRollback = %v, want ErrProtocol", err)
	}
	parent := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("c1", "deleg", `{"task":"go"}`), agent.TextTurn("done")),
		store,
		agent.WithTools(tool),
	)
	_, err := parent.Run(ctx, "p", "go")
	if err == nil || !errors.Is(err, agent.ErrConfig) {
		g, _, _, _ := tool.journaledAuthority(ctx, subRun)
		_, berr := tool.BindRollback(ctx, subRun)
		t.Fatalf("Run = %v; want a refusal (ErrConfig). Sub-run now holds grant=%v and BindRollback = %v", err, g != nil, berr)
	}
}

// Call refuses a journaled grant naming another subject (ErrProtocol, recorded), but BindRollback
// accepts the same grant and rebinds Actor = this sub-agent with AuthorityRef = the foreign
// subject's grant, an identity whose actor is not the grant's subject.
func TestRev117e_BindRollbackAcceptsForeignSubject(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	signer := rev117eSigner(t)
	root := rev117eRoot(t, signer, 0)
	ctx = WithGrant(ctx, root, signer)
	subRun := agent.SubRunID("p", "c1")
	foreign, err := SignGrant(Grant{ID: "gx", Issuer: "desk", Subject: "other", ParentRef: root.Grant.Digest(), Scope: map[string]string{"limit": "5"}}, signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordGrant(ctx, store, subRun, foreign); err != nil {
		t.Fatal(err)
	}
	tool := AttenuatingSubAgent("deleg", "d", agenttest.MustNew(answerModel{text: "ok"}, store),
		AttenuationConfig{Store: store, Narrow: narrowLimitBy(1), Rules: rev117eRules}).(*attenuatingSubAgent)
	parent := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("c1", "deleg", `{"task":"go"}`), agent.TextTurn("done")),
		store,
		agent.WithTools(tool),
	)
	if _, err := parent.Run(ctx, "p", "go"); err != nil {
		t.Logf("Run: %v", err)
	}
	recs, _ := store.History(ctx, "p")
	for _, r := range recs {
		if r.Kind == agent.StepToolResult && r.ToolUseID == "c1" && !strings.Contains(string(r.Result), "subject") {
			t.Logf("unexpected c1 result: %s", r.Result)
		}
	}
	bctx, err := tool.BindRollback(ctx, subRun)
	if err == nil {
		id, _ := agent.IdentityFrom(bctx)
		sg, _, _ := GrantFrom(bctx)
		t.Fatalf("BindRollback accepted a grant for subject %q: identity Actor=%q AuthorityRef=%q (grant digest %q); Call refuses the same grant",
			sg.Grant.Subject, id.Actor, id.AuthorityRef, foreign.Grant.Digest())
	}
}

// Only value records are authority: a record of another kind that happens to carry a grant leaf's
// or the ungranted marker's name is not what RecordGrant or the marker wrote.
func TestRev117e_JournaledAuthorityValueRecordsOnly(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	signer := rev117eSigner(t)
	subRun := agent.SubRunID("p", "c1")
	sg := rev117eRoot(t, signer, 0)
	b, err := json.Marshal(sg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{grantLeafName(sg.Grant.Digest()), ungrantedLeafName} {
		if _, err := journaltest.Do(ctx, store, subRun, name, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepSignal, Result: b}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	tool := AttenuatingSubAgent("deleg", "d", agenttest.MustNew(answerModel{text: "ok"}, store),
		AttenuationConfig{Store: store, Narrow: narrowLimitBy(1), Rules: rev117eRules}).(*attenuatingSubAgent)
	g, ungranted, any, err := tool.journaledAuthority(ctx, subRun)
	if err != nil || g != nil || ungranted || !any {
		t.Fatalf("journaledAuthority = grant %v, ungranted %v, any %v, err %v; want no authority over two records", g != nil, ungranted, any, err)
	}
}

// rev117eFlakyDoStore fails one insert whose step name has prefix, once, with a storage error,
// before the insert reaches the store.
type rev117eFlakyDoStore struct {
	agent.Store
	prefix string
	fails  atomic.Int32
}

func (s *rev117eFlakyDoStore) Unwrap() agent.Store { return s.Store }

func (s *rev117eFlakyDoStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if strings.HasPrefix(name, s.prefix) && s.fails.Add(-1) >= 0 {
		return agent.Entry{}, false, fmt.Errorf("transient write failure: %w", agent.ErrStorage)
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// A failure writing the delegation's authority (the child grant, or the ungranted marker) records
// nothing either: the resume retries the delegation.
func TestRev117e_StorageWriteFailureRecordsNothing(t *testing.T) {
	for _, withGrant := range []bool{false, true} {
		t.Run(fmt.Sprintf("grant=%v", withGrant), func(t *testing.T) {
			ctx := context.Background()
			store := agent.NewMemStore()
			j := agenttest.MustJournal(store)
			flaky := &rev117eFlakyDoStore{Store: store, prefix: ungrantedLeafName}
			if withGrant {
				signer := rev117eSigner(t)
				ctx = WithGrant(ctx, rev117eRoot(t, signer, 0), signer)
				flaky.prefix = grantLeafPrefix
			}
			flaky.fails.Store(1)
			var subCalls atomic.Int32
			deleg := AttenuatingSubAgent("deleg", "d", agenttest.MustNew(rev117eCountModel{n: &subCalls}, j),
				AttenuationConfig{Store: agenttest.MustJournal(flaky), Narrow: narrowLimitBy(1), Rules: rev117eRules})
			parent := agenttest.MustNew(
				agent.NewScriptedModel(agent.ToolTurn("c1", "deleg", `{"task":"go"}`), agent.TextTurn("done")),
				j,
				agent.WithTools(deleg),
			)
			_, err1 := parent.Run(ctx, "p", "go")
			_, err2 := parent.Run(ctx, "p", "go")
			if subCalls.Load() == 0 || err2 != nil {
				t.Fatalf("delegation never ran: first Run err=%v, resume err=%v", err1, err2)
			}
		})
	}
}

// The same with a crash halt (*OutcomeUnknown: a side effect in a deeper sub-run whose first drive
// was cut off after it began) joined with the unrecorded refusal: the sibling saga step still does
// not start.
func TestRev117e_UnrecordedJoinedWithCrashHaltInSaga(t *testing.T) {
	store := agenttest.MemJournal()
	signer := rev117eSigner(t)
	expired := rev117eRoot(t, signer, time.Now().Unix()-3600)
	deleg := AttenuatingSubAgent("deleg", "d", agenttest.MustNew(answerModel{text: "ok"}, store),
		AttenuationConfig{Store: store, Narrow: narrowLimitBy(1), Rules: rev117eRules})
	var cancelFirst context.CancelFunc
	var fired atomic.Int32
	fire := agent.Func("fire", "a side effect", agent.Safety{}, func(ctx context.Context, _ struct{}) (string, error) {
		if fired.Add(1) == 1 {
			cancelFirst() // the drive is cut off while the effect is in flight
			return "", ctx.Err()
		}
		return "ok", nil
	})
	// The side effect runs one level deeper, so its halt reaches the inner turn as a sub-agent's
	// halt (*OutcomeUnknown), joined there with the delegation's refusal.
	deep := agenttest.MustNew(rev117eMultiModel{calls: [][2]string{{"d1", "fire"}}}, store, agent.WithTools(fire))
	inner := agenttest.MustNew(
		rev117eMultiModel{calls: [][2]string{{"i1", "deleg"}, {"i2", "deep"}}},
		store,
		agent.WithTools(deleg, agent.SubAgent("deep", "d", deep)),
		agent.WithMaxConcurrency(1),
	)
	var side atomic.Int32
	sideTool := agent.Func("side", "a later step", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		side.Add(1)
		return "ok", nil
	})
	parent := agenttest.MustNew(
		rev117eMultiModel{calls: [][2]string{{"c1", "inner"}, {"c2", "side"}}},
		store,
		agent.WithTools(agent.SubAgent("inner", "i", inner), sideTool),
		agent.WithMaxConcurrency(1),
	)
	ctx1, cancel := context.WithCancel(WithGrant(context.Background(), expired, signer))
	cancelFirst = cancel
	_, err1 := parent.RunSaga(ctx1, "p", "go")
	cancel()
	_, err2 := parent.RunSaga(WithGrant(context.Background(), expired, signer), "p", "go")
	var halt *agent.OutcomeUnknown
	if !errors.As(err2, &halt) {
		t.Fatalf("setup: second drive = %v (first %v), want a crash halt joined with the refusal", err2, err1)
	}
	if side.Load() != 0 {
		t.Fatalf("sibling step started %d time(s) after a deep crash halt joined with an unrecorded refusal (err %v)", side.Load(), err2)
	}
}

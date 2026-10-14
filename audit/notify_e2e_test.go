package audit_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

// The notifier's own trail, written by the real engine under every routing shape, must verify: no
// problem, unsigned and signed, with and without the routing the verifier checks deliveries against.

type e2eScenario struct {
	name      string
	approvers []string // the gate's approvers; nil is a 1-of-1 gate
	halt      bool     // the run is halted on an unknown outcome instead of waiting at a gate
	opts      func(sink func() agent.Notifier) []agent.NotifyOption
	decide    bool // the gate is decided (or the halt resolved) after the notification: an all-clear is owed
	escalate  bool // the event is left to run past its deadline so the chain is delivered
}

func e2eRun(t *testing.T, sc e2eScenario) {
	t.Helper()
	ctx := context.Background()
	mem := agent.NewMemStore()
	j, _ := agent.NewJournal(mem)
	var tool agent.Tool
	toolName := "refund"
	var verifiers agent.ApproverVerifierFor
	switch {
	case sc.halt:
		toolName = "charge"
		tool = agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
	case sc.approvers == nil:
		tool = agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	default:
		tool = agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil },
			agent.WithApproval(&agent.ApprovalPolicy{Need: 1, Approvers: sc.approvers}))
		verifiers = func(id string) (agent.ApproverVerifier, bool) {
			return quorumVerifier(id), slices.Contains(sc.approvers, id)
		}
	}
	newAgent := func(s agent.Store) *agent.Agent {
		jj, _ := agent.NewJournal(s)
		opts := []agent.Option{agent.WithTools(tool)}
		if verifiers != nil {
			opts = append(opts, agent.WithApproverVerifiers(verifiers))
		}
		a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", toolName, `{}`), agenttest.TextTurn("done")), jj, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	if sc.halt {
		_, _ = newAgent(crashBeforeResultStore{mem}).Run(ctx, "r1", agent.UserText("go")) // dies before the result is recorded
		if _, err := newAgent(mem).Run(ctx, "r1", agent.UserText("go")); !errors.As(err, new(*agent.OutcomeUnknown)) {
			t.Fatalf("second drive = %v", err)
		}
	} else if _, err := newAgent(mem).Run(ctx, "r1", agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatalf("Run = %v", err)
	}

	es := audit.NewMemEventStore()
	trail := audit.NewNotifyTrail(es)
	clk := &stepClock{now: time.Now()}
	sinks := map[string]*toggleSink{}
	mk := func(name string) agent.Notifier { s := &toggleSink{}; sinks[name] = s; return s }
	opts := append(sweepOpts(clk), sc.opts(func() agent.Notifier { return mk("default") })...)
	sweep := func() {
		var err error
		for range 5 { // a sweep that loses an append race to its own worker reports it and is safe to repeat
			if err = agent.NotifyPending(ctx, j, mk("sweep"), trail, opts...); err == nil {
				return
			}
		}
		t.Fatalf("sweep: %v", err)
	}
	clk.now = clk.now.Add(2 * time.Minute) // a halt is reported once it is a minute old
	sweep()
	if sc.escalate {
		for range 3 {
			clk.now = clk.now.Add(2 * time.Minute)
			sweep()
		}
	}
	if sc.decide {
		if sc.halt {
			if err := agent.ResolveHalt(ctx, j, agent.HaltRef{RunID: "r1", Op: agent.OpRef{Kind: agent.OpTool, ID: "c1"}, Cause: agent.HaltCrashed}, agent.Outcome{Result: "ok"}, agent.WithoutLiveDriverCheck()); err != nil {
				t.Fatal(err)
			}
		} else if _, _, err := mem.Insert(ctx, "r1", agent.ToolResultStep("c1"), []byte(`{"name":"`+agent.ToolResultStep("c1")+`","kind":"tool_result","tool_use_id":"c1"}`)); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			clk.now = clk.now.Add(time.Minute)
			sweep()
		}
	}

	jx, err := audit.ExportJournal(ctx, j, "r1")
	if err != nil {
		t.Fatal(err)
	}
	var trails []audit.NotifyTrailExport
	var names []string
	for _, y := range []int{time.Now().UTC().Year(), time.Now().UTC().Year() + 1} {
		names = append(names, agent.OpenIndex(y))
	}
	names = append(names, trailName, agent.OpenSegment(clk.now), agent.OpenSegment(time.Now()))
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		x, err := audit.ExportNotifyTrail(ctx, es, n)
		if err != nil {
			t.Fatal(err)
		}
		if len(x.Leaves) > 0 {
			trails = append(trails, x)
		}
	}
	routing, err := agent.RoutingOf(opts...)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	var heads []audit.SignedTreeHead
	for _, x := range trails {
		h, err := audit.SignNotifyTrail(ctx, es, x.Trail, audit.Ed25519Signer{Priv: priv}, clk.now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		heads = append(heads, h)
	}
	for _, mode := range []string{"unsigned", "signed"} {
		for _, withRouting := range []bool{false, true} {
			in := audit.NotifyVerifyInput{Trails: trails, Journals: []audit.JournalExport{jx}, Now: clk.now.Add(2 * time.Minute)}
			if mode == "unsigned" {
				in.Unanchored = true
			} else {
				in.Heads, in.Verifier = heads, audit.Ed25519Verifier{Pub: pub}
			}
			if withRouting {
				in.Routing = &routing
			}
			if sc.escalate { // an unresolved event is judged overdue past its deadline however it was delivered
				in.MaxAck = 0
			}
			rep, err := audit.VerifyNotifications(in)
			problems := rep.Problems
			if sc.escalate { // the original of an unacknowledged-by-design chain may be reported late; nothing else
				problems = nil
				for _, p := range rep.Problems {
					if !(len(p) > 0 && (contains(p, "acked_late") || contains(p, "overdue"))) {
						problems = append(problems, p)
					}
				}
				if len(problems) == 0 {
					err = nil
				}
			}
			if err != nil || len(problems) > 0 {
				t.Errorf("%s routing=%v: err %v, problems %v\n%+v", mode, withRouting, err, problems, rep.Events)
			}
		}
	}
	if len(trails) == 0 || len(heads) != len(trails) {
		t.Fatalf("no trails written: %d trails %d heads", len(trails), len(heads))
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestVerifyNotifications_TheRealEnginesTrailVerifiesUnderEveryRoutingShape(t *testing.T) {
	recv := func(sink func() agent.Notifier, names ...string) []agent.NotifyOption {
		var o []agent.NotifyOption
		for _, n := range names {
			o = append(o, agent.WithNotifyReceiver(n, sink()))
		}
		return o
	}
	for _, sc := range []e2eScenario{
		{name: "no routes, 1-of-1 gate", opts: func(func() agent.Notifier) []agent.NotifyOption { return nil }},
		{name: "no routes, a halt", halt: true, opts: func(func() agent.Notifier) []agent.NotifyOption { return nil }},
		{name: "one routed approver", approvers: []string{"alice"}, opts: func(s func() agent.Notifier) []agent.NotifyOption {
			return append(recv(s, "risk"), agent.WithNotifyApproverRoute("alice", "risk"))
		}},
		{name: "all approvers routed", approvers: []string{"alice", "bob"}, opts: func(s func() agent.Notifier) []agent.NotifyOption {
			return append(recv(s, "risk", "fin"), agent.WithNotifyApproverRoute("alice", "risk"), agent.WithNotifyApproverRoute("bob", "fin"))
		}},
		{name: "partly routed", approvers: []string{"alice", "bob"}, opts: func(s func() agent.Notifier) []agent.NotifyOption {
			return append(recv(s, "risk"), agent.WithNotifyApproverRoute("alice", "risk"))
		}},
		{name: "two routed receivers and an unrouted approver", approvers: []string{"alice", "bob", "carol"}, opts: func(s func() agent.Notifier) []agent.NotifyOption {
			return append(recv(s, "risk", "fin"), agent.WithNotifyApproverRoute("alice", "risk"), agent.WithNotifyApproverRoute("bob", "fin"))
		}},
		{name: "a kind route for approvals", approvers: []string{"alice"}, opts: func(s func() agent.Notifier) []agent.NotifyOption {
			return append(recv(s, "approvers"), agent.WithNotifyRoute(agent.NotificationApprovalPending, "approvers"))
		}},
		{name: "a kind route for halts", halt: true, opts: func(s func() agent.Notifier) []agent.NotifyOption {
			return append(recv(s, "oncall"), agent.WithNotifyRoute(agent.NotificationOutcomeUnknown, "oncall"))
		}},
		{name: "an all-clear", approvers: []string{"alice"}, decide: true, opts: func(func() agent.Notifier) []agent.NotifyOption { return nil }},
		{name: "an all-clear to routed receivers", approvers: []string{"alice", "bob"}, decide: true, opts: func(s func() agent.Notifier) []agent.NotifyOption {
			return append(recv(s, "risk"), agent.WithNotifyApproverRoute("alice", "risk"))
		}},
		{name: "a halt resolved", halt: true, decide: true, opts: func(s func() agent.Notifier) []agent.NotifyOption {
			return append(recv(s, "oncall"), agent.WithNotifyRoute(agent.NotificationOutcomeUnknown, "oncall"))
		}},
		{name: "an escalation chain", approvers: []string{"alice"}, escalate: true, opts: func(s func() agent.Notifier) []agent.NotifyOption {
			return append(recv(s, "lead", "head"), agent.WithNotifyEscalation(agent.NotificationApprovalPending,
				agent.EscalationStep{Receiver: "lead", Deadline: time.Minute}, agent.EscalationStep{Receiver: "head", Deadline: time.Minute}))
		}},
		{name: "an escalation chain with routed approvers, then an all-clear", approvers: []string{"alice", "bob"}, escalate: true, decide: true, opts: func(s func() agent.Notifier) []agent.NotifyOption {
			return append(recv(s, "risk", "lead"), agent.WithNotifyApproverRoute("alice", "risk"),
				agent.WithNotifyEscalation(agent.NotificationApprovalPending, agent.EscalationStep{Receiver: "lead", Deadline: time.Minute}))
		}},
	} {
		t.Run(sc.name, func(t *testing.T) { e2eRun(t, sc) })
	}
}

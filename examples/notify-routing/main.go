// Command notify-routing shows the notifier routing events to different receivers, escalating one
// nobody handles, sending an all-clear, and writing a trail that is signed and anchored as it is
// written, which bide audit notifications (audit.VerifyNotifications) then checks offline.
//
// The application has two runs in one journal: a refund waiting at a 2-of-2 approval gate
// (alice's approvals go to the risk channel, bob's to the finance channel), and a charge that
// halted with an unknown outcome (it goes to on-call). The notifier is built with
//
//   - agent.WithNotifyReceiver: a named Notifier per destination (here in-process stand-ins; in
//     production each is a notify.NewWebhook to its own URL, secret and relay);
//   - agent.WithNotifyRoute and agent.WithNotifyApproverRoute: where an event goes, by kind and,
//     for an approval, by approver;
//   - agent.WithNotifyDeadline and agent.WithNotifyEscalation: who is told next, when the first
//     receiver has not acknowledged in time;
//   - audit.NewAuditedEventStore beneath audit.NewNotifyTrail: the trail is signed and anchored
//     as it is written, with the same audit.Signer and audit.Anchor as a journal.
//
// It runs the story twice. In the first, every receiver answers: all events are delivered and
// acknowledged, the halt is resolved and its receivers get an all-clear, and the audit passes.
// Then a record is removed from a copy of the trail, and the audit fails. In the second, on-call's
// endpoint is down: after the step's deadline the lead is told, and the audit reports that
// on-call's acknowledgement came too late (a person was reached, but not the one the route names,
// in time).
//
// It runs with no API key and no network:
//
//	go run ./examples/notify-routing
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(w io.Writer) error {
	fmt.Fprintln(w, "== every receiver answers")
	healthy, err := story(w, false)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "audit: %s\n", verdict(healthy.report, healthy.err))

	rep, err := audit.VerifyNotifications(withoutFirstRecord(healthy.in, agent.NotifyTrailName(approvalRun)))
	fmt.Fprintf(w, "audit of a copy with one record of the approval's trail removed: %s\n", verdict(rep, err))

	fmt.Fprintln(w, "== on-call's endpoint is down")
	down, err := story(w, true)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "audit: %s\n", verdict(down.report, down.err))
	return nil
}

const (
	approvalRun = "refund-2001"
	haltRun     = "charge-2002"
)

// outcome is what a story leaves: the verification input it built, and its result.
type outcome struct {
	in     audit.NotifyVerifyInput
	report audit.NotifyReport
	err    error
}

// receiver is an in-process stand-in for a webhook: it records what it was sent, and while down
// refuses it the way a webhook does that gets a 4xx (a permanent failure, retried in a later round).
type receiver struct {
	name string
	mu   sync.Mutex
	down bool
	got  []string
}

func (r *receiver) Notify(_ context.Context, n agent.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.down {
		return agent.ErrNotifyPermanent
	}
	r.got = append(r.got, string(n.Kind))
	return nil
}

func (r *receiver) set(down bool) { r.mu.Lock(); r.down = down; r.mu.Unlock() }

// crashBeforeResult refuses to store a tool's result: a process that died after the effect and
// before recording what it did, which is what makes the next drive halt.
type crashBeforeResult struct{ agent.Store }

func (s crashBeforeResult) Insert(ctx context.Context, run, name string, data []byte) (agent.Entry, bool, error) {
	if strings.HasPrefix(name, "tool:") {
		return agent.Entry{}, false, errors.New("crash")
	}
	return s.Store.Insert(ctx, run, name, data)
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func story(w io.Writer, oncallDown bool) (outcome, error) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	journal, err := agent.NewJournal(mem)
	if err != nil {
		return outcome{}, err
	}

	// Two runs that need a person: a refund at a 2-of-2 gate, and a charge halted on an unknown outcome.
	approvers := []string{"alice", "bob"}
	refund := agent.MustFunc("refund", "refund an order", func(context.Context, struct{}) (string, error) { return "refunded", nil },
		agent.WithApproval(&agent.ApprovalPolicy{Need: 2, Approvers: approvers}))
	charge := agent.MustFunc("charge", "charge a card", func(context.Context, struct{}) (string, error) { return "charged", nil })
	// The gate is never decided in this example, so no approver key is needed; the agent only
	// needs to know who the approvers may be.
	keys := func(id string) (agent.ApproverVerifier, bool) { return nil, slices.Contains(approvers, id) }
	drive := func(s agent.Store, run, tool string) error {
		j, err := agent.NewJournal(s)
		if err != nil {
			return err
		}
		a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", tool, `{}`), agenttest.TextTurn("done")), j,
			agent.WithTools(refund, charge), agent.WithApproverVerifiers(keys))
		if err != nil {
			return err
		}
		_, err = a.Run(ctx, run, agent.UserText("go"))
		return err
	}
	if err := drive(mem, approvalRun, "refund"); !agent.IsPause(err) {
		return outcome{}, fmt.Errorf("the refund did not pause at its gate: %v", err)
	}
	_ = drive(crashBeforeResult{mem}, haltRun, "charge") // dies before the result is recorded
	var halt *agent.OutcomeUnknown
	if err := drive(mem, haltRun, "charge"); !errors.As(err, &halt) {
		return outcome{}, fmt.Errorf("the charge did not halt: %v", err)
	}

	// The notifier: receivers, routes, an escalation chain, and a trail that signs itself.
	clk := &clock{now: time.Now()}
	logPub, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	anchor := audit.NewMemAnchorLog()
	audited, err := audit.NewAuditedEventStore(audit.NewMemEventStore(), audit.Ed25519Signer{Priv: logPriv}, anchor)
	if err != nil {
		return outcome{}, err
	}
	audited.WithClock(func() int64 { return clk.Now().UnixNano() })
	audited.OnError(func(trail string, err error) { fmt.Fprintf(w, "anchoring %s: %v\n", trail, err) })
	trail := audit.NewNotifyTrail(audited)

	names := []string{"ops", "risk", "finance", "oncall", "lead"}
	recv := map[string]*receiver{}
	for _, n := range names {
		recv[n] = &receiver{name: n}
	}
	recv["oncall"].set(oncallDown)
	opts := []agent.NotifyOption{
		agent.WithNotifyClock(clk.Now), agent.WithNotifyHolder("sweeper"),
		agent.WithNotifyRetry(time.Millisecond, time.Millisecond), agent.WithNotifyHaltAge(time.Minute),
		agent.WithNotifyReceiver("risk", recv["risk"]),
		agent.WithNotifyReceiver("finance", recv["finance"]),
		agent.WithNotifyReceiver("oncall", recv["oncall"]),
		agent.WithNotifyReceiver("lead", recv["lead"]),
		// An approval goes to each approver's channel; a halt goes to on-call.
		agent.WithNotifyApproverRoute("alice", "risk"),
		agent.WithNotifyApproverRoute("bob", "finance"),
		agent.WithNotifyRoute(agent.NotificationOutcomeUnknown, "oncall"),
		// Five minutes to acknowledge; then the lead is told, and has ten minutes of their own.
		agent.WithNotifyDeadline(5 * time.Minute),
		agent.WithNotifyEscalation(agent.NotificationOutcomeUnknown, agent.EscalationStep{Receiver: "lead", Deadline: 10 * time.Minute}),
	}
	sweep := func(after time.Duration) error {
		clk.now = clk.now.Add(after)
		return agent.NotifyPending(ctx, journal, recv["ops"], trail, opts...)
	}
	// A sweep is what a long-lived Agent, RecoverLoop or a timer does. Each failed delivery is
	// reported by the sweep and retried by the next one.
	for _, after := range []time.Duration{2 * time.Minute, 4 * time.Minute, time.Minute, time.Minute} {
		_ = sweep(after)
	}
	recv["oncall"].set(false) // the endpoint is back (when it was down)
	for range 3 {
		_ = sweep(time.Minute)
	}

	// The halt is resolved by an operator who checked with the card processor: the receivers that
	// were told of it get an all-clear.
	if err := agent.ResolveHalt(ctx, journal, halt.Ref(), agent.Outcome{Result: "charged (confirmed with the processor)"}, agent.WithoutLiveDriverCheck()); err != nil {
		return outcome{}, err
	}
	for range 2 {
		_ = sweep(time.Minute)
	}
	if err := audited.Flush(ctx); err != nil {
		return outcome{}, err
	}

	for _, n := range []string{"risk", "finance", "oncall", "lead"} {
		fmt.Fprintf(w, "%-8s received: %s\n", n, strings.Join(recv[n].got, ", "))
	}

	// The audit, offline: the trails and the signed heads the anchor holds, the run journals, and the
	// notifier's routing (the options it was built with).
	routing, err := agent.RoutingOf(opts...)
	if err != nil {
		return outcome{}, err
	}
	in := audit.NotifyVerifyInput{Verifier: audit.Ed25519Verifier{Pub: logPub}, Routing: &routing, Now: clk.now}
	seen := map[string]bool{}
	for _, e := range anchor.Entries() {
		in.Heads = append(in.Heads, e.STH)
		if !seen[e.RunID] { // a head's run ID is the trail's name
			seen[e.RunID] = true
			x, err := audit.ExportNotifyTrail(ctx, audited, e.RunID)
			if err != nil {
				return outcome{}, err
			}
			in.Trails = append(in.Trails, x)
		}
	}
	for _, run := range []string{approvalRun, haltRun} {
		jx, err := audit.ExportJournal(ctx, journal, run)
		if err != nil {
			return outcome{}, err
		}
		in.Journals = append(in.Journals, jx)
	}
	rep, err := audit.VerifyNotifications(in)
	return outcome{in: in, report: rep, err: err}, nil
}

// withoutFirstRecord is in with the first record of the named trail removed from the copy the
// verifier reads: the signed head no longer commits to the trail it finds.
func withoutFirstRecord(in audit.NotifyVerifyInput, trail string) audit.NotifyVerifyInput {
	in.Trails = slices.Clone(in.Trails)
	for i, t := range in.Trails {
		if t.Trail == trail {
			t.Leaves = slices.Clone(t.Leaves)[1:]
			in.Trails[i] = t
		}
	}
	return in
}

// verdict is the audit's result in a line: PASS, or the number of problems and the first.
func verdict(rep audit.NotifyReport, err error) string {
	switch {
	case err == nil && len(rep.Problems) == 0:
		return fmt.Sprintf("PASS, %d events notified and acknowledged", len(rep.Events))
	case len(rep.Problems) == 0:
		return "FAIL: " + err.Error()
	default:
		return fmt.Sprintf("FAIL, %d problem(s), the first: %s", len(rep.Problems), rep.Problems[0])
	}
}

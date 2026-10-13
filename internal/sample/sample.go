// Package sample writes a journal of runs in every state the bide view run list tells apart, driven
// by the real runtime on scripted models and fake tools (no API key, no network, no hand-written
// records), with the evidence, keys and flow configs that go with it. It is the journal behind
// bide sample, behind the run viewer guide's screenshots, and behind the runview and cmd/bide
// tests, which read it back through each store:
//
//	bide sample -out sample
//	bide view -db sample/sample.db
//
// The runs are agent refund runs (completed, halted with an unknown outcome, halted and then
// resolved, waiting at a 2-of-3 approval gate, approved with signatures, in progress, cancelled),
// a saga that aborted, a run made of durable steps only, and plan flow runs (completed on the
// graph path, waiting at a gated node, approved, halted). Where a crash is part of the story, one
// write is lost.
//
// The output is deterministic except for salts: the same runs, IDs, states, kinds, start times and
// record sequence on every call, and the same keys and decisions' signatures. The runs take their
// times from a fixed clock (agent.WithClock), and the keys come from fixed, public seeds (sample
// keys: never use them for anything real). Two things the runtime stamps from the wall clock are
// fixed at the store the runs write to: an attempt marker's attempted_at, and a plan flow's
// started_at (plan has no clock option). Every record carries a random salt (and an attempt
// marker a random claim ID), which the sample leaves random, as the runtime draws them; so the digests over salted records (the Merkle roots of
// the signed tree heads, and the evidence packages that hold them) differ between calls. Compare
// two calls with Unsalted. Only the live run (Live) uses real time.
package sample

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/plan"
)

// Flows are the sample's flow configs (flows/*.yaml): refund-v1 (the gate needs 2 of 3, and four
// assertions hold), refund-v2 (the gate needs 1 of 3, a weakened approval policy), and
// refund-bypass (a vip arm skips the gate: a violated assertion).
//
//go:embed flows/*.yaml
var Flows embed.FS

// Run is one run Write journals, and the state group the run list puts it in.
type Run struct {
	ID    string
	Note  string // what the run is, for the README
	Kind  string // "agent", "flow", or "steps" for a run with no run:start
	Group string // the run list's state filter group
}

// Result is what Write made besides the journal.
type Result struct {
	Runs []Run
	// LogPub is the public key the evidence is signed with, as bide audit -pubkey takes it.
	LogPub string
	// ApproverKeys is each approver's public key text, the file bide view -approver-keys reads.
	ApproverKeys map[string]string
	// Evidence is the evidence package of each run it was exported for, by run ID: the completed
	// refund run, and the approved one with its gate's signatures.
	Evidence map[string]audit.EvidencePackage
}

// Base is the start of the first run; each run starts seven minutes after the one before.
var Base = time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)

// evidenceAt is the time the evidence is sealed at, Unix nanoseconds.
var evidenceAt = Base.Add(24 * time.Hour).UnixNano()

// The policy of the refund's gate: 2 of these 3 approvers.
var policy = agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}

// seed names the fixed keys of the sample. It is public: the keys sign nothing but the sample.
const seed = "bide sample v1"

// keyFor is the Ed25519 key of the named signer, from the sample's seed.
func keyFor(name string) ed25519.PrivateKey {
	h := sha256.Sum256([]byte(seed + "/key/" + name))
	return ed25519.NewKeyFromSeed(h[:])
}

// Write journals the sample runs into s, each run ID prefixed with prefix, and returns them.
func Write(ctx context.Context, s agent.Store, prefix string) (*Result, error) {
	st := &sampleStore{Store: s}
	j, err := agent.NewJournal(st)
	if err != nil {
		return nil, err
	}
	logKey := keyFor("log")
	res := &Result{
		LogPub:       audit.FormatPublicKey(audit.AlgEd25519, logKey.Public().(ed25519.PublicKey)),
		ApproverKeys: map[string]string{},
		Evidence:     map[string]audit.EvidencePackage{},
	}
	approvers := map[string]ed25519.PrivateKey{}
	for _, id := range policy.Approvers {
		k := keyFor(id)
		approvers[id] = k
		res.ApproverKeys[id] = audit.FormatPublicKey(audit.AlgEd25519, k.Public().(ed25519.PublicKey))
	}
	e := &env{prefix: prefix, st: st, j: j, approvers: approvers, verify: verifiers(res.ApproverKeys)}
	for i, d := range demos {
		id := prefix + d.id
		e.at = Base.Add(time.Duration(i) * 7 * time.Minute)
		st.at = e.at
		if err := d.write(ctx, e, id); err != nil {
			return res, fmt.Errorf("sample: %s: %w", id, err)
		}
		res.Runs = append(res.Runs, Run{ID: id, Note: d.note, Kind: d.kind, Group: d.group})
	}
	signer := audit.Ed25519Signer{Priv: logKey}
	for _, runID := range []string{prefix + "refund-1041", prefix + "refund-1049"} {
		pkg, err := evidence(ctx, j, runID, signer)
		if err != nil {
			return res, fmt.Errorf("sample: evidence of %s: %w", runID, err)
		}
		res.Evidence[runID] = pkg
	}
	return res, nil
}

// evidence exports runID's evidence package: the refund call, and for a gated call the gate's
// decisions and tally.
func evidence(ctx context.Context, j *agent.Journal, runID string, signer audit.Signer) (audit.EvidencePackage, error) {
	pkg, err := audit.Evidence(ctx, j, runID, signer, evidenceAt, audit.WithToolCall(refundCall))
	if err != nil {
		return pkg, err
	}
	if !strings.HasSuffix(runID, "refund-1049") {
		return pkg, nil
	}
	decisions, err := audit.ApprovalEvidence(ctx, j, runID, refundCall, pkg.STH)
	if err != nil {
		return pkg, err
	}
	// The call itself is already packaged. Reseal: the package changed after Evidence sealed it.
	pkg.Actions = append(pkg.Actions, decisions[:len(decisions)-1]...)
	return pkg, pkg.Seal(signer)
}

// refundCall is the tool-use id the scripted model gives its refund call.
const refundCall = "c2"

// env is what one demo run is written with.
type env struct {
	prefix    string
	st        *sampleStore
	j         *agent.Journal
	at        time.Time // the run's start, and the time of everything it does
	approvers map[string]ed25519.PrivateKey
	verify    agent.ApproverVerifierFor
}

func (e *env) clock() agent.ClockOption { return agent.WithClock(func() time.Time { return e.at }) }

// decide records approver id's signed approval of the gate p pauses at.
func (e *env) decide(ctx context.Context, p *agent.ApprovalPending, id string) error {
	subject := agent.ApprovalSubject{RunID: p.RunID, ToolUseID: p.ToolUseID, ToolName: p.ToolName, Args: p.Args}
	sig := ed25519.Sign(e.approvers[id], agent.ApprovalDecisionBytes(subject, id, true))
	return agent.SubmitDecision(ctx, e.j, agent.Decision{RunID: p.RunID, ToolUseID: p.ToolUseID, ApproverID: id, Approved: true, Alg: audit.AlgEd25519, Signature: sig})
}

var demos = []struct {
	id, note, kind, group string
	write                 func(ctx context.Context, e *env, id string) error
}{
	{"refund-0999", "completed; journaled as a version that recorded no start time", "agent", "completed", func(ctx context.Context, e *env, id string) error {
		// Journaled as a version that recorded no start time writes run:start.
		e.st.noStartTime = true
		defer func() { e.st.noStartTime = false }()
		_, err := e.refundAgent(nil).Run(ctx, id, agent.UserText("Refund order 999."), e.clock())
		return err
	}},
	{"refund-1041", "completed; its evidence package proves the refund (refund-1041.evidence.json)", "agent", "completed", func(ctx context.Context, e *env, id string) error {
		// Its evidence package proves the refund call (see Result.Evidence).
		_, err := e.refundAgent(nil).Run(ctx, id, agent.UserText("Refund order 1041."), e.clock())
		return err
	}},
	{"refund-1042", "halted, outcome unknown: the process died mid-refund, and bide will not refund twice", "agent", "halted", func(ctx context.Context, e *env, id string) error {
		// The refund fires and the process dies before its result is journaled: the resume halts.
		_, err := e.crashRefund(ctx, id, "Refund order 1042.")
		return err
	}},
	{"refund-1048", "halted, then resolved with ResolveHalt and evidence, and completed", "agent", "completed", func(ctx context.Context, e *env, id string) error {
		// The same halt, resolved: the operator looked the refund up at the provider and recorded it,
		// with evidence, and the run completed.
		halt, err := e.crashRefund(ctx, id, "Refund order 1048.")
		if err != nil {
			return err
		}
		out := agent.Outcome{
			Result:   "refunded $120",
			Evidence: map[string]string{"provider_refund": "re_1043", "status": "succeeded", "source": "provider lookup by order"},
		}
		if err := agent.ResolveHalt(ctx, e.j, halt.Ref(), out, e.clock()); err != nil {
			return err
		}
		_, err = e.refundAgent(nil).Run(ctx, id, agent.UserText("Refund order 1048."), e.clock())
		return err
	}},
	{"refund-1043", "waiting at the refund's 2-of-3 approval gate, with one signed approval", "agent", "waiting", func(ctx context.Context, e *env, id string) error {
		// Waiting at the 2-of-3 gate with one signed approval.
		p, err := e.pause(ctx, id, "Refund order 1043.")
		if err != nil {
			return err
		}
		return e.decide(ctx, p, "ops")
	}},
	{"refund-1049", "approved by 2 of 3 and completed; its evidence carries the signatures (refund-1049.evidence.json)", "agent", "completed", func(ctx context.Context, e *env, id string) error {
		// Approved by two of the three approvers, who signed; the refund ran. Its evidence package
		// carries the gate's decisions (see Result.Evidence).
		p, err := e.pause(ctx, id, "Refund order 1049.")
		if err != nil {
			return err
		}
		for _, who := range []string{"ops", "finance"} {
			if err := e.decide(ctx, p, who); err != nil {
				return err
			}
		}
		_, err = e.refundAgent(&policy).Run(ctx, id, agent.UserText("Refund order 1049."), e.clock())
		return err
	}},
	{"refund-1044", "in progress: the process died after the lookup, before its result was journaled", "agent", "in-progress", func(ctx context.Context, e *env, id string) error {
		// The process dies after the read-only lookup ran, before its result is journaled: no
		// side effect started, so a resume would simply run the lookup again.
		e.st.lose = "tool:c1"
		_, err := e.refundAgent(nil).Run(ctx, id, agent.UserText("Refund order 1044."), e.clock())
		e.st.lose = ""
		if !errors.Is(err, errCrash) {
			return fmt.Errorf("crashing run = %v, want the simulated crash", err)
		}
		return nil
	}},
	{"refund-1045", "cancelled while it waited for approval", "agent", "failed", func(ctx context.Context, e *env, id string) error {
		// An operator cancels the refund while it waits for approval.
		if _, err := e.pause(ctx, id, "Refund order 1045."); err != nil {
			return err
		}
		return agent.Cancel(ctx, e.j, id, "customer withdrew the request")
	}},
	{"ship-2001", "aborted: a saga whose carrier booking failed, with the label voided", "agent", "failed", func(ctx context.Context, e *env, id string) error {
		// A saga: the label is bought, the carrier booking fails, and the label is voided.
		label := agent.MustCompensatedFunc("buy_label", "buy a shipping label",
			func(context.Context, struct{}) (string, error) { return "label L-77", nil },
			func(context.Context, struct{}, string) error { return nil })
		book := agent.MustFunc("book_carrier", "book the carrier", func(context.Context, struct{}) (string, error) {
			return "", errors.New("carrier unavailable")
		})
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "buy_label", `{}`), agenttest.ToolTurn("c2", "book_carrier", `{}`), agenttest.TextTurn("done"))
		a, err := agent.New(m, e.j, agent.WithTools(label, book))
		if err != nil {
			return err
		}
		_, err = a.Run(ctx, id, agent.UserText("Ship order 2001."), agent.WithSaga(), e.clock())
		return want[*agent.SagaAborted](err)
	}},
	{"reconcile-4001", "durable steps only (Journal.Step), no run:start: steps, done", "steps", "completed", func(ctx context.Context, e *env, id string) error {
		// Durable steps only: Journal.Step calls, no agent loop and no run:start. The run list calls it
		// "steps" and reads its state from the journal.
		ledger, err := e.j.Step[string](ctx, id, "fetch-ledger", func(context.Context) (string, error) { return "412 entries", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
		if err != nil {
			return err
		}
		if _, err := e.j.Step[string](ctx, id, "post-adjustments", func(context.Context) (string, error) { return "posted 3 adjustments to " + ledger, nil }); err != nil {
			return err
		}
		_, err = e.j.Step[string](ctx, id, "close-period", func(context.Context) (string, error) { return "period closed", nil })
		return err
	}},
	{"triage-3001", "plan flow, completed on the else arm: validate, lookup, credit", "flow", "completed", func(ctx context.Context, e *env, id string) error {
		// A small refund takes the else arm: validate, lookup, credit.
		_, err := e.refundFlow().Run(ctx, e.j, id, order{ID: "o-3001", Amount: 40})
		return err
	}},
	{"triage-3002", "plan flow, completed through review's gate: validate, lookup, review, charge, receipt", "flow", "completed", func(ctx context.Context, e *env, id string) error {
		// A large refund through the gate: review, charge, receipt.
		o := order{ID: "o-3002", Amount: 250}
		p, err := e.pauseFlow(ctx, id, o)
		if err != nil {
			return err
		}
		for _, who := range []string{"ops", "risk"} {
			if err := e.decide(ctx, p, who); err != nil {
				return err
			}
		}
		_, err = e.refundFlow().Run(ctx, e.j, id, o)
		return err
	}},
	{"triage-3003", "plan flow, halted in charge after the gate: outcome unknown", "flow", "halted", func(ctx context.Context, e *env, id string) error {
		// Through the gate, and the process dies in the charge node, after its side effect and
		// before its result is journaled: the resume halts instead of charging again.
		o := order{ID: "o-3003", Amount: 310}
		p, err := e.pauseFlow(ctx, id, o)
		if err != nil {
			return err
		}
		for _, who := range []string{"ops", "finance"} {
			if err := e.decide(ctx, p, who); err != nil {
				return err
			}
		}
		e.st.lose = "node:charge"
		_, err = e.refundFlow().Run(ctx, e.j, id, o)
		e.st.lose = ""
		if !errors.Is(err, errCrash) {
			return fmt.Errorf("crashing run = %v, want the simulated crash", err)
		}
		_, err = e.refundFlow().Run(ctx, e.j, id, o)
		return want[*agent.OutcomeUnknown](err)
	}},
	{"triage-3004", "plan flow, waiting at review's 2-of-3 gate, with one signed approval", "flow", "waiting", func(ctx context.Context, e *env, id string) error {
		// A large refund stops at review's 2-of-3 gate; one approver has signed.
		p, err := e.pauseFlow(ctx, id, order{ID: "o-3004", Amount: 180})
		if err != nil {
			return err
		}
		return e.decide(ctx, p, "finance")
	}},
}

// want is nil when err is a T, and an error saying so otherwise.
func want[T error](err error) error {
	if _, ok := errors.AsType[T](err); ok {
		return nil
	}
	var zero T
	return fmt.Errorf("run = %v, want %T", err, zero)
}

// crashRefund runs an ungated refund that the process dies in, after the refund fired and before
// its result was journaled, and resumes it: the resume halts, and crashRefund returns the halt.
func (e *env) crashRefund(ctx context.Context, id, text string) (*agent.OutcomeUnknown, error) {
	e.st.lose = "tool:" + refundCall
	_, err := e.refundAgent(nil).Run(ctx, id, agent.UserText(text), e.clock())
	e.st.lose = ""
	if !errors.Is(err, errCrash) {
		return nil, fmt.Errorf("crashing run = %v, want the simulated crash", err)
	}
	_, err = e.refundAgent(nil).Run(ctx, id, agent.UserText(text), e.clock())
	halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
	if !ok {
		return nil, fmt.Errorf("resumed run = %v, want *agent.OutcomeUnknown", err)
	}
	return halt, nil
}

// pause runs a refund behind the 2-of-3 gate to the gate.
func (e *env) pause(ctx context.Context, id, text string) (*agent.ApprovalPending, error) {
	_, err := e.refundAgent(&policy).Run(ctx, id, agent.UserText(text), e.clock())
	p, ok := errors.AsType[*agent.ApprovalPending](err)
	if !ok {
		return nil, fmt.Errorf("run = %v, want *agent.ApprovalPending", err)
	}
	return p, nil
}

// pauseFlow runs the refund flow on o to review's gate.
func (e *env) pauseFlow(ctx context.Context, id string, o order) (*agent.ApprovalPending, error) {
	_, err := e.refundFlow().Run(ctx, e.j, id, o)
	p, ok := errors.AsType[*agent.ApprovalPending](err)
	if !ok {
		return nil, fmt.Errorf("flow run = %v, want *agent.ApprovalPending", err)
	}
	return p, nil
}

// refundAgent looks an order up (read-only) and refunds it (a side effect, behind gate when set).
func (e *env) refundAgent(gate *agent.ApprovalPolicy) *agent.Agent {
	return refundAgent(e.j, gate, e.verify)
}

// refundAgent is the sample's refund agent over j: a scripted model asks for the lookup, then the
// refund, then answers.
func refundAgent(j *agent.Journal, gate *agent.ApprovalPolicy, verify agent.ApproverVerifierFor) *agent.Agent {
	return agenttest.MustNew(refundModel(), j, agent.WithTools(refundTools(gate)...), agent.WithApproverVerifiers(verify))
}

func refundModel() *agenttest.ScriptedModel {
	return agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "lookup", `{}`), agenttest.ToolTurn(refundCall, "refund", `{}`), agenttest.TextTurn("Refunded."))
}

func refundTools(gate *agent.ApprovalPolicy) []agent.Tool {
	lookup := agent.MustFunc("lookup", "look up an order", func(context.Context, struct{}) (string, error) {
		return "order: $120, paid by card", nil
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	var opts []agent.ToolOption
	if gate != nil {
		opts = append(opts, agent.WithApproval(gate))
	}
	refund := agent.MustFunc("refund", "refund an order", func(context.Context, struct{}) (string, error) {
		return "refunded $120", nil
	}, opts...)
	return []agent.Tool{lookup, refund}
}

// verifiers resolves approver ids to the public keys in keys (approver id -> key text).
func verifiers(keys map[string]string) agent.ApproverVerifierFor {
	return func(id string) (agent.ApproverVerifier, bool) {
		k, ok := keys[id]
		if !ok {
			return nil, false
		}
		v, err := audit.ParsePublicKey(k)
		if err != nil {
			return nil, false
		}
		return v, true
	}
}

type order struct {
	ID     string `json:"id"`
	Amount int    `json:"amount"`
}

// refundFlow is flows/refund-v1.yaml in Go: validate -> lookup -> Switch(amount > 100: review
// (2 of 3) -> charge -> receipt, else: credit).
func (e *env) refundFlow() *plan.Flow[order, string] {
	b := plan.New[order, string]("refund").WithApproverVerifiers(e.verify)
	validate := b.Step("validate", func(_ context.Context, o order) (order, error) { return o, nil }, plan.ReadOnly())
	lookup := b.Step("lookup", func(_ context.Context, o order) (order, error) { return o, nil })
	review := b.Step("review", func(_ context.Context, o order) (order, error) { return o, nil }, plan.ReadOnly(), plan.Approval(&policy))
	charge := b.Step("charge", func(_ context.Context, o order) (order, error) { return o, nil })
	credit := b.Step("credit", func(_ context.Context, o order) (string, error) { return "credited " + o.ID, nil }, plan.Idempotent())
	receipt := b.Step("receipt", func(_ context.Context, o order) (string, error) { return "refunded " + o.ID, nil }, plan.Idempotent())
	b.Edge(validate, lookup)
	b.Switch(lookup, plan.When(func(o order) bool { return o.Amount > 100 }, review).Named("large"), plan.Else(credit))
	b.Edge(review, charge)
	b.Edge(charge, receipt)
	return agenttest.Must(b.Build())
}

var errCrash = errors.New("simulated crash")

// sampleStore is the store the sample's runs write to. It fixes what the runtime stamps from the
// wall clock: an attempt marker's attempted_at, and run:start's started_at (a plan flow takes the
// wall clock, as plan has no clock option; an agent run's is already its clock's). It also fails
// the insert of the record named lose, when set: the write a crash loses. With noStartTime it
// writes run:start without its start time, as an earlier version wrote it.
type sampleStore struct {
	agent.Store
	at          time.Time
	lose        string
	noStartTime bool
}

var (
	startTime   = regexp.MustCompile(`,"started_at":"[^"]*"`)
	startedAt   = regexp.MustCompile(`"started_at":"[^"]*"`)
	attemptedAt = regexp.MustCompile(`"attempted_at":[0-9]+`)
)

func (s *sampleStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if s.lose != "" && name == s.lose {
		return agent.Entry{}, false, errCrash
	}
	if name == "run:start" {
		if s.noStartTime {
			data = startTime.ReplaceAll(data, nil)
		} else {
			data = startedAt.ReplaceAll(data, []byte(`"started_at":"`+s.at.UTC().Format("2006-01-02T15:04:05.000Z")+`"`))
		}
	}
	data = attemptedAt.ReplaceAll(data, []byte(`"attempted_at":`+strconv.FormatInt(s.at.UnixMilli(), 10)))
	return s.Store.Insert(ctx, runID, name, data)
}

func (s *sampleStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return s.Store.Load(ctx, runID, after)
}

func (s *sampleStore) Unwrap() agent.Store { return s.Store }

var random = regexp.MustCompile(`"(salt|claim)":"[^"]*"`)

// Unsalted is an entry's bytes without what the runtime draws at random: the record's salt and an
// attempt marker's claim ID. It is what two calls of Write agree on.
func Unsalted(data []byte) []byte { return random.ReplaceAll(data, []byte(`"$1":""`)) }

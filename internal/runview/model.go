// Package runview is a read-only, local web viewer for a journal's runs: a run list (RunList) with
// each run's kind and state, and one run's page: a plan flow's declared topology with the run's
// path drawn over it, or an agent run's timeline of model turns, tool calls, attempts, approvals
// and halts, with the audit verdict when a signed head or evidence package is given. The page of a
// run in progress, read from a store, follows the run as records land (see Follower). It never
// writes to the store it reads, and it serves on a loopback address only (see Listen). The bide
// view command (cmd/bide, its own module) is its front end.
package runview

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// View is everything the run page shows, built from the run's records alone (see Build).
type View struct {
	RunID  string `json:"run_id"`
	Kind   string `json:"kind"` // agent.RunKind, or "" when the run has no run:start
	Input  string `json:"input,omitempty"`
	Flow   string `json:"flow,omitempty"`   // the flow name run:start records
	Digest string `json:"digest,omitempty"` // the topology digest flow:digest records
	// State is the agent.RunState, except "halted" for a started run with a live attempt marker
	// and no result (see Halts): a resume returns *agent.OutcomeUnknown, so it is not in progress.
	State     string `json:"state"`
	StateText string `json:"state_text"` // State as the page shows it ("halted: outcome unknown")
	Terminal  string `json:"terminal,omitempty"`
	// EndedAt is when the run's first end marker was written, by the clock of the process that wrote
	// it (agent.RunStatus.EndedAt); empty when the run has not ended or the marker records no time.
	EndedAt string `json:"ended_at,omitempty"`
	Records int    `json:"records"`

	Events    []Event      `json:"events"`
	Halts     []Halt       `json:"halts,omitempty"`
	Gates     []Gate       `json:"gates,omitempty"`
	Graph     *Graph       `json:"graph,omitempty"`
	Conform   *Conformance `json:"conformance,omitempty"`
	Proof     *Proof       `json:"proof,omitempty"`
	Notes     []string     `json:"notes,omitempty"` // what the page cannot show for this run, and why
	LastNode  string       `json:"last_node,omitempty"`
	FinalText string       `json:"final_text,omitempty"`

	// Live: the page follows the run (live mode): it was read from a store, not an export, and has
	// no end marker. Cursor is the store sequence (agent.Entry.Seq) of its last record.
	Live   bool  `json:"live,omitempty"`
	Cursor int64 `json:"cursor,omitempty"`

	// Verdict is the answer the page opens with (see runVerdict); it is not in the JSON view.
	Verdict Verdict `json:"-"`
}

// Event is one journal record, as a timeline row.
type Event struct {
	Seq    int      `json:"seq"`   // 1-based position in the journal
	Name   string   `json:"name"`  // the record's journal name
	Kind   string   `json:"kind"`  // agent.StepKind
	Class  string   `json:"class"` // the row's style: model, tool, attempt, approval, halt, end, value
	Title  string   `json:"title"`
	Detail string   `json:"detail,omitempty"`
	Badges []string `json:"badges,omitempty"`
	// RecordedAt is the record's own recorded_at, verbatim, on the records that carry one: the
	// claim of the process that wrote it, informational (see agent.Record.RecordedAt).
	RecordedAt string `json:"recorded_at,omitempty"`

	// What the page adds to the row; none of it is in the JSON view.
	At      time.Time `json:"-"` // when the journal says the record happened (a start, an attempt marker, or a record's recorded_at), else zero
	Payload string    `json:"-"` // the record as indented JSON, cut to a readable size (see prettyJSON)
	Rel     string    `json:"-"` // At as an offset from the run's start ("+3.2s")
	Abs     string    `json:"-"` // At in UTC, for the hover
	Bar     *TimeBar  `json:"-"` // the span from At to the next timestamped record
}

// TimeBar is a timeline row's span on the run's clock, in percent of the whole: from the row's own
// time to the next timestamped record's. The journal timestamps the run's start, its attempt
// markers, and the records that note when they were written (a single approver's decision, a
// resolved halt, an end marker), so for an attempt this is how long it had been open when the next
// timestamped record was written, an upper bound on the operation, not its duration.
type TimeBar struct {
	X, W  float64
	Label string
}

// KindText is the run's kind as the page shows it.
func (v *View) KindText() string { return kindText(v.Kind) }

// IconName is the row's glyph.
func (e Event) IconName() string {
	switch {
	case e.Kind == string(agent.StepSignal):
		return "signal"
	case e.Class == "attempt":
		return "clock"
	}
	if _, ok := icons[e.Class]; ok {
		return e.Class
	}
	return "value"
}

// Halt is an operation whose attempt marker is live with no result: a resume of the run returns
// *agent.OutcomeUnknown for it until agent.ResolveHalt records its outcome.
type Halt struct {
	Op          string    `json:"op"` // "tool" or "step"
	ID          string    `json:"id"` // the tool-use ID, or the step name ("node:<name>" for a plan node)
	ToolName    string    `json:"tool_name,omitempty"`
	Marker      string    `json:"marker"`
	AttemptedAt time.Time `json:"attempted_at,omitzero"`
	Ref         string    `json:"ref"` // the agent.HaltRef to resolve it with, as Go
}

// Gate is a tool call's or a plan flow node's approval gate, from the decisions and tally the
// journal holds. A flow node's ToolUseID is its key ("node:<name>", "node:iter:<n>:<name>").
type Gate struct {
	ToolUseID  string     `json:"tool_use_id"`
	ToolName   string     `json:"tool_name,omitempty"`
	Single     bool       `json:"single,omitempty"` // a one-decision gate (agent.Approve)
	Need       int        `json:"need,omitempty"`
	Approvers  []string   `json:"approvers,omitempty"`
	ApprovedBy []string   `json:"approved_by,omitempty"`
	DeniedBy   []string   `json:"denied_by,omitempty"`
	Pending    []string   `json:"pending,omitempty"`
	Decisions  []Decision `json:"decisions,omitempty"`
	Tallied    bool       `json:"tallied"` // the gate journaled its terminal tally
	// AskedAt is when the run asked for approval, by the run's clock: a flow node's request time
	// (agent.ApprovalRequest.RecordedAt), else the pause's (agent.ApprovalWait.RecordedAt); empty
	// when the journal does not say.
	AskedAt string `json:"asked_at,omitempty"`
	// Waiting: the run paused at this gate (its approval-pending record, agent.ApprovalWait) and
	// the call has no result, no terminal tally, and (for a one-decision gate) no decision yet.
	Waiting bool `json:"waiting"`
	// Signatures is whether the gate's approver signatures were verified (audit.VerifyApprovals,
	// with -approver-keys), or why they were not checked; SignaturesOK reports a verified gate.
	Signatures   string `json:"signatures"`
	SignaturesOK bool   `json:"signatures_ok"`
	Status       string `json:"status"`
	// Reapproval: the gate's recorded tally passed on decisions that predate the signed decision
	// format (agent.DecisionFormat) and the call has not run, so the gate asks again when the run
	// resumes (agent.PendingReapproval); only decisions with a signed time count.
	Reapproval bool `json:"reapproval,omitempty"`
	// ReapprovalSoD: the re-ask is because the recorded tally passed without excluding the requester
	// and the gate's policy now excludes it (agent.PendingSoDReapproval); Reapproval is set too.
	ReapprovalSoD bool `json:"reapproval_sod,omitempty"`
	// Cards is each approver as the page shows them (see ApproverCard); not in the JSON view.
	Cards []ApproverCard `json:"-"`
	// ExcludePrincipal: the gate never counts the run's requester (agent.ApprovalPolicy.ExcludePrincipal);
	// Requesters are the requesters (the run's principal and those of the runs above it), and Excluded
	// the approvers who fill no seat (the tally's, or before
	// it, the approver ids equal to the requester ignoring case).
	ExcludePrincipal bool     `json:"exclude_principal,omitempty"`
	Requesters       []string `json:"requesters,omitempty"`
	Excluded         []string `json:"excluded,omitempty"`
}

// Decision is one recorded approval decision. A signed decision carries two times: DecidedAt, the
// approver's clock inside the signature, and RecordedAt, the submitter's clock when it was
// journaled (not signed).
type Decision struct {
	Approver   string `json:"approver,omitempty"` // empty for a one-decision gate's Approve
	Approved   bool   `json:"approved"`
	Signed     bool   `json:"signed"`
	DecidedAt  string `json:"decided_at,omitempty"`
	RecordedAt string `json:"recorded_at,omitempty"`
	// Note says why a recorded decision did not count, when the journal alone would make it look
	// as if it should have: agent.ReasonAfterExpiry for one written after the gate's expiry.
	Note string `json:"note,omitempty"`
	name string
}

// Build reads the run's records into a View. recs is the run's journal in order (header
// included); flow, when non-nil, is the declared flow the run is drawn against.
func Build(ctx context.Context, runID string, recs []agent.Record, flow *Flow) *View {
	v := &View{RunID: runID, Records: len(recs), State: string(agent.RunNotStarted)}
	if j, err := agent.NewJournal(staticStore{recs: recs}); err == nil {
		if st, err := agent.Status(ctx, j, runID); err == nil {
			v.State, v.Terminal, v.EndedAt = string(st.State), st.Terminal, st.EndedAt
		}
	}

	calls := map[string]agent.ToolUse{} // tool-use ID -> the call a model turn requested
	var callOrder []string
	for _, r := range recs {
		// A gated plan flow node's request (agent.ApprovalRequest) is its call: its ID is the
		// node's key, its name the node's.
		if q, ok := flowRequest(r); ok {
			if _, seen := calls[q.ID]; !seen {
				callOrder = append(callOrder, q.ID)
			}
			calls[q.ID] = q
			continue
		}
		if r.Kind == agent.StepModel && r.Message != nil {
			for _, p := range r.Message.Parts {
				if tu, ok := p.(agent.ToolUse); ok {
					if _, seen := calls[tu.ID]; !seen {
						callOrder = append(callOrder, tu.ID)
					}
					calls[tu.ID] = tu
				}
			}
		}
	}

	var started time.Time
	for i, r := range recs {
		e := Event{Seq: i + 1, Name: r.Name, Kind: string(r.Kind), Class: "value", Title: r.Name, Payload: payloadOf(r)}
		switch {
		case r.Kind == agent.StepHeader:
			e.Class, e.Title, e.Detail = "meta", "journal header", r.Format
		case r.Name == "run:start":
			var st agent.RunStart
			if json.Unmarshal(r.Result, &st) == nil {
				v.Kind = string(st.Kind)
				if v.Kind == "" {
					v.Kind = string(agent.RunKindAgent)
				}
				v.Input = st.Input.Text()
				if st.Flow != nil {
					v.Flow = st.Flow.Name
				}
				e.Detail = v.Input
				if t, ok := st.Started(); ok {
					started, e.At = t, t
				}
				if len(st.Tools) > 0 {
					e.Badges = append(e.Badges, "tools: "+strings.Join(st.Tools, ", "))
				}
				if st.Saga {
					e.Badges = append(e.Badges, "saga")
				}
			}
			e.Class, e.Title = "start", "run started ("+v.Kind+")"
		case r.Name == "flow:digest":
			_ = json.Unmarshal(r.Result, &v.Digest)
			e.Class, e.Title, e.Detail = "meta", "topology digest", v.Digest
		case r.Name == "run:complete" || r.Name == "run:aborted" || r.Name == "run:cancelled":
			e.Class, e.Title, e.Detail = "end", strings.TrimPrefix(r.Name, "run:"), short(r.Result)
		case r.Kind == agent.StepModel:
			e.Class = "model"
			e.Title = "model turn"
			if m := r.Model(); m != nil && m.Model != "" {
				e.Badges = append(e.Badges, m.Model)
			}
			if f := r.Finish(); f != "" {
				e.Badges = append(e.Badges, "finish: "+string(f))
			}
			var lines []string
			if r.Message != nil {
				if t := r.Message.Text(); t != "" {
					lines = append(lines, t)
					v.FinalText = t
				}
				for _, p := range r.Message.Parts {
					if tu, ok := p.(agent.ToolUse); ok {
						lines = append(lines, fmt.Sprintf("calls %s (%s) %s", tu.Name, tu.ID, compact(tu.Args)))
					}
				}
			}
			e.Detail = strings.Join(lines, "\n")
		case r.Kind == agent.StepAttempt:
			e.Class = "attempt"
			op, id := attemptOp(r)
			e.Title = "attempt claimed: " + op + " " + id
			if tu, ok := calls[id]; ok && op == "tool" {
				e.Title += " (" + tu.Name + ")"
			}
			if r.AttemptedAt > 0 {
				e.At = time.UnixMilli(r.AttemptedAt).UTC()
				e.Detail = "at " + e.At.Format(time.RFC3339)
			}
			e.Badges = append(e.Badges, "side effect")
		case r.Kind == agent.StepNotStarted || strings.HasPrefix(r.Name, "attempt:not-started:"):
			e.Class, e.Title = "attempt", "attempt never started"
			e.Detail = notStartedMarker(r.Name)
		case r.Kind == agent.StepToolResult || r.Kind == agent.StepSagaFail:
			e.Class = "tool"
			name := calls[r.ToolUseID].Name
			e.Title = "tool result: " + name + " (" + r.ToolUseID + ")"
			if r.Kind == agent.StepSagaFail {
				e.Title = "saga step failed: " + name + " (" + r.ToolUseID + ")"
			}
			e.Detail = short(r.Result)
			e.Badges = append(e.Badges, safetyLabel(r))
			if r.IsError {
				e.Badges = append(e.Badges, "error")
			}
			if r.Reconciled {
				e.Badges = append(e.Badges, "reconciled by ResolveHalt")
			}
			if len(r.Evidence) > 0 {
				e.Detail += "\nevidence: " + short(r.Evidence)
			}
			if r.Approval != nil {
				e.Badges = append(e.Badges, "gated")
			}
		case r.Kind == agent.StepApproval:
			e.Class = "approval"
			verb := "approved"
			if !r.Approved {
				verb = "denied"
			}
			who := r.Approver()
			if who == "" {
				who = "approver"
			}
			e.Title = who + " " + verb + " " + r.ToolUseID
			if who != "approver" {
				e.Badges = append(e.Badges, "signed")
			}
		case strings.HasPrefix(r.Name, "approval-pending:"):
			var w agent.ApprovalWait
			_ = json.Unmarshal(r.Result, &w)
			e.Class, e.Title = "approval", "waiting for approval: "+w.ToolName+" ("+w.ToolUseID+")"
			if w.Reask != "" {
				e.Title = "asked again for approval: " + w.ToolName + " (" + w.ToolUseID + ")"
			}
			if w.Single {
				e.Detail = "one decision"
			} else {
				e.Detail = fmt.Sprintf("%d of %d: %s", w.Need, len(w.Approvers), strings.Join(w.Approvers, ", "))
				if w.ExcludePrincipal {
					e.Detail += ", excluding the requester"
				}
			}
		case strings.HasPrefix(r.Name, "approval-request:"):
			q, _ := flowRequest(r)
			e.Class, e.Title, e.Detail = "approval", "approval requested: "+q.Name+" ("+q.ID+")", "input "+compact(q.Args)
		case strings.HasPrefix(r.Name, "approval-tally:"):
			e.Class, e.Title, e.Detail = "approval", "approval tally", short(r.Result)
		case r.Kind == agent.StepSignal:
			e.Class, e.Title, e.Detail = "value", "signal "+r.Name, short(r.Result)
		case strings.HasPrefix(r.Name, "node:"):
			e.Class, e.Title, e.Detail = "node", r.Name, short(r.Result)
		case strings.HasPrefix(r.Name, "switch:"):
			e.Class, e.Title, e.Detail = "node", r.Name, "chose "+short(r.Result)
		default:
			e.Detail = short(r.Result)
		}
		// A record that notes when it was written (a single approver's decision, a ResolveHalt result,
		// an end marker) has its place on the run's clock, if the note is in the canonical form. A
		// decision signed by an m-of-n approver has its own card; its journaling time places the row.
		if s := r.RecordedAt(); s != "" {
			e.RecordedAt = s
			if t, err := agent.ParseTimestamp(s); err == nil && e.At.IsZero() {
				e.At = t
			}
		}
		v.Events = append(v.Events, e)
	}

	timeline(v.Events, started)
	v.Halts = halts(runID, recs, calls)
	v.StateText = v.State
	if v.State == string(agent.RunStarted) && len(v.Halts) > 0 {
		v.State, v.StateText = "halted", "halted: outcome unknown"
	}
	if v.State == string(agent.RunNotStarted) && stepsOnly(recs) {
		v.Kind = KindSteps
		switch {
		case len(v.Halts) > 0:
			v.State, v.StateText = "halted", "halted: outcome unknown"
		case stepsResolved(recs):
			v.State, v.StateText = "resolved", "resolved"
		default:
			v.State, v.StateText = "done", "steps done"
		}
	}
	v.Gates = gates(recs, calls, callOrder)
	if v.State == string(agent.RunStarted) && slices.ContainsFunc(v.Gates, func(g Gate) bool { return g.Waiting }) {
		v.State, v.StateText = "waiting", "waiting for approval"
	}
	if v.Kind == string(agent.RunKindFlow) || v.Flow != "" {
		v.Graph, v.Conform, v.LastNode = flowGraph(ctx, runID, recs, flow, v)
	}
	for _, h := range v.Halts {
		if h.Op == "tool" {
			v.Notes = append(v.Notes, "a halted call's Safety is not journaled (only a result carries it); its attempt marker implies a side effect")
			break
		}
	}
	return v
}

// KindSteps is the kind of a run journaled by Journal.Step calls alone: it has no run:start, so
// the journal does not say what kind of run it is.
const KindSteps = "steps"

// stepsOnly reports whether recs are a run with no run:start but with records beyond the header:
// durable steps run straight on a Journal.
func stepsOnly(recs []agent.Record) bool {
	n := 0
	for _, r := range recs {
		if r.Name == "run:start" {
			return false
		}
		if r.Kind != agent.StepHeader {
			n++
		}
	}
	return n > 0
}

// stepsResolved reports whether a halted operation of recs was resolved: a record that
// agent.ResolveHalt reconciled with evidence, or an operation that has a re-attempt marker (which a
// resolution claims, and a resume after a not-started record writes) and a result, with that
// re-attempt not recorded as never started.
func stepsResolved(recs []agent.Record) bool {
	voided := map[string]bool{}
	results := map[string]bool{}
	for _, r := range recs {
		switch {
		case r.Reconciled:
			return true
		case strings.HasPrefix(r.Name, "attempt:not-started:"):
			voided[notStartedMarker(r.Name)] = true
		case r.Kind != agent.StepAttempt:
			results["step:"+r.Name] = true
		}
	}
	for _, r := range recs {
		if r.Kind != agent.StepAttempt || !strings.HasPrefix(r.Name, "attempt:retry:") || voided[r.Name] {
			continue
		}
		if op, id := attemptOp(r); results[op+":"+id] {
			return true
		}
	}
	return false
}

// attemptOp is the kind and ID of the operation an attempt marker guards.
func attemptOp(r agent.Record) (op, id string) {
	base := attemptBase(r.Name)
	if strings.HasPrefix(base, "attempt:tool:") {
		return "tool", r.ToolUseID
	}
	return "step", strings.TrimPrefix(base, "attempt:step:")
}

// attemptBase is the first attempt's marker key of a re-attempt's marker key
// ("attempt:retry:<g>:tool:<id>" is a re-attempt of "attempt:tool:<id>"), or key itself.
func attemptBase(key string) string {
	rest, ok := strings.CutPrefix(key, "attempt:retry:")
	if !ok {
		return key
	}
	if _, tail, ok := strings.Cut(rest, ":"); ok {
		return "attempt:" + tail
	}
	return key
}

// notStartedMarker is the marker key a not-started record names: "attempt:not-started:<claim>:<marker>".
func notStartedMarker(name string) string {
	rest := strings.TrimPrefix(name, "attempt:not-started:")
	if _, marker, ok := strings.Cut(rest, ":"); ok {
		return marker
	}
	return rest
}

// halts lists the operations whose latest attempt marker is live (not recorded as never started)
// and that have no result: what a resume halts on.
func halts(runID string, recs []agent.Record, calls map[string]agent.ToolUse) []Halt {
	voided := map[string]bool{}
	results := map[string]bool{} // tool-use IDs with a result, and names of recorded records
	for _, r := range recs {
		switch {
		case strings.HasPrefix(r.Name, "attempt:not-started:"):
			voided[notStartedMarker(r.Name)] = true
		case r.Kind == agent.StepToolResult || r.Kind == agent.StepSagaFail:
			results["tool:"+r.ToolUseID] = true
		case r.Kind != agent.StepAttempt:
			results["step:"+r.Name] = true
		}
	}
	latest := map[string]agent.Record{} // base marker key -> latest marker
	var order []string
	for _, r := range recs {
		if r.Kind != agent.StepAttempt {
			continue
		}
		b := attemptBase(r.Name)
		if _, ok := latest[b]; !ok {
			order = append(order, b)
		}
		latest[b] = r
	}
	var out []Halt
	for _, b := range order {
		m := latest[b]
		op, id := attemptOp(m)
		if voided[m.Name] || results[op+":"+id] {
			continue
		}
		h := Halt{Op: op, ID: id, Marker: m.Name}
		if m.AttemptedAt > 0 {
			h.AttemptedAt = time.UnixMilli(m.AttemptedAt).UTC()
		}
		kind := "agent.OpStep"
		if op == "tool" {
			kind = "agent.OpTool"
			h.ToolName = calls[id].Name
		}
		tool := ""
		if h.ToolName != "" {
			tool = ", ToolName: " + strconv.Quote(h.ToolName)
		}
		h.Ref = fmt.Sprintf("agent.HaltRef{RunID: %s, Op: agent.OpRef{Kind: %s, ID: %s%s}, Cause: agent.HaltCrashed}",
			strconv.Quote(runID), kind, strconv.Quote(id), tool)
		out = append(out, h)
	}
	return out
}

// flowRequest reads r as a gated plan flow node's request (agent.ApprovalRequest), as the node's
// call: its ID is the node's key, its Name the node's name, its Args the node's input.
func flowRequest(r agent.Record) (agent.ToolUse, bool) {
	if !strings.HasPrefix(r.Name, "approval-request:") {
		return agent.ToolUse{}, false
	}
	var q agent.ApprovalRequest
	if json.Unmarshal(r.Result, &q) != nil {
		return agent.ToolUse{}, false
	}
	_, call, ok := agent.FindToolCall([]agent.Record{r}, q.ToolUseID)
	return call, ok
}

// gates reads each gated call's approval decisions and tally: a tool call's, or a plan flow
// node's (its ID the node's key, and its result the node's record).
func gates(recs []agent.Record, calls map[string]agent.ToolUse, order []string) []Gate {
	byName := map[string]agent.Record{}
	for _, r := range recs {
		byName[r.Name] = r
	}
	var out []Gate
	for _, id := range order {
		g := Gate{ToolUseID: id, ToolName: calls[id].Name}
		var wait agent.ApprovalWait
		paused := false
		// The newest pause note: a gate that asked again (agent.ApprovalReaskStep) records the policy it
		// asks under there, while the first note keeps the policy of the first pause.
		if r, ok := agent.LatestPauseNote(byName, id); ok && json.Unmarshal(r.Result, &wait) == nil {
			paused = true
			g.Single, g.Need, g.Approvers, g.AskedAt = wait.Single, wait.Need, wait.Approvers, wait.RecordedAt
			g.ExcludePrincipal, g.Requesters = wait.ExcludePrincipal, wait.Requesters
		}
		// A flow node's request is when approvers were asked, and what the ordering rule counts from.
		if r, ok := byName[agent.ApprovalRequestStep(id)]; ok {
			var q agent.ApprovalRequest
			if json.Unmarshal(r.Result, &q) == nil && q.RecordedAt != "" {
				g.AskedAt = q.RecordedAt
			}
		}
		resulted := false
		for _, r := range recs {
			if (r.Kind == agent.StepToolResult || r.Kind == agent.StepSagaFail) && r.ToolUseID == id || r.Kind == agent.StepValue && r.Name == id && strings.HasPrefix(id, "node:") {
				resulted = true
			}
			if r.Kind == agent.StepApproval && r.ToolUseID == id {
				g.Decisions = append(g.Decisions, Decision{Approver: r.Approver(), Approved: r.Approved, Signed: r.Approver() != "", DecidedAt: r.DecidedAt(), RecordedAt: r.RecordedAt(), name: r.Name})
			}
			if (r.Kind == agent.StepToolResult || r.Kind == agent.StepSagaFail) && r.ToolUseID == id && r.Approval != nil {
				g.Need, g.Approvers = r.Approval.Need, r.Approval.Approvers
				// The policy the call ran under says whether it was a one-decision gate.
				if b, err := json.Marshal(r.Approval); err == nil && string(b) == `{"single":true}` {
					g.Single = true
				}
				g.ExcludePrincipal = g.ExcludePrincipal || r.Approval.ExcludePrincipal
			}
		}
		if t, ok := agent.RecordedTally(byName, id); ok {
			var tally agent.ApprovalTally
			switch {
			case json.Unmarshal(t.Result, &tally) != nil:
			case !resulted && agent.GateReask(tally, g.ExcludePrincipal) != "":
				// The gate asks again rather than act on this tally (agent.GateReask, the rule the gate
				// itself applies): it passed on decisions that predate the signed decision format, or
				// without excluding the requester under a policy that now excludes it. Only decisions
				// with a signed time count, and the requester's does not.
				g.Reapproval = true
				g.ReapprovalSoD = agent.GateReask(tally, g.ExcludePrincipal) == agent.PendingSoDReapproval
				g.Need, g.Approvers = tally.Need, tally.Approvers
			default:
				g.Tallied = true
				g.Need, g.Approvers = tally.Need, tally.Approvers
				g.ApprovedBy, g.DeniedBy, g.Pending = tally.ApprovedBy, tally.DeniedBy, tally.Pending
				g.ExcludePrincipal, g.Requesters, g.Excluded = tally.ExcludePrincipal, tally.Requesters, tally.Excluded
				if tally.Expired {
					for i := range g.Decisions {
						if g.Decisions[i].Signed && !slices.Contains(tally.Records, g.Decisions[i].name) {
							g.Decisions[i].Note = agent.ReasonAfterExpiry
						}
					}
				}
			}
		}
		if !g.Tallied && g.ExcludePrincipal {
			for _, a := range g.Approvers {
				// The gate's rule for matching a requester to an approver id.
				if slices.ContainsFunc(g.Requesters, func(r string) bool { return r != "" && agent.SameApprover(a, r) }) {
					g.Excluded = append(g.Excluded, a)
				}
			}
		}
		if len(g.Decisions) == 0 && !g.Tallied && g.Need == 0 && !paused {
			continue // an ungated call
		}
		if !g.Tallied {
			// Whether the gate is a one-decision gate comes from the pause note, or the policy the
			// call ran under, never from the decisions: an unsigned agent.Approve record on an
			// m-of-n call is ignored by the runtime, and must not turn the gate into a single one.
			for _, d := range g.Decisions {
				switch {
				case slices.Contains(g.Excluded, d.Approver): // the requester: never counts
				case g.Reapproval && d.DecidedAt == "": // signed before the format: never counts now
				case d.Signed && d.Approved && !slices.Contains(g.ApprovedBy, d.Approver):
					g.ApprovedBy = append(g.ApprovedBy, d.Approver)
				case d.Signed && !d.Approved && !slices.Contains(g.DeniedBy, d.Approver):
					g.DeniedBy = append(g.DeniedBy, d.Approver)
				}
			}
			for _, id := range g.Approvers {
				if !slices.Contains(g.ApprovedBy, id) && !slices.Contains(g.DeniedBy, id) && !slices.Contains(g.Excluded, id) {
					g.Pending = append(g.Pending, id)
				}
			}
		}
		_, singleDecided := singleDecision(g)
		g.Waiting = (paused || g.Reapproval) && !resulted && !g.Tallied && !(g.Single && singleDecided)
		g.Status = gateStatus(g)
		out = append(out, g)
	}
	return out
}

// gateStatus says where a gate stands, as "<state> (<approved> of <n> approvers; <k> needed)":
// approved once k approved, denied once enough denied that k can no longer approve, and pending
// otherwise. Without the gate's tally the counts are of signed decisions, whose signatures the gate
// checks when the run resumes.
// singleDecision is the one-decision gate's decision: the first unsigned record (agent.Approve). A
// signed record on such a gate is not one.
func singleDecision(g Gate) (Decision, bool) {
	if !g.Single {
		return Decision{}, false
	}
	for _, d := range g.Decisions {
		if !d.Signed {
			return d, true
		}
	}
	return Decision{}, false
}

func gateStatus(g Gate) string {
	if g.Single {
		d, ok := singleDecision(g)
		switch {
		case !ok:
			return "pending (one decision; none recorded yet)"
		case d.Approved:
			return "approved (single decision)"
		}
		return "denied (single decision)"
	}
	n, a, d := len(g.Approvers), len(g.ApprovedBy), len(g.DeniedBy)
	if g.Need == 0 || n == 0 {
		return fmt.Sprintf("pending (%d approved, %d denied so far; the policy is journaled with the call's result or the gate's tally)", a, d)
	}
	counts := fmt.Sprintf("%d of %d approvers", a, n)
	if d > 0 {
		counts += fmt.Sprintf(", %d denied", d)
	}
	counts += fmt.Sprintf("; %d needed", g.Need)
	if len(g.Excluded) > 0 {
		counts += fmt.Sprintf("; %s excluded", strings.Join(g.Excluded, ", "))
	}
	if g.ReapprovalSoD && a < g.Need {
		return "approval re-required: the recorded approvals were counted without excluding the requester, whom the gate now excludes (" + counts + ")"
	}
	if g.Reapproval && a < g.Need {
		return "approval re-required: the recorded approvals predate " + agent.DecisionFormat + " (" + counts + ")"
	}
	// Only the gate's own tally is a count the gate made. Counted from the decisions recorded, the
	// numbers are of records whose signatures nothing here has checked (the gate checks them when the
	// run resumes), so they are never called an approval or a denial.
	switch {
	case a >= g.Need && g.Tallied:
		return "approved (" + counts + ")"
	case n-len(g.Excluded)-d < g.Need && g.Tallied:
		return "denied (" + counts + ")"
	case a >= g.Need:
		return "approvals recorded, unverified (" + counts + "; the gate checks every signature when the run resumes)"
	case n-len(g.Excluded)-d < g.Need:
		return "denials recorded, unverified (" + counts + "; the gate checks every signature when the run resumes)"
	case g.Tallied:
		return "closed, not approved (" + counts + ")"
	default:
		return "pending (" + counts + "; signatures are checked when the run resumes)"
	}
}

// safetyLabel names the Safety a tool result records.
func safetyLabel(r agent.Record) string {
	switch {
	case r.Safety == nil:
		return "side effect (resolved)"
	case r.Safety.ReadOnly:
		return "read-only"
	case r.Safety.Idempotent:
		return "idempotent"
	default:
		return "side effect"
	}
}

// compact renders raw JSON on one line, or "" for none.
func compact(b json.RawMessage) string {
	if len(b) == 0 {
		return ""
	}
	return string(b)
}

// short renders a record's JSON result for a timeline row: a JSON string as its text, anything
// else as JSON, cut to a readable length.
func short(b json.RawMessage) string {
	if len(b) == 0 {
		return ""
	}
	s := string(b)
	var str string
	if json.Unmarshal(b, &str) == nil {
		s = str
	}
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}

// payloadOf is the record as indented JSON: the bytes the store holds, or its encoding for a record
// built in memory.
func payloadOf(r agent.Record) string {
	raw := r.Raw()
	if len(raw) == 0 {
		raw, _ = json.Marshal(r)
	}
	return prettyJSON(raw)
}

// timeline sets the time of each row the journal timestamps: its offset from start (or, with no
// start time, from the first timestamped row), the UTC time, and its bar on the run's clock.
func timeline(evs []Event, start time.Time) {
	var at []int
	for i, e := range evs {
		if !e.At.IsZero() {
			at = append(at, i)
		}
	}
	if len(at) == 0 {
		return
	}
	origin := start
	if origin.IsZero() {
		origin = evs[at[0]].At
	}
	// The span is the earliest to the latest time, not the first to the last in journal order: a
	// record stamped by a process whose clock is behind (a resume on another host) can sit earlier
	// than the rows before it, and must not erase the others' bars.
	lo, hi := origin, origin
	for _, i := range at {
		lo, hi = minTime(lo, evs[i].At), maxTime(hi, evs[i].At)
	}
	for _, i := range at {
		e := &evs[i]
		e.Abs = e.At.UTC().Format("2006-01-02 15:04:05.000 UTC")
		e.Rel = "+" + durText(e.At.Sub(origin))
		if e.At.Equal(start) {
			e.Rel = "start"
		}
	}
	span := hi.Sub(lo)
	if span <= 0 || len(at) < 2 {
		return
	}
	for k, i := range at {
		e := &evs[i]
		from := e.At.Sub(lo)
		to := from
		label := "last timestamped record"
		if k+1 < len(at) {
			to = evs[at[k+1]].At.Sub(lo)
			label = durText(to-from) + " until the next timestamped record"
		}
		bar := &TimeBar{X: 100 * float64(from) / float64(span), W: 100 * float64(to-from) / float64(span), Label: label}
		bar.W = max(bar.W, 1.2)
		bar.X = min(bar.X, 100-bar.W) // lo is the earliest time, so X is never negative
		e.Bar = bar
	}
}

// durText phrases a duration in two units at most: 850ms, 3.2s, 4m 05s, 2h 10m, 3d 4h.
func durText(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

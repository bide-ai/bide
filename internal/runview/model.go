// Package runview is a read-only, local web viewer for one run's journal: a plan flow's declared
// topology with the run's path drawn over it, or an agent run's timeline of model turns, tool
// calls, attempts, approvals and halts, with the audit verdict when a signed head or evidence
// package is given. It never writes to the store it reads, and it serves on a loopback address
// only (see Listen). The bide-view command (cmd/bide-view, its own module) is its front end.
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
	Records   int    `json:"records"`

	Events    []Event      `json:"events"`
	Halts     []Halt       `json:"halts,omitempty"`
	Gates     []Gate       `json:"gates,omitempty"`
	Graph     *Graph       `json:"graph,omitempty"`
	Conform   *Conformance `json:"conformance,omitempty"`
	Proof     *Proof       `json:"proof,omitempty"`
	Notes     []string     `json:"notes,omitempty"` // what the page cannot show for this run, and why
	LastNode  string       `json:"last_node,omitempty"`
	FinalText string       `json:"final_text,omitempty"`
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
	// Waiting: the run paused at this gate (its approval-pending record, agent.ApprovalWait) and
	// the call has no result, no terminal tally, and (for a one-decision gate) no decision yet.
	Waiting bool `json:"waiting"`
	// Signatures is whether the gate's approver signatures were verified (audit.VerifyApprovals,
	// with -approver-keys), or why they were not checked; SignaturesOK reports a verified gate.
	Signatures   string `json:"signatures"`
	SignaturesOK bool   `json:"signatures_ok"`
	Status       string `json:"status"`
}

// Decision is one recorded approval decision.
type Decision struct {
	Approver string `json:"approver,omitempty"` // empty for a one-decision gate's Approve
	Approved bool   `json:"approved"`
	Signed   bool   `json:"signed"`
}

// Build reads the run's records into a View. recs is the run's journal in order (header
// included); flow, when non-nil, is the declared flow the run is drawn against.
func Build(ctx context.Context, runID string, recs []agent.Record, flow *Flow) *View {
	v := &View{RunID: runID, Records: len(recs), State: string(agent.RunNotStarted)}
	if j, err := agent.NewJournal(staticStore{recs: recs}); err == nil {
		if st, err := agent.Status(ctx, j, runID); err == nil {
			v.State, v.Terminal = string(st.State), st.Terminal
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

	for i, r := range recs {
		e := Event{Seq: i + 1, Name: r.Name, Kind: string(r.Kind), Class: "value", Title: r.Name}
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
				e.Detail = "at " + time.UnixMilli(r.AttemptedAt).UTC().Format(time.RFC3339)
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
			if w.Single {
				e.Detail = "one decision"
			} else {
				e.Detail = fmt.Sprintf("%d of %d: %s", w.Need, len(w.Approvers), strings.Join(w.Approvers, ", "))
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
		v.Events = append(v.Events, e)
	}

	v.Halts = halts(runID, recs, calls)
	v.StateText = v.State
	if v.State == string(agent.RunStarted) && len(v.Halts) > 0 {
		v.State, v.StateText = "halted", "halted: outcome unknown"
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
		if r, ok := byName[agent.ApprovalPendingStep(id)]; ok && json.Unmarshal(r.Result, &wait) == nil {
			paused = true
			g.Single, g.Need, g.Approvers = wait.Single, wait.Need, wait.Approvers
		}
		resulted := false
		for _, r := range recs {
			if (r.Kind == agent.StepToolResult || r.Kind == agent.StepSagaFail) && r.ToolUseID == id || r.Kind == agent.StepValue && r.Name == id && strings.HasPrefix(id, "node:") {
				resulted = true
			}
			if r.Kind == agent.StepApproval && r.ToolUseID == id {
				g.Decisions = append(g.Decisions, Decision{Approver: r.Approver(), Approved: r.Approved, Signed: r.Approver() != ""})
			}
			if (r.Kind == agent.StepToolResult || r.Kind == agent.StepSagaFail) && r.ToolUseID == id && r.Approval != nil {
				g.Need, g.Approvers = r.Approval.Need, r.Approval.Approvers
			}
		}
		if t, ok := byName[agent.ApprovalTallyStep(id)]; ok {
			var tally agent.ApprovalTally
			if json.Unmarshal(t.Result, &tally) == nil {
				g.Tallied = true
				g.Need, g.Approvers = tally.Need, tally.Approvers
				g.ApprovedBy, g.DeniedBy, g.Pending = tally.ApprovedBy, tally.DeniedBy, tally.Pending
			}
		}
		if len(g.Decisions) == 0 && !g.Tallied && g.Need == 0 && !paused {
			continue // an ungated call
		}
		if !g.Tallied {
			g.Single = g.Single || len(g.Decisions) > 0 && !g.Decisions[0].Signed
			for _, d := range g.Decisions {
				switch {
				case d.Signed && d.Approved && !slices.Contains(g.ApprovedBy, d.Approver):
					g.ApprovedBy = append(g.ApprovedBy, d.Approver)
				case d.Signed && !d.Approved && !slices.Contains(g.DeniedBy, d.Approver):
					g.DeniedBy = append(g.DeniedBy, d.Approver)
				}
			}
			for _, id := range g.Approvers {
				if !slices.Contains(g.ApprovedBy, id) && !slices.Contains(g.DeniedBy, id) {
					g.Pending = append(g.Pending, id)
				}
			}
		}
		g.Waiting = paused && !resulted && !g.Tallied && !(g.Single && len(g.Decisions) > 0)
		g.Status = gateStatus(g)
		out = append(out, g)
	}
	return out
}

// gateStatus says where a gate stands, as "<state> (<approved> of <n> approvers; <k> needed)":
// approved once k approved, denied once enough denied that k can no longer approve, and pending
// otherwise. Without the gate's tally the counts are of signed decisions, whose signatures the gate
// checks when the run resumes.
func gateStatus(g Gate) string {
	if g.Single {
		if len(g.Decisions) == 0 {
			return "pending (one decision; none recorded yet)"
		}
		if g.Decisions[0].Approved {
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
	switch {
	case a >= g.Need:
		return "approved (" + counts + ")"
	case n-d < g.Need:
		return "denied (" + counts + ")"
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

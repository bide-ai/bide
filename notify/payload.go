package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Schema is the value of the payload's "schema" field. New fields are additive within v1; a
// receiver must ignore fields it does not know. A breaking change is a new schema string.
const Schema = "bide.notification.v1"

// maxErrorText bounds the allowlisted error text so one huge error cannot become a huge request.
const maxErrorText = 4096

// A Field is a payload field that is left out unless named in WithFields.
type Field struct{ apply func(*fieldSet) }

type fieldSet struct {
	errorText bool
	args      map[string]bool // tool name -> include the call's arguments
}

// ErrorText allows the typed error's text ("error") in the payload. The text can embed tool
// arguments, account or card numbers and personal data, so a deployment under PCI-style rules
// should leave it out and put detail behind the viewer, where access control applies.
var ErrorText = Field{func(f *fieldSet) { f.errorText = true }}

// Args allows the arguments of an approval-pending call ("approval.args") for the named tools only.
// Arguments of any other tool are never sent.
func Args(tools ...string) Field {
	return Field{func(f *fieldSet) {
		if f.args == nil {
			f.args = map[string]bool{}
		}
		for _, t := range tools {
			f.args[t] = true
		}
	}}
}

// Payload is the JSON body of a webhook request, schema bide.notification.v1. Exactly one of Halt,
// Approval, Abort, Failing and Clears is non-nil, where the kind has one. It is exported so a receiver can
// decode it; a receiver must ignore unknown fields.
type Payload struct {
	Schema     string     `json:"schema"`
	ID         string     `json:"id"`
	Key        string     `json:"key"`
	Kind       string     `json:"kind"`
	RunID      string     `json:"run_id"`
	EventAt    *time.Time `json:"event_at"`
	ObservedAt *time.Time `json:"observed_at"`
	DeadlineAt *time.Time `json:"deadline_at"`
	Attempt    int        `json:"attempt"`
	Summary    string     `json:"summary"`
	Step       *Step      `json:"step"`
	Halt       *Halt      `json:"halt"`
	Principal  *Principal `json:"principal"`
	Approval   *Approval  `json:"approval"`
	Abort      *Abort     `json:"abort"`
	Failing    *Failing   `json:"failing"`
	// Stop is present for a stop_set or stop_lifted event: the kill switch stop it is about.
	Stop *Stop `json:"stop"`
	// Clears is present for a resolved notification: the event it ends.
	Clears *Clears `json:"clears"`
	// Escalates is present for an escalated notification: the event that was not acknowledged (an
	// approval: not decided) by its deadline, and the step of the chain this is. The event's own
	// detail (halt, approval, step) is filled in as the event's own notifications carry it.
	Escalates *Escalates `json:"escalates"`
	// Receiver is the name of the receiver this request is to ("default" for the notifier's own).
	Receiver string `json:"receiver,omitempty"`
	Reason   string `json:"reason,omitempty"`
	View     View   `json:"view"`
	// Error is present only with WithFields(ErrorText).
	Error string `json:"error,omitempty"`
}

// Step names the operation an event is about.
type Step struct {
	OpKind string `json:"op_kind"`
	OpID   string `json:"op_id"`
	Tool   string `json:"tool,omitempty"`
}

// Halt is the detail of an outcome_unknown event.
type Halt struct {
	Cause       string     `json:"cause"`
	AttemptedAt *time.Time `json:"attempted_at"`
}

// Principal is the party the run acts for. A receiver routes away from it as one input.
type Principal struct {
	OnBehalfOf   string `json:"on_behalf_of"`
	AuthorityRef string `json:"authority_ref"`
}

// Approval is the detail of an approval_pending event: who may decide and how many must.
type Approval struct {
	ToolUseID string   `json:"tool_use_id"`
	Tool      string   `json:"tool"`
	Policy    Policy   `json:"policy"`
	Approvers []string `json:"approvers"`
	Approved  int      `json:"approved"`
	// Excluded are the approvers who may not decide (separation of duties: requesters of the run,
	// agent.ApprovalPolicy.ExcludePrincipal); omitted when there are none.
	Excluded []string `json:"excluded,omitempty"`
	// Reask says why the gate asks again over a recorded tally (agent.PendingReapproval,
	// agent.PendingSoDReapproval); omitted on the gate's first ask.
	Reask string          `json:"reask,omitempty"`
	Args  json.RawMessage `json:"args,omitempty"` // only with WithFields(Args(tool))
}

// Policy is the gate's k of n.
type Policy struct {
	Need int `json:"need"`
	Of   int `json:"of"`
}

// Abort is the detail of a run_aborted event; Reason is a code from a closed set, not text.
type Abort struct {
	Reason string `json:"reason"`
}

// Failing is the detail of a recovery_failing event; Cause is a class, not text.
type Failing struct {
	Cause string `json:"cause"`
}

// Stop is the detail of a stop_set or stop_lifted event. It holds no key, signature or secret. Scope is
// "global", "tool", "flow" or "agent", and Name the tool, flow or agent (empty for global). By is the setter:
// asserted in basic mode, authenticated by the signature when Signed. Reason is the operator's text.
// Compensation is "allow" or "block". LiftedBy is present for stop_lifted.
type Stop struct {
	ID           string   `json:"id"`
	Scope        string   `json:"scope"`
	Name         string   `json:"name,omitempty"`
	Reason       string   `json:"reason"`
	By           string   `json:"by"`
	Host         string   `json:"host,omitempty"`
	Mode         string   `json:"mode"`
	Signed       bool     `json:"signed"`
	Compensation string   `json:"compensation"`
	LiftedBy     []string `json:"lifted_by,omitempty"`
}

// Clears is the detail of a resolved notification, the all-clear: the event that stopped being
// current. ID and Key are those of that event's own notifications (their "id" and "key"). An event
// does not become current again and its key is never reused, so a receiver may treat every
// delivery of ID, before or after this one, as superseded: the all-clear can arrive first.
type Clears struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Kind string `json:"kind"`
}

// Escalates is the detail of an escalated notification. ID and Key are those of the event's own
// notifications (their "id" and "key"); Level is the step of the escalation chain, from 1.
type Escalates struct {
	ID    string `json:"id"`
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Level int    `json:"level"`
}

// View points at where to look. It never carries an approve or resolve link: acting from a
// notification is a non-goal, and a link that decides on a click is a phishing target.
type View struct {
	Command string  `json:"command"`
	URL     *string `json:"url"`
}

// viewPlaceholder stands for the journal path when the webhook was not told it. It is angle
// bracketed so it cannot be mistaken for a real file and fails loudly if pasted as it is.
const viewPlaceholder = "<journal.db>"

// viewCommand is the bide view command for a run: with the store flag when WithViewDB set it, and
// with a marked template otherwise, since bide view needs one of -db, -postgres and -journal.
func (w *Webhook) viewCommand(runID string) string {
	db := viewPlaceholder
	if w.viewDB != "" {
		db = shellWord(w.viewDB)
	}
	return "bide view -db " + db + " -run " + shellWord(runID)
}

func tp(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC().Truncate(time.Second)
	return &t
}

func (w *Webhook) payload(n agent.Notification) Payload {
	if n.Kind == agent.NotificationEscalated && n.Escalates != nil {
		// The event's own detail, under the escalation's identity.
		inner := n
		inner.Kind = n.Escalates.Kind
		p := w.payload(inner)
		p.ID, p.Key, p.Kind, p.Reason = n.ID, n.Key, string(n.Kind), n.Reason
		p.Escalates = &Escalates{ID: n.Escalates.ID, Key: n.Escalates.Key, Kind: string(n.Escalates.Kind), Level: n.Escalates.Level}
		p.Summary = fmt.Sprintf("Escalation %d: %s Its notification was not acknowledged, or it is not decided, by its deadline.", n.Escalates.Level, p.Summary)
		return p
	}
	p := Payload{
		Schema:     Schema,
		ID:         n.ID,
		Key:        n.Key,
		Kind:       string(n.Kind),
		RunID:      n.RunID,
		EventAt:    tp(n.EventAt),
		ObservedAt: tp(n.Observed),
		DeadlineAt: tp(n.Deadline),
		Attempt:    n.Attempt,
		Receiver:   n.Receiver,
		View:       View{Command: w.viewCommand(n.RunID)},
	}
	if w.viewURL != "" {
		u := strings.TrimRight(w.viewURL, "/") + "/" + url.PathEscape(n.RunID)
		p.View.URL = &u
	}
	if n.Principal != nil {
		p.Principal = &Principal{OnBehalfOf: n.Principal.OnBehalfOf, AuthorityRef: n.Principal.AuthorityRef}
	}
	run := n.RunID
	switch n.Kind {
	case agent.NotificationApprovalPending:
		if a := n.Approval; a != nil {
			p.Step = &Step{OpKind: string(agent.OpTool), OpID: a.ToolUseID, Tool: a.ToolName}
			ap := &Approval{ToolUseID: a.ToolUseID, Tool: a.ToolName, Policy: Policy{Need: a.Need, Of: a.Of}, Approvers: a.Approvers, Approved: a.Approved, Excluded: a.Excluded, Reask: a.Reask}
			if ap.Approvers == nil {
				ap.Approvers = []string{}
			}
			if w.fields.args[a.ToolName] {
				var pend *agent.ApprovalPending
				if errors.As(n.Err, &pend) && len(pend.Args) > 0 && json.Valid(pend.Args) {
					ap.Args = pend.Args
				}
			}
			p.Approval = ap
			p.Summary = fmt.Sprintf("Run %s is waiting for approval of tool %s (call %s).", run, a.ToolName, a.ToolUseID)
		} else {
			p.Summary = fmt.Sprintf("Run %s is waiting for approval.", run)
		}
	case agent.NotificationApprovalExpired:
		if a := n.Approval; a != nil {
			p.Step = &Step{OpKind: string(agent.OpTool), OpID: a.ToolUseID, Tool: a.ToolName}
			p.Approval = &Approval{ToolUseID: a.ToolUseID, Tool: a.ToolName, Approvers: []string{}}
			p.Summary = fmt.Sprintf("The approval gate of call %s in run %s expired: its deadline passed with no approval, and the call will not run.", a.ToolUseID, run)
		} else {
			p.Summary = fmt.Sprintf("An approval gate in run %s expired.", run)
		}
	case agent.NotificationOutcomeUnknown:
		if h := n.Halt; h != nil {
			p.Step = &Step{OpKind: string(h.Op.Kind), OpID: h.Op.ID, Tool: h.Op.ToolName}
			p.Halt = &Halt{Cause: string(h.Cause), AttemptedAt: tp(h.AttemptedAt)}
			what := "tool " + h.Op.ToolName + " (call " + h.Op.ID + ")"
			if h.Op.Kind == agent.OpStep {
				what = "step " + h.Op.ID
			}
			p.Summary = fmt.Sprintf("Run %s halted: the outcome of %s is unknown and needs a person to confirm.", run, what)
		} else {
			p.Summary = fmt.Sprintf("Run %s halted on an operation whose outcome is unknown.", run)
		}
	case agent.NotificationRunAborted:
		p.Abort = &Abort{Reason: n.Reason}
		p.Summary = fmt.Sprintf("Run %s was aborted.", run)
	case agent.NotificationRecoveryFailing:
		p.Failing = &Failing{Cause: n.Reason}
		p.Summary = fmt.Sprintf("Recovery of run %s keeps failing.", run)
	case agent.NotificationStopSet, agent.NotificationStopLifted:
		p.stopDetail(w, n)
	case agent.NotificationResolved:
		p.Reason = n.Reason
		if c := n.Clears; c != nil {
			p.Clears = &Clears{ID: c.ID, Key: c.Key, Kind: string(c.Kind)}
			p.Summary = fmt.Sprintf("Run %s: the %s event is over (%s). No action is needed for it.", run, c.Kind, n.Reason)
		} else {
			p.Summary = fmt.Sprintf("Run %s: an event that was reported is over (%s).", run, n.Reason)
		}
	case agent.NotificationOverdue:
		p.Reason = n.Reason
		p.Summary = fmt.Sprintf("A notification for run %s was not acknowledged by its deadline.", run)
	default:
		p.Summary = fmt.Sprintf("Run %s needs attention (%s).", run, n.Kind)
	}
	if w.fields.errorText && n.Err != nil {
		p.Error = clip(n.Err.Error(), maxErrorText)
	}
	return p
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// shellWord quotes s for a shell unless it is made of safe characters, so a run ID with spaces or
// metacharacters cannot turn the view command into something else if someone pastes it.
func shellWord(s string) string {
	safe := s != "" && s[0] != '-'
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:@/-", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// stopDetail fills in the payload of a stop_set or stop_lifted event. The view command points at the stop,
// not at a run: the control run is not one.
func (p *Payload) stopDetail(w *Webhook, n agent.Notification) {
	db := viewPlaceholder
	if w.viewDB != "" {
		db = shellWord(w.viewDB)
	}
	if s := n.Stop; s != nil {
		p.Stop = &Stop{ID: s.ID, Scope: string(s.Scope.Kind), Name: s.Scope.Name, Reason: s.Reason, By: s.By, Host: s.Host, Mode: string(s.Mode), Signed: s.Signed, Compensation: s.Compensation, LiftedBy: s.LiftedBy}
		p.View.Command = "bide stop -db " + db + " -show " + shellWord(s.ID)
		what := "everything"
		if s.Scope.Kind != agent.StopGlobal {
			what = s.Scope.String()
		}
		// The summary names the scope and no operator text (the setter, the reason): a chat message posts it
		// as it is, and text an operator typed would format it. The structured stop carries them.
		if n.Kind == agent.NotificationStopLifted {
			p.Summary = fmt.Sprintf("The stop of %s was lifted. Held runs carry on when they are next driven.", what)
		} else {
			p.Summary = fmt.Sprintf("Agents are stopped: %s. The stop names who set it and why.", what)
		}
	} else {
		p.View.Command = "bide stop -db " + db + " -list"
		p.Summary = fmt.Sprintf("A kill switch stop was %s.", map[bool]string{true: "lifted", false: "set"}[n.Kind == agent.NotificationStopLifted])
	}
	if w.viewURL != "" {
		u := strings.TrimRight(w.viewURL, "/") + "/"
		p.View.URL = &u
	}
}

package runview

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/approveline"
)

// AttentionList is the Needs attention page: every unfinished run that needs a person, and every
// aborted run: gates with a deadline first, the nearest first, then the rest newest first. It is built from each run's journal alone (the same View the run page
// shows), plus, when Options.Events is given, the run's notification trail. A load reads the runs
// that have no run:complete marker (a store lists them in its query), so it reads no finished run
// and has no cap.
type AttentionList struct {
	// Status is what the headline says, for a script: attention, clear or unknown (see Coverage).
	Status string          `json:"status"`
	Items  []AttentionItem `json:"items"`
	// Coverage says what was read; the headline is clear only when it is complete.
	Coverage
	// Live: the source is a store the runs are still written to, so the page refreshes itself.
	Live bool `json:"live,omitempty"`
	// Now is the time the waits are measured to.
	Now time.Time `json:"now"`
	// Runs is how many runs were read.
	Runs  int      `json:"runs"`
	Notes []string `json:"notes,omitempty"`
}

// Attention item kinds.
const (
	AttentionApproval = "approval" // a call waits for approvers
	AttentionHalt     = "halt"     // an operation's outcome is unknown
	AttentionAborted  = "aborted"  // the run aborted (a saga rolled back)
	AttentionRecovery = "recovery" // recovery has kept failing for the run (from the notify trail)
)

// AttentionItem is one thing a person has to do something about.
type AttentionItem struct {
	Kind     string `json:"kind"`
	KindText string `json:"kind_text"`
	RunID    string `json:"run_id"`
	// Call is the tool-use ID of the call (or the node key, or the operation's ID); Tool its name.
	Call string `json:"call,omitempty"`
	Tool string `json:"tool,omitempty"`
	// Since is when it began needing a person, by the journal's (or the trail's) clock: the gate's
	// ask time, the halted attempt's time, the abort's time, or when recovery began failing. Zero
	// when the journal does not say; such items come last.
	Since time.Time `json:"since,omitzero"`
	// Waiting is Since to the list's Now, as durText phrases it.
	Waiting string `json:"waiting,omitempty"`
	Summary string `json:"summary"`

	Gate *AttentionGate `json:"gate,omitempty"` // for an approval
	Halt *Halt          `json:"halt,omitempty"` // for a halt: its HaltRef is the ResolveHalt hint
	// Reason is why a run aborted, or the class of cause a recovery keeps failing with.
	Reason string `json:"reason,omitempty"`
	// Command is the bide approve command that records a decision on the call, when the viewer
	// knows how to write one (Options.ApproveCommand). Empty otherwise.
	Command string `json:"command,omitempty"`
	// NoCommand says why an approval has none although the viewer writes them: an id that cannot be
	// shown or pasted safely (approveline.ErrUnsafe).
	NoCommand string `json:"no_command,omitempty"`
	// Notify says whether the item was notified and acknowledged; nil without the notify trail.
	Notify *NotifyStatus `json:"notify,omitempty"`
}

// Link is the run's page.
func (i AttentionItem) Link() string { return "/run?id=" + url.QueryEscape(i.RunID) }

// SinceText is Since as the list shows it, or "" when the journal does not say.
func (i AttentionItem) SinceText() string {
	if i.Since.IsZero() {
		return ""
	}
	return i.Since.UTC().Format("2006-01-02 15:04:05 UTC")
}

// AttentionGate is a waiting approval gate as the list shows it.
type AttentionGate struct {
	Single     bool     `json:"single,omitempty"` // a one-decision gate (agent.Approve)
	Need       int      `json:"need,omitempty"`
	Of         int      `json:"of,omitempty"`
	Approvers  []string `json:"approvers,omitempty"`
	ApprovedBy []string `json:"approved_by,omitempty"`
	DeniedBy   []string `json:"denied_by,omitempty"`
	Pending    []string `json:"pending,omitempty"`
	Reapproval bool     `json:"reapproval,omitempty"`
	Status     string   `json:"status"` // the run page's gate status
	// ExpiresAt is the gate's recorded deadline, empty for a gate with none. Due says where it stands
	// on the list's clock (Gate.Due), DueSoon that it falls within dueSoonWindow (or has passed), and
	// Overdue that it has passed with no expiry recorded: the gate refuses decisions, and the run
	// records the expiry when it is next driven.
	ExpiresAt string `json:"expires_at,omitempty"`
	Due       string `json:"due,omitempty"`
	DueSoon   bool   `json:"due_soon,omitempty"`
	Overdue   bool   `json:"overdue,omitempty"`
}

// dueSoonWindow is how close to its deadline a waiting gate is flagged on the Needs attention list.
const dueSoonWindow = time.Hour

// deadline is when the item's gate expires, zero for an item without a deadline.
func (i AttentionItem) deadline() time.Time {
	if i.Gate == nil {
		return time.Time{}
	}
	return parseTime(i.Gate.ExpiresAt)
}

// Notify states, in the order a delivery goes through them.
const (
	NotifyNone     = "none"       // no notifier recorded the event
	NotifyObserved = "observed"   // seen, not yet delivered
	NotifyTried    = "delivering" // delivered at least once, not acknowledged
	NotifyFailing  = "failing"    // delivery has failed repeatedly
	NotifyAcked    = "acked"      // the sink acknowledged
	NotifyCleared  = "cleared"    // the notifier stopped: resolved, decided or ended
)

// NotifyStatus is what the notify trail says about an item.
type NotifyStatus struct {
	State string `json:"state"`
	Text  string `json:"text"`
	// Deadline is when the notification is overdue if unacknowledged; Overdue says it is.
	Deadline time.Time `json:"deadline,omitzero"`
	Overdue  bool      `json:"overdue,omitempty"`
}

// Class styles the status: good once acknowledged; bad when delivery fails, an event is overdue,
// nothing was recorded, or the notifier cleared an item the journal still shows open; else warn.
func (n NotifyStatus) Class() string {
	switch {
	case n.State == NotifyAcked:
		return "good"
	case n.State == NotifyFailing || n.State == NotifyNone || n.State == NotifyCleared || n.Overdue:
		return "bad"
	}
	return "warn"
}

// refreshSeconds is how often a live page reloads itself.
const refreshSeconds = 5

// Refresh is the seconds after which the page reloads itself, 0 for a page that does not.
func (l AttentionList) Refresh() int {
	if l.Live {
		return refreshSeconds
	}
	return 0
}

// Verdict is the page's headline.
func (l AttentionList) Verdict() Verdict {
	n := len(l.Items)
	var d Verdict
	if n == 0 {
		d = l.Coverage.nothingFound(false, "NOTHING NEEDS A PERSON", "No run is waiting for approval, halted with an unknown outcome or aborted, and no recovery keeps failing.")
	} else {
		cls, icon := "wait", "wait"
		if len(itemsByKind(l.Items, AttentionApproval)) < n {
			cls, icon = "bad", "halt"
		}
		d = Verdict{Class: cls, Status: StatusAttention, Word: strconv.Itoa(n) + " NEED" + map[bool]string{true: "S", false: ""}[n == 1] + " A PERSON", Icon: icon, Line: "Gates with a deadline first, then newest first. Each item says what to do."}
		if g := l.Gap(); g != "" {
			d.Details = append(d.Details, "Not everything was read: "+g)
		}
	}
	d.Stats = []Stat{{Value: strconv.Itoa(n), Label: "to do", Sub: "by deadline, then newest", Class: map[bool]string{true: "bad", false: ""}[n > 0]}}
	for _, k := range []struct{ kind, label string }{{AttentionApproval, "waiting for approval"}, {AttentionHalt, "halted"}, {AttentionAborted, "aborted"}, {AttentionRecovery, "recovery failing"}} {
		if c := len(itemsByKind(l.Items, k.kind)); c > 0 || k.kind != AttentionRecovery || l.Trail {
			d.Stats = append(d.Stats, Stat{Value: strconv.Itoa(c), Label: k.label})
		}
	}
	return d
}

func itemsByKind(items []AttentionItem, kind string) []AttentionItem {
	return slices.DeleteFunc(slices.Clone(items), func(i AttentionItem) bool { return i.Kind != kind })
}

// noApproveNote is the note a page with approvals but no ApproveCommand carries.
const noApproveNote = "No approve command: this viewer does not know the journal's name as bide approve takes it. " +
	"Run bide approve -list against the journal."

// attention reads the unfinished runs and lists those that need a person.
func (o Options) attention(ctx context.Context) AttentionList {
	now := o.now()
	l := AttentionList{Now: now.UTC(), Items: []AttentionItem{}}
	l.Coverage = Coverage{Listable: canList(o.Source), Trail: o.Events != nil}
	_, l.Live = o.Source.(Follower)
	var ids []string
	if l.Listable {
		var err error
		if u, ok := o.Source.(UnfinishedLister); ok {
			ids, err = u.Unfinished(ctx)
		} else {
			ids, err = o.Source.Runs(ctx)
		}
		if err != nil {
			l.Unread++
			l.Err = "listing failed (" + errClass(err) + ")"
		}
	}
	slices.Sort(ids)
	var tr *trailReader
	if o.Events != nil {
		tr = newTrailReader(ctx, o.Events)
	}
	for _, id := range ids {
		recs, err := o.Source.History(ctx, id)
		switch {
		case err != nil:
			l.Unread++
			l.Err = cmp.Or(l.Err, id+" could not be read ("+errClass(err)+")")
			continue
		case len(recs) == 0:
			l.Empty++
			continue
		}
		l.Runs++
		v := Build(ctx, id, recs, nil)
		items := o.runItems(v, recs)
		// A trail is read for a run that has not ended or has an item: a finished run with nothing to
		// do has nothing in its trail that a person must act on.
		if tr != nil && (!ended(v.State) || len(items) > 0) {
			// An event of the run is filed under the trail of the root that drove it: the root its ID
			// names (agent.SubRunID), or the run itself when it drove itself (a flow gate files its pause
			// under its own run, whatever its ID looks like). Both are read.
			unread := false
			for _, root := range slices.Compact([]string{id, treeRoot(id)}) {
				unread = unread || tr.of(root).err != nil
			}
			if unread {
				l.TrailUnread++
			} else if st := tr.of(id).st; st.FailingSince != nil && !ended(v.State) {
				// A recovery failure is recorded under the run recovery was run for: the run's own trail.
				items = append(items, AttentionItem{Kind: AttentionRecovery, KindText: "recovery failing", RunID: id, Since: st.FailingSince.At,
					Reason: st.FailingSince.Reason, Summary: "Recovery has kept failing for this run" + reasonSuffix(st.FailingSince.Reason) + "."})
			}
		}
		l.Items = append(l.Items, items...)
	}
	if tr != nil {
		if len(ids) > 0 && !tr.probe(ids[0]) {
			l.TrailUnread++
		}
		var states []*agent.NotifyState
		for _, root := range slices.Sorted(maps.Keys(tr.folds)) {
			if f := tr.folds[root]; f.err == nil {
				states = append(states, &f.st)
			}
		}
		// An event is filed under the trail of the root run that drove the one it is about, so an item
		// is matched against every trail read (its keys name the run, so they cannot collide).
		for i := range l.Items {
			l.Items[i].Notify = notifyStatus(states, l.Items[i], l.Items[i].RunID)
		}
	}
	for i := range l.Items {
		if !l.Items[i].Since.IsZero() {
			l.Items[i].Waiting = durText(now.Sub(l.Items[i].Since))
		}
	}
	if l.TrailUnread > 0 {
		l.Notes = append(l.Notes, "The notify trail could not be read for "+count(l.TrailUnread, "run")+" ("+errClassNote+"); those items say nothing about notification.")
	}
	if l.Empty > 0 {
		l.Notes = append(l.Notes, count(l.Empty, "listed run")+" hold no records and are not shown.")
	}
	if o.ApproveCommand == nil && len(itemsByKind(l.Items, AttentionApproval)) > 0 {
		l.Notes = append(l.Notes, noApproveNote)
	}
	// Gates with a deadline come first, the nearest deadline first (a passed one before the rest);
	// everything else is newest first.
	slices.SortStableFunc(l.Items, func(a, b AttentionItem) int {
		switch da, db := a.deadline(), b.deadline(); {
		case !da.IsZero() && !db.IsZero():
			if c := da.Compare(db); c != 0 {
				return c
			}
		case !da.IsZero():
			return -1
		case !db.IsZero():
			return 1
		}
		if c := b.Since.Compare(a.Since); c != 0 {
			return c
		}
		return cmp.Or(strings.Compare(a.RunID, b.RunID), strings.Compare(a.Kind, b.Kind), strings.Compare(a.Call, b.Call))
	})
	l.Status = l.Verdict().Status
	return l
}

// errClassNote is what the page says of a notify trail it could not read: the store's own text is
// not shown.
const errClassNote = "the notification store returned an error"

// treeRoot is the run whose notification trail holds the events of runID: the run itself for a
// root run, and for a sub-agent's run (agent.SubRunID, "<parent>><encoded call>") the root it hangs
// off. A session's journal and turn runs ("<id>>@...") are driven by the session, which is their root,
// and a sub-run inside a turn hangs off that turn. The journal records no root (run:start does not),
// so this is agent's tree root rule read from the ID alone; a test holds it to agent.SubRunID.
func treeRoot(runID string) string {
	id, rest, ok := strings.Cut(runID, ">")
	if !ok || !strings.HasPrefix(rest, "@") {
		return id
	}
	seg, _, _ := strings.Cut(rest, ">")
	return id + ">" + seg
}

func reasonSuffix(r string) string {
	if r == "" {
		return ""
	}
	return " (" + strings.ReplaceAll(r, "_", " ") + ")"
}

// runItems are the items the run's own journal gives: its waiting gates, its halts and its abort.
func (o Options) runItems(v *View, recs []agent.Record) []AttentionItem {
	var out []AttentionItem
	started := runStarted(recs)
	// A run with its end marker has no approval to give, whatever its gates still say (a run
	// cancelled at a gate keeps the gate's pause record).
	over := ended(v.State)
	now := o.now()
	gates := slices.Clone(v.Gates)
	markDeadlines(gates, now)
	for _, g := range gates {
		if !g.Waiting || over {
			continue
		}
		it := AttentionItem{Kind: AttentionApproval, KindText: "waiting for approval", RunID: v.RunID, Call: g.ToolUseID, Tool: g.ToolName,
			Since: cmp.Or(parseTime(g.AskedAt), started), Gate: &AttentionGate{Single: g.Single, Need: g.Need, Of: len(g.Approvers), Approvers: g.Approvers,
				ApprovedBy: g.ApprovedBy, DeniedBy: g.DeniedBy, Pending: g.Pending, Reapproval: g.Reapproval, Status: g.Status,
				ExpiresAt: g.ExpiresAt, Due: g.Due, Overdue: g.Overdue}}
		it.Summary = "Approval for " + cmp.Or(g.ToolName, g.ToolUseID) + " is waiting."
		if at := parseTime(g.ExpiresAt); !at.IsZero() {
			it.Gate.DueSoon = at.Sub(now) <= dueSoonWindow
		}
		if g.Overdue {
			// The gate refuses decisions, so there is no command to write.
			it.Summary = "The deadline of the approval for " + cmp.Or(g.ToolName, g.ToolUseID) + " passed with no decision; the run records the expiry when it is next driven."
			out = append(out, it)
			continue
		}
		if o.ApproveCommand != nil {
			cmd, err := o.ApproveCommand(v.RunID, g.ToolUseID, g.Single)
			switch {
			case errors.Is(err, approveline.ErrUnsafe):
				it.NoCommand = "no command (unsafe id)"
			case err != nil:
				it.NoCommand = "no command"
			default:
				it.Command = cmd
			}
		}
		out = append(out, it)
	}
	// A halt stays on the list after the run ended: a cancel or an abort does not say whether the
	// operation it attempted took effect.
	for _, h := range v.Halts {
		out = append(out, AttentionItem{Kind: AttentionHalt, KindText: "halted: outcome unknown", RunID: v.RunID, Call: h.ID, Tool: h.ToolName, Since: cmp.Or(h.AttemptedAt, started),
			Halt: &h, Summary: "The " + h.Op + " " + h.ID + " was attempted and has no result: its outcome is unknown."})
	}
	if v.State == string(agent.RunAborted) {
		at := cmp.Or(abortedAt(recs), started)
		out = append(out, AttentionItem{Kind: AttentionAborted, KindText: "aborted", RunID: v.RunID, Since: at, Reason: v.Terminal,
			Summary: "The run aborted" + map[bool]string{true: ": " + v.Terminal, false: ""}[v.Terminal != ""] + "."})
	}
	return out
}

// runStarted is when the run's first drive started, zero when it records none.
func runStarted(recs []agent.Record) time.Time {
	for _, r := range recs {
		if r.Kind == agent.StepValue && r.Name == "run:start" {
			var st agent.RunStart
			if json.Unmarshal(r.Result, &st) == nil {
				t, _ := st.Started()
				return t
			}
		}
	}
	return time.Time{}
}

// abortedAt is when the run:aborted record says the run aborted, zero when it says none (an
// earlier version's records the reason alone).
func abortedAt(recs []agent.Record) time.Time {
	for _, r := range recs {
		if r.Name == "run:aborted" {
			var v struct {
				At string `json:"aborted_at"`
			}
			if json.Unmarshal(r.Result, &v) == nil {
				return parseTime(v.At)
			}
		}
	}
	return time.Time{}
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// notifyStatus says what the folded trails record for the item. Its events are those whose key is the
// item's: an approval gate's reminders are one gate, and a halt is its attempt (the marker's time is in
// the key). An event observed before the item began is of an earlier round of the same call, not this
// one. A trail that records the event cleared while the journal still shows it open is called out.
func notifyStatus(states []*agent.NotifyState, it AttentionItem, runID string) *NotifyStatus {
	var prefix string
	exact := ""
	switch it.Kind {
	case AttentionApproval:
		prefix = strings.TrimSuffix(agent.ApprovalKey(runID, it.Call, 0), "0")
	case AttentionHalt:
		if t := it.Halt.AttemptedAt; !t.IsZero() {
			exact = agent.HaltedKey(runID, agent.OpKind(it.Halt.Op), it.Call, t.UnixMilli())
		} else {
			prefix = strings.TrimSuffix(agent.HaltedKey(runID, agent.OpKind(it.Halt.Op), it.Call, 0), "0")
		}
	case AttentionAborted:
		exact = agent.AbortedKey(runID)
	case AttentionRecovery:
		prefix = strings.TrimSuffix(agent.RecoveryKey(runID, 0), "0")
	}
	seen := map[string]bool{}
	var evs []*agent.NotifyEvent
	for _, st := range states {
		for _, k := range st.Keys {
			if seen[k] || (exact != "" && k != exact) || (exact == "" && !strings.HasPrefix(k, prefix)) {
				continue
			}
			e := st.Events[k]
			if !it.Since.IsZero() && it.Kind == AttentionApproval && e.ObservedAt.Before(it.Since) {
				continue
			}
			seen[k] = true
			evs = append(evs, e)
		}
	}
	if it.Kind == AttentionHalt && exact == "" && len(evs) > 1 {
		// An untimed marker's key carries a generation, not its time: only the newest event can be of
		// the attempt that is open now.
		slices.SortStableFunc(evs, func(a, b *agent.NotifyEvent) int { return a.ObservedAt.Compare(b.ObservedAt) })
		evs = evs[len(evs)-1:]
	}
	if len(evs) == 0 {
		if it.Kind == AttentionRecovery {
			return &NotifyStatus{State: NotifyObserved, Text: "recovery_failing is told once the failure has lasted past the configured time"}
		}
		return &NotifyStatus{State: NotifyNone, Text: "no notification recorded: no notifier has seen it"}
	}
	n := &NotifyStatus{Deadline: evs[0].DeadlineAt}
	for _, e := range evs {
		n.Overdue = n.Overdue || e.Overdue != nil
	}
	last := evs[len(evs)-1]
	switch {
	case slices.ContainsFunc(evs, func(e *agent.NotifyEvent) bool { return e.Acked != nil }):
		var a *agent.NotifyRecord
		for _, e := range evs {
			if e.Acked != nil && (a == nil || e.Acked.At.After(a.At)) {
				a = e.Acked
			}
		}
		n.State, n.Text = NotifyAcked, "notified, acknowledged "+a.At.UTC().Format("2006-01-02 15:04:05 UTC")+statusSuffix(a.Status)
	case last.Cleared != nil:
		n.State, n.Text = NotifyCleared, "the notifier cleared this ("+cmp.Or(last.Cleared.Reason, "no reason recorded")+"), but the journal still shows it needing a person"
	case last.Failing != nil:
		n.State, n.Text = NotifyFailing, "delivery failing ("+count(last.Failing.Failures, "failure")+"), not acknowledged"
	case last.Rounds > 0:
		n.State, n.Text = NotifyTried, "delivery attempted ("+count(last.Rounds, "round")+"), not acknowledged"
	default:
		n.State, n.Text = NotifyObserved, "seen by a notifier, not delivered yet"
	}
	if n.Overdue {
		n.Text += "; overdue"
	}
	return n
}

func statusSuffix(status int) string {
	if status == 0 {
		return ""
	}
	return " (HTTP " + strconv.Itoa(status) + ")"
}

// serveAttention answers GET /attention (and /attention.json).
func (o Options) serveAttention(asJSON bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := o.cachedAttention(r.Context())
		if asJSON {
			writeJSON(w, l)
			return
		}
		render(w, "attention.html", l)
	}
}

// cachedAttention is attention, served again for cacheTTL (see ttlCache): a page that refreshes itself
// every few seconds, or several viewers, cost the store one read of its runs per ttl.
func (o Options) cachedAttention(ctx context.Context) AttentionList {
	if o.cache == nil {
		return o.attention(ctx)
	}
	return o.cache.get("attention", func() any {
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildTimeout)
		defer cancel()
		return o.attention(bctx)
	}).(AttentionList)
}

// cachedRunList is runList, served again for cacheTTL per filter.
func (o Options) cachedRunList(ctx context.Context, f ListFilter) RunList {
	if o.cache == nil {
		return o.runList(ctx, f)
	}
	return o.cache.get("runs"+query(f), func() any {
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildTimeout)
		defer cancel()
		return o.runList(bctx, f)
	}).(RunList)
}

// buildTimeout bounds one build of a cached page (the request that triggers it may go away, and the
// others waiting on it must not wait for a hung store): past it the reads fail as "timed out", which
// the page counts as unread and says so, never as clear.
var buildTimeout = 30 * time.Second

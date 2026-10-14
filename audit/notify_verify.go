package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// NotifyStatus is what a trail and the run journals together say about one event that needed a
// person. NotifyStatus.Problem says which statuses fail an audit.
type NotifyStatus string

const (
	// NotifyStatusAcked: the sink acknowledged the event within its deadline (and within the bound).
	NotifyStatusAcked NotifyStatus = "acked"
	// NotifyStatusAckedLate: acknowledged, but after the event's deadline or the bound. A problem.
	NotifyStatusAckedLate NotifyStatus = "acked_late"
	// NotifyStatusPending: observed, not yet acknowledged, and not yet past its deadline at Now.
	NotifyStatusPending NotifyStatus = "pending"
	// NotifyStatusFailing: like pending, and delivery has failed repeatedly (a failing record).
	NotifyStatusFailing NotifyStatus = "failing"
	// NotifyStatusOverdue: observed, not acknowledged, and past its deadline or the bound at Now.
	// A problem.
	NotifyStatusOverdue NotifyStatus = "overdue"
	// NotifyStatusClearedBeforeNotification: the event stopped being current before any delivery
	// was attempted, so no one was owed a notification.
	NotifyStatusClearedBeforeNotification NotifyStatus = "cleared_before_notification"
	// NotifyStatusClearedUnacknowledged: delivery was attempted, never acknowledged, and the event
	// then stopped being current. Whether the receiver was told is unknown, which is why the
	// all-clear is owed (see NotifyStatusNeverNotified for a missing one).
	NotifyStatusClearedUnacknowledged NotifyStatus = "cleared_unacknowledged"
	// NotifyStatusNeverNotified: the journal (or the trail itself) shows an event that needed a
	// person and the trail holds no observation of it. A problem.
	NotifyStatusNeverNotified NotifyStatus = "never_notified"
	// NotifyStatusNotYetDue: like never_notified, but the event is younger than the grace period at
	// Now, so a notifier has not yet been late with it.
	NotifyStatusNotYetDue NotifyStatus = "not_yet_due"
	// NotifyStatusUnobservedResolved: the journal shows a gate that was decided, and the trail
	// holds no observation of it. The person who decided it knew of it, so this is not a failure,
	// and it is reported so a gate that was never notified is not invisible.
	NotifyStatusUnobservedResolved NotifyStatus = "unobserved_resolved"
	// NotifyStatusAllClearUntimed: an acknowledged event was resolved (the journal shows its gate
	// decided) and the trail holds no all-clear, but the journal holds no time for the resolution
	// (a tool's result, a run's end), so the all-clear cannot be judged late. A warning, like
	// unobserved_resolved.
	NotifyStatusAllClearUntimed NotifyStatus = "all_clear_untimed"
	// NotifyStatusUnattested: the event's observation, acknowledgement or clearing is in the records
	// after the newest signed head, which nothing attests: whoever can append to the stored trail
	// could have written it. A problem unless NotifyVerifyInput.AcceptTail is set.
	NotifyStatusUnattested NotifyStatus = "unattested"
	// NotifyStatusNoDelivery: the event was delivered to some receiver and not to an expected one
	// (NotifyVerifyInput.ExpectedReceivers). A problem.
	NotifyStatusNoDelivery NotifyStatus = "no_delivery"
)

// Problem reports whether a status fails an audit.
func (s NotifyStatus) Problem() bool {
	switch s {
	case NotifyStatusAckedLate, NotifyStatusOverdue, NotifyStatusNeverNotified, NotifyStatusUnattested, NotifyStatusNoDelivery:
		return true
	}
	return false
}

// Warning reports whether a status is shown as a warning: it does not fail an audit unless
// NotifyVerifyInput.Strict is set.
func (s NotifyStatus) Warning() bool {
	return s == NotifyStatusUnobservedResolved || s == NotifyStatusAllClearUntimed
}

// NotifyVerifyInput is what VerifyNotifications reads.
type NotifyVerifyInput struct {
	// Trails are the notification trails to verify. A trail the journals say is owed events and
	// that is not here is read as a trail with no records.
	Trails []NotifyTrailExport
	// Heads are signed tree heads over the trails (SignNotifyTrail), matched to a trail by RunID,
	// which is the trail's name. They are verified under Verifier.
	Heads []SignedTreeHead
	// Verifier verifies Heads. It is required if there are any, or unless Unanchored.
	Verifier Verifier
	// Unanchored allows a trail with no signed head. The trail is then read but nothing attests that
	// it is complete or unaltered, and the report says so (NotifyTrailReport.Anchored is false).
	Unanchored bool
	// Journals are run journals the events are cross-checked against: every gate, halt and abort
	// they show is expected in the trail of its root run.
	Journals []JournalExport

	// AcceptTail accepts the verdicts that rest on records after the newest signed head (a live
	// trail between heads). Without it such an event is NotifyStatusUnattested and fails the audit.
	AcceptTail bool
	// Routing, if set, is the notifier's routing (agent.RoutingOf, the same options the notifier was
	// built with): each event that needs a person must have been delivered to the receivers it
	// routes to (NotifyRouting.Receivers, the method the notifier routes by), and each escalation
	// step that was due to its chain's receiver. A missing delivery is NotifyStatusNoDelivery. An
	// approval routed by its approvers needs its journal to be supplied, to know them; without it
	// the approval is not checked. Without Routing, per-receiver completeness is not checked: the
	// trail alone cannot know the routing.
	Routing *agent.NotifyRouting

	// Strict makes a warning status (NotifyStatus.Warning) a problem, so an audit fails on it.
	Strict bool

	// Now is the time pending and overdue events are judged at; time.Now if zero.
	Now time.Time
	// MaxAck, if positive, is the longest an event may wait, from the time the run entered the state
	// (the observation time if the record has none) to its acknowledgement; an event past it is late
	// or overdue even within its recorded deadline. The recorded deadline always applies.
	MaxAck time.Duration
	// Grace is how long after an event began a missing observation is not yet a failure. It is the
	// notifier's detection window: the default, 5 minutes, matches agent.WithNotifyHaltAge and the
	// default deadline.
	Grace time.Duration
	// RecoveryFailingAfter is how long a recovery failure episode may be open before a
	// recovery_failing event is owed. The default, 15 minutes, is the notifier's.
	RecoveryFailingAfter time.Duration
	// MaxClockSkew is how far a record may be dated before the one that precedes it (the writers'
	// clocks differ) before the trail is out of order. The default is DefaultClockSkew.
	MaxClockSkew time.Duration
}

// NotifyTrailReport is what was established about one trail.
type NotifyTrailReport struct {
	Trail   string `json:"trail"`
	Records int    `json:"records"`
	// Anchored is true when a signed head over the trail verified, and the trail's first HeadSize
	// records are exactly the leaves it commits to.
	Anchored bool `json:"anchored"`
	HeadSize int  `json:"head_size,omitempty"`
	// Tail counts the records after the head: read, but not attested by it.
	Tail int `json:"tail,omitempty"`
	// TailFirstAt and TailLastAt are the times of the first and last record of the tail.
	TailFirstAt time.Time `json:"tail_first_at,omitzero"`
	TailLastAt  time.Time `json:"tail_last_at,omitzero"`
	HeadTime    time.Time `json:"head_time,omitzero"`
	// Violations are the ways the trail is malformed, out of order, or does not match its head.
	Violations []string `json:"violations,omitempty"`
}

// NotifyEventVerdict is the verdict on one event.
type NotifyEventVerdict struct {
	Trail  string       `json:"trail"`
	Key    string       `json:"key"`
	Kind   string       `json:"kind"`
	RunID  string       `json:"run_id,omitempty"`
	Status NotifyStatus `json:"status"`
	// Why says, in a sentence, what makes the status a problem or what the status rests on.
	Why string `json:"why,omitempty"`
	// InJournal is true when a journal supplied shows the event.
	InJournal bool `json:"in_journal,omitempty"`

	EventAt     time.Time `json:"event_at,omitzero"`
	ObservedAt  time.Time `json:"observed_at,omitzero"`
	DeadlineAt  time.Time `json:"deadline_at,omitzero"`
	FirstSentAt time.Time `json:"first_sent_at,omitzero"`
	AckedAt     time.Time `json:"acked_at,omitzero"`
	ClearedAt   time.Time `json:"cleared_at,omitzero"`
	Attempts    int       `json:"attempts,omitempty"`
	Rounds      int       `json:"rounds,omitempty"`
	// Detection is ObservedAt - EventAt: how long the event was unseen. Delivery is AckedAt -
	// ObservedAt. Total is AckedAt - EventAt.
	Detection time.Duration `json:"detection_ns,omitempty"`
	Delivery  time.Duration `json:"delivery_ns,omitempty"`
	Total     time.Duration `json:"total_ns,omitempty"`
	// Auth is how the acknowledged delivery was authenticated.
	Auth *agent.NotifyAuth `json:"auth,omitempty"`
	// Receiver is the receiver the event was to ("default" when the trail names none): the verdicts
	// of one receiver are the answer to whether every event reached it. Level is the step of an
	// escalation chain for a NotificationEscalated event, 0 otherwise.
	Receiver string `json:"receiver,omitempty"`
	Level    int    `json:"level,omitempty"`
}

// NotifyReport is the result of VerifyNotifications.
type NotifyReport struct {
	Trails   []NotifyTrailReport  `json:"trails"`
	Events   []NotifyEventVerdict `json:"events"`
	Problems []string             `json:"problems,omitempty"`
}

// Counts returns how many events have each status.
func (r NotifyReport) Counts() map[NotifyStatus]int {
	m := map[NotifyStatus]int{}
	for _, e := range r.Events {
		m[e.Status]++
	}
	return m
}

// CountsByReceiver returns, for each receiver, how many of its events have each status.
func (r NotifyReport) CountsByReceiver() map[string]map[NotifyStatus]int {
	m := map[string]map[NotifyStatus]int{}
	for _, e := range r.Events {
		name := e.Receiver
		if name == "" {
			name = agent.DefaultReceiver
		}
		if m[name] == nil {
			m[name] = map[NotifyStatus]int{}
		}
		m[name][e.Status]++
	}
	return m
}

var notifyKinds = []agent.NotificationKind{
	agent.NotificationOutcomeUnknown, agent.NotificationApprovalPending, agent.NotificationRunAborted,
	agent.NotificationRecoveryFailing, agent.NotificationOverdue, agent.NotificationResolved,
	agent.NotificationEscalated,
}

var notifyRecordKinds = []agent.NotifyRecordKind{
	agent.NotifyObserved, agent.NotifyClaimed, agent.NotifyAcked, agent.NotifyFailing, agent.NotifyOverdue,
	agent.NotifyCleared, agent.NotifyFailingSince, agent.NotifyRecovered, agent.NotifyWatch, agent.NotifyUnwatch,
	agent.NotifyCheckpoint, agent.NotifySeal, agent.NotifySegment, agent.NotifyBackfilled,
}

var notifyClearReasons = []string{"resolved", "decided", "ended", "recovered"}

// decodeNotifyLeafStrict reads a notification record from a stored event leaf, and refuses what
// the engine's NotifyTrail.Load keeps: a field it does not know, a duplicate key, a record kind
// this version does not know, a leaf whose JSON is not the canonical encoding of the record it
// holds. A verifier that skipped a record it could not read would pass a trail it did not check.
func decodeNotifyLeafStrict(leaf []byte) (agent.NotifyRecord, error) {
	body, ok := bytes.CutPrefix(leaf, []byte(eventLeafTag))
	if !ok {
		return agent.NotifyRecord{}, malformed(errors.New("not an event leaf"))
	}
	var el eventLeaf
	if err := UnmarshalStrict(body, &el); err != nil {
		return agent.NotifyRecord{}, err
	}
	if len(el.Salt) != agent.SaltSize {
		return agent.NotifyRecord{}, malformed(fmt.Errorf("%d-byte salt, want %d", len(el.Salt), agent.SaltSize))
	}
	kind, isNotify := strings.CutPrefix(el.Kind, "notify_")
	if !isNotify {
		return agent.NotifyRecord{}, malformed(fmt.Errorf("leaf kind %q is not a notify record", el.Kind))
	}
	var rec agent.NotifyRecord
	if err := UnmarshalStrict(el.Event, &rec); err != nil {
		return agent.NotifyRecord{}, err
	}
	if string(rec.Kind) != kind {
		return agent.NotifyRecord{}, malformed(fmt.Errorf("leaf kind %q holds a %q record", el.Kind, rec.Kind))
	}
	if !slices.Contains(notifyRecordKinds, rec.Kind) {
		return agent.NotifyRecord{}, malformed(fmt.Errorf("record kind %q is not one this version reads", rec.Kind))
	}
	if again, err := json.Marshal(rec); err != nil || !bytes.Equal(again, el.Event) {
		return agent.NotifyRecord{}, malformed(errors.New("the record is not in canonical form"))
	}
	return rec, nil
}

// VerifyNotifications reads notification trails strictly and says, for every event that needed a
// person, whether it was notified and acknowledged, and when. It checks:
//
//   - every leaf is a notification record of a kind and shape this version reads (else the error
//     wraps ErrMalformed and no verdict is given);
//   - each trail is a valid history: an event is observed once and before anything else about it,
//     nothing follows its acknowledgement or clearing, an acknowledgement follows a claim and
//     carries a time, an attempt count and a 2xx status, an overdue record follows the deadline
//     and states the latency the times imply, an overdue notice follows the overdue record it
//     reports, an all-clear follows the clearing or acknowledgement it reports, records are dated
//     in order within MaxClockSkew, and the recovery failure episodes open and close in turn;
//   - with a signed head, the head verifies, is a head of this trail, and commits to its first
//     HeadSize leaves, so a record deleted, reordered or changed before the head is found;
//   - against the journals, every approval gate, unknown-outcome halt and aborted run they show is
//     observed in the trail of its root run (or is within Grace of beginning); a recovery failure
//     episode open longer than RecoveryFailingAfter, an overdue record, and a clearing after an
//     attempted delivery each owe a notification that the trail must hold.
//
// The error is nil only if the trails are well formed and anchored (or Unanchored), and no event
// has a status that is a Problem. The report is returned with every error but a malformed leaf,
// so a failing audit can say what failed. An error wraps ErrNotVerified, ErrMalformed or
// agent.ErrConfig (for input that cannot be used together: two exports of one trail, heads and
// no Verifier).
//
// What it does not establish: that anyone read a notification (an acknowledgement is the sink's
// 2xx); the times are those of the notifying processes' clocks, bounded above only by the head's
// timestamp; an event that left no trace in the supplied journals (a halt a later result resolved)
// and no record in the trail cannot be found; and records after the newest head are read but not
// attested.
func VerifyNotifications(in NotifyVerifyInput) (NotifyReport, error) {
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	if in.Grace <= 0 {
		in.Grace = 5 * time.Minute
	}
	if in.RecoveryFailingAfter <= 0 {
		in.RecoveryFailingAfter = 15 * time.Minute
	}
	if in.MaxClockSkew <= 0 {
		in.MaxClockSkew = DefaultClockSkew
	}
	if in.Verifier == nil && (len(in.Heads) > 0 || !in.Unanchored) {
		return NotifyReport{}, fmt.Errorf("audit: verify notifications: signed heads need a Verifier (or set Unanchored): %w", agent.ErrConfig)
	}

	trails := map[string]*NotifyTrailExport{}
	var names []string
	for i := range in.Trails {
		t := &in.Trails[i]
		if err := formatOf(*t, t.Format); err != nil {
			return NotifyReport{}, err
		}
		if _, dup := trails[t.Trail]; dup {
			return NotifyReport{}, fmt.Errorf("audit: verify notifications: two exports of trail %q: %w", t.Trail, agent.ErrConfig)
		}
		trails[t.Trail] = t
		names = append(names, t.Trail)
	}
	heads := map[string]SignedTreeHead{}
	for _, h := range in.Heads {
		if _, dup := heads[h.RunID]; dup {
			return NotifyReport{}, fmt.Errorf("audit: verify notifications: two signed heads for trail %q: %w", h.RunID, agent.ErrConfig)
		}
		heads[h.RunID] = h
	}

	// The events the journals show, by the trail that owes them.
	owed := map[string][]agent.JournalNotifyEvent{}
	for _, jx := range in.Journals {
		recs, err := jx.Journal()
		if err != nil {
			return NotifyReport{}, err
		}
		for _, ev := range agent.JournalNotifyEvents(jx.RunID, recs) {
			name := agent.NotifyTrailName(ev.RunID)
			owed[name] = append(owed[name], ev)
		}
	}
	for name := range owed {
		if trails[name] == nil {
			names = append(names, name)
		}
	}
	for name := range heads {
		if trails[name] == nil && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	var rep NotifyReport
	var decodeErrs []error
	all := map[string][]agent.NotifyRecord{} // the trails that decoded
	for _, name := range names {
		var leaves [][]byte
		if t := trails[name]; t != nil {
			leaves = t.Leaves
		}
		recs := make([]agent.NotifyRecord, len(leaves))
		ok := true
		for i, leaf := range leaves {
			r, err := decodeNotifyLeafStrict(leaf)
			if err != nil {
				decodeErrs = append(decodeErrs, fmt.Errorf("audit: notify trail %s record %d: %w", name, i, err))
				ok = false
				break
			}
			recs[i] = r
		}
		if ok {
			all[name] = recs
		}
	}
	openV := checkOpenGroup(all, in.MaxClockSkew)
	for _, name := range names {
		var leaves [][]byte
		if t := trails[name]; t != nil {
			leaves = t.Leaves
		}
		tr := NotifyTrailReport{Trail: name, Records: len(leaves)}
		in.anchor(&tr, leaves, heads, trails[name] != nil)
		if recs, ok := all[name]; ok {
			switch openTrailKind(name) {
			case openNone:
				tr.Violations = append(tr.Violations, checkNotifyTrail(name, recs, in.MaxClockSkew)...)
				attested := len(recs)
				if tr.Anchored {
					attested = tr.HeadSize
					if tr.Tail > 0 {
						tr.TailFirstAt, tr.TailLastAt = recs[attested].At, recs[len(recs)-1].At
					}
				}
				rep.Events = append(rep.Events, in.verdicts(name, recs, owed[name], attested)...)
			default:
				tr.Violations = append(tr.Violations, openV[name]...)
			}
		}
		rep.Trails = append(rep.Trails, tr)
	}
	if len(decodeErrs) > 0 {
		return rep, errors.Join(decodeErrs...)
	}
	for _, tr := range rep.Trails {
		for _, v := range tr.Violations {
			rep.Problems = append(rep.Problems, fmt.Sprintf("trail %s: %s", tr.Trail, v))
		}
	}
	for _, e := range rep.Events {
		if e.Status.Problem() || in.Strict && e.Status.Warning() {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s %s: %s: %s", e.Kind, e.Key, e.Status, e.Why))
		}
	}
	if len(rep.Problems) > 0 {
		return rep, notVerified("audit: notifications: %d problem(s), first: %s", len(rep.Problems), rep.Problems[0])
	}
	return rep, nil
}

// anchor checks the trail against its signed head and records the outcome in tr.
func (in NotifyVerifyInput) anchor(tr *NotifyTrailReport, leaves [][]byte, heads map[string]SignedTreeHead, supplied bool) {
	h, ok := heads[tr.Trail]
	if !ok {
		if supplied && !in.Unanchored {
			tr.Violations = append(tr.Violations, "no signed head: nothing attests that the trail is complete or unaltered (give one, or allow an unanchored trail)")
		}
		return
	}
	bad := func(format string, args ...any) { tr.Violations = append(tr.Violations, fmt.Sprintf(format, args...)) }
	if err := h.Verify(in.Verifier); err != nil {
		bad("signed head does not verify: %v", err)
		return
	}
	if h.Kind != TreeEvents {
		bad("signed head is of the %q tree, not an event trail", h.Kind)
		return
	}
	if err := CheckTimestamp(h.TreeHead, in.Now, in.MaxClockSkew); err != nil {
		bad("%v", err)
		return
	}
	tr.HeadSize, tr.HeadTime = h.Size, time.Unix(0, h.TimestampNanos).UTC()
	if h.Size > len(leaves) {
		bad("trail has %d record(s) and its signed head commits to %d: records were deleted", len(leaves), h.Size)
		return
	}
	if !bytes.Equal(merkleRoot(leaves[:h.Size]), h.Root) {
		bad("the first %d record(s) are not the ones the signed head commits to: a record was changed, reordered or replaced", h.Size)
		return
	}
	tr.Anchored, tr.Tail = true, len(leaves)-h.Size
}

// checkNotifyTrail returns the ways recs, the records of trail, are not a history a notifier
// writes. It is stricter than agent.FoldNotify, which ignores what does not apply.
func checkNotifyTrail(trail string, recs []agent.NotifyRecord, skew time.Duration) []string {
	var out []string
	bad := func(i int, format string, args ...any) {
		out = append(out, fmt.Sprintf("record %d (%s): %s", i, recs[i].Kind, fmt.Sprintf(format, args...)))
	}
	root := strings.TrimPrefix(trail, "notify:")
	events := map[string]*trailEvent{}
	episodeOpen, episodes := false, 0
	for i, r := range recs {
		if r.At.IsZero() {
			bad(i, "has no time")
		}
		if i > 0 && r.At.Before(recs[i-1].At.Add(-skew)) {
			bad(i, "is dated %s, more than %s before record %d (%s): the trail is out of order, or a clock is wrong", r.At.Format(time.RFC3339Nano), skew, i-1, recs[i-1].At.Format(time.RFC3339Nano))
		}
		if root != "@digest" && r.RunID != root && r.Kind != agent.NotifyFailingSince && r.Kind != agent.NotifyRecovered {
			bad(i, "names run %q in the trail of run %q", r.RunID, root)
		}
		switch r.Kind {
		case agent.NotifyWatch, agent.NotifyUnwatch, agent.NotifyCheckpoint, agent.NotifySeal, agent.NotifySegment, agent.NotifyBackfilled:
			bad(i, "belongs to the open trail (%s), not a run's", agent.NotifyOpenTrail)
			continue
		case agent.NotifyFailingSince:
			if r.Key != "" {
				bad(i, "has an event key")
			}
			if episodeOpen {
				bad(i, "opens a recovery failure episode while one is open")
			}
			episodeOpen = true
			continue
		case agent.NotifyRecovered:
			if r.Key != "" {
				bad(i, "has an event key")
			}
			if !episodeOpen {
				bad(i, "closes a recovery failure episode that is not open")
			}
			episodeOpen = false
			episodes++
			continue
		}
		if r.Key == "" {
			bad(i, "has no event key")
			continue
		}
		if r.Kind != agent.NotifyObserved && (r.Receiver != "" || r.Level != 0) {
			bad(i, "names a receiver or a level; only an observation does")
		}
		if r.Kind == agent.NotifyObserved {
			if events[r.Key] != nil {
				bad(i, "observes event %q a second time", r.Key)
				continue
			}
			e := &trailEvent{kind: agent.NotificationKind(r.EventKind), observed: r}
			events[r.Key] = e
			if r.Receiver != "" && !agent.ValidReceiver(r.Receiver) {
				bad(i, "receiver %q is not a name a notifier records", r.Receiver)
			}
			if r.Level != 0 && e.kind != agent.NotificationEscalated {
				bad(i, "a level on an event that is not an escalation step")
			}
			// A fan-out delivery names its receiver in its key, and the record agrees.
			key := r.Key
			if to, base, ok := agent.SplitToKey(r.Key); ok {
				key = base
				if to != r.Receiver {
					bad(i, "key %q is a delivery to receiver %q and the record names %q", r.Key, to, r.Receiver)
				}
				if e.kind != agent.NotificationApprovalPending {
					bad(i, "only an approval is delivered to several receivers")
				}
			}
			if !slices.Contains(notifyKinds, e.kind) {
				bad(i, "event kind %q is not one this version reads", r.EventKind)
			}
			if r.EventAt.IsZero() {
				bad(i, "has no event time")
			}
			switch {
			case e.kind == agent.NotificationResolved:
				if !r.DeadlineAt.IsZero() {
					bad(i, "an all-clear has a deadline")
				}
				if !slices.Contains(notifyClearReasons, r.Reason) {
					bad(i, "an all-clear's reason %q is not one this version reads", r.Reason)
				}
			case !r.DeadlineAt.After(r.At):
				bad(i, "deadline %s is not after the observation", r.DeadlineAt.Format(time.RFC3339Nano))
			}
			checkOpFields(bad, i, r, e.kind)
			switch e.kind {
			case agent.NotificationOutcomeUnknown:
				requirePrefix(bad, i, key, "halted:")
			case agent.NotificationApprovalPending:
				requirePrefix(bad, i, key, "approval:")
			case agent.NotificationRunAborted:
				requirePrefix(bad, i, r.Key, "aborted:")
			case agent.NotificationRecoveryFailing:
				requirePrefix(bad, i, r.Key, "recovery:")
				if !episodeOpen {
					bad(i, "reports a recovery failure with no episode open")
				}
				if want := agent.RecoveryKey(r.RunID, episodes); r.Key != want {
					bad(i, "key %q is not the key of episode %d (%q)", r.Key, episodes, want)
				}
			case agent.NotificationOverdue:
				orig, ok := strings.CutPrefix(r.Key, "overdue:")
				if !ok {
					bad(i, "key %q is not an overdue notice's", r.Key)
				} else if p := events[orig]; p == nil || !p.overdue {
					bad(i, "reports event %q overdue before an overdue record says so", orig)
				} else if p.observed.Receiver != r.Receiver {
					bad(i, "an overdue notice goes to the receiver of its event (%q), not %q", p.observed.Receiver, r.Receiver)
				}
			case agent.NotificationResolved:
				orig, ok := strings.CutPrefix(r.Key, "resolved:")
				if !ok {
					bad(i, "key %q is not an all-clear's", r.Key)
				} else if p := events[orig]; p == nil || !(p.acked || p.cleared) {
					bad(i, "clears event %q before it was acknowledged or cleared", orig)
				} else if p.observed.Receiver != r.Receiver {
					bad(i, "an all-clear goes to the receiver of its event (%q), not %q", p.observed.Receiver, r.Receiver)
				}
			case agent.NotificationEscalated:
				checkEscalationStep(bad, i, r, events, skew)
			}
			continue
		}
		e := events[r.Key]
		if e == nil {
			bad(i, "is about event %q, which has not been observed", r.Key)
			continue
		}
		if e.acked || e.cleared {
			bad(i, "follows the event's %s", map[bool]string{true: "acknowledgement", false: "clearing"}[e.acked])
			continue
		}
		switch r.Kind {
		case agent.NotifyClaimed:
			if r.Holder == "" {
				bad(i, "has no holder")
			}
			if !r.Until.After(r.At) {
				bad(i, "claim ends at %s, not after it begins", r.Until.Format(time.RFC3339Nano))
			}
			e.claimed = true
		case agent.NotifyAcked:
			if e.kind != agent.NotificationOverdue && !e.claimed {
				bad(i, "acknowledges an event that was never claimed for delivery")
			}
			if r.FirstSentAt.IsZero() || r.FirstSentAt.After(r.At) {
				bad(i, "first send time %s is missing or after the acknowledgement", r.FirstSentAt.Format(time.RFC3339Nano))
			}
			if r.Attempts < 1 {
				bad(i, "records %d attempts", r.Attempts)
			}
			if r.Status != 0 && (r.Status < 200 || r.Status > 299) { // 0: a sink that is not HTTP
				bad(i, "records status %d, not a 2xx", r.Status)
			}
			if r.At.Before(e.observed.At.Add(-skew)) {
				bad(i, "is dated before the event's observation")
			}
			e.acked = true
		case agent.NotifyFailing:
			if r.Failures < 1 {
				bad(i, "records %d failures", r.Failures)
			}
			e.failing = true
		case agent.NotifyOverdue:
			if e.kind == agent.NotificationOverdue || e.kind == agent.NotificationResolved || e.kind == agent.NotificationEscalated {
				bad(i, "a notice of kind %s is never overdue", e.kind)
			}
			if e.overdue {
				bad(i, "marks the event overdue a second time")
			}
			if !r.At.After(e.observed.DeadlineAt) {
				bad(i, "is dated at or before the deadline %s", e.observed.DeadlineAt.Format(time.RFC3339Nano))
			}
			if r.Latency != r.At.Sub(e.observed.At) {
				bad(i, "latency %s is not the time from observation to this record (%s)", r.Latency, r.At.Sub(e.observed.At))
			}
			e.overdue, e.overdueRec = true, r
		case agent.NotifyCleared:
			if !slices.Contains(notifyClearReasons, r.Reason) {
				bad(i, "reason %q is not one this version reads", r.Reason)
			}
			e.cleared = true
		}
	}
	return out
}

// trailEvent is what checkNotifyTrail has seen of one event so far.
type trailEvent struct {
	kind                    agent.NotificationKind
	observed                agent.NotifyRecord
	claimed, acked, cleared bool
	failing, overdue        bool
	overdueRec              agent.NotifyRecord
}

// checkEscalationStep checks an observed escalation step: its key and level agree and are within the
// bound; the event it escalates was observed, is one that can be escalated, and has not been cleared
// (nor, unless it is an approval, acknowledged, itself or by an earlier step); and it comes after
// the deadline of the event (step 1) or of the step before it, which was observed. The clocks of two
// writers may differ by skew, so only an observation earlier than that is a violation.
func checkEscalationStep(bad func(int, string, ...any), i int, r agent.NotifyRecord, events map[string]*trailEvent, skew time.Duration) {
	level, orig, ok := agent.ParseEscalationKey(r.Key)
	if !ok {
		bad(i, "key %q is not an escalation step's", r.Key)
		return
	}
	if r.Level != level {
		bad(i, "level %d is not the key's %d", r.Level, level)
	}
	if level > agent.MaxEscalationSteps {
		bad(i, "level %d is beyond the bound of an escalation chain (%d)", level, agent.MaxEscalationSteps)
	}
	p := events[orig]
	if p == nil {
		bad(i, "escalates event %q, which has not been observed", orig)
		return
	}
	if !owesAllClear(string(p.kind)) {
		bad(i, "escalates event %q of kind %s, which is not escalated", orig, p.kind)
	}
	if p.cleared {
		bad(i, "escalates event %q, which was already cleared", orig)
	}
	if p.kind != agent.NotificationApprovalPending {
		satisfied := p.acked
		for l := 1; l < level; l++ {
			if s := events[agent.EscalationKey(orig, l)]; s != nil && s.acked {
				satisfied = true
			}
		}
		if satisfied {
			bad(i, "escalates event %q, which was already acknowledged (itself or an earlier step)", orig)
		}
	}
	due, what := p.observed.DeadlineAt, "the event's deadline"
	if level > 1 {
		prev := events[agent.EscalationKey(orig, level-1)]
		if prev == nil {
			bad(i, "is step %d of %q and step %d was not observed", level, orig, level-1)
			return
		}
		due, what = prev.observed.DeadlineAt, fmt.Sprintf("step %d's deadline", level-1)
	}
	if r.At.Add(skew).Before(due) {
		bad(i, "is dated %s, before %s (%s)", r.At.Format(time.RFC3339Nano), what, due.Format(time.RFC3339Nano))
	}
}

func requirePrefix(bad func(int, string, ...any), i int, key, prefix string) {
	if !strings.HasPrefix(key, prefix) {
		bad(i, "key %q does not begin %q", key, prefix)
	}
}

// verdicts folds recs and the journal events owed to the trail into a verdict for each event.
func (in NotifyVerifyInput) verdicts(trail string, recs []agent.NotifyRecord, journal []agent.JournalNotifyEvent, attested int) []NotifyEventVerdict {
	st := agent.FoldNotify(recs)
	var headSt agent.NotifyState
	checkTail := attested < len(recs) && !in.AcceptTail
	if checkTail {
		headSt = agent.FoldNotify(recs[:attested])
	}
	var out []NotifyEventVerdict
	seen := map[string]int{} // key -> index in out
	inJournal := map[string]bool{}
	for _, j := range journal {
		inJournal[j.Key] = true
	}
	add := func(v NotifyEventVerdict) { seen[v.Key] = len(out); out = append(out, v) }
	for _, key := range st.Keys {
		v := in.verdict(trail, st.Events[key], inJournal[key])
		if he, e := headSt.Events[key], st.Events[key]; checkTail && (he == nil || e.Acked != nil && he.Acked == nil || e.Cleared != nil && he.Cleared == nil) {
			v.Status = NotifyStatusUnattested
			v.Why = "the event's observation, acknowledgement or clearing is among the last " + fmt.Sprint(len(recs)-attested) + " record(s), after the newest signed head, which nothing attests"
		}
		add(v)
	}
	missing := func(key, kind, runID string, at time.Time, why string, status NotifyStatus) {
		if _, ok := seen[key]; ok {
			return
		}
		if status == NotifyStatusNeverNotified && !at.IsZero() && in.Now.Before(at.Add(in.Grace)) {
			status, why = NotifyStatusNotYetDue, "began "+at.UTC().Format(time.RFC3339)+", within the grace period"
		}
		add(NotifyEventVerdict{Trail: trail, Key: key, Kind: kind, RunID: runID, Status: status, Why: why, EventAt: at, InJournal: inJournal[key]})
	}
	// An approval routed to several receivers is several deliveries, each under a key of its own
	// (agent.ToKey): the journal's key is accounted for by any of them, and each is judged alone.
	deliveries := func(key string) []string {
		var ks []string
		for _, k := range st.Keys {
			if k == key {
				ks = append(ks, k)
			} else if _, base, ok := agent.SplitToKey(k); ok && base == key {
				ks = append(ks, k)
			}
		}
		return ks
	}
	for _, j := range journal {
		if len(deliveries(j.Key)) > 0 {
			continue
		}
		if j.Current {
			missing(j.Key, string(j.Kind), j.RunID, j.At, "the journal shows the event and the trail has no record of it", NotifyStatusNeverNotified)
		} else {
			missing(j.Key, string(j.Kind), j.RunID, j.At, "the journal shows a gate that was decided; the trail never observed it", NotifyStatusUnobservedResolved)
		}
	}
	root := strings.TrimPrefix(trail, "notify:")
	if in.Routing != nil {
		in.checkRouting(trail, st, journal, &out)
	}
	for _, j := range journal {
		for _, key := range deliveries(j.Key) {
			e := st.Events[key]
			if j.Current || e.Cleared != nil || e.Rounds == 0 && e.Acked == nil || !owesAllClear(e.Kind) || st.Events[agent.ResolvedKey(key)] != nil {
				continue
			}
			if j.ResolvedAt.IsZero() {
				missing(agent.ResolvedKey(key), string(agent.NotificationResolved), e.RunID, time.Time{},
					"the journal shows the event resolved, with no time for it, and the trail holds no all-clear", NotifyStatusAllClearUntimed)
				continue
			}
			missing(agent.ResolvedKey(key), string(agent.NotificationResolved), e.RunID, j.ResolvedAt,
				"the journal shows the event resolved at "+j.ResolvedAt.UTC().Format(time.RFC3339)+" and the trail never observed the all-clear", NotifyStatusNeverNotified)
		}
	}
	for _, key := range st.Keys {
		e := st.Events[key]
		switch {
		case e.Overdue != nil && (e.Acked != nil || e.Open()) && st.Events[agent.OverdueKey(key)] == nil:
			missing(agent.OverdueKey(key), string(agent.NotificationOverdue), e.RunID, e.Overdue.At,
				"the event was marked overdue and the trail never observed its overdue notice", NotifyStatusNeverNotified)
		case e.Cleared != nil && e.Rounds > 0 && st.Events[agent.ResolvedKey(key)] == nil && owesAllClear(e.Kind):
			missing(agent.ResolvedKey(key), string(agent.NotificationResolved), e.RunID, e.Cleared.At,
				"delivery of the event was attempted and it was then cleared, and the trail never observed the all-clear", NotifyStatusNeverNotified)
		}
	}
	if st.FailingSince != nil && in.Now.Sub(st.FailingSince.At) > in.RecoveryFailingAfter {
		missing(agent.RecoveryKey(root, st.Episodes), string(agent.NotificationRecoveryFailing), root, st.FailingSince.At.Add(in.RecoveryFailingAfter),
			"a recovery failure episode has been open longer than "+in.RecoveryFailingAfter.String()+" and the trail never observed a recovery_failing event", NotifyStatusNeverNotified)
	}
	return out
}

func owesAllClear(kind string) bool {
	switch agent.NotificationKind(kind) {
	case agent.NotificationOutcomeUnknown, agent.NotificationApprovalPending, agent.NotificationRecoveryFailing, agent.NotificationEscalated:
		return true
	}
	return false
}

// verdict is the verdict on one observed event.
func (in NotifyVerifyInput) verdict(trail string, e *agent.NotifyEvent, inJournal bool) NotifyEventVerdict {
	v := NotifyEventVerdict{
		Trail: trail, Key: e.Key, Kind: e.Kind, RunID: e.RunID, InJournal: inJournal, Rounds: e.Rounds,
		EventAt: e.EventAt, ObservedAt: e.ObservedAt, DeadlineAt: e.DeadlineAt,
		Detection: e.ObservedAt.Sub(e.EventAt),
		Receiver:  e.ReceiverName(), Level: e.Level,
	}
	ref := e.EventAt
	if ref.IsZero() {
		ref = e.ObservedAt
	}
	past := func(t time.Time) (bool, string) {
		switch {
		case !e.DeadlineAt.IsZero() && t.After(e.DeadlineAt):
			return true, "past its deadline " + e.DeadlineAt.UTC().Format(time.RFC3339)
		case in.MaxAck > 0 && t.Sub(ref) > in.MaxAck:
			return true, fmt.Sprintf("%s after the event began, past the bound %s", t.Sub(ref).Round(time.Second), in.MaxAck)
		}
		return false, ""
	}
	switch {
	case e.Acked != nil:
		a := e.Acked
		v.AckedAt, v.FirstSentAt, v.Attempts, v.Auth = a.At, a.FirstSentAt, a.Attempts, a.Auth
		v.Delivery, v.Total = a.At.Sub(e.ObservedAt), a.At.Sub(ref)
		v.Status = NotifyStatusAcked
		if late, why := past(a.At); late {
			v.Status, v.Why = NotifyStatusAckedLate, "acknowledged "+why
		}
	case e.Cleared != nil:
		v.ClearedAt = e.Cleared.At
		if e.Rounds == 0 {
			v.Status, v.Why = NotifyStatusClearedBeforeNotification, "cleared ("+e.Cleared.Reason+") before any delivery was attempted"
		} else {
			v.Status, v.Why = NotifyStatusClearedUnacknowledged, "cleared ("+e.Cleared.Reason+") after delivery was attempted and never acknowledged"
		}
	default:
		v.Status = NotifyStatusPending
		if e.Failing != nil {
			v.Status = NotifyStatusFailing
		}
		if late, why := past(in.Now); late {
			v.Status, v.Why = NotifyStatusOverdue, "not acknowledged, "+why
		}
	}
	return v
}

// checkOpFields checks the operation a halt or approval observation names (op_run, op_kind,
// op_id) against its key, which encodes the same run, kind and ID. All three are set together or
// none are (a record from before they were written). The key's last part (the attempt time, the
// reminder number) is not in them, so it is left out of the comparison.
func checkOpFields(bad func(int, string, ...any), i int, r agent.NotifyRecord, kind agent.NotificationKind) {
	if r.OpRun == "" && r.OpKind == "" && r.OpID == "" {
		return
	}
	if r.OpRun == "" || r.OpKind == "" || r.OpID == "" {
		bad(i, "names an operation by some of op_run, op_kind and op_id, not all three")
		return
	}
	var want string
	switch kind {
	case agent.NotificationOutcomeUnknown:
		if r.OpKind != string(agent.OpTool) && r.OpKind != string(agent.OpStep) {
			bad(i, "op_kind %q is not tool or step", r.OpKind)
			return
		}
		want = agent.HaltedKey(r.OpRun, agent.OpKind(r.OpKind), r.OpID, 0)
	case agent.NotificationApprovalPending:
		if r.OpKind != "gate" {
			bad(i, "op_kind %q is not gate", r.OpKind)
			return
		}
		want = agent.ApprovalKey(r.OpRun, r.OpID, 0)
	default:
		bad(i, "an event of kind %s names an operation", kind)
		return
	}
	cut := func(k string) string { return k[:strings.LastIndex(k, ":")] }
	if cut(r.Key) != cut(want) {
		bad(i, "key %q does not match its operation (op_run %q, op_kind %q, op_id %q)", r.Key, r.OpRun, r.OpKind, r.OpID)
	}
}

// checkRouting adds a NotifyStatusNoDelivery verdict for every delivery the routing owes an event
// that the trail does not hold: to each receiver the event routes to, and, for an event with an
// escalation chain, to each step's receiver once the step is due. Deliveries of one event are its
// own key (the default receiver) and its agent.ToKey keys (one per named receiver).
func (in NotifyVerifyInput) checkRouting(trail string, st agent.NotifyState, journal []agent.JournalNotifyEvent, out *[]NotifyEventVerdict) {
	approvers := map[string][]string{}
	over := map[string]bool{} // events the journal shows resolved
	for _, j := range journal {
		over[j.Key] = !j.Current
		if j.Kind == agent.NotificationApprovalPending {
			approvers[j.Key] = j.Approvers
		}
	}
	type group struct {
		first *agent.NotifyEvent
		to    map[string]bool
	}
	groups := map[string]*group{}
	var order []string
	for _, key := range st.Keys {
		e := st.Events[key]
		switch agent.NotificationKind(e.Kind) {
		case agent.NotificationOutcomeUnknown, agent.NotificationApprovalPending, agent.NotificationRunAborted, agent.NotificationRecoveryFailing:
		default:
			continue
		}
		base := key
		if _, b, ok := agent.SplitToKey(key); ok {
			base = b
		}
		g := groups[base]
		if g == nil {
			g = &group{first: e, to: map[string]bool{}}
			groups[base] = g
			order = append(order, base)
		}
		g.to[e.ReceiverName()] = true
	}
	for _, base := range order {
		g := groups[base]
		kind := agent.NotificationKind(g.first.Kind)
		var apps []string
		if kind == agent.NotificationApprovalPending {
			var known bool
			if apps, known = approvers[base]; !known && in.Routing.ApproverRouted() {
				continue // the gate's approvers are not known: its receivers cannot be derived
			}
		}
		for _, want := range in.Routing.Receivers(kind, apps) {
			if g.to[want] {
				continue
			}
			key := agent.ToKey(want, base)
			if want == agent.DefaultReceiver {
				key = base
			}
			status, why := NotifyStatusNoDelivery, "the routing sends the event to receiver "+want+" and the trail has no delivery to it"
			if in.Now.Before(g.first.ObservedAt.Add(in.Grace)) {
				status, why = NotifyStatusNotYetDue, "observed within the grace period; no delivery to receiver "+want+" yet"
			}
			*out = append(*out, NotifyEventVerdict{Trail: trail, Key: key, Kind: g.first.Kind, RunID: g.first.RunID, Status: status, Why: why,
				EventAt: g.first.EventAt, ObservedAt: g.first.ObservedAt, Receiver: want})
		}
	}
	// Escalation: each observed delivery of a kind with a chain owes the step receivers in turn, a
	// step being due when the deadline before it (the event's, or the previous step's) has passed
	// with the event not acknowledged (an approval: not cleared) and not cleared.
	for _, key := range st.Keys {
		e := st.Events[key]
		chain := in.Routing.Escalation[agent.NotificationKind(e.Kind)]
		if len(chain) == 0 || e.Cleared != nil || e.DeadlineAt.IsZero() || over[key] || st.Events[agent.ResolvedKey(key)] != nil {
			continue // no chain, or the event stopped being current: the chain stops with it
		}
		due := e.DeadlineAt
		isApproval := agent.NotificationKind(e.Kind) == agent.NotificationApprovalPending
		stopped := !isApproval && e.Acked != nil
		for l, want := range chain {
			if stopped || in.Now.Before(due.Add(in.Grace)) {
				break
			}
			sk := agent.EscalationKey(key, l+1)
			se := st.Events[sk]
			if se == nil || se.ReceiverName() != want {
				why := "the escalation step is due to receiver " + want + " and the trail has no delivery of it to that receiver"
				*out = append(*out, NotifyEventVerdict{Trail: trail, Key: sk, Kind: string(agent.NotificationEscalated), RunID: e.RunID,
					Status: NotifyStatusNoDelivery, Why: why, EventAt: due, Receiver: want, Level: l + 1})
				break
			}
			if !isApproval && se.Acked != nil {
				break
			}
			if se.Cleared != nil {
				break
			}
			due = se.DeadlineAt
		}
	}
}

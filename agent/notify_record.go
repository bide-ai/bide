package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// This file is the record vocabulary of the notification trail: the audited, append-only log in
// which a notifier writes that it observed an event that needs a person, claimed it for delivery,
// and saw the sink acknowledge it. The trail is separate from the run's journal, so nothing here
// touches resume, Replay or the run lifecycle. See docs/design/notifier.md.

// NotifyRecordKind says what a NotifyRecord records.
type NotifyRecordKind string

const (
	// NotifyObserved: a notifier process first saw the event. Written once per event.
	NotifyObserved NotifyRecordKind = "observed"
	// NotifyClaimed: a process claimed the event for one delivery round, until Until.
	NotifyClaimed NotifyRecordKind = "claimed"
	// NotifyAcked: the sink acknowledged the event. Ends delivery.
	NotifyAcked NotifyRecordKind = "acked"
	// NotifyFailing: delivery has failed Failures times. Written once; delivery continues.
	NotifyFailing NotifyRecordKind = "failing"
	// NotifyOverdue: the event was not acknowledged by its deadline. Written once; delivery continues.
	NotifyOverdue NotifyRecordKind = "overdue"
	// NotifyCleared: the event stopped being current (resolved, decided, or the run ended).
	NotifyCleared NotifyRecordKind = "cleared"
	// NotifyFailingSince: a recovery pass failed and no failure episode was open for the run.
	NotifyFailingSince NotifyRecordKind = "failing_since"
	// NotifyRecovered: a recovery pass succeeded while a failure episode was open.
	NotifyRecovered NotifyRecordKind = "recovered"
	// NotifyWatch: an event that can stop being current is about to be observed; written to the
	// NotifyOpenTrail, not to a run's trail. Idempotent per key while the key is watched.
	NotifyWatch NotifyRecordKind = "watch"
	// NotifyUnwatch: nothing more is owed for the watched event (its all-clear was acknowledged, or
	// it was cleared before any delivery was attempted, or it was never observed); written to the
	// NotifyOpenTrail.
	NotifyUnwatch NotifyRecordKind = "unwatch"
	// NotifyCheckpoint: the first record of every open-trail segment but the first: the events
	// listed when the previous segment was sealed, carried forward (Watching, Backfilled), so a read
	// starts at the newest segment.
	NotifyCheckpoint NotifyRecordKind = "checkpoint"
	// NotifySeal: the last record of an open-trail segment; Next names the segment that continues it.
	NotifySeal NotifyRecordKind = "seal"
	// NotifySegment: one record of the open trail's index, naming (Next) a segment that was created.
	NotifySegment NotifyRecordKind = "segment"
	// NotifyBackfilled: the events observed before the open trail existed were listed (once).
	NotifyBackfilled NotifyRecordKind = "backfilled"
)

// NotifyAuth records how a delivery was authenticated, next to the delivery it authenticated.
type NotifyAuth struct {
	Signature    string `json:"signature,omitempty"`     // the scheme, "hmac-sha256", empty if unsigned
	SecretSlot   string `json:"secret_slot,omitempty"`   // "current" or "previous"; never the secret
	MTLS         bool   `json:"mtls,omitempty"`          // a client certificate was presented
	ClientSPKI   string `json:"client_spki,omitempty"`   // hex SHA-256 of the client certificate's SubjectPublicKeyInfo
	ServerPinned bool   `json:"server_pinned,omitempty"` // the receiver's key matched a pin
	CustomCA     bool   `json:"custom_ca,omitempty"`     // the receiver was verified against a custom CA bundle
	TLSVersion   string `json:"tls_version,omitempty"`   // the negotiated version, such as "1.3"
}

// NotifyRecord is one record of a notification trail. Which fields are set depends on Kind (see
// the constants); every record names its time in At, by the clock of the process that wrote it. A
// record is a leaf of the trail's tamper-evident log, so it holds no secret, no tool arguments and
// no error text.
//
// NotifyRecord is an agent.RunEvent only so the audit package can commit it as an event leaf; a
// run's stream never emits it.
type NotifyRecord struct {
	Kind  NotifyRecordKind `json:"kind"`
	Key   string           `json:"key,omitempty"` // the event key; empty for failing_since and recovered
	RunID string           `json:"run_id,omitempty"`
	At    time.Time        `json:"at"`
	// ClockAt is, on an open-trail record only, the writer's own clock reading when At was moved
	// later than it to fit the record's segment (see openAt): At is never earlier than the clock, so
	// a time moved only ever makes a delivery look later. Zero when At is the clock's reading.
	ClockAt time.Time `json:"clock_at,omitzero"`

	// observed
	EventKind string `json:"event_kind,omitempty"` // the NotificationKind of the event
	// Receiver is the receiver the event is to (see WithNotifyReceiver); empty for the default one.
	Receiver string `json:"receiver,omitempty"`
	// Level is the step of the escalation chain of an escalated event, from 1.
	Level      int       `json:"level,omitempty"`
	EventAt    time.Time `json:"event_at,omitzero"` // when the run entered the state, from the journal
	DeadlineAt time.Time `json:"deadline_at,omitzero"`

	// claimed
	Holder string    `json:"holder,omitempty"`
	Until  time.Time `json:"until,omitzero"`

	// acked
	FirstSentAt time.Time   `json:"first_sent_at,omitzero"`
	Attempts    int         `json:"attempts,omitempty"`
	Status      int         `json:"status,omitempty"` // the sink's HTTP status
	Auth        *NotifyAuth `json:"auth,omitempty"`

	// failing
	Failures int `json:"failures,omitempty"`

	// overdue
	Latency time.Duration `json:"latency,omitempty"` // At minus ObservedAt

	// observed, for a halt or an approval event: the operation or gate the key names, so that whether
	// the event is still current can be checked from the record when an ID in the key was too long to
	// encode (it is a digest). OpRun is the run whose journal holds the operation or gate, OpKind
	// "tool" or "step" for a halt and "gate" for an approval, OpID the operation's or call's ID. A
	// record written before these fields has none; the key is read instead.
	OpRun  string `json:"op_run,omitempty"`
	OpKind string `json:"op_kind,omitempty"`
	OpID   string `json:"op_id,omitempty"`

	// checkpoint: the events listed, and whether the backfill is done. seal: the next segment.
	Watching   []Watched `json:"watching,omitempty"`
	Backfilled bool      `json:"backfilled,omitempty"`
	// Backfills is how many backfill passes the carried state holds (at most two), and BackfillAt
	// when the last one was recorded: a second pass, a day after the first, catches the events an
	// older process delivered without listing them during a rolling upgrade.
	Backfills  int       `json:"backfills,omitempty"`
	BackfillAt time.Time `json:"backfill_at,omitzero"`
	Next       string    `json:"next,omitempty"`

	// cleared: "resolved", "decided", "ended" or "recovered". observed, for a resolved event: the same
	// code, why the event it clears ended. failing_since: a class of cause, never error text.
	Reason string `json:"reason,omitempty"`
}

func (NotifyRecord) agentEvent() {}

// Normalized returns r with every time in UTC and stripped of its monotonic clock reading, the
// form a trail stores and compares, so two records of one instant are equal whatever location they
// were written in.
func (r NotifyRecord) Normalized() NotifyRecord {
	r.At = normTime(r.At)
	r.ClockAt = normTime(r.ClockAt)
	r.BackfillAt = normTime(r.BackfillAt)
	r.EventAt = normTime(r.EventAt)
	r.DeadlineAt = normTime(r.DeadlineAt)
	r.Until = normTime(r.Until)
	r.FirstSentAt = normTime(r.FirstSentAt)
	if r.Watching != nil {
		w := make([]Watched, len(r.Watching))
		for i, x := range r.Watching {
			x.At = normTime(x.At)
			w[i] = x
		}
		r.Watching = w
	}
	return r
}

func normTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC().Round(0)
}

// NotifyTrailName is the name of runID's notification trail in a NotifyTrail.
func NotifyTrailName(runID string) string { return "notify:" + runID }

// NotifyOpenTrail is the prefix of the trail that lists the events whose all-clear may still be
// owed: a watch record before an outcome_unknown, approval_pending or recovery_failing event is
// observed, an unwatch record when nothing more is owed for it. It belongs to no run, so a sweep or a
// recovery pass finds the runs that need a scan, including a run that has completed since, without
// reading every finished run.
//
// It is kept in one segment per UTC day, OpenSegment(t), so a read is bounded by the events listed
// now and the day's records, not by history. A writer that finds the newest segment is not today's
// appends seal to it (the append race makes the seal final: nothing is appended after it), then
// checkpoint, the listed set folded from it, as the first record of today's. The trails stay
// append-only and verifiable: a checkpoint must equal the fold of the segment it continues.
const NotifyOpenTrail = "notify:@open"

// OpenFirstYear is the first year an open-trail index can exist: the year the open trail began. Every
// lookup, and every audit, looks at the years from it to the present, so none depends on a bound
// chosen for a deployment's age.
const OpenFirstYear = 2026

// OpenIndex is the name of the open trail's index for a UTC year: one NotifySegment record per
// segment created, so the newest segment is found by reading the last record of the newest index
// (one or two loads, whatever the gaps and the age of the deployment), and the index is the size of
// a year of days at most.
func OpenIndex(year int) string { return fmt.Sprintf("%s:index:%04d", NotifyOpenTrail, year) }

// OpenSegment is the name of the open-trail segment of the UTC day t is in.
func OpenSegment(t time.Time) string { return NotifyOpenTrail + ":" + t.UTC().Format("2006-01-02") }

// NotifyDigestTrail is the name of the trail the periodic digest uses; it belongs to no run.
const NotifyDigestTrail = "notify:@digest"

// NotifyID is the stable delivery ID of an event: the hex SHA-256 of its key. It is the same on
// every attempt, in every process and after every restart.
func NotifyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// HaltedKey is the event key of an unknown-outcome halt of the operation opID of kind on runID.
// attemptedMS is the live attempt marker's time in Unix milliseconds (OutcomeUnknown.AttemptedAt;
// 0 if unknown), the same on every drive that reports the halt and different for an attempt made
// after the first was resolved.
func HaltedKey(runID string, kind OpKind, opID string, attemptedMS int64) string {
	return "halted:" + encodeID(runID) + ":" + encodeID(string(kind)) + ":" + encodeID(opID) + ":" + strconv.FormatInt(attemptedMS, 10)
}

// ApprovalKey is the event key of the approval gate for call on runID. reminder is 0 for the first
// notification of the gate; a later reminder uses 1, 2, and so on.
func ApprovalKey(runID, call string, reminder int) string {
	return "approval:" + encodeID(runID) + ":" + encodeID(call) + ":" + strconv.Itoa(reminder)
}

// AbortedKey is the event key of runID reaching run:aborted.
func AbortedKey(runID string) string { return "aborted:" + encodeID(runID) }

// RecoveryKey is the event key of runID's episode-th recovery failure (episode counts the
// recovered records before it, from zero).
func RecoveryKey(runID string, episode int) string {
	return "recovery:" + encodeID(runID) + ":" + strconv.Itoa(episode)
}

// ResolvedKey is the event key of the all-clear for the event with key: the notification that it
// stopped being current. It has records of its own in the trail, like the overdue notice, and the
// delivery ID NotifyID(ResolvedKey(key)).
func ResolvedKey(key string) string { return "resolved:" + key }

// OverdueKey is the event key of the overdue notice for the event with key.
func OverdueKey(key string) string { return "overdue:" + key }

// DigestKey is the event key of the digest for time bucket (an opaque, stable label).
func DigestKey(bucket string) string { return "digest:" + encodeID(bucket) }

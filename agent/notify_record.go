package agent

import (
	"crypto/sha256"
	"encoding/hex"
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

	// observed
	EventKind  string    `json:"event_kind,omitempty"` // the NotificationKind of the event
	EventAt    time.Time `json:"event_at,omitzero"`    // when the run entered the state, from the journal
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

	// cleared: "resolved", "decided" or "ended". failing_since: a class of cause, never error text.
	Reason string `json:"reason,omitempty"`
}

func (NotifyRecord) agentEvent() {}

// Normalized returns r with every time in UTC and stripped of its monotonic clock reading, the
// form a trail stores and compares, so two records of one instant are equal whatever location they
// were written in.
func (r NotifyRecord) Normalized() NotifyRecord {
	r.At = normTime(r.At)
	r.EventAt = normTime(r.EventAt)
	r.DeadlineAt = normTime(r.DeadlineAt)
	r.Until = normTime(r.Until)
	r.FirstSentAt = normTime(r.FirstSentAt)
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

// OverdueKey is the event key of the overdue notice for the event with key.
func OverdueKey(key string) string { return "overdue:" + key }

// DigestKey is the event key of the digest for time bucket (an opaque, stable label).
func DigestKey(bucket string) string { return "digest:" + encodeID(bucket) }

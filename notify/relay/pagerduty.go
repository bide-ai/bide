package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bide-ai/bide/notify"
)

// DefaultPagerDutyURL is the Events API v2 endpoint. An EU-region account uses
// https://events.eu.pagerduty.com/v2/enqueue.
const DefaultPagerDutyURL = "https://events.pagerduty.com/v2/enqueue"

const pagerDutySummaryMax = 1024

// PagerDutyConfig configures a PagerDuty sink.
type PagerDutyConfig struct {
	// RoutingKey is the integration key of an Events API v2 integration. It is a credential: it
	// is never put in an error or a log line. Required.
	RoutingKey string
	// URL overrides DefaultPagerDutyURL (the EU endpoint, or a test server).
	URL string
	// Source is the event's source field; empty means "bide".
	Source string
	// Severity, if set, picks the severity ("critical", "error", "warning" or "info") of a trigger;
	// nil means DefaultSeverity.
	Severity func(notify.Payload) string
	// ResolveTarget, if set, says whether p is a clear that ends an incident, and the dedup key of the
	// incident it ends. nil means DefaultResolveTarget. See ResolveKind.
	ResolveTarget func(notify.Payload) (dedupKey string, ok bool)
	// HTTPClient, Attempts, Timeout and Backoff tune the outbound call, as for SlackConfig.
	HTTPClient *http.Client
	Attempts   int
	Timeout    time.Duration
	Backoff    time.Duration
}

// PagerDuty is a Sink that triggers and resolves PagerDuty incidents through the Events API v2.
type PagerDuty struct {
	out      *outbound
	routing  string
	source   string
	severity func(notify.Payload) string
	resolve  func(notify.Payload) (string, bool)
}

// NewPagerDuty returns a PagerDuty sink.
func NewPagerDuty(c PagerDutyConfig) (*PagerDuty, error) {
	if c.RoutingKey == "" {
		return nil, fmt.Errorf("relay: pagerduty: RoutingKey is required")
	}
	u := c.URL
	if u == "" {
		u = DefaultPagerDutyURL
	}
	out, err := newOutbound("pagerduty", u, c.HTTPClient, c.Attempts, c.Timeout, c.Backoff)
	if err != nil {
		return nil, err
	}
	pd := &PagerDuty{out: out, routing: c.RoutingKey, source: c.Source, severity: c.Severity, resolve: c.ResolveTarget}
	if pd.source == "" {
		pd.source = "bide"
	}
	if pd.severity == nil {
		pd.severity = DefaultSeverity
	}
	if pd.resolve == nil {
		pd.resolve = DefaultResolveTarget
	}
	return pd, nil
}

// Name is "pagerduty".
func (d *PagerDuty) Name() string { return "pagerduty" }

// Send triggers an incident for p, or resolves one if p is a clear (see ResolveTarget).
func (d *PagerDuty) Send(ctx context.Context, p notify.Payload) error {
	if _, ok := d.resolve(p); !ok && p.Kind == ResolveKind {
		return nil // an all-clear that names no event: there is no incident to resolve, and it must not open one
	}
	var ev any
	if key, ok := d.resolve(p); ok {
		ev = PagerDutyResolve(d.routing, key)
	} else {
		ev = PagerDutyTrigger(d.routing, d.source, d.severity(p), p)
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("relay: pagerduty: encoding the event: %w", err)
	}
	return d.out.post(ctx, body)
}

// DedupKey is the PagerDuty dedup_key of the incident an event raises: the event's notification ID,
// the SHA-256 of its key. It is stable across every delivery of the event, so PagerDuty collapses
// redeliveries into one incident, and it is what a clear names to resolve it. Another key scheme
// is possible by wrapping the sink, but a resolve must use the same one.
func DedupKey(p notify.Payload) string { return p.ID }

// ResolveKind is the notification kind that clears an incident: the notifier's "resolved" kind
// (agent.NotificationResolved). Its payload has a top-level clears object whose id is the ID of the
// event it ends, which is this sink's dedup key for the incident.
const ResolveKind = "resolved"

// ClearsID returns the ID of the event a clear notification ends, or "" if p is not a clear. It is
// the payload's clears.id, which is the "id" the ended event's own notifications carried, so it is
// also the dedup key of the incident that event opened.
func ClearsID(p notify.Payload) string {
	if p.Kind != ResolveKind || p.Clears == nil {
		return ""
	}
	return p.Clears.ID
}

// DefaultResolveTarget treats a notification of kind ResolveKind as a resolve of the incident whose
// dedup key is the ID of the event it clears (ClearsID), and everything else as a trigger. A
// resolved notification with no clears detail names no incident: the sink sends nothing for it.
func DefaultResolveTarget(p notify.Payload) (string, bool) {
	if p.Kind != ResolveKind {
		return "", false
	}
	id := ClearsID(p)
	return id, id != ""
}

// DefaultSeverity is the severity of the incident a notification opens: a halt on an unknown outcome
// and an overdue notification are critical (a missed alert is expensive there), an abort and a
// failing recovery are errors, and an approval waiting is a warning.
func DefaultSeverity(p notify.Payload) string {
	switch p.Kind {
	case "outcome_unknown", "notification_overdue":
		return "critical"
	case "run_aborted", "recovery_failing":
		return "error"
	case "approval_pending":
		return "warning"
	}
	return "warning"
}

// PagerDutyEvent is an Events API v2 request body.
type PagerDutyEvent struct {
	RoutingKey  string            `json:"routing_key"`
	EventAction string            `json:"event_action"`
	DedupKey    string            `json:"dedup_key"`
	Payload     *PagerDutyPayload `json:"payload,omitempty"`
	Links       []PagerDutyLink   `json:"links,omitempty"`
}

// PagerDutyPayload is the payload of a trigger.
type PagerDutyPayload struct {
	Summary       string         `json:"summary"`
	Source        string         `json:"source"`
	Severity      string         `json:"severity"`
	Timestamp     string         `json:"timestamp,omitempty"`
	Component     string         `json:"component,omitempty"`
	Group         string         `json:"group,omitempty"`
	Class         string         `json:"class,omitempty"`
	CustomDetails map[string]any `json:"custom_details,omitempty"`
}

// PagerDutyLink is a link on an incident.
type PagerDutyLink struct {
	Href string `json:"href"`
	Text string `json:"text"`
}

// PagerDutyTrigger builds the trigger for p: the dedup key is DedupKey(p), the summary is the
// generated one, and the details hold the structured fields (run, kind, policy, approvers,
// deadline, view command). It never carries error text or tool arguments.
func PagerDutyTrigger(routingKey, source, severity string, p notify.Payload) PagerDutyEvent {
	details := map[string]any{
		"run_id":       p.RunID,
		"kind":         p.Kind,
		"id":           p.ID,
		"attempt":      p.Attempt,
		"view_command": p.View.Command,
	}
	if p.DeadlineAt != nil {
		details["deadline_at"] = p.DeadlineAt.UTC().Format(time.RFC3339)
	}
	if p.Principal != nil {
		details["on_behalf_of"] = p.Principal.OnBehalfOf
		details["authority_ref"] = p.Principal.AuthorityRef
	}
	if a := p.Approval; a != nil {
		details["approval"] = map[string]any{"tool": a.Tool, "need": a.Policy.Need, "of": a.Policy.Of, "approved": a.Approved, "approvers": a.Approvers}
	}
	if h := p.Halt; h != nil {
		details["halt_cause"] = h.Cause
	}
	if a := p.Abort; a != nil {
		details["abort_reason"] = a.Reason
	}
	if f := p.Failing; f != nil {
		details["failing_cause"] = f.Cause
	}
	if p.Reason != "" {
		details["reason"] = p.Reason
	}
	pl := &PagerDutyPayload{
		Summary:       clip(p.Summary, pagerDutySummaryMax),
		Source:        source,
		Severity:      severity,
		Class:         p.Kind,
		Group:         p.RunID,
		CustomDetails: details,
	}
	if p.EventAt != nil {
		pl.Timestamp = p.EventAt.UTC().Format(time.RFC3339)
	}
	if p.Step != nil {
		pl.Component = p.Step.Tool
	}
	ev := PagerDutyEvent{RoutingKey: routingKey, EventAction: "trigger", DedupKey: DedupKey(p), Payload: pl}
	if link := viewLink(p, ""); link != "" {
		ev.Links = []PagerDutyLink{{Href: link, Text: "Open run in viewer"}}
	}
	return ev
}

// PagerDutyResolve builds the resolve of the incident with dedupKey.
func PagerDutyResolve(routingKey, dedupKey string) PagerDutyEvent {
	return PagerDutyEvent{RoutingKey: routingKey, EventAction: "resolve", DedupKey: dedupKey}
}

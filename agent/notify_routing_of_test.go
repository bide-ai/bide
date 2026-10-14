package agent

import (
	"slices"
	"testing"
	"time"
)

func TestRoutingOf_ReceiversAndEscalation(t *testing.T) {
	r, err := RoutingOf(WithNotifyReceiver("risk", &fakeSink{}), WithNotifyReceiver("fin", &fakeSink{}), WithNotifyReceiver("oncall", &fakeSink{}),
		WithNotifyApproverRoute("a", "risk"), WithNotifyApproverRoute("b", "fin"),
		WithNotifyRoute(NotificationOutcomeUnknown, "oncall"),
		WithNotifyEscalation(NotificationApprovalPending, EscalationStep{Receiver: "oncall", Deadline: time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		kind      NotificationKind
		approvers []string
		want      []string
	}{
		"fan-out": {NotificationApprovalPending, []string{"b", "a"}, []string{"fin", "risk"}},
		"an unrouted approver also reaches the kind's receiver": {NotificationApprovalPending, []string{"b", "a", "x"}, []string{DefaultReceiver, "fin", "risk"}},
		"unrouted gate":    {NotificationApprovalPending, []string{"x"}, []string{DefaultReceiver}},
		"1-of-1 gate":      {NotificationApprovalPending, nil, []string{DefaultReceiver}},
		"a routed kind":    {NotificationOutcomeUnknown, nil, []string{"oncall"}},
		"an unrouted kind": {NotificationRunAborted, nil, []string{DefaultReceiver}},
	} {
		if got := r.Receivers(tc.kind, tc.approvers); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	if !r.ApproverRouted() || !slices.Equal(r.Escalation[NotificationApprovalPending], []string{"oncall"}) {
		t.Fatalf("routing = %+v", r)
	}
	// The engine delivers by the same method.
	e := &notifyEngine{cfg: defaultNotifyConfig()}
	e.cfg.approverRoutes = map[string]string{"a": "risk", "b": "fin"}
	ds := e.routed(notifyEvent{key: "k", kind: NotificationApprovalPending, approval: &NotifyApproval{Approvers: []string{"a", "b"}}})
	if len(ds) != 2 || ds[0].key != ToKey("fin", "k") || ds[1].key != ToKey("risk", "k") {
		t.Fatalf("deliveries = %+v", ds)
	}
	if _, err := RoutingOf(WithNotifyRoute(NotificationOverdue, "x")); err == nil {
		t.Fatal("an invalid option must error")
	}
}

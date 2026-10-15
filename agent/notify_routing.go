package agent

import (
	"slices"
)

// NotifyRouting is the routing of a notifier: which receiver each kind of event goes to, which
// receivers an approval goes to by its approvers, and the receivers of each escalation chain. It is
// the part of the notifier's configuration an auditor needs to say which deliveries an event is
// owed (audit.NotifyVerifyInput.Routing), and the notifier routes by the same method
// (Receivers), so the two cannot differ. Receivers are named as DefaultReceiver or as given to
// WithNotifyReceiver.
type NotifyRouting struct {
	// Routes is the receiver by kind (WithNotifyRoute); a kind not named goes to DefaultReceiver.
	Routes map[NotificationKind]string `json:"routes,omitempty"`
	// ApproverRoutes is the receiver by approver (WithNotifyApproverRoute).
	ApproverRoutes map[string]string `json:"approver_routes,omitempty"`
	// Escalation is, for each kind with a chain (WithNotifyEscalation), the receiver of each step.
	Escalation map[NotificationKind][]string `json:"escalation,omitempty"`
}

// RoutingOf returns the routing the options give a notifier: build it from the same NotifyOptions
// you pass to WithNotifier or NotifyPending, and write it (as JSON) for the auditor. Options that
// are not about routing are accepted and ignored.
func RoutingOf(opts ...NotifyOption) (NotifyRouting, error) {
	cfg := defaultNotifyConfig()
	if err := applyOptions("RoutingOf", &cfg, opts, NotifyOption.applyNotify); err != nil {
		return NotifyRouting{}, err
	}
	return cfg.routing(), nil
}

// routing is the routing of c.
func (c notifyConfig) routing() NotifyRouting {
	r := NotifyRouting{Routes: c.routes, ApproverRoutes: c.approverRoutes}
	if len(c.escalation) > 0 {
		r.Escalation = map[NotificationKind][]string{}
		for k, steps := range c.escalation {
			for _, s := range steps {
				r.Escalation[k] = append(r.Escalation[k], s.Receiver)
			}
		}
	}
	return r
}

// Receivers returns the receivers an event of kind is delivered to, sorted: for an approval whose
// approvers are routed, the receiver of each (one delivery each), and, when any approver of the gate
// has no route, the kind's route as well (else DefaultReceiver), through which that approver is
// reached; for any other event, or an approval none of whose approvers is routed, the kind's route,
// else DefaultReceiver. approvers is the gate's approvers, nil for a 1-of-1 gate.
func (r NotifyRouting) Receivers(kind NotificationKind, approvers []string) []string {
	kindRecv := DefaultReceiver
	if to, ok := r.Routes[kind]; ok {
		kindRecv = receiverLabel(receiverID(to))
	}
	if (kind == NotificationApprovalPending || kind == NotificationApprovalExpired) && len(r.ApproverRoutes) > 0 {
		var recvs []string
		unrouted := false
		for _, a := range approvers {
			to, ok := r.ApproverRoutes[a]
			switch {
			case !ok:
				unrouted = true
			case !slices.Contains(recvs, receiverLabel(receiverID(to))):
				recvs = append(recvs, receiverLabel(receiverID(to)))
			}
		}
		if len(recvs) > 0 {
			if unrouted && !slices.Contains(recvs, kindRecv) {
				recvs = append(recvs, kindRecv)
			}
			slices.Sort(recvs)
			return recvs
		}
	}
	return []string{kindRecv}
}

// ApproverRouted reports whether routing depends on a gate's approvers, so that a gate's receivers
// cannot be known without them.
func (r NotifyRouting) ApproverRouted() bool { return len(r.ApproverRoutes) > 0 }

package declarations

import (
	"fmt"
)

// Finding is something to look at in one document. It is a note, not a verdict: nothing here says the
// declarations are wrong, only where a reader should look.
type Finding struct {
	Subject string
	Message string
}

// Findings reads one document. A tool whose provenance nobody stated (it is neither Untrusted nor a trusted
// output) is reported when the taint policy does not require one: the engine cannot tell it from a tool that
// is simply trusted.
func (d Document) Findings() []Finding {
	var out []Finding
	add := func(subject, format string, args ...any) {
		out = append(out, Finding{Subject: subject, Message: fmt.Sprintf(format, args...)})
	}
	if d.Policy == nil {
		for _, t := range d.Tools {
			if t.Effect {
				add("tool "+t.Name, "can have a side effect and the agent has no taint policy: a run that reads untrusted content is not gated for it")
			}
			if t.Untrusted != "" {
				add("tool "+t.Name, "declares untrusted source %q and the agent has no taint policy: marks are recorded, nothing is gated", t.Untrusted)
			}
		}
	} else if g := d.Policy.Gate; g != nil && !d.Policy.Refuse {
		switch {
		case g.Single:
			add("policy", "the escalation gate is one decision: unsigned, and the requester can approve")
		case g.Need < 2:
			add("policy", "the escalation gate needs %d decision: a bank deployment is advised need >= 2", g.Need)
		case !g.ExcludePrincipal:
			add("policy", "the escalation gate does not exclude the requester: the person whose input provoked the call can approve it")
		}
	}
	if d.Policy != nil && !d.Policy.RequireProvenance {
		for _, t := range d.Tools {
			if t.Untrusted == "" && t.Trusted == "" {
				add("tool "+t.Name, "declares no provenance, and the taint policy does not require one: if its results can carry outside text, declare Untrusted; if they are yours, declare a trusted output; then require provenance")
			}
		}
	}
	for _, t := range d.Tools {
		if t.Exempt != "" {
			add("tool "+t.Name, "is exempt from the escalation gate (%q)", t.Exempt)
		}
		if t.RaisesSafety != "" {
			add("tool "+t.Name, "declares a Safety safer than a tool inside it (RaisesSafety: %q)", t.RaisesSafety)
		}
		if len(t.Caps) > 0 {
			add("tool "+t.Name, "carries %d cap rule(s)", len(t.Caps))
		}
	}
	return out
}

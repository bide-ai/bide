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

// Findings reads one document. An "unclassified tool" finding (a tool whose provenance nobody stated) is
// not here: it needs an explicit trusted declaration, which the library does not have yet.
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
	for _, t := range d.Tools {
		if t.Exempt != "" {
			add("tool "+t.Name, "is exempt from the escalation gate (%q)", t.Exempt)
		}
		if len(t.Caps) > 0 {
			add("tool "+t.Name, "carries %d cap rule(s)", len(t.Caps))
		}
	}
	return out
}

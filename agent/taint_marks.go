package agent

import (
	"fmt"
	"unicode/utf8"

	"github.com/bide-ai/bide/internal/printable"
)

// Provenance declarations and the marks they leave in a run's journal (docs/design/untrusted-content-
// and-amount-caps.md, sections 5.1, 5.2 and 5.4). A host says which content it did not write: a tool
// whose results come from outside (Untrusted), a run whose input does (WithUntrustedInput), a retrieval
// layer whose documents do (RetrievalUntrusted). The engine records the declaration with the content, so
// the journal shows what a run read. Nothing here gates a call: a mark is evidence, and a taint policy
// that acts on marks is a later part of the design. A mark costs a record member and nothing else.

// maxProvenanceName bounds a source name or an exemption reason, which are shown to people and kept in
// the journal.
const maxProvenanceName = 128

// checkProvenanceName refuses an empty name, one that is not valid UTF-8, one over maxProvenanceName
// bytes, and one with a control, format or other non-printing character.
func checkProvenanceName(what, s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%s: the name is empty: %w", what, ErrConfig)
	case len(s) > maxProvenanceName:
		return fmt.Errorf("%s: the name is %d bytes, over %d: %w", what, len(s), maxProvenanceName, ErrConfig)
	case !utf8.ValidString(s):
		return fmt.Errorf("%s: the name is not valid UTF-8: %w", what, ErrConfig)
	}
	if r, bad := printable.Bad(s); bad {
		return fmt.Errorf("%s: the name has the non-printing character %s: %w", what, printable.Describe(r), ErrConfig)
	}
	return nil
}

// Untrusted declares that the results of the tool are untrusted content, from the named source (for
// example "crm-notes" or "inbound-email"): text the host did not write and an attacker may have. Every
// result the tool returns is recorded with the source name (Safety.Untrusted of the result record),
// whether or not the agent has a taint policy. The model never declares this, and nothing in a result
// can. A tool whose result is sometimes trusted and sometimes not is split into two tools. A tool that
// embeds or unwraps another must declare it too, or registration refuses it (see checkEmbedded and
// checkUnwrapChain); a wrapper that does neither is not seen, and carrying the declaration is its author's
// duty. A plan flow node
// takes the declaration, but flows do not enforce taint yet, so building a flow with such a tool is
// refused with ErrConfig.
func Untrusted(source string) ToolOption {
	return toolOption(func(c *toolConfig) error {
		if err := checkProvenanceName("Untrusted", source); err != nil {
			return err
		}
		c.spec.Untrusted = source
		return nil
	})
}

// TaintExempt declares that the tool's calls need no escalation gate even in a run that read untrusted
// content, and why (the reason is required, and is kept for the audit). It is a declaration in Go, the
// only place such a loosening can be made. It does nothing until a taint policy exists. A plan flow
// node cannot yet be built from a tool that declares it (see Untrusted).
func TaintExempt(reason string) ToolOption {
	return toolOption(func(c *toolConfig) error {
		if err := checkProvenanceName("TaintExempt", reason); err != nil {
			return err
		}
		c.spec.TaintExempt = reason
		return nil
	})
}

// Trusted declares that the results of the tool are the host's own, and why (the reason is required and
// is kept in the declarations): the tool reads a table the host writes, calls a service it runs. It is the
// positive counterpart of Untrusted, and records nothing in a result: a trusted result carries no mark. It
// exists so that a taint policy that requires provenance can tell a tool someone classified from one nobody
// did. A tool cannot be both Untrusted and trusted. Experimental.
func Trusted(reason string) ToolOption {
	return toolOption(func(c *toolConfig) error {
		if err := checkProvenanceName("trusted output", reason); err != nil {
			return err
		}
		c.spec.Trusted = reason
		return nil
	})
}

// RaisesSafety lets a wrapper declare a Safety that is safer than a tool it wraps (Unwrap) or embeds, and
// says why (the reason is required, follows the rules of Trusted, and is kept in the declarations and their
// digest). Safety is ordered side effect, then Idempotent, then ReadOnly (the safest: its calls are re-run
// on resume and are not gated by the taint policy). Without this option the agent refuses, with ErrConfig, a
// wrapper whose Safety is safer than any tool inside it, since the wrapper's Safety is the one the agent
// acts on: a ReadOnly wrapper over a tool with side effects would lose the taint gate, and an Idempotent
// one would have its call re-run on resume. A wrapper that really does make the call safer (it stops at
// a dry run, say) declares so and gives the reason. The reason is checked for form only (the rules of
// Trusted), not for meaning: it is recorded for the reviewer, who reads it in the declarations. A tool that
// wraps nothing is not changed by it.
// Experimental.
func RaisesSafety(reason string) ToolOption {
	return toolOption(func(c *toolConfig) error {
		if err := checkProvenanceName("RaisesSafety", reason); err != nil {
			return err
		}
		c.spec.RaisesSafety = reason
		return nil
	})
}

// WithUntrustedInput declares that the run's input is untrusted content from the named source: an agent
// started by an inbound email or a customer's message. It is journaled in run:start's settings, and a
// later drive that passes another source is ErrConfig, as for any setting that differs from the
// journaled one. It cannot be combined with WithTrustedInput.
func WithUntrustedInput(source string) RunOption {
	return runOption(func(c *runConfig) error {
		if err := checkProvenanceName("WithUntrustedInput", source); err != nil {
			return err
		}
		c.untrustedInput = &source
		return nil
	})
}

// WithTrustedInput declares that the run's input is the host's own, and why. It is journaled in
// run:start's settings like WithUntrustedInput, and exists so that a deployment which requires every
// run to state its input's provenance can say so for a run it trusts. It cannot be combined with
// WithUntrustedInput.
func WithTrustedInput(reason string) RunOption {
	return runOption(func(c *runConfig) error {
		if err := checkProvenanceName("WithTrustedInput", reason); err != nil {
			return err
		}
		c.trustedInput = &reason
		return nil
	})
}

// RetrievalUntrusted declares that the documents the layer retrieves are untrusted content from the
// named source. The mark is recorded with the layer's retrieval step ("@retrieval/<layer>"), so a
// resume and an audit read the mark the first drive recorded.
func RetrievalUntrusted(source string) RetrievalOption {
	return retrievalOption(func(l *retrievalLayer) error {
		if err := checkProvenanceName("RetrievalUntrusted", source); err != nil {
			return err
		}
		l.untrusted = source
		return nil
	})
}

// untrustedSource is the source a spec declares for its tool's results: ToolSpec.Untrusted, else
// Safety.Untrusted. The two agree after normalizeUntrusted.
func (s ToolSpec) untrustedSource() string {
	if s.Untrusted != "" {
		return s.Untrusted
	}
	return s.Safety.Untrusted
}

// checkRaisesSafety holds a RaisesSafety reason on a spec built by hand to the rules of the option (those of
// Trusted). An empty one is not set.
func checkRaisesSafety(s ToolSpec) error {
	if s.RaisesSafety == "" {
		return nil
	}
	return checkProvenanceName("RaisesSafety of tool "+s.Name, s.RaisesSafety)
}

// normalizeUntrusted folds Safety.Untrusted into ToolSpec.Untrusted, so that a source given through
// WithSafety is a declaration like the option's and is never ignored. Both given and different, or an
// invalid name, is ErrConfig.
func normalizeUntrusted(s *ToolSpec) error {
	if s.Safety.Untrusted != "" {
		if err := checkProvenanceName("Safety.Untrusted of tool "+s.Name, s.Safety.Untrusted); err != nil {
			return err
		}
		if s.Untrusted != "" && s.Untrusted != s.Safety.Untrusted {
			return fmt.Errorf("agent: tool %q declares untrusted source %q (Untrusted) and %q (Safety.Untrusted): declare it once: %w", s.Name, s.Untrusted, s.Safety.Untrusted, ErrConfig)
		}
	}
	if s.Trusted != "" { // a spec built by hand carries no option's check
		if err := checkProvenanceName("Trusted of tool "+s.Name, s.Trusted); err != nil {
			return err
		}
	}
	if err := checkRaisesSafety(*s); err != nil {
		return err
	}
	s.Untrusted = s.untrustedSource()
	if s.Untrusted != "" && s.Trusted != "" {
		return fmt.Errorf("agent: tool %q declares untrusted source %q and trusted output %q: it is one or the other: %w", s.Name, s.Untrusted, s.Trusted, ErrConfig)
	}
	return nil
}

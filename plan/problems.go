package plan

import (
	"errors"
	"fmt"
	"strings"
)

// problemList is an error that keeps the problems it reports as a list, each with its Finding. Its
// text joins their messages with "; " after prefix (and lead, which every one of them begins with), and
// ends with the sentinel when there is one, as the single string it replaces did. ConfigReport.attribute
// reads the findings, so that a problem whose own text holds "; " (a node may be named "a; b") stays one
// problem, and the report's Problems and Findings come from the same value, with no reading of the text.
//
// A finding with no Kind is a line that is not a problem of its own (the "and N more type problems"
// that ends a capped list): it is in the text and in Problems, and has no Finding.
type problemList struct {
	prefix   string // "plan: load \"f\": " or "plan: build \"f\": "
	lead     string // text each problem begins with, such as `wiring[2] join "j" `
	finds    []Finding
	extra    []Finding // findings of problems the list leaves out of its text (see typeProblemsError)
	sentinel error     // wrapped, and named at the end of the text; nil for none
}

func (e *problemList) Error() string {
	msgs := make([]string, len(e.finds))
	for i, f := range e.finds {
		msgs[i] = f.Message
	}
	s := e.prefix + e.lead + strings.Join(msgs, "; ")
	if e.sentinel != nil {
		s += ": " + e.sentinel.Error()
	}
	return s
}

func (e *problemList) Unwrap() error { return e.sentinel }

// problemParts returns the problems of the list, each with its message as the report shows it (with the
// lead), and the findings of the problems the list leaves out of its text.
func (e *problemList) problemParts() (parts, extra []Finding) {
	parts = make([]Finding, len(e.finds))
	for i, f := range e.finds {
		f.Message = e.lead + f.Message
		parts[i] = f
	}
	return parts, e.extra
}

// problemParts returns the problems an error from assemble, Build or a graph check carries, or false
// for an error that carries its text only.
func problemParts(err error) (parts, extra []Finding, ok bool) {
	var l interface {
		problemParts() (parts, extra []Finding)
	}
	if errors.As(err, &l) {
		parts, extra = l.problemParts()
		return parts, extra, true
	}
	return nil, nil, false
}

// otherf is a problem that has no kind of its own (yet): a Finding of kind ProblemOther with the message
// formatted. A site that reports a problem without a specific kind says so by calling this.
func otherf(format string, a ...any) Finding {
	return Finding{Kind: ProblemOther, Message: fmt.Sprintf(format, a...)}
}

// found is a problem with a specific kind, the full names it is about and, for a type or safety kind,
// Got and Want.
func found(kind ProblemKind, names []string, got, want, format string, a ...any) Finding {
	return Finding{Kind: kind, Names: names, Got: got, Want: want, Message: fmt.Sprintf(format, a...)}
}

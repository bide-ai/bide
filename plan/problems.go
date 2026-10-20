package plan

import (
	"errors"
	"strings"
)

// problemList is an error that keeps the problems it reports as a list. Its text joins them with
// "; " after prefix (and lead, which every one of them begins with), and ends with the sentinel
// when there is one, as the single string it replaces did. ConfigReport.attribute reads parts, so
// that a problem whose own text holds "; " (a node may be named "a; b") stays one problem.
type problemList struct {
	prefix   string // "plan: load \"f\": " or "plan: build \"f\": "
	lead     string // text each problem begins with, such as `wiring[2] join "j" `
	msgs     []string
	sentinel error // wrapped, and named at the end of the text; nil for none
}

func (e *problemList) Error() string {
	s := e.prefix + e.lead + strings.Join(e.msgs, "; ")
	if e.sentinel != nil {
		s += ": " + e.sentinel.Error()
	}
	return s
}

func (e *problemList) Unwrap() error { return e.sentinel }

func (e *problemList) problemParts() []string {
	out := make([]string, len(e.msgs))
	for i, m := range e.msgs {
		out[i] = e.lead + m
	}
	return out
}

// problemParts returns the problems an error from assemble, Build or a graph check carries, or false
// for an error that carries its text only.
func problemParts(err error) ([]string, bool) {
	var l interface{ problemParts() []string }
	if errors.As(err, &l) {
		return l.problemParts(), true
	}
	return nil, false
}

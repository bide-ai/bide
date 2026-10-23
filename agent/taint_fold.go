package agent

import (
	"encoding/json"
	"strings"

	"github.com/bide-ai/bide/internal/strictjson"
)

// taintMark is one piece of untrusted content a run's journal shows: where it came from (the declared
// source), where it was read (Ref: the tool and call, "input" or "retrieval") and its position in the
// journal prefix the fold read (Pos).
type taintMark struct {
	Source string
	Ref    string
	Pos    int
}

// turnTaint is the untrusted content the model turn at recs[ModelIdx] had been shown: the marks that
// precede it. The calls the turn requested are tainted exactly when Marks is not empty.
type turnTaint struct {
	ModelIdx int
	Marks    []taintMark
}

// foldTaint is the pure fold of section 5.2 of docs/design/untrusted-content-and-amount-caps.md: for
// each model turn of recs, the marks recorded before it. A mark is the input's (run:start's settings),
// a retrieval step's, or a tool result's (the result's Safety.Untrusted, which every result path
// records), in journal order, after any inherited marks (a parent's, a session's: the caller reads
// those). A call's siblings share its turn, and their results are recorded after it, so a turn is
// never tainted by a result of its own calls.
//
// declared, when not nil, names the source a tool declares now ("" for none); a result of a tool it
// names is marked even when the record carries none. That is the larger-of-recorded-and-current rule
// for a run under taint (an earlier result of a tool the host has since declared untrusted); a caller
// that is not enforcing taint passes nil. A record's own mark is never lowered by declared.
//
// It reads no clock, store or configuration, and the same records give the same result.
func foldTaint(recs []Record, inherited []taintMark, declared func(tool string) string) []turnTaint {
	toolNames := map[string]string{} // tool-use ID -> tool name, from the model records before the result
	marks := append([]taintMark(nil), inherited...)
	var out []turnTaint
	for i, r := range recs {
		switch r.Kind {
		case StepModel:
			out = append(out, turnTaint{ModelIdx: i, Marks: append([]taintMark(nil), marks...)})
			if r.Message != nil {
				for _, tu := range r.Message.toolUses() {
					toolNames[tu.ID] = tu.Name
				}
			}
		case StepToolResult, StepSagaFail:
			src := ""
			if r.Safety != nil {
				src = r.Safety.Untrusted
			}
			if src == "" && declared != nil {
				src = declared(toolNames[r.ToolUseID])
			}
			if src != "" {
				marks = append(marks, taintMark{Source: src, Ref: "tool:" + toolNames[r.ToolUseID] + "/" + r.ToolUseID, Pos: i})
			}
		case StepValue:
			switch {
			case r.Name == runStartStep:
				if src := startInputSource(r); src != "" {
					marks = append(marks, taintMark{Source: src, Ref: "input", Pos: i})
				}
			case strings.HasPrefix(r.Name, "@retrieval/"):
				if src := retrievalSource(r); src != "" {
					marks = append(marks, taintMark{Source: src, Ref: "retrieval", Pos: i})
				}
			}
		}
	}
	return out
}

// taintOfCall is the marks the turn that requested toolUseID had been shown, and whether recs has
// such a turn. The call is tainted when the marks are not empty.
func taintOfCall(recs []Record, toolUseID string, inherited []taintMark, declared func(tool string) string) ([]taintMark, bool) {
	for _, t := range foldTaint(recs, inherited, declared) {
		if m := recs[t.ModelIdx].Message; m != nil {
			for _, tu := range m.toolUses() {
				if tu.ID == toolUseID {
					return t.Marks, true
				}
			}
		}
	}
	return nil, false
}

// startInputSource is the untrusted source run:start's settings declare, "" for none or a record that
// does not read.
func startInputSource(r Record) string {
	var s struct {
		Settings struct {
			UntrustedInput *string `json:"untrusted_input"`
		} `json:"settings"`
	}
	if strictjson.Unmarshal(r.Result, &s, &strictjson.Options{AllowUnknown: true}) != nil || s.Settings.UntrustedInput == nil {
		return ""
	}
	return *s.Settings.UntrustedInput
}

// retrievalSource is the untrusted source a retrieval step's record carries, "" for none or a record
// that does not read.
func retrievalSource(r Record) string {
	var v struct {
		Untrusted string `json:"untrusted"`
	}
	if json.Unmarshal(r.Result, &v) != nil {
		return ""
	}
	return v.Untrusted
}

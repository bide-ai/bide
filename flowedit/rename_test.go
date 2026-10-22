package flowedit

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// renameFlow is testFlow with an ungated step between charge and receipt, so a rename can land on the
// name of a node that is a different node in the baseline.
const renameFlow = `version: 1
flow: refund
entry: validate
nodes:
  - {name: validate, block: validate, safety: readonly}
  - name: charge
    block: charge
    safety: side_effect
    approval: {need: 2, approvers: [ops, finance, risk], exclude_principal: true, timeout: 24h}
  - {name: audit, block: plain, safety: idempotent}
  - {name: receipt, block: receipt, safety: idempotent}
wiring:
  - edge: [validate, charge]
  - edge: [charge, audit]
  - edge: [audit, receipt]
assert:
  - every path to charge passes approval >= 2 of 3
  - receipt reachable
`

// session is the page's side of an editing session: the text, the renames that came with it, and the
// undo stack of both, so a test makes the same requests the page does.
type session struct {
	t       *testing.T
	s       *Server
	h       http.Handler
	text    string
	renames []Rename
	undo    []snap
}

type snap struct {
	text    string
	renames []Rename
}

func newSession(t *testing.T, s *Server, text string) *session {
	return &session{t: t, s: s, h: s.Handler(), text: text}
}

func (e *session) post(path string, body map[string]any, out any) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	w := call(e.h, e.s, path, string(b), nil)
	if w.Code != 200 {
		e.t.Fatalf("%s: %d %s", path, w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		e.t.Fatalf("%s: %v\n%s", path, err, w.Body)
	}
}

// do applies an operation as the page does and returns the verdict on the result.
func (e *session) do(op Operation) *Weakening {
	e.t.Helper()
	var ed struct {
		Edited
		Refused string `json:"refused"`
	}
	e.post("/api/apply", map[string]any{"text": e.text, "operation": op, "renames": e.renames}, &ed)
	if ed.Refused != "" {
		e.t.Fatalf("%s %s: refused: %s", op.Op, op.Node, ed.Refused)
	}
	if ed.Changed {
		e.undo = append(e.undo, snap{e.text, e.renames})
		e.text, e.renames = ed.Text, ed.Renames
	}
	return ed.Weakening
}

// back is the page's undo: the report of the earlier text under the renames it had.
func (e *session) back() *Weakening {
	e.t.Helper()
	top := e.undo[len(e.undo)-1]
	var v View
	e.post("/api/report", map[string]any{"text": top.text, "renames": top.renames}, &v)
	e.undo = e.undo[:len(e.undo)-1]
	e.text, e.renames = top.text, top.renames
	return v.Weakening
}

// typeText is an edit made in the source pane: the text changes, the renames stay.
func (e *session) typeText(text string) *Weakening {
	e.t.Helper()
	var v View
	e.post("/api/report", map[string]any{"text": text, "renames": e.renames}, &v)
	e.text = text
	return v.Weakening
}

func (e *session) verdict() *Weakening {
	var v View
	e.post("/api/report", map[string]any{"text": e.text, "renames": e.renames}, &v)
	return v.Weakening
}

func rn(from, to string) Operation { return Operation{Op: "rename_node", Node: from, Value: to} }

// eachMode runs f on the editor over the Registry, over its description and over neither, each opened
// on the baseline text.
func eachMode(t *testing.T, baseline string, f func(t *testing.T, kind string, s *Server)) {
	t.Helper()
	for kind, s := range typedServers(t, "refund.yaml", baseline) {
		t.Run(kind, func(t *testing.T) { f(t, kind, s) })
	}
}

func notWeakened(t *testing.T, what string, w *Weakening) {
	t.Helper()
	if w == nil || len(w.Weakened) > 0 || w.Word != "" || w.Line != "" || w.CompareError != "" {
		t.Errorf("%s: read as weakened or not compared: %+v", what, w)
	}
}

func weakenedWith(t *testing.T, what string, w *Weakening, want string) {
	t.Helper()
	if w == nil || len(w.Weakened) == 0 || w.Word == "" || !strings.Contains(strings.Join(w.Weakened, "\n"), want) {
		t.Errorf("%s: want a weakening naming %q, got %+v", what, want, w)
	}
}

// Renaming a gated node is not a gate removed and a new ungated node: the editor knows which node
// the new name belongs to.
func TestRenameOfAGatedNodeIsNotWeakened(t *testing.T) {
	eachMode(t, renameFlow, func(t *testing.T, kind string, s *Server) {
		e := newSession(t, s, renameFlow)
		w := e.do(rn("charge", "charge_card"))
		notWeakened(t, "a pure rename", w)
		if !slices.Equal(w.Renamed, []string{"charge -> charge_card"}) {
			t.Errorf("the verdict does not say which rename it honoured: %q", w.Renamed)
		}
		// The saved file is not changed by any of this: the confirmation, the save and the verdict of
		// the saved file agree.
		var pv Preview
		e.post("/api/preview", map[string]any{"text": e.text, "hash": hashOf([]byte(renameFlow)), "renames": e.renames}, &pv)
		if len(pv.NeedsConfirmation) != 0 {
			t.Errorf("a pure rename needs a confirmation: %q", pv.NeedsConfirmation)
		}
		var d Doc
		e.post("/api/save", map[string]any{"text": e.text, "hash": pv.Hash, "confirmed": pv.NeedsConfirmation, "renames": e.renames}, &d)
		notWeakened(t, "the document a save answers with", d.Weakening)
		// The renames are the session's, not the file's: the same file without them is compared by
		// name, as bide check -diff compares it.
		var v View
		e.post("/api/report", map[string]any{"text": e.text}, &v)
		if len(v.Weakening.Renamed) != 0 {
			t.Errorf("a request without renames honoured some: %q", v.Weakening.Renamed)
		}
	})
}

// A rename beside another edit of the renamed node or its wiring still matches the node, and the other
// edit is judged on what it is. The classifier's own pairing of a renamed node gives up on these.
func TestRenameBesideAnotherEditMatchesTheNode(t *testing.T) {
	eachMode(t, renameFlow, func(t *testing.T, kind string, s *Server) {
		e := newSession(t, s, renameFlow)
		e.do(rn("charge", "charge_card"))
		w := e.do(Operation{Op: "add_edge", Node: "charge_card", To: "receipt"})
		notWeakened(t, "a rename and a new edge from the gated node", w)
		// Rename the node beside it too.
		w = e.do(rn("audit", "ledger"))
		notWeakened(t, "two renames and a new edge", w)
	})
}

// A rename never hides a weaker gate, safety or assertion made in the same session, whichever came
// first.
func TestRenameDoesNotHideAWeakening(t *testing.T) {
	three := []string{"ops", "finance", "risk"}
	cases := []struct {
		name string
		op   func(node string) Operation
		want string
		// structural: the typed editor refuses the operation (a config may not raise a safety above its
		// registration), so only the structural editor makes it.
		structural bool
	}{
		{"lowered need", func(n string) Operation {
			return Operation{Op: "set_approval", Node: n, Approval: gate(1, three, true, "24h")}
		}, "approval 2 of 3", false},
		{"removed gate", func(n string) Operation { return Operation{Op: "clear_approval", Node: n} }, "approval", false},
		{"dropped exclusion", func(n string) Operation {
			return Operation{Op: "set_approval", Node: n, Approval: gate(2, three, false, "24h")}
		}, "charge", false},
		{"lengthened deadline", func(n string) Operation {
			return Operation{Op: "set_approval", Node: n, Approval: gate(2, three, true, "72h")}
		}, "charge", false},
		{"lowered safety", func(n string) Operation { return Operation{Op: "set_safety", Node: n, Value: "idempotent"} }, "charge", true},
		{"removed assertion", func(string) Operation { return Operation{Op: "remove_assertion", Index: 0} }, "assertion", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eachMode(t, renameFlow, func(t *testing.T, kind string, s *Server) {
				if c.structural && kind != "structural" {
					// The typed editor must refuse it, so there is no weakening to read.
					e := newSession(t, s, renameFlow)
					e.do(rn("charge", "charge_card"))
					var out struct {
						Refused string `json:"refused"`
					}
					e.post("/api/apply", map[string]any{"text": e.text, "operation": c.op("charge_card"), "renames": e.renames}, &out)
					if out.Refused == "" {
						t.Errorf("the typed editor made %s", c.name)
					}
					return
				}
				e := newSession(t, s, renameFlow)
				notWeakened(t, "the rename", e.do(rn("charge", "charge_card")))
				weakenedWith(t, "rename, then weaken", e.do(c.op("charge_card")), c.want)
				f := newSession(t, s, renameFlow)
				weakenedWith(t, "weaken, then rename", func() *Weakening { f.do(c.op("charge")); return f.do(rn("charge", "charge_card")) }(), c.want)
				// Undoing the weakening leaves the rename, which is not one.
				notWeakened(t, "undo of the weakening", e.back())
			})
		})
	}
}

// An undone rename is no longer a rename: the text is the one before it, and the list is the one before.
func TestRenameUndone(t *testing.T) {
	eachMode(t, renameFlow, func(t *testing.T, kind string, s *Server) {
		e := newSession(t, s, renameFlow)
		e.do(rn("charge", "charge_card"))
		w := e.back()
		notWeakened(t, "after undo", w)
		if e.text != renameFlow || len(e.renames) != 0 || len(w.Renamed) != 0 {
			t.Errorf("undo left text changed %v, renames %v, honoured %v", e.text != renameFlow, e.renames, w.Renamed)
		}
		// With the rename undone, a hand edit that renames the node in the source is a rename the
		// editor was not told of, and reads as it always did (here the classifier's own pairing of an
		// unchanged node still sees it, so lower the need as well: no pairing can hide that).
		hand := strings.NewReplacer("charge]", "charge_card]", "[charge,", "[charge_card,", "name: charge\n", "name: charge_card\n", "to charge ", "to charge_card ").Replace(renameFlow)
		hand = strings.Replace(hand, "need: 2", "need: 1", 1)
		weakenedWith(t, "a hand-typed rename and a lowered need", e.typeText(hand), "charge")
		// Rename, lower the need, undo twice, redo-by-hand is not needed: the stack restores each list.
		f := newSession(t, s, renameFlow)
		f.do(rn("charge", "charge_card"))
		f.do(Operation{Op: "set_approval", Node: "charge_card", Approval: gate(1, []string{"ops", "finance", "risk"}, true, "24h")})
		f.back()
		if !slices.Equal(f.renames, []Rename{{"charge", "charge_card"}}) {
			t.Errorf("one undo should keep the rename: %v", f.renames)
		}
		notWeakened(t, "rename kept, need restored", f.verdict())
		f.back()
		if len(f.renames) != 0 {
			t.Errorf("two undos should drop the rename: %v", f.renames)
		}
	})
}

// A node renamed twice is matched to its baseline self under the last name, and renames compose: two
// names swapped through a third are the same graph.
func TestChainedRenames(t *testing.T) {
	eachMode(t, renameFlow, func(t *testing.T, kind string, s *Server) {
		e := newSession(t, s, renameFlow)
		e.do(rn("charge", "charge_card"))
		w := e.do(rn("charge_card", "charge_final"))
		notWeakened(t, "charge -> charge_card -> charge_final", w)
		if !slices.Equal(w.Renamed, []string{"charge -> charge_card", "charge_card -> charge_final"}) {
			t.Errorf("renamed %q", w.Renamed)
		}
		weakenedWith(t, "chained, then a lowered need", e.do(Operation{Op: "set_approval", Node: "charge_final", Approval: gate(1, []string{"ops", "finance", "risk"}, true, "24h")}), "approval 2 of 3")
		// The names of two nodes swapped through a third: the graph is the baseline's, and the
		// baseline's gate is on the node that now has the other name.
		f := newSession(t, s, renameFlow)
		f.do(rn("charge", "tmp"))
		f.do(rn("audit", "charge"))
		w = f.do(rn("tmp", "audit"))
		notWeakened(t, "charge and audit swapped", w)
		// ... and a chain through a name the baseline had, ending on a fresh one.
		g := newSession(t, s, renameFlow)
		g.do(rn("audit", "audit2"))
		g.do(rn("charge", "audit"))
		notWeakened(t, "audit renamed away, charge renamed onto its name", g.do(rn("audit", "charge_card")))
	})
}

// A rename onto a name the baseline has for a different node is not a rename of that node: the node
// whose name was taken is gone, and the node that took it is judged as the node that had the name.
func TestRenameOntoABaselineNameIsWeakened(t *testing.T) {
	eachMode(t, renameFlow, func(t *testing.T, kind string, s *Server) {
		e := newSession(t, s, renameFlow)
		// audit is removed (its edges with it), then the gated charge is renamed to audit: the text has no
		// charge and an audit that is the gated node, wired as charge was.
		var dry struct {
			Edited
			Refused string `json:"refused"`
		}
		e.post("/api/apply", map[string]any{"text": e.text, "operation": Operation{Op: "remove_node", Node: "audit", DryRun: true}, "renames": e.renames}, &dry)
		e.do(Operation{Op: "remove_node", Node: "audit", Confirm: dry.Cascade})
		w := e.do(rn("charge", "audit"))
		if len(w.Renamed) != 0 {
			t.Errorf("a rename onto a name the baseline holds was honoured: %q", w.Renamed)
		}
		weakenedWith(t, "charge renamed onto audit's name", w, "charge")
		// The same when audit is not removed but renamed away first and a third node takes its name back:
		// that is a legitimate chain (see TestChainedRenames); the rename onto a name that is still a
		// different baseline node is refused by the editor itself.
		f := newSession(t, s, renameFlow)
		var out struct {
			Refused string `json:"refused"`
		}
		f.post("/api/apply", map[string]any{"text": f.text, "operation": rn("charge", "audit")}, &out)
		if out.Refused == "" {
			t.Errorf("the editor let a node be renamed onto another node's name")
		}
	})
}

// The list is a hint the server checks, not a claim it takes: renames that name nothing, repeat, or
// come in thousands change no verdict that the text does not earn.
func TestRenamesAreOnlyAHint(t *testing.T) {
	eachMode(t, renameFlow, func(t *testing.T, kind string, s *Server) {
		lowered := strings.Replace(renameFlow, "need: 2", "need: 1", 1)
		for name, rs := range map[string][]Rename{
			"unknown node":         {{"nobody", "charge"}},
			"onto an existing one": {{"audit", "charge"}, {"charge", "audit"}},
			"identity":             {{"charge", "charge"}},
			"empty names":          {{"", ""}},
		} {
			w := s.compareRenamed([]byte(lowered), rs)
			weakenedWith(t, name, w, "approval 2 of 3")
			if len(w.Renamed) != 0 {
				t.Errorf("%s: honoured %q", name, w.Renamed)
			}
		}
		many := make([]Rename, maxRenames+1)
		for i := range many {
			many[i] = Rename{"charge", "charge"}
		}
		many[0] = Rename{"charge", "charge_card"}
		text := strings.NewReplacer("name: charge\n", "name: charge_card\n", "[validate, charge]", "[validate, charge_card]", "[charge, audit]", "[charge_card, audit]", "to charge ", "to charge_card ").Replace(renameFlow)
		if w := s.compareRenamed([]byte(text), many[:2]); len(w.Weakened) != 0 {
			t.Errorf("a short list: %+v", w)
		}
		if w := s.compareRenamed([]byte(text), many); w.Renamed != nil {
			t.Errorf("a list past the cap was honoured: %q", w.Renamed)
		}
	})
}

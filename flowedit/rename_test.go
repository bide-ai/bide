package flowedit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
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

// insertBefore adds an ungated step between validate and node, which the classifier alone cannot pair
// a renamed node across (its edges are not the ones it had) and which weakens nothing.
func (e *session) insertBefore(node string) *Weakening {
	e.t.Helper()
	e.do(Operation{Op: "add_node", Node: "prep", Value: "validate"})
	e.do(Operation{Op: "set_safety", Node: "prep", Value: "readonly"})
	e.do(Operation{Op: "add_edge", Node: "prep", To: node})
	e.do(Operation{Op: "remove_edge", Node: "validate", To: node})
	return e.do(Operation{Op: "add_edge", Node: "validate", To: "prep"})
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
		// The text that follows is one the classifier alone reads as weakened (it cannot pair the renamed
		// node once its edges differ), so each answer below depends on the list.
		notWeakened(t, "a rename and a step before the gate", e.insertBefore("charge_card"))
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
		w := e.insertBefore("charge_card")
		notWeakened(t, "a rename and a step before the gated node", w)
		// Rename the node beside it too.
		w = e.do(rn("audit", "ledger"))
		notWeakened(t, "two renames and a step before the gate", w)
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
		f.insertBefore("charge_card")
		f.do(Operation{Op: "set_approval", Node: "charge_card", Approval: gate(1, []string{"ops", "finance", "risk"}, true, "24h")})
		w = f.back()
		notWeakened(t, "undo to a text only the rename explains", w)
		if !slices.Equal(f.renames, []Rename{{"charge", "charge_card"}}) {
			t.Errorf("one undo should keep the rename: %v", f.renames)
		}
		notWeakened(t, "rename kept, need restored", f.verdict())
		for len(f.undo) > 1 {
			f.back()
			if len(f.renames) != 1 {
				t.Errorf("the rename is made until the text before it comes back: %v at %d steps", f.renames, len(f.undo))
			}
		}
		f.back()
		if len(f.renames) != 0 || f.text != renameFlow {
			t.Errorf("the last undo should drop the rename: %v", f.renames)
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

// replayByOperations is the baseline renamed one rename_node operation at a time, each a parse and a
// splice of the text: the slow path the document replay replaced, kept as the oracle of what is refused
// and what is rewritten.
func replayByOperations(t *testing.T, name, baseline string, renames []Rename) (string, []Rename) {
	t.Helper()
	text := []byte(baseline)
	var made []Rename
	for _, r := range renames {
		res, err := ApplyOperation(name, text, Operation{Op: "rename_node", Node: r.From, Value: r.To})
		if err != nil || !res.Changed {
			continue
		}
		text = res.Text
		made = append(made, r)
	}
	return string(text), made
}

func normalJSON(t *testing.T, name string, data []byte, isJSON bool) any {
	t.Helper()
	if formatOf(name) == "yaml" && !isJSON {
		var err error
		if data, err = ToJSON(data); err != nil {
			t.Fatal(err)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The document replay refuses what the operations refuse and rewrites what they rewrite: on flows with
// a switch, a join, a quoted name with a space and assertions, over renames that are valid, chained,
// swapped, onto a node's or a join's name, of a join, of nothing, to a name that does not print, and to
// a name an assertion cannot hold, the renamed baseline is the one the operations make.
func TestRenameReplayAgreesWithTheOperations(t *testing.T) {
	lists := map[string][]Rename{
		"valid":             {{"a", "a2"}, {"e", "e2"}},
		"chained":           {{"a", "x"}, {"x", "y"}, {"y", "z"}},
		"swapped":           {{"a", "tmp"}, {"e", "a"}, {"tmp", "e"}},
		"onto a node":       {{"a", "b"}, {"c", "d"}},
		"onto a join":       {{"a", "j"}},
		"of a join":         {{"j", "k"}},
		"of nothing":        {{"zz", "yy"}, {"", "q"}},
		"identity":          {{"a", "a"}},
		"unprintable":       {{"a", "a‮b"}, {"b", " b"}, {"c", ""}},
		"a name with quote": {{"e", `e"x`}},
		"spaced":            {{"e", "e e"}, {"e e", "e3"}},
		"a loop target":     {{"b", "b2"}, {"d", "d2"}},
	}
	// dupFlow names two nodes alike (a text in progress can): the operations refuse to rename either.
	dupFlow := strings.Replace(joinFlow, "{name: e, block: e}", "{name: a, block: e}", 1)
	flows := map[string]string{"join.yaml": joinFlow, "struct.yaml": structFlow, "dup.yaml": dupFlow}
	for file, flow := range flows {
		for name, rs := range lists {
			t.Run(file+"/"+name, func(t *testing.T) {
				if file == "struct.yaml" {
					rs = rs[:0:0]
					for _, r := range lists[name] {
						rs = append(rs, Rename{strings.NewReplacer("a", "validate", "b", "charge", "c", "receipt", "d", "manual review", "e", "charge").Replace(r.From), r.To})
					}
				}
				want, wantMade := replayByOperations(t, file, flow, rs)
				got, isJSON, gotMade := renameBaseline([]byte(flow), rs, true)
				if !slices.Equal(gotMade, wantMade) {
					t.Errorf("made %v, the operations made %v", gotMade, wantMade)
				}
				if !reflect.DeepEqual(normalJSON(t, file, got, isJSON), normalJSON(t, file, []byte(want), false)) {
					t.Errorf("the renamed baseline differs from the operations' result (made %v):\n%s\nwant\n%s", gotMade, got, want)
				}
			})
		}
	}
}

// A long chain of steps renamed many times is not slow: the baseline is decoded once and the renames are
// applied to that document. (Replaying each rename as an operation took about 0.3 s a rename on a file of
// this size, a minute for the cap.)
func TestRenameReplayIsFast(t *testing.T) {
	var b strings.Builder
	b.WriteString("version: 1\nflow: chain\nentry: n0\nnodes:\n")
	const nodes = 1500
	for i := 0; i < nodes; i++ {
		fmt.Fprintf(&b, "  - {name: n%04d, block: step, safety: idempotent}\n", i)
	}
	b.WriteString("  - name: gate\n    block: step\n    safety: side_effect\n    approval: {need: 2, approvers: [ops, finance, risk]}\nwiring:\n")
	for i := 0; i+1 < nodes; i++ {
		fmt.Fprintf(&b, "  - edge: [n%04d, n%04d]\n", i, i+1)
	}
	fmt.Fprintf(&b, "  - edge: [n%04d, gate]\nassert:\n  - every path to gate passes approval >= 2 of 3\n", nodes-1)
	baseline := b.String()
	if len(baseline) < 90_000 {
		t.Fatalf("the flow is %d bytes, wanted about 100 KB", len(baseline))
	}
	renames := make([]Rename, maxRenames)
	text := baseline
	for i := range renames {
		renames[i] = Rename{fmt.Sprintf("n%04d", i), fmt.Sprintf("m%04d", i)}
		text = strings.NewReplacer(renames[i].From+",", renames[i].To+",", renames[i].From+"]", renames[i].To+"]", renames[i].From+" ", renames[i].To+" ").Replace(text)
	}
	renames[0] = Rename{"gate", "payment_gate"}
	text = strings.NewReplacer("gate", "payment_gate", "to payment_gate", "to payment_gate").Replace(text)
	start := time.Now()
	_, _, made := renameBaseline([]byte(baseline), renames, true)
	if took := time.Since(start); took > time.Second {
		t.Errorf("%d renames of a %d byte flow took %s", len(renames), len(baseline), took)
	}
	if len(made) != len(renames) {
		t.Errorf("%d of %d renames were made", len(made), len(renames))
	}
	// And a whole comparison with them is fast too, and finds nothing weaker.
	start = time.Now()
	w := compare("chain.yaml", "test", []byte(baseline), []byte(text), nil, renames)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the comparison took %s", took)
	}
	if len(w.Renamed) != len(renames) {
		t.Errorf("the verdict honoured %d renames: %v", len(w.Renamed), w.CompareError)
	}
}

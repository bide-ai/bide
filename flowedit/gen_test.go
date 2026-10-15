package flowedit

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// The generator writes random flow configs as text, in many styles, with comments and blank
// lines in many positions. It models what a person writes, not only what the examples show.

type style struct {
	indent      int
	nodesFlow   bool // each node is a flow mapping
	approvalFlw bool // the approval block is a flow mapping
	listsFlow   bool // approvers, edge pairs, inputs are flow sequences
	armsFlow    bool // switch arms are flow mappings
	dedentSeq   bool // "key:\n- item" rather than "key:\n  - item"
	quote       int  // 0 plain, 1 single, 2 double, 3 mixed
	comments    float64
	trailing    float64
	blanks      float64
	docStart    bool
	shuffleTop  bool
}

func randStyle(r *rand.Rand) style {
	s := style{
		indent:      []int{2, 2, 2, 4}[r.IntN(4)],
		nodesFlow:   r.IntN(2) == 0,
		approvalFlw: r.IntN(2) == 0,
		listsFlow:   r.IntN(3) != 0,
		armsFlow:    r.IntN(2) == 0,
		dedentSeq:   r.IntN(5) == 0,
		quote:       r.IntN(4),
		docStart:    r.IntN(6) == 0,
		shuffleTop:  r.IntN(3) == 0,
	}
	switch r.IntN(3) {
	case 1:
		s.comments, s.trailing, s.blanks = 0.08, 0.1, 0.08
	case 2:
		s.comments, s.trailing, s.blanks = 0.3, 0.3, 0.25
	}
	return s
}

type emitter struct {
	r     *rand.Rand
	st    style
	lines []string
}

func (e *emitter) str(s string) string {
	q := e.st.quote
	if q == 3 {
		q = e.r.IntN(3)
	}
	needs := strings.ContainsAny(s, ":#,[]{}\"'") || strings.ContainsAny(s[:min(1, len(s))], "-&*!|>%@`") || strings.TrimSpace(s) != s || s == ""
	switch {
	case q == 1 || (needs && strings.Contains(s, "\"") && !strings.Contains(s, "'") ):
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	case q == 2 || needs:
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	return s
}

func (e *emitter) flow(v any) string {
	switch x := v.(type) {
	case Map:
		p := make([]string, len(x))
		for i, kv := range x {
			p[i] = kv.Key + ": " + e.flow(kv.Val)
		}
		return "{" + strings.Join(p, ", ") + "}"
	case []any:
		p := make([]string, len(x))
		for i, it := range x {
			p[i] = e.flow(it)
		}
		return "[" + strings.Join(p, ", ") + "]"
	case string:
		s := e.str(x)
		if strings.ContainsAny(s, ",[]{}") && !strings.HasPrefix(s, `"`) && !strings.HasPrefix(s, "'") {
			s = `"` + s + `"`
		}
		return s
	}
	return fmt.Sprint(v)
}

func (e *emitter) add(pad int, s string) { e.lines = append(e.lines, strings.Repeat(" ", pad)+s) }

// entry writes "key: value" at pad. flowKeys lists keys whose composite values are written inline.
func (e *emitter) entry(pad int, key string, v any, inline bool) {
	switch x := v.(type) {
	case Map:
		if inline || len(x) == 0 {
			e.add(pad, key+": "+e.flow(v))
			return
		}
		e.add(pad, key+":")
		for _, kv := range x {
			e.entry(pad+e.st.indent, kv.Key, kv.Val, e.st.approvalFlw && kv.Key == "approvers" && false || e.st.listsFlow && isList(kv.Val))
		}
	case []any:
		if inline || len(x) == 0 {
			e.add(pad, key+": "+e.flow(v))
			return
		}
		e.add(pad, key+":")
		p := pad + e.st.indent
		if e.st.dedentSeq {
			p = pad
		}
		itemsInline := (key == "nodes" || key == "wiring") && e.st.nodesFlow || key == "when" && e.st.armsFlow
		for _, it := range x {
			e.item(p, it, itemsInline)
		}
	default:
		e.add(pad, key+": "+e.flow(v))
	}
}

func isList(v any) bool { _, ok := v.([]any); return ok }

// item writes "- value" with the dash at pad.
func (e *emitter) item(pad int, v any, inline bool) {
	m, ok := v.(Map)
	if !ok || inline || len(m) == 0 {
		e.add(pad, "- "+e.flow(v))
		return
	}
	before := len(e.lines)
	for i, kv := range m {
		inl := e.st.listsFlow && isList(kv.Val) && (kv.Key != "when" || e.st.armsFlow)
		if mm, isM := kv.Val.(Map); isM && kv.Key == "approval" {
			_ = mm
			inl = e.st.approvalFlw
		}
		e.entry(pad+2, kv.Key, kv.Val, inl)
		if i == 0 {
			e.lines[before] = strings.Repeat(" ", pad) + "- " + strings.TrimPrefix(e.lines[before], strings.Repeat(" ", pad+2))
		}
	}
}

func genConfig(r *rand.Rand) string {
	st := randStyle(r)
	e := &emitter{r: r, st: st}
	n := 3 + r.IntN(6)
	names := make([]string, n)
	for i := range names {
		names[i] = []string{"n%d", "step-%d", "do_%d", "send receipt %d"}[r.IntN(4)]
		names[i] = fmt.Sprintf(names[i], i)
	}
	var nodes []any
	for _, nm := range names {
		m := Map{{"name", nm}, {"block", "b" + nm}}
		if r.IntN(3) == 0 {
			m = append(m, KV{"safety", []string{"readonly", "idempotent", "side_effect"}[r.IntN(3)]})
		}
		if r.IntN(3) == 0 {
			ap := []string{"ops", "finance", "risk", "legal"}[:2+r.IntN(3)]
			items := make([]any, len(ap))
			for i, a := range ap {
				items[i] = a
			}
			a := Map{{"need", 1 + r.IntN(len(ap))}, {"approvers", items}}
			if r.IntN(2) == 0 {
				a = append(a, KV{"exclude_principal", true})
			}
			if r.IntN(3) == 0 {
				a = append(a, KV{"timeout", "48h"})
			}
			m = append(m, KV{"approval", a})
		}
		nodes = append(nodes, m)
	}
	var wiring []any
	for i := 0; i+1 < n; i++ {
		switch r.IntN(3) {
		case 0:
			wiring = append(wiring, Map{{"edge", []any{names[i], names[i+1]}}})
		case 1:
			arm := Map{{"pred", "p" + fmt.Sprint(i)}, {"to", names[i+1]}}
			if r.IntN(4) == 0 && i > 0 {
				arm = Map{{"pred", "again"}, {"to", names[i-1]}, {"loop_max", 3}}
			}
			wiring = append(wiring, Map{{"switch", names[i]}, {"when", []any{arm}}, {"else", names[min(i+2, n-1)]}})
		default:
			wiring = append(wiring, Map{{"join", "j" + fmt.Sprint(i)}, {"inputs", []any{names[i], names[i+1]}}, {"merge", "m"}})
		}
	}
	var asserts []any
	for i := 0; i < r.IntN(4); i++ {
		nm := names[r.IntN(n)]
		if strings.Contains(nm, " ") {
			nm = `"` + nm + `"`
		}
		asserts = append(asserts, []string{nm + " reachable", "no side effect after " + nm, "every path to " + nm + " passes approval >= 2 of 3"}[r.IntN(3)])
	}
	top := Map{{"version", 1}, {"flow", "gen"}}
	if r.IntN(2) == 0 {
		top = append(top, KV{"in", "main.In"}, KV{"out", "main.Out"})
	}
	top = append(top, KV{"entry", names[0]}, KV{"nodes", nodes}, KV{"wiring", wiring})
	if len(asserts) > 0 {
		top = append(top, KV{"assert", asserts})
	}
	if st.shuffleTop {
		r.Shuffle(len(top), func(i, j int) { top[i], top[j] = top[j], top[i] })
	}
	if st.docStart {
		e.lines = append(e.lines, "---")
	}
	for _, kv := range top {
		inl := false
		e.entry(0, kv.Key, kv.Val, inl)
	}
	// nodes and wiring items: re-emit with the style rules (entry() writes seq items via item()).
	return e.decorate(r)
}

// decorate adds comment lines, trailing comments and blank lines.
func (e *emitter) decorate(r *rand.Rand) string {
	var out []string
	if r.IntN(3) == 0 {
		out = append(out, "# yaml-language-server: $schema=https://example.com/flow.schema.json")
	}
	for i, l := range e.lines {
		pad := len(l) - len(strings.TrimLeft(l, " "))
		if i > 0 && l != "---" {
			if r.Float64() < e.st.comments {
				out = append(out, strings.Repeat(" ", pad)+"# "+[]string{"the gated one", "see the policy", "digest order", "TODO"}[r.IntN(4)])
			}
			if r.Float64() < e.st.blanks {
				out = append(out, "")
			}
		}
		if r.Float64() < e.st.trailing && !strings.HasSuffix(l, ":") && l != "---" && !strings.HasSuffix(l, "|") {
			l += "  # note"
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n") + "\n"
}

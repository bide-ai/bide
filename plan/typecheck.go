package plan

import (
	"errors"
	"reflect"
)

// typeProblems are the type problems of a config against a Registry, by kind, each with its Finding. They
// are found from the config and the registered types alone, for every part whose names resolve, so another
// error in the config (an unknown name, a safety above the registered one, a drifted block) does not hide
// them: a caller that compares reports (the typed editor) must see a type problem beside any other error.
type typeProblems struct {
	edges     []Finding // an edge between steps of other types
	arms      []Finding // a switch arm whose predicate tests another type than the switched node gives
	joins     []Finding // a join input that does not fit its merge
	terminals []Finding // a reached terminal that gives another type than the flow's output
}

// all returns every finding, in the order edges, arms, joins, terminals.
func (p typeProblems) all() []Finding {
	out := append([]Finding(nil), p.edges...)
	out = append(out, p.arms...)
	out = append(out, p.joins...)
	return append(out, p.terminals...)
}

// typeNode is a node of the config as far as its types are known.
type typeNode struct {
	in, out reflect.Type
	join    bool
}

// checkTypes returns the type problems of cfg against reg, by reflect.Type identity. A part of the
// config whose names do not resolve is left to the checks that report the names.
func checkTypes(cfg *config, reg *Registry) typeProblems {
	var p typeProblems
	nodes := map[string]typeNode{}
	var order []string // declared steps in order, then joins in wiring order: the order Build registers them
	for _, n := range cfg.Nodes {
		b, ok := reg.blocks[n.Block]
		if _, dup := nodes[n.Name]; !ok || dup {
			continue
		}
		nodes[n.Name] = typeNode{in: b.inType, out: b.outType}
		order = append(order, n.Name)
	}
	for _, w := range cfg.Wiring {
		if !w.isJoin() {
			continue
		}
		m, ok := reg.merges[w.Merge]
		if _, dup := nodes[w.Join]; !ok || dup {
			continue
		}
		nodes[w.Join] = typeNode{out: m.outType, join: true}
		order = append(order, w.Join)
	}

	hasOut := map[string]bool{}
	next := map[string][]string{}
	for i, w := range cfg.Wiring {
		switch {
		case w.isEdge() && len(w.Edge) == 2:
			from, to := w.Edge[0], w.Edge[1]
			next[from] = append(next[from], to)
			hasOut[from] = true
			f, fok := nodes[from]
			t, tok := nodes[to]
			if fok && tok && !t.join && f.out != t.in {
				p.edges = append(p.edges, found(ProblemEdgeTypes, []string{from, to}, typeName(f.out), typeName(t.in), "edge %q -> %q connects %s to %s (types must match exactly)",
					from, to, typeName(f.out), typeName(t.in)))
			}
		case w.isJoin():
			m, ok := reg.merges[w.Merge]
			for j, src := range w.Inputs {
				next[src] = append(next[src], w.Join)
				hasOut[src] = true
				in, iok := nodes[src]
				if ok && iok && j < len(m.inTypes) && m.inTypes[j] != nil && in.out != nil && in.out != m.inTypes[j] {
					p.joins = append(p.joins, found(ProblemJoinInputTypes, []string{w.Join, src}, typeName(in.out), typeName(m.inTypes[j]), "join %q input %q produces %s but the merge expects %s",
						w.Join, src, typeName(in.out), typeName(m.inTypes[j])))
				}
			}
		case w.isSwitch():
			hasOut[w.Switch] = true
			over, ok := nodes[w.Switch]
			for j, a := range w.When {
				next[w.Switch] = append(next[w.Switch], a.To)
				if pr, pok := reg.preds[a.Pred]; ok && pok && pr.mType != over.out {
					p.arms = append(p.arms, found(ProblemArmTypes, []string{w.Switch, a.Pred, a.To}, typeName(pr.mType), typeName(over.out), "wiring[%d].when[%d] predicate %q tests %s but switched node %q produces %s",
						i, j, a.Pred, typeName(pr.mType), w.Switch, typeName(over.out)))
				}
			}
			if w.Else != "" {
				next[w.Switch] = append(next[w.Switch], w.Else)
			}
		}
	}

	// Terminals: Build reaches this check only for nodes reached from the entry (a node still to be
	// wired in is reported as not reached, and its types say nothing yet). The first terminal sets the
	// flow's output.
	entry := cfg.Entry
	if entry == "" && len(cfg.Nodes) > 0 {
		entry = cfg.Nodes[0].Name
	}
	reached := map[string]bool{}
	if _, ok := nodes[entry]; ok {
		queue := []string{entry}
		reached[entry] = true
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			for _, m := range next[n] {
				if !reached[m] {
					reached[m] = true
					queue = append(queue, m)
				}
			}
		}
	}
	var outType reflect.Type
	for _, name := range order {
		if !hasOut[name] && outType == nil {
			outType = nodes[name].out
		}
	}
	for _, name := range order {
		n := nodes[name]
		if hasOut[name] || !reached[name] {
			continue
		}
		if outType != nil && n.out != nil && n.out != outType {
			p.terminals = append(p.terminals, found(ProblemTerminalTypes, []string{name}, typeName(n.out), typeName(outType), "terminal step %q produces %s but the flow output is %s",
				name, typeName(n.out), typeName(outType)))
		}
	}
	return p
}

// safetyAboveRegistration returns a problem for each node whose stated safety is above what its
// block registers in Go (a config may only lower retry safety: a step marked safe to run twice that
// the program did not mark so is a false claim, which crash recovery would act on). It is found from
// the config and the registered blocks alone, so another error does not hide it.
func safetyAboveRegistration(cfg *config, reg *Registry) []Finding {
	var out []Finding
	seen := map[string]bool{}
	for _, n := range cfg.Nodes {
		b, ok := reg.blocks[n.Block]
		if !ok || n.Safety == "" || seen[n.Name] {
			continue
		}
		seen[n.Name] = true
		_, err := safetyFromConfig(b.safety, n.Safety)
		if above, ok := errors.AsType[*safetyAboveError](err); ok {
			out = append(out, found(ProblemSafetyAboveRegistration, []string{n.Name}, above.stated, above.registered, "node %q %v", n.Name, err))
		}
	}
	return out
}

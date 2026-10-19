package flowedit

import (
	"reflect"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// swap plans the exchange of the items op.Index and op.Index+1 of the sequence at op.Path: one
// splice over both items that writes the second, then what lay between them, then the first. An
// item is its own text: for a block sequence the lines from the comments directly above it to the
// end of its last line (a trailing comment included), for a flow sequence the item between its
// delimiters. What lies between the items, and after the second, is left alone, so a blank line or a
// comment that belongs to the file stays where it was.
func (d *doc) swap(op Op) (lo, hi int, frag string, err error) {
	l, err := d.locate(op.Path)
	if err != nil {
		return 0, 0, "", refuse("swap: %v", err)
	}
	p := l.node
	if err := plainNode(p); err != nil {
		return 0, 0, "", err
	}
	if p.Kind != yaml.SequenceNode {
		return 0, 0, "", refuse("swap: only the items of a list can be exchanged")
	}
	i := op.Index
	if i < 0 || i+1 >= len(p.Content) {
		return 0, 0, "", refuse("swap: there is no pair of items at %d", i)
	}
	a, b := p.Content[i], p.Content[i+1]
	if isFlow(p) {
		sa, sb := d.start(a), d.start(b)
		ea, err := d.end(a, true)
		if err != nil {
			return 0, 0, "", err
		}
		eb, err := d.end(b, true)
		if err != nil {
			return 0, 0, "", err
		}
		if ea > sb {
			return 0, 0, "", refuse("swap: the items overlap")
		}
		return sa, eb, string(d.src[sb:eb]) + string(d.src[ea:sb]) + string(d.src[sa:ea]), nil
	}
	span := func(k int) (int, int, error) {
		n := p.Content[k]
		line := n.Line - 1
		// The item must begin its line (after the dash): nothing else shares it.
		before := strings.TrimSpace(string(d.src[d.lineStart[line]:d.start(n)]))
		if before != "-" {
			return 0, 0, refuse("swap: item %d does not begin its own line", k)
		}
		e, err := d.entryEnd(p, k, false)
		if err != nil {
			return 0, 0, err
		}
		return d.lineStart[d.attached(line)], d.lineEnd(d.lineOf(e - 1)), nil
	}
	sa, ea, err := span(i)
	if err != nil {
		return 0, 0, "", err
	}
	sb, eb, err := span(i + 1)
	if err != nil {
		return 0, 0, "", err
	}
	if ea > sb {
		return 0, 0, "", refuse("swap: the items overlap")
	}
	return sa, eb, string(d.src[sb:eb]) + string(d.src[ea:sb]) + string(d.src[sa:ea]), nil
}

// verifySwap checks that out is old with two neighbouring items of one list exchanged and nothing
// else: the data (every other value and key order as it was), the lines (the result holds exactly the
// lines of the old text, and every line before the two items and after them is where it was), and the
// comments (each item has the comments directly above it that it had, and its first and last lines are
// as they were). It shares no code with swap beyond the parser.
func verifySwap(old, out []byte, op Op) error {
	do, err := parse(old)
	if err != nil {
		return err
	}
	dn, err := parse(out)
	if err != nil {
		return refuse("the result is not editable (%v)", err)
	}
	// the data
	var on, nn yaml.Node
	if err := yaml.Unmarshal(old, &on); err != nil {
		return err
	}
	if err := yaml.Unmarshal(out, &nn); err != nil {
		return refuse("the result does not parse (%v)", err)
	}
	want, err := toOrdered(on.Content[0])
	if err != nil {
		return err
	}
	if want, err = swapOrdered(want, op.Path, op.Index); err != nil {
		return err
	}
	got, err := toOrdered(nn.Content[0])
	if err != nil {
		return refuse("the result: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		return refuse("the result is not the two items exchanged (values or key order)")
	}
	// the lines: the same lines, and the same before and after the pair
	ol, nl := strings.Split(string(old), "\n"), strings.Split(string(out), "\n")
	if len(ol) != len(nl) {
		return refuse("the exchange changed the number of lines")
	}
	seqOld, err := nodeAt(do.root, op.Path)
	if err != nil {
		return err
	}
	seqNew, err := nodeAt(dn.root, op.Path)
	if err != nil {
		return err
	}
	if len(seqOld.Content) != len(seqNew.Content) {
		return refuse("the list changed length")
	}
	first := headLine(ol, seqOld.Content[op.Index])
	last := lastLine(seqOld.Content[op.Index+1])
	// The second item may end on a line no node starts on (a closing bracket, a block scalar's text).
	if e, err := do.entryEnd(seqOld, op.Index+1, isFlow(seqOld)); err == nil && e > 0 {
		last = max(last, do.lineOf(e-1))
	}
	if isFlow(seqOld) {
		// The items share lines with their neighbours and with the brackets, so the result is checked
		// as bytes: the same bytes in another order.
		if !sameBytes(old, out) {
			return refuse("the exchange is not a permutation of the old bytes")
		}
		first = seqOld.Content[op.Index].Line - 1
	} else if !sameLines(ol, nl) {
		return refuse("the exchange is not a permutation of the old lines")
	}
	for k := 0; k < first; k++ {
		if ol[k] != nl[k] {
			return refuse("the exchange changed a line before the two items")
		}
	}
	for k := last + 1; k < len(ol); k++ {
		if ol[k] != nl[k] {
			return refuse("the exchange changed a line after the two items")
		}
	}
	// the comments: each item is the item it was, with the comments above it and its own lines
	if isFlow(seqOld) {
		return nil // a flow list has no comment lines to own; the line checks above are the check
	}
	sigma := func(k int) int {
		switch k {
		case op.Index:
			return op.Index + 1
		case op.Index + 1:
			return op.Index
		}
		return k
	}
	for k := range seqOld.Content {
		o, n := seqOld.Content[k], seqNew.Content[sigma(k)]
		if !reflect.DeepEqual(headComments(ol, o), headComments(nl, n)) {
			return refuse("an item lost or gained a comment above it")
		}
		if ol[o.Line-1] != nl[n.Line-1] || ol[lastLine(o)] != nl[lastLine(n)] {
			return refuse("an item's first or last line changed")
		}
	}
	return nil
}

func sameBytes(a, b []byte) bool {
	var x, y [256]int
	for _, c := range a {
		x[c]++
	}
	for _, c := range b {
		y[c]++
	}
	return x == y
}

func sameLines(a, b []string) bool {
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return reflect.DeepEqual(x, y)
}

// headLine is the first line (0-based) of n's own text: the comment lines directly above it count.
func headLine(lines []string, n *yaml.Node) int {
	l := n.Line - 1
	for l > 0 && strings.HasPrefix(strings.TrimLeft(lines[l-1], " \t"), "#") {
		l--
	}
	return l
}

// headComments are the comment lines directly above n, in order.
func headComments(lines []string, n *yaml.Node) []string {
	l := n.Line - 1
	var c []string
	for l > 0 && strings.HasPrefix(strings.TrimLeft(lines[l-1], " \t"), "#") {
		l--
		c = append([]string{lines[l]}, c...)
	}
	return c
}

// lastLine is the last line (0-based) on which n or anything inside it starts.
func lastLine(n *yaml.Node) int {
	m := n.Line - 1
	for _, c := range n.Content {
		if l := lastLine(c); l > m {
			m = l
		}
	}
	return m
}

func nodeAt(root *yaml.Node, path []any) (*yaml.Node, error) {
	cur := root
	for _, p := range path {
		switch p := p.(type) {
		case string:
			found := false
			if cur.Kind == yaml.MappingNode {
				for i := 0; i+1 < len(cur.Content); i += 2 {
					if cur.Content[i].Value == p {
						cur, found = cur.Content[i+1], true
						break
					}
				}
			}
			if !found {
				return nil, refuse("path: missing key %q", p)
			}
		case int:
			if cur.Kind != yaml.SequenceNode || p < 0 || p >= len(cur.Content) {
				return nil, refuse("path: index %d", p)
			}
			cur = cur.Content[p]
		}
	}
	return cur, nil
}

// swapOrdered is o with items i and i+1 of the list at path exchanged.
func swapOrdered(o ov, path []any, i int) (ov, error) {
	if len(path) == 0 {
		if o.kind != 'q' || i < 0 || i+1 >= len(o.items) {
			return ov{}, refuse("swap: no pair of items at %d", i)
		}
		items := append([]ov{}, o.items...)
		items[i], items[i+1] = items[i+1], items[i]
		o.items = items
		return o, nil
	}
	switch p := path[0].(type) {
	case string:
		if o.kind != 'm' {
			return ov{}, refuse("path")
		}
		for k, key := range o.keys {
			if key == p {
				c, err := swapOrdered(o.items[k], path[1:], i)
				if err != nil {
					return ov{}, err
				}
				items := append([]ov{}, o.items...)
				items[k] = c
				o.items = items
				return o, nil
			}
		}
	case int:
		if o.kind == 'q' && p >= 0 && p < len(o.items) {
			c, err := swapOrdered(o.items[p], path[1:], i)
			if err != nil {
				return ov{}, err
			}
			items := append([]ov{}, o.items...)
			items[p] = c
			o.items = items
			return o, nil
		}
	}
	return ov{}, refuse("path: missing")
}

package flowedit

import (
	"fmt"
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
	flow := isFlow(seqOld)
	if flow {
		// The items share lines with their neighbours and with the brackets, so the result is checked
		// as bytes: the same bytes in another order.
		if !sameBytes(old, out) {
			return refuse("the exchange is not a permutation of the old bytes")
		}
	} else if !sameLines(ol, nl) {
		return refuse("the exchange is not a permutation of the old lines")
	}
	// The text, cut at the parser's item boundaries into what each item owns (the comments directly
	// above it, its own lines, a trailing comment) and the gaps between them (and before the first
	// and after the last). Every gap must be as it was, in place, and each item must be the old item,
	// all its lines, wherever it now stands.
	ownOld, gapsOld, err := partition(do, ol, seqOld, flow)
	if err != nil {
		return err
	}
	ownNew, gapsNew, err := partition(dn, nl, seqNew, flow)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(gapsOld, gapsNew) {
		return refuse("the exchange changed what lies between or around the two items")
	}
	for k := range ownOld {
		j := k
		switch k {
		case op.Index:
			j = op.Index + 1
		case op.Index + 1:
			j = op.Index
		}
		if ownOld[k] != ownNew[j] {
			return refuse("an item's text, or a comment that belongs to it, changed")
		}
	}
	return nil
}

// partition cuts the text of a list into what each item owns and the gaps around them: gaps[0] is
// everything before the first item, gaps[k] what lies between item k-1 and item k, and gaps[len(own)]
// everything after the last. For a block list the pieces are whole lines, and an item owns the comment
// lines directly above it through the last line of its entry; for a flow list they are byte ranges
// of the text, an item being what lies between its own delimiters.
func partition(d *doc, lines []string, seq *yaml.Node, flow bool) (own []string, gaps []string, err error) {
	n := len(seq.Content)
	if flow {
		prev := 0
		for _, it := range seq.Content {
			s := d.start(it)
			e, err := d.end(it, true)
			if err != nil {
				return nil, nil, err
			}
			if s < prev || e < s {
				return nil, nil, refuse("the items overlap")
			}
			gaps = append(gaps, string(d.src[prev:s]))
			own = append(own, string(d.src[s:e]))
			prev = e
		}
		return own, append(gaps, string(d.src[prev:])), nil
	}
	next := 0
	for k := 0; k < n; k++ {
		first := headLine(lines, seq.Content[k])
		e, err := d.entryEnd(seq, k, false)
		if err != nil {
			return nil, nil, err
		}
		last := d.lineOf(e - 1)
		if first < next || last < first {
			return nil, nil, refuse("the items overlap")
		}
		gaps = append(gaps, lineBlock(lines[next:first]))
		own = append(own, lineBlock(lines[first:last+1]))
		next = last + 1
	}
	return own, append(gaps, lineBlock(lines[next:])), nil
}

// lineBlock is lines as one comparable string that tells no lines from one empty line.
func lineBlock(lines []string) string {
	return fmt.Sprintf("%d|%s", len(lines), strings.Join(lines, "\n"))
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

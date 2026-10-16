package flowedit

import (
	"fmt"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Verify checks that out is old with op applied and nothing else: the data, the key order, the
// lines outside the operated entry, and which entry each comment belongs to. Apply runs it on
// every result; it is exported so that a test can hand it a hostile output.
//
// The checks, each of which refuses an output:
//  1. the result parses and is editable (single document, no CRLF);
//  2. its data, in document order, is the old data with the operation applied: a map gets a new
//     key at the end, a sequence the item at its index, nothing else is reordered;
//  3. every line outside the lines the operation owns is the same line (before them from the start,
//     after them from the end);
//  4. every comment keeps its owner: a comment that sits directly above an entry, or trails it,
//     belongs to that entry; only the operated entry's own comments may disappear (on Delete), and
//     no comment may appear, move to another entry, or be left without its entry.
func Verify(old, out []byte, op Op) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = refuse("internal: unplaceable structure (%v)", r)
		}
	}()
	do, err := parse(old)
	if err != nil {
		return err
	}
	if _, err := parse(out); err != nil {
		return refuse("the result is not editable (%v)", err)
	}
	// 2: the data, in order
	var on yaml.Node
	if err := yaml.Unmarshal(old, &on); err != nil {
		return err
	}
	var nn yaml.Node
	if err := yaml.Unmarshal(out, &nn); err != nil {
		return refuse("the result does not parse (%v)", err)
	}
	want, err := toOrdered(on.Content[0])
	if err != nil {
		return err
	}
	want, err = applyOrdered(want, op.Path, op)
	if err != nil {
		return err
	}
	got, err := toOrdered(nn.Content[0])
	if err != nil {
		return refuse("the result: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		return refuse("the result is not the operation applied to the data (values or key order)")
	}
	// 3: the lines
	oldLines, newLines := strings.Split(string(old), "\n"), strings.Split(string(out), "\n")
	sp, entry, ok := do.spans(op)
	if !ok {
		return refuse("cannot work out the lines the operation owns")
	}
	if lineOutside(oldLines, newLines, sp) {
		return refuse("the result changes a line outside the entry it operates on")
	}
	// 4: the comments
	return commentOwnership(oldLines, newLines, op, entry, blockLines(oldLines, on.Content[0]), blockLines(newLines, nn.Content[0]))
}

// ---- 2: ordered data ----

type ov struct {
	kind  byte // 's' scalar, 'm' mapping, 'q' sequence
	v     any
	keys  []string
	items []ov
}

func toOrdered(n *yaml.Node) (ov, error) {
	switch n.Kind {
	case yaml.AliasNode:
		return toOrdered(n.Alias)
	case yaml.ScalarNode:
		var x any
		if err := n.Decode(&x); err != nil {
			return ov{}, err
		}
		return ov{kind: 's', v: x}, nil
	case yaml.SequenceNode:
		o := ov{kind: 'q', items: []ov{}}
		for _, c := range n.Content {
			e, err := toOrdered(c)
			if err != nil {
				return ov{}, err
			}
			o.items = append(o.items, e)
		}
		return o, nil
	case yaml.MappingNode:
		o := ov{kind: 'm', keys: []string{}, items: []ov{}}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode {
				return ov{}, refuse("complex key")
			}
			if k.Value == "<<" {
				return ov{}, refuse("merge key")
			}
			e, err := toOrdered(n.Content[i+1])
			if err != nil {
				return ov{}, err
			}
			o.keys = append(o.keys, k.Value)
			o.items = append(o.items, e)
		}
		return o, nil
	}
	return ov{}, refuse("node")
}

func fromValue(v any) ov {
	switch x := v.(type) {
	case Map:
		o := ov{kind: 'm', keys: []string{}, items: []ov{}}
		for _, kv := range x {
			o.keys = append(o.keys, kv.Key)
			o.items = append(o.items, fromValue(kv.Val))
		}
		return o
	case []any:
		o := ov{kind: 'q', items: []ov{}}
		for _, e := range x {
			o.items = append(o.items, fromValue(e))
		}
		return o
	}
	return ov{kind: 's', v: v}
}

func applyOrdered(o ov, path []any, op Op) (ov, error) {
	if len(path) == 0 {
		switch op.Kind {
		case Set:
			return fromValue(op.Value), nil
		case Insert:
			switch o.kind {
			case 'm':
				for _, k := range o.keys {
					if k == op.Key {
						return ov{}, refuse("key exists")
					}
				}
				o.keys = append(append([]string{}, o.keys...), op.Key)
				o.items = append(append([]ov{}, o.items...), fromValue(op.Value))
				return o, nil
			case 'q':
				if op.Index < 0 || op.Index > len(o.items) {
					return ov{}, refuse("index")
				}
				items := append([]ov{}, o.items[:op.Index]...)
				items = append(items, fromValue(op.Value))
				o.items = append(items, o.items[op.Index:]...)
				return o, nil
			}
		}
		return ov{}, refuse("operation does not apply here")
	}
	last := len(path) == 1
	switch p := path[0].(type) {
	case string:
		if o.kind != 'm' {
			return ov{}, refuse("path")
		}
		for i, k := range o.keys {
			if k != p {
				continue
			}
			keys, items := append([]string{}, o.keys...), append([]ov{}, o.items...)
			switch {
			case last && op.Kind == Delete:
				keys, items = append(keys[:i], keys[i+1:]...), append(items[:i], items[i+1:]...)
			case last && op.Kind == Set:
				items[i] = fromValue(op.Value)
			default:
				c, err := applyOrdered(items[i], path[1:], op)
				if err != nil {
					return ov{}, err
				}
				items[i] = c
			}
			o.keys, o.items = keys, items
			return o, nil
		}
		if last && op.Kind == Set {
			o.keys = append(append([]string{}, o.keys...), p)
			o.items = append(append([]ov{}, o.items...), fromValue(op.Value))
			return o, nil
		}
		return ov{}, refuse("path: missing key")
	case int:
		if o.kind != 'q' || p < 0 || p >= len(o.items) {
			return ov{}, refuse("path")
		}
		items := append([]ov{}, o.items...)
		switch {
		case last && op.Kind == Delete:
			items = append(items[:p], items[p+1:]...)
		case last && op.Kind == Set:
			items[p] = fromValue(op.Value)
		default:
			c, err := applyOrdered(items[p], path[1:], op)
			if err != nil {
				return ov{}, err
			}
			items[p] = c
		}
		o.items = items
		return o, nil
	}
	return ov{}, refuse("path element")
}

// ---- 3: the lines an operation owns ----

type lineSpan struct{ lo, hi int } // 0-based, inclusive

// span works out, from the old text's node start lines alone, which lines the operation may
// change. An entry owns the comment lines directly above it, its own lines, and the lines after it
// up to the comments directly above the next entry.
func (d *doc) span(op Op) (lineSpan, bool) {
	sp, _, ok := d.spans(op)
	return sp, ok
}

// spans is span with, for a Delete, the lines of the deleted entry itself.
func (d *doc) spans(op Op) (lineSpan, lineSpan, bool) {
	lines := strings.Split(string(d.src), "\n")
	var all []*yaml.Node
	var collect func(*yaml.Node)
	collect = func(x *yaml.Node) {
		all = append(all, x)
		for _, c := range x.Content {
			collect(c)
		}
	}
	collect(d.root)
	maxStart := func(x *yaml.Node) (int, int) {
		l, c := x.Line, x.Column
		var rec func(*yaml.Node)
		rec = func(y *yaml.Node) {
			if y.Line > l || (y.Line == l && y.Column > c) {
				l, c = y.Line, y.Column
			}
			for _, k := range y.Content {
				rec(k)
			}
		}
		rec(x)
		return l, c
	}
	headLo := func(l0 int) int {
		for l0 > 0 && l0-1 < len(lines) && strings.HasPrefix(strings.TrimLeft(lines[l0-1], " \t"), "#") {
			l0--
		}
		return l0
	}
	// ownEnd: the last line the entry owns, 0-based: its own last line, or the line before the
	// comments attached to the next node when that is later.
	ownEnd := func(x *yaml.Node, par int) int {
		l, c := maxStart(x)
		own := max(l-1, blockLast(lines, x))
		if isBlockScalar(x) {
			own = max(own, blockScalarLast(lines, x.Line-1, par))
		}
		next := -1
		for _, y := range all {
			if y.Line > l || (y.Line == l && y.Column > c) {
				if next == -1 || y.Line-1 < next {
					next = y.Line - 1
				}
			}
		}
		if next == -1 {
			return len(lines) - 1
		}
		return max(own, headLo(next)-1)
	}
	cur := d.root
	var key, parentKey, parent *yaml.Node
	for _, p := range op.Path {
		parent, parentKey, key = cur, key, nil
		switch p := p.(type) {
		case string:
			found := false
			for i := 0; i+1 < len(cur.Content); i += 2 {
				if cur.Content[i].Kind == yaml.ScalarNode && cur.Content[i].Value == p {
					key, cur, found = cur.Content[i], cur.Content[i+1], true
					break
				}
			}
			if !found {
				if op.Kind == Set {
					return d.spans(Op{Kind: Insert, Path: op.Path[:len(op.Path)-1], Key: p})
				}
				return lineSpan{}, lineSpan{}, false
			}
		case int:
			if cur.Kind != yaml.SequenceNode || p < 0 || p >= len(cur.Content) {
				return lineSpan{}, lineSpan{}, false
			}
			cur = cur.Content[p]
		}
	}
	for cur.Kind == yaml.AliasNode {
		return lineSpan{}, lineSpan{}, false
	}
	switch op.Kind {
	case Set:
		l := cur.Line - 1
		return lineSpan{l, l}, lineSpan{}, true
	case Delete:
		if parent == nil {
			return lineSpan{}, lineSpan{}, false
		}
		first := cur
		if key != nil {
			first = key
		}
		lo := headLo(first.Line - 1)
		hi := ownEnd(cur, parIndent(parent, key))
		dashKey := key != nil && parent.Content[0] == key && len(parent.Content) > 2 && !isFlow(parent) &&
			strings.Contains(lines[key.Line-1][:min(key.Column-1, len(lines[key.Line-1]))], "-")
		soleDash := key != nil && len(parent.Content) == 2 && !isFlow(parent) &&
			strings.Contains(lines[key.Line-1][:min(key.Column-1, len(lines[key.Line-1]))], "-")
		if soleDash {
			lo = key.Line - 1 // the item stays as {}, with its comments
		}
		if dashKey {
			// The first key of a "- key: v" item: the comments above are the item's, not the key's.
			lo = key.Line - 1
			hi = max(hi, parent.Content[2].Line-1)
		}
		if isFlow(parent) {
			ml, _ := maxStart(cur)
			hi = max(hi, ml-1)
			// The next entry may start on the line this one ends on; removing the entry joins them.
			for i, c := range parent.Content {
				if c == cur && i+1 < len(parent.Content) && parent.Content[i+1].Line-1 <= ml {
					hi = max(hi, parent.Content[i+1].Line-1)
				}
			}
			if parent.Kind == yaml.SequenceNode {
				for i, c := range parent.Content {
					if c == cur && i == len(parent.Content)-1 && i > 0 {
						pl, _ := maxStart(parent.Content[i-1])
						lo = min(lo, pl-1)
					}
				}
			} else {
				for i := 1; i < len(parent.Content); i += 2 {
					if parent.Content[i] == cur && i == len(parent.Content)-1 && i > 1 {
						pl, _ := maxStart(parent.Content[i-2])
						lo = min(lo, pl-1)
					}
				}
			}
		}
		step := 1
		if parent.Kind == yaml.MappingNode {
			step = 2
		}
		if len(parent.Content)/step == 1 && !isFlow(parent) && parentKey != nil {
			lo = min(lo, parentKey.Line-1)
		}
		eml, _ := maxStart(cur)
		entry := lineSpan{lo, max(eml-1, first.Line-1, ownEnd0(lines, cur, parIndent(parent, key)))}
		if soleDash {
			// the item stays as {}: its line and the comments above are the item's; the value's lines go
			entry = lineSpan{key.Line, max(eml-1, key.Line, ownEnd0(lines, cur, parIndent(parent, key)))}
		}
		if isFlow(parent) {
			entry = lineSpan{1, 0} // a member of a flow collection owns no comments: they belong to the line
		}
		return lineSpan{lo, hi}, entry, true
	case Insert:
		if len(cur.Content) == 0 {
			l := cur.Line - 1
			return lineSpan{l, l}, lineSpan{}, true
		}
		if isFlow(cur) {
			lo := cur.Line - 1
			ml, _ := maxStart(cur)
			return lineSpan{lo, max(ownEnd(cur, 0), ml-1, lo)}, lineSpan{}, true
		}
		step := 1
		if cur.Kind == yaml.MappingNode {
			step = 2
		}
		count := len(cur.Content) / step
		i := count
		if cur.Kind == yaml.SequenceNode {
			i = op.Index
		}
		if i < count {
			f := cur.Content[i*step]
			return lineSpan{headLo(f.Line - 1), f.Line - 1}, lineSpan{}, true
		}
		last := cur.Content[(count-1)*step]
		lastV := cur.Content[len(cur.Content)-1]
		par := cur.Column - 1
		if cur.Kind == yaml.MappingNode {
			par = cur.Content[len(cur.Content)-2].Column - 1
		}
		return lineSpan{last.Line - 1, ownEnd(lastV, par)}, lineSpan{}, true
	}
	return lineSpan{}, lineSpan{}, false
}

// lineOutside reports whether new differs from old on a line outside sp.
func lineOutside(a, b []string, sp lineSpan) bool {
	if sp.lo > len(a) || sp.hi+1 > len(a) || sp.lo > sp.hi+1 {
		return true
	}
	if len(b) < len(a)-(sp.hi+1-sp.lo) {
		return true
	}
	for i := 0; i < sp.lo; i++ {
		if a[i] != b[i] {
			return true
		}
	}
	tail := len(a) - (sp.hi + 1)
	for i := 0; i < tail; i++ {
		if a[len(a)-1-i] != b[len(b)-1-i] {
			return true
		}
	}
	return false
}

// ---- 4: comment ownership ----

type comment struct {
	text  string
	owner string // the code line it belongs to, without its own trailing comment
	line  int
	own   int  // the owner's line, -1 for none
	kind  byte // 'h' above an entry, 't' trailing, 'o' on its own after a blank line
}

// splitTrailing splits a code line into the code and a trailing comment, outside quotes.
func splitTrailing(l string) (code, cm string) {
	q := byte(0)
	for i := 0; i < len(l); i++ {
		c := l[i]
		switch {
		case q == '"' && c == '\\':
			i++
		case q != 0 && c == q:
			q = 0
		case q == 0 && (c == '"' || c == '\''):
			// a quote starts a quoted scalar only at the start of a token
			if i == 0 || strings.IndexByte(" \t[{,:-", l[i-1]) >= 0 {
				q = c
			}
		case q == 0 && c == '#' && (i == 0 || l[i-1] == ' ' || l[i-1] == '\t'):
			return strings.TrimRight(l[:i], " "), l[i:]
		}
	}
	return l, ""
}

func commentsOf(lines []string, skip map[int]bool) []comment {
	var out []comment
	n := len(lines)
	for i := 0; i < n; i++ {
		t := strings.TrimSpace(lines[i])
		switch {
		case skip[i+1]:
		case t == "":
		case strings.HasPrefix(t, "#"):
			j := i + 1
			for j < n && !skip[j+1] && strings.HasPrefix(strings.TrimSpace(lines[j]), "#") {
				j++
			}
			if j < n && strings.TrimSpace(lines[j]) != "" {
				code, _ := splitTrailing(lines[j])
				out = append(out, comment{text: t, owner: strings.TrimSpace(code), line: i, own: j, kind: 'h'})
			} else {
				out = append(out, comment{text: t, line: i, own: -1, kind: 'o'})
			}
		default:
			if code, cm := splitTrailing(lines[i]); cm != "" {
				out = append(out, comment{text: cm, owner: strings.TrimSpace(code), line: i, own: i, kind: 't'})
			}
		}
	}
	return out
}

func commentOwnership(a, b []string, op Op, entry lineSpan, skipOld, skipNew map[int]bool) error {
	// The changed region is ambiguous when the lines at its edges equal their neighbours (two
	// "# note" lines, say), so every alignment that explains the difference is tried, and the edit
	// passes if one of them keeps every comment with its owner.
	la, lb := len(a), len(b)
	p := 0
	for p < la && p < lb && a[p] == b[p] {
		p++
	}
	smax := 0
	for smax < la && smax < lb && a[la-1-smax] == b[lb-1-smax] {
		smax++
	}
	d := lb - la
	x := p
	y := max(x, x-d, la-smax)
	if y > la || y+d > lb || y+d < x {
		return refuse("no alignment of the old and new lines")
	}
	// The region is placed as far right as it can be; where the line at its left edge equals the
	// line at its right edge in the new text, it can slide left without changing the result
	// ("# note" inserted beside another "# note"). Every such placement is tried; none that
	// is not an equal-line shift is.
	var first error
	for {
		err := ownership(a, b, op, entry, x, y, skipOld, skipNew)
		if err == nil {
			return nil
		}
		if first == nil {
			first = err
		}
		if x == 0 || y == 0 || a[y-1] != b[y-1+d] {
			break
		}
		x, y = x-1, y-1
		if x > y {
			break
		}
	}
	return first
}

func ownership(a, b []string, op Op, entry lineSpan, ra, rb int, skipOld, skipNew map[int]bool) error {
	inRegion := func(l int) bool { return l >= ra && l < rb }
	oc, nc := commentsOf(a, skipOld), commentsOf(b, skipNew)
	used := make([]bool, len(nc))
	take := func(match func(c comment) bool) bool {
		for i, c := range nc {
			if !used[i] && match(c) {
				used[i] = true
				return true
			}
		}
		return false
	}
	shift := len(b) - len(a)
	changed := func(c comment) bool { return inRegion(c.line) || (c.own >= 0 && inRegion(c.own)) }
	// First the comments that sit away from the edit: each must still be at its place, with the
	// same owner. Matching them first keeps a changed comment from taking their match.
	for _, c := range oc {
		if changed(c) {
			continue
		}
		want := c.line
		if c.line >= rb {
			want += shift
		}
		if !take(func(x comment) bool {
			return x.line == want && x.text == c.text && x.kind == c.kind && x.owner == c.owner
		}) {
			return refuse("a comment lost its entry or moved to another (%q)", c.text)
		}
	}
	// Then the comments on or owned by the lines the edit changed: the ones that must survive first,
	// then those of the deleted entry, which may go (or stay, on a line that stays).
	goes := func(c comment) bool {
		return op.Kind == Delete && c.line >= entry.lo && c.line <= entry.hi
	}
	for _, c := range oc {
		if changed(c) && !goes(c) && !take(func(x comment) bool { return x.text == c.text }) {
			return refuse("a comment was dropped (%q)", c.text)
		}
	}
	for _, c := range oc {
		if changed(c) && goes(c) && take(func(x comment) bool { return x.text == c.text }) {
			return refuse("a comment of the deleted entry was left behind (%q)", c.text)
		}
	}
	for i, u := range used {
		if !u {
			return refuse("a comment appeared (%q)", nc[i].text)
		}
	}
	return nil
}

var _ = fmt.Sprint

func isBlockScalar(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0
}

// blockLast returns the 0-based last line of any block scalar inside x, or -1.
func blockLast(lines []string, x *yaml.Node) int {
	res := -1
	switch x.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(x.Content); i += 2 {
			v := x.Content[i+1]
			if isBlockScalar(v) {
				res = max(res, blockScalarLast(lines, v.Line-1, x.Content[i].Column-1))
			} else {
				res = max(res, blockLast(lines, v))
			}
		}
	case yaml.SequenceNode:
		for _, c := range x.Content {
			if isBlockScalar(c) {
				res = max(res, blockScalarLast(lines, c.Line-1, x.Column-1))
			} else {
				res = max(res, blockLast(lines, c))
			}
		}
	}
	return res
}

// blockLines returns the lines inside block scalars (after their indicator lines).
func blockLines(lines []string, root *yaml.Node) map[int]bool {
	out := map[int]bool{}
	var walk func(x *yaml.Node)
	mark := func(v *yaml.Node, par int) {
		for l := v.Line; l <= blockScalarLast(lines, v.Line-1, par); l++ {
			out[l] = true
		}
	}
	walk = func(x *yaml.Node) {
		switch x.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(x.Content); i += 2 {
				if v := x.Content[i+1]; isBlockScalar(v) {
					mark(v, x.Content[i].Column-1)
				} else {
					walk(v)
				}
			}
		case yaml.SequenceNode:
			for _, c := range x.Content {
				if isBlockScalar(c) {
					mark(c, x.Column-1)
				} else {
					walk(c)
				}
			}
		}
	}
	walk(root)
	return out
}

// parIndent is the indentation of the collection that holds the entry whose key is key (nil for a
// sequence item): the block scalar lines of the entry are indented further.
func parIndent(parent, key *yaml.Node) int {
	if key != nil {
		return key.Column - 1
	}
	return parent.Column - 1
}

// ownEnd0 is the last line of x including the lines of any block scalar in or at x, or -1.
func ownEnd0(lines []string, x *yaml.Node, par int) int {
	r := blockLast(lines, x)
	if isBlockScalar(x) {
		r = max(r, blockScalarLast(lines, x.Line-1, par))
	}
	return r
}

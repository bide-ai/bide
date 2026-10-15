// Package flowedit is the edit engine of the flow editor (docs/design/visual-editor.md): it applies
// one operation to the text of a flow config by splicing, so every byte outside the edit is the
// original byte, and it returns the text only after checking that the result means what the
// operation says (it re-parses the result and compares it with the operation applied to the
// data). When it cannot splice safely it refuses, and says why; it never returns unverified text.
//
// This is the phase 0 spike: generic operations on paths (Set, Insert, Delete), measured over a
// corpus by the tests in this package. The editor's own operations (set a node's approval, add an
// edge) are built on these.
package flowedit

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Kind is the kind of an Operation.
type Kind int

const (
	// Set replaces the scalar at Path, or adds the key when Path names a missing map key.
	Set Kind = iota
	// Insert adds Value at Index of the sequence at Path, or, when Path names a map, adds the key
	// Key with Value.
	Insert
	// Delete removes the map entry or sequence item at Path.
	Delete
)

// Op is one operation. Path elements are string keys and int indices.
type Op struct {
	Kind  Kind
	Path  []any
	Index int    // Insert into a sequence: the position, 0 to len
	Key   string // Insert into a map: the new key
	Value any    // string, int, bool, []any or Map
}

// KV is one entry of a Map.
type KV struct {
	Key string
	Val any
}

// Map is an ordered map value.
type Map []KV

// Refusal is why the engine would not apply an operation. The text is untouched.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return "flowedit: refused: " + r.Reason }

func refuse(f string, a ...any) error { return &Refusal{fmt.Sprintf(f, a...)} }

// Result is an applied operation: the new text and the byte range of the original it replaced.
type Result struct {
	Text   []byte
	Lo, Hi int // the original bytes [Lo, Hi) were replaced by Frag
	Frag   []byte
}

type doc struct {
	src       []byte
	root      *yaml.Node
	lineStart []int
	indent    int  // detected indent step, 2 by default
	js        bool // a JSON-style document: the root is a flow mapping, so new text is written as JSON
}

// Apply applies op to src. It returns a *Refusal error for anything it will not splice, and
// otherwise text it has verified.
func Apply(src []byte, op Op) (res *Result, err error) {
	// A node the scanners cannot place (an implicit empty node has no position) is a refusal, not a crash.
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, refuse("internal: unplaceable structure (%v)", r)
		}
	}()
	d, err := parse(src)
	if err != nil {
		return nil, err
	}
	var lo, hi int
	var frag string
	switch op.Kind {
	case Set:
		lo, hi, frag, err = d.set(op)
	case Insert:
		lo, hi, frag, err = d.insert(op)
	case Delete:
		lo, hi, frag, err = d.del(op)
	default:
		err = refuse("unknown operation")
	}
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(src)-(hi-lo)+len(frag))
	out = append(append(append(out, src[:lo]...), frag...), src[hi:]...)
	// The verification: the result is the old text with the operation applied, and nothing else.
	if !bytes.Equal(out[:lo], src[:lo]) || !bytes.Equal(out[lo+len(frag):], src[hi:]) {
		return nil, refuse("internal: bytes outside the edit changed")
	}
	if err := Verify(src, out, op); err != nil {
		return nil, err
	}
	return &Result{Text: out, Lo: lo, Hi: hi, Frag: []byte(frag)}, nil
}

func parse(src []byte) (*doc, error) {
	if bytes.Contains(src, []byte("\r")) {
		return nil, refuse("carriage returns (CRLF) are not reproduced")
	}
	if bytes.HasPrefix(src, []byte("\xef\xbb\xbf")) {
		return nil, refuse("byte-order mark")
	}
	if !utf8.Valid(src) {
		return nil, refuse("not UTF-8")
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var n yaml.Node
	if err := dec.Decode(&n); err != nil {
		return nil, refuse("does not parse: %v", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, refuse("more than one document")
	}
	if n.Kind != yaml.DocumentNode || len(n.Content) != 1 || n.Content[0].Kind != yaml.MappingNode {
		return nil, refuse("the document is not a mapping")
	}
	d := &doc{src: src, root: n.Content[0], indent: 2}
	d.lineStart = append(d.lineStart, 0)
	for i, c := range src {
		if c == '\n' {
			d.lineStart = append(d.lineStart, i+1)
		}
	}
	d.indent = d.detectIndent()
	d.js = isFlow(d.root)
	return d, nil
}

// plainNode refuses a node the splicer cannot place exactly: an alias, an anchored or tagged node,
// a block scalar. Only an operation that touches such a node is refused; the rest of the file is
// edited as usual (the verification catches an edit that changes what an alias means).
func plainNode(n *yaml.Node) error {
	switch {
	case n.Kind == yaml.AliasNode:
		return refuse("alias")
	case n.Anchor != "":
		return refuse("anchor")
	case n.Style&yaml.TaggedStyle != 0:
		return refuse("explicit tag")
	case n.Kind == yaml.ScalarNode && n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
		return refuse("block scalar")
	}
	return nil
}

func checkNodes(n *yaml.Node) error {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return refuse("anchors and aliases")
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return refuse("explicit tags")
	}
	if n.Kind == yaml.ScalarNode && n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return refuse("block scalars")
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode {
				return refuse("non-scalar keys")
			}
			if k.Value == "<<" {
				return refuse("merge keys")
			}
		}
	}
	for _, c := range n.Content {
		if err := checkNodes(c); err != nil {
			return err
		}
	}
	return nil
}

// ---- offsets ----

func (d *doc) off(line, col int) int {
	o := d.lineStart[line-1]
	for i := 1; i < col; i++ {
		_, w := utf8.DecodeRune(d.src[o:])
		o += w
	}
	return o
}

func (d *doc) start(n *yaml.Node) int { return d.off(n.Line, n.Column) }

func (d *doc) lineOf(off int) int { // 0-based
	return sort.Search(len(d.lineStart), func(i int) bool { return d.lineStart[i] > off }) - 1
}

func (d *doc) lineEnd(l int) int { // offset of the newline ending line l (or EOF)
	if l+1 < len(d.lineStart) {
		return d.lineStart[l+1] - 1
	}
	return len(d.src)
}

func (d *doc) lineText(l int) string { return string(d.src[d.lineStart[l]:d.lineEnd(l)]) }

// attached returns the first line of the comment lines directly above line l (no blank line
// between): the comments that belong to the entry that starts on l.
func (d *doc) attached(l int) int {
	for l > 0 && strings.HasPrefix(strings.TrimLeft(d.lineText(l-1), " \t"), "#") {
		l--
	}
	return l
}

func (d *doc) detectIndent() int {
	var walk func(n *yaml.Node, parentCol int) int
	walk = func(n *yaml.Node, parentCol int) int {
		if n.Style&yaml.FlowStyle != 0 {
			return 0
		}
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if v.Kind == yaml.MappingNode && v.Style&yaml.FlowStyle == 0 && v.Line > k.Line {
					return v.Column - k.Column
				}
				if r := walk(v, k.Column); r > 0 {
					return r
				}
			}
		case yaml.SequenceNode:
			for _, c := range n.Content {
				if r := walk(c, n.Column); r > 0 {
					return r
				}
			}
		}
		return 0
	}
	if i := walk(d.root, 1); i > 0 {
		return i
	}
	return 2
}

// ---- extents ----

func isFlow(n *yaml.Node) bool { return n.Style&yaml.FlowStyle != 0 }

// end returns the byte offset after n's content (a trailing comment is not content).
func (d *doc) end(n *yaml.Node, inFlow bool) (int, error) {
	if err := plainNode(n); err != nil {
		return 0, err
	}
	s := d.start(n)
	switch n.Kind {
	case yaml.ScalarNode:
		return d.scalarEnd(n, s, inFlow)
	case yaml.MappingNode, yaml.SequenceNode:
		if isFlow(n) {
			e, err := d.flowEnd(s)
			if err != nil {
				return 0, err
			}
			var a, b any
			_ = n.Decode(&a)
			if yaml.Unmarshal(d.src[s:e], &b) != nil || !reflect.DeepEqual(a, b) {
				return 0, refuse("flow collection extent not confirmed")
			}
			return e, nil
		}
		if len(n.Content) == 0 {
			return 0, refuse("empty block collection")
		}
		return d.entryEnd(n, len(n.Content)-1, false)
	}
	return 0, refuse("node kind")
}

func (d *doc) scalarEnd(n *yaml.Node, s int, inFlow bool) (int, error) {
	if err := plainNode(n); err != nil {
		return 0, err
	}
	if n.Value == "" && n.Style == 0 {
		return 0, refuse("empty value")
	}
	src := d.src
	if s >= len(src) {
		return 0, refuse("scalar position")
	}
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		if src[s] != '"' {
			return 0, refuse("double-quoted scalar not at its position")
		}
		for i := s + 1; i < len(src); i++ {
			switch src[i] {
			case '\\':
				i++
			case '"':
				return d.confirmScalar(n, s, i+1)
			case '\n':
				return 0, refuse("multi-line quoted scalar")
			}
		}
	case n.Style&yaml.SingleQuotedStyle != 0:
		if src[s] != '\'' {
			return 0, refuse("single-quoted scalar not at its position")
		}
		for i := s + 1; i < len(src); i++ {
			switch {
			case src[i] == '\'' && i+1 < len(src) && src[i+1] == '\'':
				i++
			case src[i] == '\'':
				return d.confirmScalar(n, s, i+1)
			case src[i] == '\n':
				return 0, refuse("multi-line quoted scalar")
			}
		}
	default:
		i := s
		for i < len(src) && src[i] != '\n' {
			c := src[i]
			if c == ':' && (i+1 >= len(src) || src[i+1] == ' ' || src[i+1] == '\t' || src[i+1] == '\n' || (inFlow && strings.IndexByte(",]}", src[i+1]) >= 0)) {
				break
			}
			if c == '#' && i > s && (src[i-1] == ' ' || src[i-1] == '\t') {
				break
			}
			if inFlow && strings.IndexByte(",[]{}", c) >= 0 {
				break
			}
			i++
		}
		e := i
		for e > s && (src[e-1] == ' ' || src[e-1] == '\t') {
			e--
		}
		return d.confirmScalar(n, s, e)
	}
	return 0, refuse("unterminated scalar")
}

func (d *doc) confirmScalar(n *yaml.Node, s, e int) (int, error) {
	var v string
	if err := yaml.Unmarshal(d.src[s:e], &v); err != nil || v != n.Value {
		// A key or value that is not a string (an int, a bool) decodes to another Go type; compare text.
		var a any
		if yaml.Unmarshal(d.src[s:e], &a) != nil || fmt.Sprint(a) != n.Value {
			return 0, refuse("scalar extent not confirmed")
		}
	}
	return e, nil
}

// flowEnd returns the offset after the bracket matching the one at s.
func (d *doc) flowEnd(s int) (int, error) {
	depth := 0
	for i := s; i < len(d.src); i++ {
		switch c := d.src[i]; c {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1, nil
			}
		case '"':
			for i++; i < len(d.src) && d.src[i] != '"'; i++ {
				if d.src[i] == '\\' {
					i++
				}
			}
		case '\'':
			for i++; i < len(d.src); i++ {
				if d.src[i] == '\'' {
					if i+1 < len(d.src) && d.src[i+1] == '\'' {
						i++
						continue
					}
					break
				}
			}
		case '#':
			if i > 0 && (d.src[i-1] == ' ' || d.src[i-1] == '\t' || d.src[i-1] == '\n') {
				return 0, refuse("comment inside a flow collection")
			}
		}
	}
	return 0, refuse("unterminated flow collection")
}

// ---- locating ----

type loc struct {
	node   *yaml.Node // the node at the path
	parent *yaml.Node // its container (nil for the root)
	key    *yaml.Node // the key node, for a map entry
	idx    int        // index in parent.Content of node
	inFlow bool       // node is inside a flow collection
}

func (d *doc) locate(path []any) (loc, error) {
	cur := loc{node: d.root}
	for _, p := range path {
		n := cur.node
		flow := cur.inFlow || isFlow(n)
		switch p := p.(type) {
		case string:
			if n.Kind != yaml.MappingNode {
				return loc{}, refuse("path: %q is not in a mapping", p)
			}
			found := false
			for i := 0; i < len(n.Content); i += 2 {
				if n.Content[i].Kind == yaml.ScalarNode && n.Content[i].Value == p {
					cur = loc{node: n.Content[i+1], parent: n, key: n.Content[i], idx: i + 1, inFlow: flow}
					found = true
					break
				}
			}
			if !found {
				return loc{}, errMissing
			}
		case int:
			if n.Kind != yaml.SequenceNode || p < 0 || p >= len(n.Content) {
				return loc{}, refuse("path: index %d", p)
			}
			cur = loc{node: n.Content[p], parent: n, idx: p, inFlow: flow}
		default:
			return loc{}, refuse("path element")
		}
	}
	return cur, nil
}

var errMissing = errors.New("missing")

// ---- Set ----

func (d *doc) set(op Op) (lo, hi int, frag string, err error) {
	l, err := d.locate(op.Path)
	if errors.Is(err, errMissing) {
		// Setting a missing map key adds it.
		parent := op.Path[:len(op.Path)-1]
		key, ok := op.Path[len(op.Path)-1].(string)
		if !ok {
			return 0, 0, "", refuse("path")
		}
		return d.insert(Op{Kind: Insert, Path: parent, Key: key, Value: op.Value})
	}
	if err != nil {
		return 0, 0, "", err
	}
	if l.node.Kind != yaml.ScalarNode || isComposite(op.Value) {
		return 0, 0, "", refuse("set replaces scalars only")
	}
	s := d.start(l.node)
	e, err := d.scalarEnd(l.node, s, l.inFlow)
	if err != nil {
		return 0, 0, "", err
	}
	q := byte(0)
	switch {
	case l.node.Style&yaml.DoubleQuotedStyle != 0:
		q = '"'
	case l.node.Style&yaml.SingleQuotedStyle != 0:
		q = '\''
	}
	f, err := scalar(op.Value, q, l.inFlow, d.js)
	return s, e, f, err
}

// ---- Delete ----

func (d *doc) del(op Op) (lo, hi int, frag string, err error) {
	if len(op.Path) == 0 {
		return 0, 0, "", refuse("delete the document")
	}
	l, err := d.locate(op.Path)
	if err != nil {
		return 0, 0, "", refuse("delete: %v", err)
	}
	p := l.parent
	var ck *yaml.Node // the key whose value is the container, if any
	if len(op.Path) >= 2 {
		if pl, err := d.locate(op.Path[:len(op.Path)-1]); err == nil {
			ck = pl.key
		}
	}
	var entryStart *yaml.Node = l.node
	if l.key != nil {
		entryStart = l.key
	}
	if p.Kind == yaml.MappingNode && l.idx >= 0 {
		// siblings are entries (key, value pairs)
		return d.delEntry(p, ck, entryStart, l.inFlow || isFlow(p), 2, l.idx-1)
	}
	return d.delEntry(p, ck, entryStart, l.inFlow || isFlow(p), 1, l.idx)
}

// delEntry removes entry number (idx/step) of container p, whose first content node is
// p.Content[first] (the key for a mapping, the item for a sequence).
func (d *doc) delEntry(p, ck, first *yaml.Node, flow bool, step, firstIdx int) (int, int, string, error) {
	n := len(p.Content) / step
	k := firstIdx / step
	valEnd, err := d.entryEnd(p, firstIdx+step-1, flow)
	if err != nil {
		return 0, 0, "", err
	}
	s := d.start(first)
	if flow {
		switch {
		case k+1 < n:
			next := d.start(p.Content[(k+1)*step])
			if d.lineOf(next) == d.lineOf(valEnd-1) {
				return s, next, "", nil // the next entry starts on the line this one ends on
			}
			after := valEnd
			for after < len(d.src) && (d.src[after] == ' ' || d.src[after] == '\t') {
				after++
			}
			if after < len(d.src) && d.src[after] == ',' {
				after++
			}
			for after < len(d.src) && (d.src[after] == ' ' || d.src[after] == '\t') {
				after++
			}
			ls := d.lineStart[d.lineOf(s)]
			if len(bytes.TrimLeft(d.src[ls:s], " \t")) == 0 && after < len(d.src) && d.src[after] == '\n' {
				return ls, after + 1, "", nil // the entry has its own lines: remove them
			}
			return s, after, "", nil
		case k > 0:
			prevEnd, err := d.entryEnd(p, (k-1)*step+step-1, flow)
			if err != nil {
				return 0, 0, "", err
			}
			return prevEnd, valEnd, "", nil
		}
		return s, valEnd, "", nil
	}
	// Block. The entry starts on its own line, or, as the first key of a sequence item, after the dash.
	if n == 1 {
		return d.delOnly(p, ck, first, s, valEnd)
	}
	if p.Kind == yaml.SequenceNode {
		for s > 0 && d.src[s-1] == ' ' {
			s--
		}
		if s == 0 || d.src[s-1] != '-' {
			return 0, 0, "", refuse("sequence item not after a dash")
		}
		s--
	}
	line := d.lineOf(s)
	prefix := d.src[d.lineStart[line]:s]
	if len(bytes.TrimLeft(prefix, " \t")) != 0 {
		if p.Kind == yaml.MappingNode && k == 0 && n > 1 && strings.HasSuffix(strings.TrimRight(string(prefix), " "), "-") {
			// The first key of a "- key: v" item: remove it up to the next key, which takes its place.
			next := d.start(p.Content[step])
			if d.lineOf(next) != d.lineOf(valEnd-1)+1 {
				return 0, 0, "", refuse("comments or blank lines between the first key and the next")
			}
			return s, next, "", nil
		}
		return 0, 0, "", refuse("entry shares its line with something else")
	}
	lo := d.lineStart[d.attached(line)]
	hi := d.lineEnd(d.lineOf(valEnd - 1))
	if hi < len(d.src) {
		hi++
	}
	return lo, hi, "", nil
}

// ---- Insert ----

func (d *doc) insert(op Op) (lo, hi int, frag string, err error) {
	l, err := d.locate(op.Path)
	if err != nil {
		return 0, 0, "", refuse("insert: %v", err)
	}
	p := l.node
	if err := plainNode(p); err != nil {
		return 0, 0, "", err
	}
	flow := isFlow(p)
	isMap := p.Kind == yaml.MappingNode
	if !isMap && p.Kind != yaml.SequenceNode {
		return 0, 0, "", refuse("insert into a scalar")
	}
	if isMap {
		for i := 0; i < len(p.Content); i += 2 {
			if p.Content[i].Kind == yaml.ScalarNode && p.Content[i].Value == op.Key {
				return 0, 0, "", refuse("key exists")
			}
		}
	}
	step := 1
	if isMap {
		step = 2
	}
	n := len(p.Content) / step
	idx := n
	if !isMap {
		idx = op.Index
		if idx < 0 || idx > n {
			return 0, 0, "", refuse("index")
		}
	}
	if flow {
		item, err := d.flowItem(isMap, op, d.flowHint(op))
		if err != nil {
			return 0, 0, "", err
		}
		s := d.start(p)
		if ind, ok := d.itemLines(p, step); ok {
			// One entry to a line, as pretty-printed JSON has it: the new entry gets a line of its own.
			if idx < n {
				at := d.lineStart[d.lineOf(d.start(p.Content[idx*step]))]
				return at, at, ind + item + ",\n", nil
			}
			end, err := d.entryEnd(p, len(p.Content)-1, true)
			if err != nil {
				return 0, 0, "", err
			}
			return end, end, ",\n" + ind + item, nil
		}
		switch {
		case n == 0:
			return s + 1, s + 1, item, nil
		case idx < n:
			at := d.start(p.Content[idx*step])
			return at, at, item + ", ", nil
		}
		e, err := d.entryEnd(p, len(p.Content)-1, true)
		if err != nil {
			return 0, 0, "", err
		}
		return e, e, ", " + item, nil
	}
	if n == 0 {
		return 0, 0, "", refuse("empty block collection")
	}
	pad := strings.Repeat(" ", p.Column-1)
	if p.Kind == yaml.SequenceNode {
		// p.Column is the first dash's column.
		first := d.start(p.Content[0])
		dash := first
		for dash > 0 && d.src[dash-1] == ' ' {
			dash--
		}
		if dash == 0 || d.src[dash-1] != '-' {
			return 0, 0, "", refuse("sequence item not after a dash")
		}
		dash--
		gap := first - dash
		dl := d.lineOf(dash)
		if len(bytes.TrimLeft(d.src[d.lineStart[dl]:dash], " ")) != 0 && idx == 0 {
			return 0, 0, "", refuse("nested dash: nothing can go before the first item, which shares its line")
		}
		pad = strings.Repeat(" ", utf8.RuneCount(d.src[d.lineStart[dl]:dash]))
		text, err := d.blockItem(pad, gap, op.Value, d.flowHint(op))
		if err != nil {
			return 0, 0, "", err
		}
		if idx < n {
			at := d.start(p.Content[idx])
			at = d.lineStart[d.attached(d.lineOf(at))]
			return at, at, text, nil
		}
		return d.afterLast(p, text)
	}
	text, err := d.blockEntry(pad, op.Key, op.Value, d.flowHint(op))
	if err != nil {
		return 0, 0, "", err
	}
	return d.afterLast(p, text)
}

// afterLast inserts text (whole lines) after the last entry of block collection p.
func (d *doc) afterLast(p *yaml.Node, text string) (int, int, string, error) {
	e, err := d.entryEnd(p, len(p.Content)-1, false)
	if err != nil {
		return 0, 0, "", err
	}
	le := d.lineEnd(d.lineOf(e - 1))
	if le >= len(d.src) {
		return le, le, "\n" + strings.TrimSuffix(text, "\n"), nil
	}
	return le + 1, le + 1, text, nil
}

// flowHint reports whether a composite inserted by op should be written in flow style, copying
// what the document does for the same key (or sequence).
func (d *doc) flowHint(op Op) bool {
	want := op.Key
	var found, flow bool
	var walk func(n *yaml.Node, inSeq bool)
	walk = func(n *yaml.Node, inSeq bool) {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i < len(n.Content); i += 2 {
				if want != "" && n.Content[i].Value == want && !found && isComposite(toGo(n.Content[i+1])) {
					found, flow = true, isFlow(n.Content[i+1])
				}
				walk(n.Content[i+1], false)
			}
		case yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c, true)
			}
		}
	}
	walk(d.root, false)
	if found {
		return flow
	}
	// No key to copy: follow the container's own style for items (a sequence of flow mappings).
	if l, err := d.locate(op.Path); err == nil && l.node.Kind == yaml.SequenceNode && len(l.node.Content) > 0 {
		return isFlow(l.node.Content[0])
	}
	return false
}

func toGo(n *yaml.Node) any {
	switch n.Kind {
	case yaml.MappingNode, yaml.SequenceNode:
		return []any{}
	}
	return ""
}

// delOnly removes the only entry of block collection p: the collection becomes [] or {}. ck is
// the key whose value p is, or nil when p is an item of a sequence.
func (d *doc) delOnly(p, ck, first *yaml.Node, s, valEnd int) (int, int, string, error) {
	empty := "{}"
	if p.Kind == yaml.SequenceNode {
		empty = "[]"
	}
	if ck == nil {
		// A "- key: v" item with one key: the item becomes {}.
		if p.Kind == yaml.MappingNode {
			prefix := strings.TrimRight(string(d.src[d.lineStart[d.lineOf(s)]:s]), " ")
			if strings.HasSuffix(prefix, "-") && len(strings.TrimLeft(prefix, " ")) == 1 {
				return s, valEnd, "{}", nil
			}
		}
		return 0, 0, "", refuse("removing the only entry of a collection that is not under a key")
	}
	ke, err := d.scalarEnd(ck, d.start(ck), false)
	if err != nil {
		return 0, 0, "", err
	}
	colon := ke
	for colon < len(d.src) && (d.src[colon] == ' ' || d.src[colon] == '\t') {
		colon++
	}
	if colon >= len(d.src) || d.src[colon] != ':' {
		return 0, 0, "", refuse("key colon not found")
	}
	kl := d.lineOf(colon)
	// Only the entry's own comments go with it: a comment above them, inside the collection, stays.
	for l := kl + 1; l < d.attached(d.lineOf(s)); l++ {
		if strings.HasPrefix(strings.TrimLeft(d.lineText(l), " \t"), "#") {
			return 0, 0, "", refuse("a comment inside the collection that is not the entry's")
		}
	}
	hi := d.lineEnd(d.lineOf(valEnd - 1))
	if hi < len(d.src) {
		hi++
	}
	rest := string(d.src[colon+1 : d.lineEnd(kl)])
	if hi == len(d.src) && !strings.HasSuffix(string(d.src), "\n") {
		return colon + 1, hi, " " + empty + rest, nil
	}
	return colon + 1, hi, " " + empty + rest + "\n", nil
}

// entryEnd returns the offset after the content of p.Content[i]: for the value of a mapping entry
// that is empty (a null), the colon after its key.
func (d *doc) entryEnd(p *yaml.Node, i int, flow bool) (int, error) {
	n := p.Content[i]
	if n.Kind == yaml.ScalarNode && n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return d.blockScalarEnd(p, i)
	}
	if p.Kind == yaml.MappingNode && i%2 == 1 && n.Kind == yaml.ScalarNode && n.Value == "" && n.Style == 0 && n.Tag == "!!null" {
		k := p.Content[i-1]
		ke, err := d.scalarEnd(k, d.start(k), flow)
		if err != nil {
			return 0, err
		}
		c := ke
		for c < len(d.src) && (d.src[c] == ' ' || d.src[c] == '\t') {
			c++
		}
		if c >= len(d.src) || d.src[c] != ':' {
			return 0, refuse("key colon not found")
		}
		return c + 1, nil
	}
	return d.end(n, flow)
}

// applyRaw is Apply without the verification, for diagnosing refusals in tests.
func applyRaw(src []byte, op Op) (string, error) {
	d, err := parse(src)
	if err != nil {
		return "", err
	}
	var lo, hi int
	var frag string
	switch op.Kind {
	case Set:
		lo, hi, frag, err = d.set(op)
	case Insert:
		lo, hi, frag, err = d.insert(op)
	case Delete:
		lo, hi, frag, err = d.del(op)
	}
	if err != nil {
		return "", err
	}
	return string(src[:lo]) + frag + string(src[hi:]), nil
}

// parentIndent is the indentation of the collection that holds p.Content[i]: a block scalar's
// lines must be indented further than it.
func parentIndent(p *yaml.Node, i int) int {
	if p.Kind == yaml.MappingNode {
		return p.Content[i-1].Column - 1
	}
	return p.Column - 1
}

// blockScalarLast returns the 0-based last line of the block scalar that starts at line start0
// (its indicator line) inside a collection indented parent columns: the last non-blank line that is
// indented further than parent, or the indicator line when there is none.
func blockScalarLast(lines []string, start0, parent int) int {
	last := start0
	for l := start0 + 1; l < len(lines); l++ {
		t := lines[l]
		if strings.TrimSpace(t) == "" {
			continue
		}
		if len(t)-len(strings.TrimLeft(t, " ")) <= parent {
			break
		}
		last = l
	}
	return last
}

// blockScalarEnd returns the offset after the last content line of the block scalar p.Content[i].
func (d *doc) blockScalarEnd(p *yaml.Node, i int) (int, error) {
	n := p.Content[i]
	if err := plainNodeBlock(n); err != nil {
		return 0, err
	}
	s := d.start(n)
	if s >= len(d.src) || (d.src[s] != '|' && d.src[s] != '>') {
		return 0, refuse("block scalar not at its indicator")
	}
	ind := d.lineText(d.lineOf(s))[s-d.lineStart[d.lineOf(s)]:]
	ind, _ = splitTrailing(ind)
	if strings.Contains(ind, "+") {
		return 0, refuse("block scalar that keeps trailing lines")
	}
	lines := strings.Split(string(d.src), "\n")
	last := blockScalarLast(lines, d.lineOf(s), parentIndent(p, i))
	return d.lineEnd(last), nil
}

// plainNodeBlock is plainNode for a block scalar: only its anchor and tag matter.
func plainNodeBlock(n *yaml.Node) error {
	if n.Anchor != "" {
		return refuse("anchor")
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return refuse("explicit tag")
	}
	return nil
}

// itemLines reports whether every entry of flow collection p starts a line of its own, after
// nothing but the same indentation, and returns that indentation.
func (d *doc) itemLines(p *yaml.Node, step int) (string, bool) {
	n := len(p.Content) / step
	if n == 0 {
		return "", false
	}
	ind := ""
	prev := d.lineOf(d.start(p))
	for i := 0; i < n; i++ {
		s := d.start(p.Content[i*step])
		l := d.lineOf(s)
		pre := string(d.src[d.lineStart[l]:s])
		if l <= prev || strings.TrimLeft(pre, " \t") != "" {
			return "", false
		}
		if i == 0 {
			ind = pre
		} else if pre != ind {
			return "", false
		}
		prev = l
	}
	return ind, true
}

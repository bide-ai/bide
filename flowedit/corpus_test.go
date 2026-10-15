package flowedit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// ---- the corpus ----

type file struct {
	group string
	name  string
	text  string
}

func isFlowConfig(text string) bool {
	var v map[string]any
	if yaml.Unmarshal([]byte(text), &v) != nil {
		return false
	}
	_, a := v["nodes"]
	_, b := v["wiring"]
	_, c := v["assert"]
	return a || b || c
}

var fence = regexp.MustCompile("(?s)```(?:yaml|yml)\n(.*?)\n```")
var rawGo = regexp.MustCompile("(?s)`(\\{[^`]*\"nodes\"[^`]*)`")

func repoRoot(t testing.TB) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func collect(t testing.TB) []file {
	root := repoRoot(t)
	var out []file
	seen := map[string]bool{}
	add := func(group, name, text string) {
		if seen[text] || !isFlowConfig(text) {
			return
		}
		seen[text] = true
		out = append(out, file{group, name, text})
	}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "flowedit":
				return filepath.SkipDir
			}
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		switch filepath.Ext(p) {
		case ".yaml", ".yml":
			add("repo YAML files", rel, string(b))
		case ".md":
			for i, m := range fence.FindAllSubmatch(b, -1) {
				add("YAML in the docs", fmt.Sprintf("%s#%d", rel, i), string(m[1])+"\n")
			}
		case ".go":
			if strings.HasSuffix(p, "_test.go") && strings.Contains(rel, "plan/") {
				for i, m := range rawGo.FindAllSubmatch(b, -1) {
					add("plan test configs (JSON)", fmt.Sprintf("%s#%d", rel, i), string(m[1]))
					add("plan test configs (JSON, tabs as spaces)", fmt.Sprintf("%s#%d", rel, i), strings.ReplaceAll(string(m[1]), "\t", "  "))
				}
			}
		}
		return nil
	})
	return out
}

func generated(n int) []file {
	r := rand.New(rand.NewPCG(20261015, 1))
	var out []file
	for i := 0; i < n; i++ {
		out = append(out, file{"generated variants", fmt.Sprintf("gen-%d", i), genConfig(r)})
	}
	return out
}

// ---- the operations ----

func fresh(v any) any { // a value of the same shape as v, with new scalars
	switch x := v.(type) {
	case map[string]any:
		keys := sortedKeys(x)
		m := Map{}
		for _, k := range keys {
			m = append(m, KV{k, fresh(x[k])})
		}
		return m
	case []any:
		out := []any{}
		for _, e := range x {
			out = append(out, fresh(e))
		}
		return out
	case string:
		return "new-" + x
	case int:
		return x + 1
	case bool:
		return !x
	}
	return "new"
}

func sortedKeys(m map[string]any) []string {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}

// ops lists the operations to try on the document: every scalar set, every entry and item
// deleted, a key and a composite added to every mapping, an item added at the start, the middle
// and the end of every sequence.
func ops(text string) []Op {
	v, err := decode([]byte(text))
	if err != nil {
		return nil
	}
	var out []Op
	var walk func(path []any, v any)
	walk = func(path []any, v any) {
		cp := func() []any { return append([]any(nil), path...) }
		switch x := v.(type) {
		case map[string]any:
			out = append(out, Op{Kind: Insert, Path: cp(), Key: "zz_new", Value: "added"})
			out = append(out, Op{Kind: Insert, Path: cp(), Key: "zz_block", Value: Map{{"need", 2}, {"approvers", []any{"a", "b"}}}})
			for _, k := range sortedKeys(x) {
				p := append(cp(), k)
				out = append(out, Op{Kind: Delete, Path: p})
				walk(p, x[k])
			}
		case []any:
			var item any = "zz item"
			if len(x) > 0 {
				switch x[0].(type) {
				case map[string]any:
					item = fresh(x[0])
				case string:
					item = "zz item"
				case int:
					item = 7
				}
			}
			for _, i := range uniq(0, len(x)/2, len(x)) {
				out = append(out, Op{Kind: Insert, Path: cp(), Index: i, Value: item})
			}
			for i, e := range x {
				p := append(cp(), i)
				out = append(out, Op{Kind: Delete, Path: p})
				walk(p, e)
			}
		case string:
			out = append(out, Op{Kind: Set, Path: cp(), Value: "changed"})
			out = append(out, Op{Kind: Set, Path: cp(), Value: "needs, quoting: [x] # y"})
		case int:
			out = append(out, Op{Kind: Set, Path: cp(), Value: x + 1})
		case bool:
			out = append(out, Op{Kind: Set, Path: cp(), Value: !x})
		}
	}
	walk(nil, v)
	return out
}

func uniq(a ...int) []int {
	seen := map[int]bool{}
	var out []int
	for _, x := range a {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// ---- the independent oracle: the operation on the data, written separately from the engine ----

func oracle(src string, op Op) (any, bool) {
	v, err := decode([]byte(src))
	if err != nil {
		return nil, false
	}
	return rebuild(v, op.Path, op), true
}

func rebuild(v any, path []any, op Op) any {
	if len(path) == 0 {
		switch op.Kind {
		case Set:
			return plain(op.Value)
		case Insert:
			switch x := v.(type) {
			case map[string]any:
				n := map[string]any{}
				for k, e := range x {
					n[k] = e
				}
				n[op.Key] = plain(op.Value)
				return n
			case []any:
				n := append([]any{}, x[:op.Index]...)
				n = append(n, plain(op.Value))
				return append(n, x[op.Index:]...)
			}
		}
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		k := path[0].(string)
		n := map[string]any{}
		for kk, e := range x {
			if kk == k {
				if len(path) == 1 && op.Kind == Delete {
					continue
				}
				if len(path) == 1 && op.Kind == Set {
					n[kk] = plain(op.Value)
					continue
				}
				n[kk] = rebuild(e, path[1:], op)
			} else {
				n[kk] = e
			}
		}
		if _, ok := x[k]; !ok && len(path) == 1 && op.Kind == Set {
			n[k] = plain(op.Value)
		}
		return n
	case []any:
		i := path[0].(int)
		n := []any{}
		for j, e := range x {
			switch {
			case j != i:
				n = append(n, e)
			case len(path) == 1 && op.Kind == Delete:
			case len(path) == 1 && op.Kind == Set:
				n = append(n, plain(op.Value))
			default:
				n = append(n, rebuild(e, path[1:], op))
			}
		}
		return n
	}
	return v
}

// ---- the independent span: which lines may an operation change? ----

type span struct{ lo, hi int } // 0-based, inclusive lines

// intended returns the lines the operation may change, worked out from the original text and the
// documented ownership rule (an entry owns its lines, the comment lines directly above it, and
// the comment and blank lines up to the next entry). It uses yaml.Node start lines only.
func intended(text string, op Op) (span, bool) {
	var n yaml.Node
	if yaml.Unmarshal([]byte(text), &n) != nil || len(n.Content) == 0 {
		return span{}, false
	}
	lines := strings.Split(text, "\n")
	var all []*yaml.Node
	var collect func(*yaml.Node)
	collect = func(x *yaml.Node) {
		all = append(all, x)
		for _, c := range x.Content {
			collect(c)
		}
	}
	collect(n.Content[0])
	// subtree max start (line, col) and the next node start after it
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
	nextLine := func(x *yaml.Node) int {
		l, c := maxStart(x)
		best := len(lines) + 1
		for _, y := range all {
			if y.Line > l || (y.Line == l && y.Column > c) {
				best = min(best, y.Line)
			}
		}
		if best == len(lines)+1 {
			return len(lines) - 1
		}
		return best - 2 // the line before the next node's line (0-based inclusive)
	}
	headLo := func(line0 int) int {
		for line0 > 0 && strings.HasPrefix(strings.TrimLeft(lines[line0-1], " "), "#") {
			line0--
		}
		return line0
	}
	// locate the node (or container) at the path
	cur := n.Content[0]
	var key, parentKey *yaml.Node
	var parent *yaml.Node
	var idx int
	for _, p := range op.Path {
		parent = cur
		parentKey = key
		key = nil
		switch p := p.(type) {
		case string:
			found := false
			for i := 0; i < len(cur.Content); i += 2 {
				if cur.Content[i].Value == p {
					key, cur, idx, found = cur.Content[i], cur.Content[i+1], i, true
					break
				}
			}
			if !found {
				// a Set of a missing key is an insert into the parent
				op = Op{Kind: Insert, Path: op.Path[:len(op.Path)-1], Key: p}
				return intended(text, op)
			}
		case int:
			cur, idx = cur.Content[p], p
		}
	}
	switch op.Kind {
	case Set:
		l := cur.Line - 1
		return span{l, l}, true
	case Delete:
		first := cur
		if key != nil {
			first = key
		}
		_ = idx
		lo := headLo(first.Line - 1)
		ml, _ := maxStart(cur)
		hi := max(nextLine(cur), ml-1)
		// The first key of a "- key: v" item: the next key takes its place on the dash line, so
		// its line is rewritten too.
		if key != nil && parent.Content[0] == key && len(parent.Content) > 2 && !isFlow(parent) &&
			strings.Contains(lines[key.Line-1][:key.Column-1], "-") {
			hi = max(hi, parent.Content[2].Line-1)
		}
		// The last entry of a flow collection takes the comma before it: the previous entry's line changes.
		if isFlow(parent) && parent.Kind == yaml.SequenceNode && idx == len(parent.Content)-1 && idx > 0 {
			pl, _ := maxStart(parent.Content[idx-1])
			lo = min(lo, pl-1)
		}
		if isFlow(parent) && parent.Kind == yaml.MappingNode && idx == len(parent.Content)-2 && idx > 0 {
			pl, _ := maxStart(parent.Content[idx-1])
			lo = min(lo, pl-1)
		}
		// The only entry of a block collection: the key's line takes the "[]" or "{}".
		step := 1
		if parent.Kind == yaml.MappingNode {
			step = 2
		}
		if len(parent.Content)/step == 1 && !isFlow(parent) && parentKey != nil {
			lo = min(lo, parentKey.Line-1)
		}
		return span{lo, hi}, true
	case Insert:
		if len(cur.Content) == 0 {
			l := cur.Line - 1
			return span{l, l}, true
		}
		if isFlow(cur) {
			// anywhere from the container's first line to its last
			lo := cur.Line - 1
			ml, _ := maxStart(cur)
			return span{lo, max(nextLine(cur), ml-1, lo)}, true
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
			lo := headLo(f.Line - 1)
			return span{lo, f.Line - 1}, true
		}
		last := cur.Content[(count-1)*step]
		ml, _ := maxStart(cur)
		return span{last.Line - 1, max(nextLine(cur.Content[len(cur.Content)-1]), ml-1)}, true
	}
	return span{}, false
}

// touched returns the original lines [lo, hi] an applied edit changed, from the byte range.
func touched(text string, r *Result) (lo, hi int) {
	ls := strings.Count(text[:r.Lo], "\n")
	if r.Hi == r.Lo {
		return ls, ls
	}
	return ls, strings.Count(text[:r.Hi-1], "\n")
}

// ---- the baseline: the same operation by re-emitting a yaml.Node tree ----

func nodeOf(v any) *yaml.Node {
	switch x := v.(type) {
	case Map:
		n := &yaml.Node{Kind: yaml.MappingNode}
		for _, kv := range x {
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: kv.Key}, nodeOf(kv.Val))
		}
		return n
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode}
		for _, e := range x {
			n.Content = append(n.Content, nodeOf(e))
		}
		return n
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: x}
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(x)}
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(x)}
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprint(v)}
}

func reemit(text string, op Op, indent int) (string, bool) {
	var doc yaml.Node
	if yaml.Unmarshal([]byte(text), &doc) != nil || len(doc.Content) == 0 {
		return "", false
	}
	cur := doc.Content[0]
	var parent *yaml.Node
	var at int
	for i, p := range op.Path {
		parent = cur
		last := i == len(op.Path)-1
		switch p := p.(type) {
		case string:
			found := false
			for j := 0; j < len(cur.Content); j += 2 {
				if cur.Content[j].Value == p {
					at, cur, found = j, cur.Content[j+1], true
					break
				}
			}
			if !found {
				if last && op.Kind == Set {
					cur.Content = append(cur.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: p}, nodeOf(op.Value))
					return encodeDoc(&doc, indent), true
				}
				return "", false
			}
		case int:
			at, cur = p, cur.Content[p]
		}
	}
	switch op.Kind {
	case Set:
		nn := nodeOf(op.Value)
		cur.Value, cur.Tag = nn.Value, nn.Tag
		cur.Style &^= yaml.TaggedStyle
	case Delete:
		if parent.Kind == yaml.MappingNode {
			parent.Content = append(parent.Content[:at], parent.Content[at+2:]...)
		} else {
			parent.Content = append(parent.Content[:at], parent.Content[at+1:]...)
		}
	case Insert:
		if cur.Kind == yaml.MappingNode {
			cur.Content = append(cur.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: op.Key}, nodeOf(op.Value))
		} else {
			c := append([]*yaml.Node{}, cur.Content[:op.Index]...)
			c = append(c, nodeOf(op.Value))
			cur.Content = append(c, cur.Content[op.Index:]...)
		}
	}
	return encodeDoc(&doc, indent), true
}

func encodeDoc(n *yaml.Node, indent int) string {
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(indent)
	_ = e.Encode(n)
	_ = e.Close()
	return b.String()
}

// outside reports whether new differs from old on a line outside the span: the lines before the
// span must be the same lines, and the lines after it must be the same lines (aligned at the end),
// so an edit confined to the span, whatever it did inside, passes and one that touched any other
// line, or moved a blank line or a comment elsewhere, does not.
func outside(old, new string, sp span) bool {
	a, b := strings.Split(old, "\n"), strings.Split(new, "\n")
	if sp.lo > len(a) || sp.hi+1 > len(a) {
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

// ---- the measurement ----

type tally struct {
	files, ops                    int
	applied, inSpan, refused      int
	oneLine, setOps               int
	wrong                         int // applied but disagreeing with the independent oracle: must be 0
	baseline, baselineOK          int
	refusals                      map[string]int
	byKind                        [3][2]int // [kind][ops, in span]
}

func (ta *tally) add(f file) {
	ta.files++
	if ta.refusals == nil {
		ta.refusals = map[string]int{}
	}
	for _, op := range ops(f.text) {
		ta.ops++
		sp, spOK := intended(f.text, op)
		ta.byKind[op.Kind][0]++
		r, err := Apply([]byte(f.text), op)
		if err != nil {
			var rf *Refusal
			if !errors.As(err, &rf) {
				ta.wrong++
				continue
			}
			ta.refused++
			ta.refusals[bucket(rf.Reason)]++
		} else {
			ta.applied++
			want, ok := oracle(f.text, op)
			got, derr := decode(r.Text)
			if !ok || derr != nil || !reflect.DeepEqual(want, got) {
				ta.wrong++
			}
			lo, hi := touched(f.text, r)
			if spOK && !outside(f.text, string(r.Text), sp) {
				ta.inSpan++
				ta.byKind[op.Kind][1]++
			}
			if op.Kind == Set {
				ta.setOps++
				if lo == hi {
					ta.oneLine++
				}
			}
		}
		// the baseline, on the same operation
		if out, ok := reemit(f.text, op, 2); ok && spOK {
			ta.baseline++
			if !outside(f.text, out, sp) {
				ta.baselineOK++
			}
		}
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func bucket(r string) string {
	r = regexp.MustCompile(`\([^)]*\)`).ReplaceAllString(r, "(...)")
	r = regexp.MustCompile(`%?"[^"]*"`).ReplaceAllString(r, "X")
	return r
}

func TestCorpusReport(t *testing.T) {
	groups := map[string]*tally{}
	var order []string
	corpus := append(collect(t), generated(300)...)
	for _, f := range corpus {
		ta := groups[f.group]
		if ta == nil {
			ta = &tally{}
			groups[f.group] = ta
			order = append(order, f.group)
		}
		ta.add(f)
	}
	sort.Strings(order)
	var all tally
	all.refusals = map[string]int{}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-28s %5s %7s %8s %8s %8s %9s\n", "group", "files", "ops", "applied", "in span", "refused", "baseline")
	row := func(name string, ta *tally) {
		fmt.Fprintf(&b, "%-28s %5d %7d %7.1f%% %7.1f%% %7.1f%% %8.1f%%\n", name, ta.files, ta.ops,
			pct(ta.applied, ta.ops), pct(ta.inSpan, ta.ops), pct(ta.refused, ta.ops), pct(ta.baselineOK, ta.baseline))
	}
	for _, g := range order {
		ta := groups[g]
		row(g, ta)
		all.files += ta.files
		all.ops += ta.ops
		all.applied += ta.applied
		all.inSpan += ta.inSpan
		all.refused += ta.refused
		all.wrong += ta.wrong
		all.oneLine += ta.oneLine
		all.setOps += ta.setOps
		for k := range all.byKind {
			all.byKind[k][0] += ta.byKind[k][0]
			all.byKind[k][1] += ta.byKind[k][1]
		}
		all.baseline += ta.baseline
		all.baselineOK += ta.baselineOK
		for k, v := range ta.refusals {
			all.refusals[k] += v
		}
	}
	row("ALL", &all)
	fmt.Fprintf(&b, "\nwrong (applied text that disagrees with the independent oracle, or a non-refusal error): %d\n", all.wrong)
	for k, n := range []string{"set", "insert", "delete"} {
		fmt.Fprintf(&b, "%-7s %6d ops, %5.1f%% applied within the intended lines\n", n, all.byKind[k][0], pct(all.byKind[k][1], all.byKind[k][0]))
	}
	fmt.Fprintf(&b, "set-scalar ops that changed exactly one line: %d of %d\n", all.oneLine, all.setOps)
	type kv struct {
		k string
		v int
	}
	var rs []kv
	for k, v := range all.refusals {
		rs = append(rs, kv{k, v})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].v > rs[j].v })
	fmt.Fprintf(&b, "refusals:\n")
	for _, r := range rs {
		fmt.Fprintf(&b, "  %6d  %s\n", r.v, r.k)
	}
	t.Log(b.String())
	if all.wrong != 0 {
		t.Errorf("%d operations produced text the oracle disagrees with", all.wrong)
	}
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

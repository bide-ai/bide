package flowedit

import (
	"bytes"
	"encoding/json"
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
	real  bool // a file somebody wrote for another purpose, not made for this test
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
		out = append(out, file{group, name, text, group == "repo YAML files" || group == "YAML in the docs" || group == "JSON configs (valid JSON)"})
	}
	// JSON configs: files, and raw strings in Go sources. Those that are not valid JSON are test
	// templates with a placeholder where a value goes (ASSERT, APPROVAL); they are measured apart.
	addJSON := func(name, text string) {
		if !strings.Contains(text, `"nodes"`) {
			return
		}
		if json.Valid([]byte(text)) {
			add("JSON configs (valid JSON)", name, text)
		} else {
			add("JSON test templates (placeholders)", name, text)
		}
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
		case ".json":
			addJSON(rel, string(b))
		case ".go":
			if strings.HasPrefix(rel, "plan/") || strings.HasPrefix(rel, "examples/") {
				for i, m := range rawGo.FindAllSubmatch(b, -1) {
					addJSON(fmt.Sprintf("%s#%d", rel, i), string(m[1]))
				}
			}
		}
		return nil
	})
	for _, dir := range []struct {
		glob, group string
		real        bool
	}{
		{"testdata/external/*.y*ml", "external open-source files", true},
		{"testdata/adversarial/*.yaml", "adversarial (hand-written)", false},
	} {
		fs, _ := filepath.Glob(dir.glob)
		sort.Strings(fs)
		for _, f := range fs {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if dir.real && documents(b) > 1 {
				// A flow config is one document (the loader reads the first and ignores the rest), so a
				// file of several is outside the bar; the adversarial set holds one on purpose.
				multiDoc = append(multiDoc, filepath.Base(f))
				multiDocFiles = append(multiDocFiles, file{dir.group, filepath.Base(f), string(b), true})
				continue
			}
			out = append(out, file{dir.group, filepath.Base(f), string(b), dir.real})
		}
	}
	return out
}

var multiDoc []string
var multiDocFiles []file

func documents(b []byte) int {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	n := 0
	for {
		var v yaml.Node
		if dec.Decode(&v) != nil {
			return n
		}
		n++
	}
}

func generated(n int) []file {
	r := rand.New(rand.NewPCG(20261015, 1))
	var out []file
	for i := 0; i < n; i++ {
		out = append(out, file{"generated variants", fmt.Sprintf("gen-%d", i), genConfig(r), false})
	}
	return out
}

// ---- the operations ----

func sortedKeys(m map[string]any) []string {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}

// like returns a value of the same shape as n with new scalars (a new item for a sequence of them).
func like(n *yaml.Node) any {
	switch n.Kind {
	case yaml.MappingNode:
		m := Map{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Kind == yaml.ScalarNode {
				m = append(m, KV{n.Content[i].Value, like(n.Content[i+1])})
			}
		}
		return m
	case yaml.SequenceNode:
		out := []any{}
		for _, e := range n.Content {
			out = append(out, like(e))
		}
		return out
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!int":
			return 7
		case "!!bool":
			return true
		}
		if strings.ContainsAny(n.Value, "\n\r\t") {
			return "new"
		}
		return "new-" + n.Value
	}
	return "new"
}

// ops lists the operations to try on the document: every scalar set, every entry and item
// deleted, a key and a composite added to every mapping, an item added at the start, the middle
// and the end of every sequence. It walks the parsed nodes, so a file the data decoder cannot
// read (a complex key) still gets its operations.
func ops(text string) []Op {
	var doc yaml.Node
	if yaml.Unmarshal([]byte(text), &doc) != nil || len(doc.Content) == 0 {
		return nil
	}
	var out []Op
	var walk func(path []any, n *yaml.Node, depth int)
	walk = func(path []any, n *yaml.Node, depth int) {
		if depth > 12 {
			return
		}
		cp := func() []any { return append([]any(nil), path...) }
		switch n.Kind {
		case yaml.AliasNode:
		case yaml.MappingNode:
			out = append(out, Op{Kind: Insert, Path: cp(), Key: "zz_new", Value: "added"})
			out = append(out, Op{Kind: Insert, Path: cp(), Key: "zz_block", Value: Map{{"need", 2}, {"approvers", []any{"a", "b"}}}})
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				if k.Kind != yaml.ScalarNode {
					continue
				}
				p := append(cp(), k.Value)
				out = append(out, Op{Kind: Delete, Path: p})
				walk(p, n.Content[i+1], depth+1)
			}
		case yaml.SequenceNode:
			var item any = "zz item"
			if len(n.Content) > 0 {
				item = like(n.Content[0])
			}
			for _, i := range uniq(0, len(n.Content)/2, len(n.Content)) {
				out = append(out, Op{Kind: Insert, Path: cp(), Index: i, Value: item})
			}
			for i, e := range n.Content {
				p := append(cp(), i)
				out = append(out, Op{Kind: Delete, Path: p})
				walk(p, e, depth+1)
			}
		case yaml.ScalarNode:
			switch n.Tag {
			case "!!int":
				var x int
				if n.Decode(&x) == nil {
					out = append(out, Op{Kind: Set, Path: cp(), Value: x + 1})
				}
			case "!!bool":
				var x bool
				if n.Decode(&x) == nil {
					out = append(out, Op{Kind: Set, Path: cp(), Value: !x})
				}
			case "!!null":
				out = append(out, Op{Kind: Set, Path: cp(), Value: "was null"})
			default:
				out = append(out, Op{Kind: Set, Path: cp(), Value: "changed"})
				out = append(out, Op{Kind: Set, Path: cp(), Value: "needs, quoting: [x] # y"})
			}
		}
	}
	walk(nil, doc.Content[0], 0)
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
	if err != nil || !stringKeys(v) {
		return nil, false // the data oracle models string keys only; Verify checks the rest
	}
	return rebuild(v, op.Path, op), true
}

func stringKeys(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			if !stringKeys(e) {
				return false
			}
		}
	case []any:
		for _, e := range x {
			if !stringKeys(e) {
				return false
			}
		}
	case map[any]any:
		return false
	}
	return true
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
	headLo := func(line0 int) int {
		for line0 > 0 && strings.HasPrefix(strings.TrimLeft(lines[line0-1], " "), "#") {
			line0--
		}
		return line0
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
		return headLo(best-1) - 1 // the line before the comments attached to the next node (0-based inclusive)
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

type fileRate struct {
	name    string
	ops, ok int
}

type tally struct {
	files, ops               int
	applied, inSpan, refused int
	oneLine, setOps          int
	wrong                    int // applied but disagreeing with the independent oracle: must be 0
	baseline, baselineOK     int
	refusals                 map[string]int
	byKind                   [3][2]int // [kind][ops, in span]
	rates                    []fileRate
}

func (ta *tally) addTo(o *tally) {
	o.files += ta.files
	o.ops += ta.ops
	o.applied += ta.applied
	o.inSpan += ta.inSpan
	o.refused += ta.refused
	o.oneLine += ta.oneLine
	o.setOps += ta.setOps
	o.wrong += ta.wrong
	o.baseline += ta.baseline
	o.baselineOK += ta.baselineOK
	if o.refusals == nil {
		o.refusals = map[string]int{}
	}
	for k, v := range ta.refusals {
		o.refusals[k] += v
	}
	for k := range o.byKind {
		o.byKind[k][0] += ta.byKind[k][0]
		o.byKind[k][1] += ta.byKind[k][1]
	}
	o.rates = append(o.rates, ta.rates...)
}

// perFile is the mean over files of each file's share of operations applied within the intended
// lines, so a large file does not outweigh a small one.
func (ta *tally) perFile() float64 {
	var sum float64
	n := 0
	for _, r := range ta.rates {
		if r.ops > 0 {
			sum += float64(r.ok) / float64(r.ops)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return 100 * sum / float64(n)
}

func (ta *tally) worst(n int) []fileRate {
	r := append([]fileRate(nil), ta.rates...)
	sort.Slice(r, func(i, j int) bool {
		return float64(r[i].ok)/float64(max(r[i].ops, 1)) < float64(r[j].ok)/float64(max(r[j].ops, 1))
	})
	if len(r) > n {
		r = r[:n]
	}
	return r
}

func (ta *tally) add(f file) {
	ta.files++
	if ta.refusals == nil {
		ta.refusals = map[string]int{}
	}
	fr := fileRate{name: f.name}
	for _, op := range ops(f.text) {
		ta.ops++
		fr.ops++
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
			if want, ok := oracle(f.text, op); ok {
				got, derr := decode(r.Text)
				if derr != nil || !reflect.DeepEqual(want, got) {
					ta.wrong++
				}
			}
			lo, hi := touched(f.text, r)
			if spOK && !outside(f.text, string(r.Text), sp) {
				ta.inSpan++
				fr.ok++
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
	ta.rates = append(ta.rates, fr)
}

func bucket(r string) string {
	r = regexp.MustCompile(`\([^)]*\)`).ReplaceAllString(r, "(...)")
	r = regexp.MustCompile(`%?"[^"]*"`).ReplaceAllString(r, "X")
	return r
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func TestCorpusReport(t *testing.T) {
	groups := map[string]*tally{}
	var order []string
	real := map[string]bool{}
	corpus := append(collect(t), generated(300)...)
	for _, f := range corpus {
		ta := groups[f.group]
		if ta == nil {
			ta = &tally{}
			groups[f.group] = ta
			order = append(order, f.group)
		}
		real[f.group] = real[f.group] || f.real
		ta.add(f)
	}
	sort.Strings(order)
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-42s %5s %7s %8s %8s %8s %10s %9s\n", "group", "files", "ops", "applied", "per op", "per file", "refused", "re-emit")
	row := func(name string, ta *tally) {
		fmt.Fprintf(&b, "%-42s %5d %7d %7.1f%% %7.1f%% %7.1f%% %9.1f%% %8.1f%%\n", name, ta.files, ta.ops,
			pct(ta.applied, ta.ops), pct(ta.inSpan, ta.ops), ta.perFile(), pct(ta.refused, ta.ops), pct(ta.baselineOK, ta.baseline))
	}
	var realAll, all tally
	for _, g := range order {
		if real[g] {
			row(g+" [real]", groups[g])
			groups[g].addTo(&realAll)
		} else {
			row(g, groups[g])
		}
		groups[g].addTo(&all)
	}
	fmt.Fprintf(&b, "\n")
	row("REAL, single-document files (headline)", &realAll)
	strict := realAll
	strict.rates = append([]fileRate(nil), realAll.rates...)
	for _, f := range multiDocFiles {
		n := len(ops(f.text))
		strict.files++
		strict.ops += n
		strict.refused += n
		strict.rates = append(strict.rates, fileRate{name: f.name, ops: n})
	}
	row(fmt.Sprintf("REAL, with the %d multi-document files as misses", len(multiDocFiles)), &strict)
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
	for _, g := range append(order, "REAL") {
		ta := groups[g]
		if g == "REAL" {
			ta = &realAll
		}
		var rs []kv
		for k, v := range ta.refusals {
			rs = append(rs, kv{k, v})
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i].v > rs[j].v })
		if len(rs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "refusals, %s:\n", g)
		for _, r := range rs {
			fmt.Fprintf(&b, "  %6d  %s\n", r.v, r.k)
		}
		if g == "REAL" || g == "external open-source files" || g == "JSON configs (valid JSON)" || g == "adversarial (hand-written)" {
			for _, w := range ta.worst(6) {
				if w.ok < w.ops {
					fmt.Fprintf(&b, "    worst file: %-60s %5.1f%% of %d\n", w.name, pct(w.ok, w.ops), w.ops)
				}
			}
		}
	}
	// The bar (docs/design/visual-editor.md, decision 3): at least 95 percent of operations touch
	// only the intended lines, with a refusal counted as a miss, on the real files taken together
	// (per operation and per file) and on each other group but two. The adversarial files are
	// hand-written to be refused in part, and the plan-test JSON with tab indentation is read by
	// another path in the editor.
	if p := pct(realAll.inSpan, realAll.ops); p < 95 {
		t.Errorf("real single-document files: %.1f%% of operations within the intended lines, want at least 95%%", p)
	}
	if p := realAll.perFile(); p < 95 {
		t.Errorf("real files: %.1f%% per file within the intended lines, want at least 95%%", p)
	}
	for _, g := range order {
		if g == "adversarial (hand-written)" || g == "JSON test templates (placeholders)" {
			continue
		}
		if p := pct(groups[g].inSpan, groups[g].ops); p < 95 {
			t.Errorf("%s: %.1f%% of operations within the intended lines, want at least 95%%", g, p)
		}
	}
	fmt.Fprintf(&b, "\nexternal files left out because they hold several documents: %d %v\n", len(multiDoc), multiDoc)
	t.Log(b.String())
	if all.wrong != 0 {
		t.Errorf("%d operations produced text the oracle disagrees with", all.wrong)
	}
}

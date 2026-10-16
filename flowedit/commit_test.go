package flowedit

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// commit is the only way text leaves the engine, so a hostile splice (one the scanners could
// produce if they had a bug) must be refused there, not only by the Verify unit tests: the data
// wrong, the range outside the text, bytes outside the range changed, a comment moved.
func TestCommitRefusesHostileSplices(t *testing.T) {
	src := []byte(sample)
	flowAt := bytes.Index(src, []byte("refund"))
	set := Op{Kind: Set, Path: []any{"flow"}, Value: "x"}
	good, err := Apply(src, set)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		lo, hi int
		frag   string
		op     Op
	}{
		"the wrong value":                   {flowAt, flowAt + len("refund"), "y", set},
		"a value that is not a scalar":      {flowAt, flowAt + len("refund"), "[x]", set},
		"extra data after the value":        {flowAt, flowAt + len("refund"), "x\nstray: 1", set},
		"the range reversed":                {flowAt + 3, flowAt, "x", set},
		"a negative start":                  {-1, flowAt + len("refund"), "x", set},
		"an end past the text":              {flowAt, len(src) + 1, "x", set},
		"the right data over too much":      {flowAt, flowAt + len("refund   # the label"), "x", set},
		"the right data over too little":    {flowAt + 1, flowAt + len("refund"), "x", set},
		"the label comment swallowed":       {flowAt, bytes.Index(src, []byte("\n\n# nodes")), "x", set},
		"a delete that removes nothing":     {flowAt, flowAt, "", Op{Kind: Delete, Path: []any{"flow"}}},
		"an insert that adds the wrong key": {len(src), len(src), "other: 1\n", Op{Kind: Insert, Path: nil, Key: "entry2", Value: 1}},
	} {
		if r, err := commit(src, c.lo, c.hi, c.frag, c.op); err == nil {
			t.Errorf("%s: accepted\n%s", name, r.Text)
		}
	}
	// The real splice passes through commit unchanged.
	r, err := commit(src, good.Lo, good.Hi, string(good.Frag), set)
	if err != nil || !bytes.Equal(r.Text, good.Text) {
		t.Fatalf("the real splice was refused or changed: %v", err)
	}
}

// Whatever Apply returns, Text is the original with Frag in place of [Lo, Hi), and nothing else.
func TestResultIsTheSpliceAndNothingElse(t *testing.T) {
	src := []byte(sample)
	for _, op := range ops(sample) {
		r, err := Apply(src, op)
		if err != nil {
			continue
		}
		want := append(append(append([]byte{}, src[:r.Lo]...), r.Frag...), src[r.Hi:]...)
		if !bytes.Equal(r.Text, want) {
			t.Fatalf("op %+v: Text is not src[:Lo]+Frag+src[Hi:]", op)
		}
	}
}

// The guarantee is "no text leaves the engine unverified". It holds only while there is one place
// that builds a Result, and that place verifies. A second constructor, even an unexported one for
// tests, is a way around it, so this fails when one appears.
func TestOnlyCommitBuildsAResult(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var builders []string
	verifies := false
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "Result" {
						builders = append(builders, fn.Name.Name)
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "Verify" && fn.Name.Name == "commit" {
						verifies = true
					}
				}
				return true
			})
		}
	}
	if len(builders) != 1 || builders[0] != "commit" {
		t.Errorf("a Result is built in %v; only commit may build one", builders)
	}
	if !verifies {
		t.Error("commit does not call Verify")
	}
}

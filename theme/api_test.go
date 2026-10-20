package theme

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// publicAPI is every exported name of the package, reviewed. The stylesheet generators and the
// command are not on it on purpose: a host passes checked themes to a handler and gets pages, never
// CSS, and has no way around the checks. A new exported name fails TestPublicAPI until a reviewer
// adds it here; read docs/design/themes.md ("Embedding") first.
var publicAPI = []string{
	"Check", "CheckError", "Error", "Load", "Lookup", "MaxSize", "Measure", "Parse", "Report",
	"Severity", "Shipped", "Theme", "Version", "Warning",
	"CheckError.Error", "Measure.String", "Report.Err", "Report.Failures", "Severity.String",
	"Theme.Dark", "Theme.Name", "Theme.Report", "Theme.Shipped",
}

// stringers are the exported functions and methods that return a string, and so the only ones that
// could hand a host text such as CSS. Each is a name or a message.
var stringers = []string{"CheckError.Error", "Measure.String", "Severity.String", "Theme.Name"}

// apiOf lists the exported names of the package's non-test files, methods as Type.Method, and the
// subset of functions and methods that return a string.
func apiOf(t *testing.T) (names, returnsString []string) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				name := d.Name.Name
				if d.Recv != nil {
					recv := d.Recv.List[0].Type
					if star, ok := recv.(*ast.StarExpr); ok {
						recv = star.X
					}
					rt := recv.(*ast.Ident).Name
					if !ast.IsExported(rt) {
						continue
					}
					name = rt + "." + name
				}
				if !ast.IsExported(d.Name.Name) {
					continue
				}
				names = append(names, name)
				if d.Type.Results != nil {
					for _, r := range d.Type.Results.List {
						if id, ok := r.Type.(*ast.Ident); ok && id.Name == "string" {
							returnsString = append(returnsString, name)
						}
					}
				}
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					switch sp := sp.(type) {
					case *ast.TypeSpec:
						if sp.Name.IsExported() {
							names = append(names, sp.Name.Name)
						}
					case *ast.ValueSpec:
						for _, n := range sp.Names {
							if n.IsExported() {
								names = append(names, n.Name)
							}
						}
					}
				}
			}
		}
	}
	sort.Strings(names)
	sort.Strings(returnsString)
	return names, returnsString
}

func TestPublicAPI(t *testing.T) {
	names, strs := apiOf(t)
	want := slices.Clone(publicAPI)
	sort.Strings(want)
	if !slices.Equal(names, want) {
		t.Errorf("the exported API changed.\n got: %v\nwant: %v\nA new exported name is a public commitment; a reviewer adds it to publicAPI (see its comment).", names, want)
	}
	wantStr := slices.Clone(stringers)
	sort.Strings(wantStr)
	if !slices.Equal(strs, wantStr) {
		t.Errorf("the exported functions returning a string are %v, want %v: a host must not be able to get CSS out of the package", strs, wantStr)
	}
}

// reachable lists, for the exported types a host holds, every exported field (promoted ones included)
// and every method in the method set (promoted ones included) with its type. The AST list above
// sees only top-level names and the methods declared on exported types; this one sees what a
// value can do, so a field such as `CSS string`, or a method promoted from an embedded unexported
// type, is a failure here.
func reachable() []string {
	var out []string
	for _, v := range []any{&Theme{}, Report{}, Measure{}, &CheckError{}, Severity(0)} {
		rt := reflect.TypeOf(v)
		base := rt
		if base.Kind() == reflect.Pointer {
			base = base.Elem()
		}
		if base.Kind() == reflect.Struct {
			for _, f := range reflect.VisibleFields(base) {
				if f.IsExported() {
					out = append(out, base.Name()+"."+f.Name+" "+f.Type.String())
				}
			}
		}
		for i := 0; i < rt.NumMethod(); i++ {
			m := rt.Method(i)
			out = append(out, base.Name()+"."+m.Name+" "+m.Type.String())
		}
	}
	sort.Strings(out)
	return out
}

// publicSurface is the reviewed result of reachable. A host holds a *Theme, and none of what it can
// reach returns anything but a name, a message, a flag or a report.
var publicSurface = []string{
	"CheckError.Error func(*theme.CheckError) string",
	"CheckError.Report theme.Report",
	"Measure.Check string",
	"Measure.Pass bool",
	"Measure.Severity theme.Severity",
	"Measure.String func(theme.Measure) string",
	"Measure.Subject string",
	"Measure.Threshold float64",
	"Measure.Value float64",
	"Report.Err func(theme.Report) error",
	"Report.Failures func(theme.Report, theme.Severity) []theme.Measure",
	"Report.Measures []theme.Measure",
	"Report.Theme string",
	"Severity.String func(theme.Severity) string",
	"Theme.Dark func(*theme.Theme) bool",
	"Theme.Name func(*theme.Theme) string",
	"Theme.Report func(*theme.Theme) theme.Report",
	"Theme.Shipped func(*theme.Theme) bool",
}

func TestPublicSurface(t *testing.T) {
	got := reachable()
	if !slices.Equal(got, publicSurface) {
		t.Errorf("what a host can reach on a theme changed.\n got: %q\nwant: %q\nA new field or method is a public commitment and a possible route to CSS: a reviewer adds it to publicSurface.", got, publicSurface)
	}
}

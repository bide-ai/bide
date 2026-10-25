// Command browsertests fails when a test that needs a browser is not the test its workflow runs. The
// Chrome jobs (.github/workflows/*-chrome.yml, page-mtls.yml) set an environment variable such as
// BIDE_EDIT_BROWSER=1 and run `go test -run '<pattern>'`; a test that checks that variable but whose
// name the pattern does not match is skipped everywhere, locally for lack of Chrome and in CI by the
// pattern, and passes silently. For every test that reaches a check of a BIDE_*_BROWSER variable
// (directly, or through the helpers of its package), the command checks that some workflow sets the
// variable and that its -run pattern matches the test's name, and that every variable a workflow sets
// is checked by some test.
//
// Usage: browsertests [root]   (default: the current directory, the repository root)
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	problems, err := check(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "browsertests:", err)
		os.Exit(2)
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, p)
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
}

var (
	envVar   = regexp.MustCompile(`^BIDE_[A-Z_]*BROWSER$`)
	setLine  = regexp.MustCompile(`^\s*(BIDE_[A-Z_]*BROWSER):\s*['"]?1['"]?`)
	stepLine = regexp.MustCompile(`^\s*- (name|uses):`)
	runLine  = regexp.MustCompile(`^\s*run:\s*go test\b(.*)$`)
	patFlag  = regexp.MustCompile(`-run[ =]+('[^']*'|"[^"]*"|\S+)`)
)

// workflowPatterns reads, for each BIDE_*_BROWSER variable a workflow sets, the -run patterns of the go
// test commands in the same step, by variable.
func workflowPatterns(root string) (map[string][]string, error) {
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			m := setLine.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			v := m[1]
			if _, ok := out[v]; !ok {
				out[v] = nil
			}
			for _, n := range lines[i+1:] {
				if stepLine.MatchString(n) {
					break
				}
				if r := runLine.FindStringSubmatch(n); r != nil {
					if p := patFlag.FindStringSubmatch(r[1]); p != nil {
						out[v] = append(out[v], strings.Trim(p[1], `'"`))
					} else {
						out[v] = append(out[v], "") // no -run: every test runs
					}
				}
			}
		}
	}
	return out, nil
}

// gatedTests returns, by variable, the names of the tests that reach a check of it. A function reaches a
// variable if it names it in a string, or calls (by a bare name) a function of its package that does.
func gatedTests(root string) (map[string][]string, error) {
	dirs := map[string][]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "node_modules" || n == ".git" || n == "testdata" || n == "browsertests" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") {
			dirs[filepath.Dir(p)] = append(dirs[filepath.Dir(p)], p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, files := range dirs {
		type fn struct {
			test    bool
			vars    map[string]bool
			callees map[string]bool
		}
		fns := map[string]*fn{}
		any := false
		for _, f := range files {
			src, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			file, err := parser.ParseFile(token.NewFileSet(), f, src, 0)
			if err != nil {
				continue // a file this command cannot read is not its concern
			}
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil || fd.Recv != nil {
					continue
				}
				x := &fn{test: strings.HasPrefix(fd.Name.Name, "Test") && strings.HasSuffix(f, "_test.go"), vars: map[string]bool{}, callees: map[string]bool{}}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.BasicLit:
						if n.Kind == token.STRING {
							if s, err := strconv.Unquote(n.Value); err == nil && envVar.MatchString(s) {
								x.vars[s] = true
								any = true
							}
						}
					case *ast.CallExpr:
						if id, ok := n.Fun.(*ast.Ident); ok {
							x.callees[id.Name] = true
						}
					}
					return true
				})
				fns[fd.Name.Name] = x
			}
		}
		if !any {
			continue
		}
		var reach func(name string, seen map[string]bool, into map[string]bool)
		reach = func(name string, seen map[string]bool, into map[string]bool) {
			if seen[name] || fns[name] == nil {
				return
			}
			seen[name] = true
			for v := range fns[name].vars {
				into[v] = true
			}
			for c := range fns[name].callees {
				reach(c, seen, into)
			}
		}
		for name, x := range fns {
			if !x.test {
				continue
			}
			vars := map[string]bool{}
			reach(name, map[string]bool{}, vars)
			for v := range vars {
				out[v] = append(out[v], name)
			}
		}
	}
	for v := range out {
		slices.Sort(out[v])
	}
	return out, nil
}

// check returns one line per problem found below root.
func check(root string) ([]string, error) {
	wf, err := workflowPatterns(root)
	if err != nil {
		return nil, err
	}
	tests, err := gatedTests(root)
	if err != nil {
		return nil, err
	}
	var problems []string
	for v, names := range tests {
		pats, ok := wf[v]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s is checked by %s but no workflow sets it, so no job runs them", v, strings.Join(names, ", ")))
			continue
		}
		for _, name := range names {
			matched := false
			for _, p := range pats {
				// go test -run splits the pattern at slashes; the first element selects the top-level test.
				top, _, _ := strings.Cut(p, "/")
				if re, err := regexp.Compile(top); err == nil && re.MatchString(name) {
					matched = true
				}
			}
			if !matched {
				problems = append(problems, fmt.Sprintf("%s needs %s but the workflow's -run pattern %q does not match it: CI never runs it", name, v, strings.Join(pats, " | ")))
			}
		}
	}
	for v := range wf {
		if len(tests[v]) == 0 {
			problems = append(problems, fmt.Sprintf("a workflow sets %s but no test checks it", v))
		}
	}
	slices.Sort(problems)
	return problems, nil
}

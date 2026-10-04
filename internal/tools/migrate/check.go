package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
)

// CheckModule type-checks the module at dir with the rewritten files (out) against the new bide
// API, and returns a finding for each error, so a run never ends quietly with code that does not
// compile. The new API is:
//
//   - bide, a directory holding the new bide module or a version of it (it replaces any version
//     or replace directive the module's go.mod names); empty means the version this command was
//     built from (go run .../migrate@<version>);
//   - with neither, the module itself, when it is bide or reaches bide through a local replace
//     directive (bide's own modules).
//
// When no new API can be found (a command built from a checkout, with no -bide), the run is not
// checked, and that is itself a finding.
func CheckModule(dir string, patterns []string, out map[string][]byte, bide string) ([]Finding, error) {
	if bide == "" {
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Path == bidePath && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			bide = bi.Main.Version
		}
	}
	flags, cleanup, err := checkFlags(dir, bide)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if flags == nil && bide == "" {
		if self, err := againstItself(dir); err != nil {
			return nil, err
		} else if !self {
			return []Finding{{Pos: token.Position{Filename: filepath.Join(dir, "go.mod")}, Rule: "check",
				Msg: "the rewritten code was not type-checked against the new API: pass -bide <version or directory of the new bide module>"}}, nil
		}
	}
	pkgs, err := Load(dir, patterns, out, flags...)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var fs []Finding
	for _, p := range pkgs {
		for _, e := range p.TypeErrors {
			var te types.Error
			if !errors.As(e, &te) {
				fs = append(fs, Finding{Rule: "check", Msg: "does not type-check after the rewrite: " + e.Error()})
				continue
			}
			pos := te.Fset.Position(te.Pos)
			key := pos.String() + te.Msg
			if seen[key] {
				continue // a package and its test variant share files
			}
			seen[key] = true
			fs = append(fs, Finding{Pos: pos, Rule: "check", Msg: "does not type-check after the rewrite: " + te.Msg})
		}
	}
	remapFindings(fs, out, nil) // the errors are in the rewritten files: name the lines on disk
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i].Pos, fs[j].Pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Offset < b.Offset
	})
	return fs, nil
}

// goMod is what `go mod edit -json` reports of a go.mod.
type goMod struct {
	Module  struct{ Path string }
	Require []struct{ Path, Version string }
	Replace []struct {
		Old struct{ Path string }
		New struct{ Path, Version string }
	}
}

func readGoMod(dir string) (*goMod, error) {
	cmd := exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go mod edit -json in %s: %v", dir, err)
	}
	var m goMod
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// againstItself reports whether the module at dir already builds against the bide in the same
// tree: it is bide, or replaces bide with a local directory.
func againstItself(dir string) (bool, error) {
	m, err := readGoMod(dir)
	if err != nil {
		return false, err
	}
	if m.Module.Path == bidePath || strings.HasPrefix(m.Module.Path, bidePath+"/") {
		return true, nil
	}
	for _, r := range m.Replace {
		if r.Old.Path == bidePath && r.New.Version == "" {
			return true, nil
		}
	}
	return false, nil
}

// checkFlags returns the go command flags that build the module at dir against bide (a version or
// a directory), through a copy of its go.mod; none when it builds against itself or bide is empty.
func checkFlags(dir, bide string) (flags []string, cleanup func(), err error) {
	cleanup = func() {}
	if bide == "" {
		return nil, cleanup, nil
	}
	m, err := readGoMod(dir)
	if err != nil {
		return nil, cleanup, err
	}
	if m.Module.Path == bidePath {
		return nil, cleanup, nil // bide's root module is the new API
	}
	tmp, err := os.MkdirTemp("", "migrate-check")
	if err != nil {
		return nil, cleanup, err
	}
	cleanup = func() { os.RemoveAll(tmp) }
	mod := filepath.Join(tmp, "check.mod")
	for _, f := range [][2]string{{"go.mod", mod}, {"go.sum", filepath.Join(tmp, "check.sum")}} {
		b, err := os.ReadFile(filepath.Join(dir, f[0]))
		if err != nil && f[0] == "go.sum" && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, cleanup, err
		}
		if err := os.WriteFile(f[1], b, 0o644); err != nil {
			return nil, cleanup, err
		}
	}
	var edits []string
	if st, err := os.Stat(bide); err == nil && st.IsDir() {
		// every module of the bide tree, replaced by its directory
		abs, err := filepath.Abs(bide)
		if err != nil {
			return nil, cleanup, err
		}
		err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && p != abs {
				return filepath.SkipDir
			}
			if !d.IsDir() && d.Name() == "go.mod" {
				rel, _ := filepath.Rel(abs, filepath.Dir(p))
				path := bidePath
				if rel != "." {
					path += "/" + filepath.ToSlash(rel)
				}
				edits = append(edits, "-replace="+path+"="+filepath.Dir(p))
			}
			return nil
		})
		if err != nil {
			return nil, cleanup, err
		}
	} else {
		edits = append(edits, "-require="+bidePath+"@"+bide)
		for _, r := range m.Require {
			if strings.HasPrefix(r.Path, bidePath+"/") {
				edits = append(edits, "-require="+r.Path+"@"+bide)
			}
		}
	}
	cmd := exec.Command("go", append([]string{"mod", "edit", "-modfile=" + mod}, edits...)...)
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, cleanup, fmt.Errorf("go mod edit: %v\n%s", err, b)
	}
	return []string{"-modfile=" + mod, "-mod=mod"}, cleanup, nil
}

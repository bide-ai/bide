package snip

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// PackageNames lists the packages a block may use without importing them: every package of the
// standard library and of the main module(s) at root, except internal, vendored and main
// packages, by name. When two of them share a name, a package of the main module(s) wins over the
// standard library, and within the standard library the one with the shortest import path wins;
// a name still tied is ambiguous, and maps to "". It also returns the main module's Go version, as
// "go1.N".
func PackageNames(root string) (map[string]string, string, error) {
	goCmd := func(args ...string) ([]byte, error) {
		cmd := exec.Command("go", args...)
		cmd.Dir = root
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
		}
		return out, nil
	}
	names := map[string]string{}
	goVer := ""
	ver, err := goCmd("list", "-m", "-f", "{{.Path}} {{.GoVersion}}")
	if err != nil {
		return nil, "", err
	}
	var modules []string
	for _, line := range strings.Split(strings.TrimSpace(string(ver)), "\n") {
		path, gv, _ := strings.Cut(line, " ")
		modules = append(modules, path+"/...")
		if gv != "" && goVer == "" {
			goVer = "go" + goMinor(gv)
		}
	}
	out, err := goCmd(append([]string{"list", "-e", "-f", "{{.ImportPath}} {{.Name}} {{.Standard}}", "std"}, modules...)...)
	if err != nil {
		return nil, "", err
	}
	type cand struct {
		path string
		std  bool
	}
	byName := map[string][]cand{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[1] == "main" || !importable(f[0]) {
			continue
		}
		byName[f[1]] = append(byName[f[1]], cand{f[0], f[2] == "true"})
	}
	for name, cs := range byName {
		var mod, std []string
		for _, x := range cs {
			if x.std {
				std = append(std, x.path)
			} else {
				mod = append(mod, x.path)
			}
		}
		switch {
		case len(mod) == 1:
			names[name] = mod[0]
		case len(mod) > 1:
			names[name] = ""
		default:
			sort.Slice(std, func(i, j int) bool { return strings.Count(std[i], "/") < strings.Count(std[j], "/") })
			if len(std) > 1 && strings.Count(std[0], "/") == strings.Count(std[1], "/") {
				names[name] = ""
			} else {
				names[name] = std[0]
			}
		}
	}
	return names, goVer, nil
}

// goMinor turns "1.27.0" into "1.27".
func goMinor(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return strings.Join(parts, ".")
}

func importable(path string) bool {
	for _, el := range strings.Split(path, "/") {
		if el == "internal" || el == "vendor" || el == "testdata" || el == "cmd" || el == "examples" {
			return false
		}
	}
	return true
}

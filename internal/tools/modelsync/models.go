package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// readmePath is the README that holds the model-to-code maps, relative to the root.
const readmePath = "spec/tla/README.md"

// model is one model under spec/tla: its directory name, the actions its spec defines, and what
// the README's model-to-code map says about it.
type model struct {
	name    string          // the directory under spec/tla, and the name markers use
	spec    string          // spec/tla/<name>/<Name>.tla, relative to the root
	defined map[string]bool // PlusCal labels and top-level operators of the spec
	mapped  map[string]int  // actions listed in the README map, with the README line
	noCode  map[string]int  // actions the README lists as having no Go region, with the line
	hasMap  bool
}

var (
	// A PlusCal label: an identifier and a colon (or ":+" / ":-") at the start of a line.
	labelRE = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*):[-+]?(\s|$)`)
	// A top-level operator definition: Name == or Name(args) ==, at column 0.
	operatorRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*(\([^)]*\))?\s*==`)
	// The README anchors.
	mapAnchorRE    = regexp.MustCompile(`^<!--\s*modelsync:\s*map\s+([a-z0-9_-]+)\s*-->\s*$`)
	noCodeAnchorRE = regexp.MustCompile(`^<!--\s*modelsync:\s*no-code\s+([a-z0-9_-]+)\s+(.*?)\s*-->\s*$`)
	anyAnchorRE    = regexp.MustCompile(`^<!--\s*modelsync:`)
	// An action named in a map table cell: `Name` or `Name(args)`.
	cellActionRE = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_]*)(\\([^)`]*\\))?`")
	tableSepRE   = regexp.MustCompile(`^\|[\s|:-]+\|\s*$`)
)

// loadModels finds every model directory under spec/tla (one holding <Name>.tla, Name being the
// directory name in any case: claims/Claims.tla, toolcall/ToolCall.tla), reads the actions its
// spec defines, and reads the README's map tables. Problems in the README are findings; an
// unreadable file is an error.
func loadModels(root string, r *report) (map[string]*model, error) {
	dirs, err := os.ReadDir(filepath.Join(root, "spec/tla"))
	if err != nil {
		return nil, err
	}
	models := map[string]*model{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		name := d.Name()
		file, err := specFile(filepath.Join(root, "spec/tla", name), name)
		if err != nil {
			return nil, err
		}
		if file == "" {
			continue
		}
		spec := filepath.ToSlash(filepath.Join("spec/tla", name, file))
		defined, err := tlaActions(filepath.Join(root, spec))
		if err != nil {
			return nil, err
		}
		models[name] = &model{name: name, spec: spec, defined: defined, mapped: map[string]int{}, noCode: map[string]int{}}
	}
	if err := readMaps(filepath.Join(root, readmePath), models, r); err != nil {
		return nil, err
	}
	return models, nil
}

// specFile returns the name of the file in dir that is the model's spec, <name>.tla matched
// without regard to case, or "" when there is none. The directory is listed rather than the
// name built and opened, so the result is the same on a case-sensitive file system and on one
// that is not.
func specFile(dir, name string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(e.Name(), name+".tla") {
			return e.Name(), nil
		}
	}
	return "", nil
}

// tlaActions returns the PlusCal labels and top-level operators defined in a .tla file.
func tlaActions(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if m := labelRE.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
		if m := operatorRE.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out, nil
}

// readMaps reads the map tables and no-code lists of the README into models.
func readMaps(path string, models map[string]*model, r *report) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return err
	}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		at := fmt.Sprintf("%s:%d", readmePath, i+1)
		if m := mapAnchorRE.FindStringSubmatch(line); m != nil {
			mod := models[m[1]]
			if mod == nil {
				r.errorf("%s: map anchor names %q, which is not a model directory under spec/tla", at, m[1])
				continue
			}
			mod.hasMap = true
			j := i + 1
			for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
				j++
			}
			if j >= len(lines) || !strings.HasPrefix(strings.TrimSpace(lines[j]), "|") {
				r.errorf("%s: map anchor for %s is not followed by a table", at, m[1])
				continue
			}
			rows := 0
			for ; j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), "|"); j++ {
				row := strings.TrimSpace(lines[j])
				rows++
				if rows == 1 || tableSepRE.MatchString(row) {
					continue // the header row and the separator
				}
				cells := strings.Split(strings.Trim(row, "|"), "|")
				acts := cellActionRE.FindAllStringSubmatch(cells[0], -1)
				if len(acts) == 0 {
					r.errorf("%s:%d: map row of %s names no action in its first cell", readmePath, j+1, m[1])
				}
				for _, a := range acts {
					if _, dup := mod.mapped[a[1]]; !dup {
						mod.mapped[a[1]] = j + 1
					}
				}
			}
			i = j - 1
			continue
		}
		if m := noCodeAnchorRE.FindStringSubmatch(line); m != nil {
			mod := models[m[1]]
			if mod == nil {
				r.errorf("%s: no-code list names %q, which is not a model directory under spec/tla", at, m[1])
				continue
			}
			for _, a := range strings.Fields(m[2]) {
				mod.noCode[a] = i + 1
			}
			continue
		}
		if anyAnchorRE.MatchString(line) {
			r.errorf("%s: malformed modelsync comment %q", at, line)
		}
	}
	return nil
}

// checkConsistency checks markers against the specs and the README maps.
func checkConsistency(models map[string]*model, regions []region, r *report) {
	marked := map[string]map[string]bool{}
	for _, rg := range regions {
		mod := models[rg.model]
		if marked[rg.model] == nil {
			marked[rg.model] = map[string]bool{}
		}
		for _, a := range rg.actions {
			marked[rg.model][a] = true
			at := fmt.Sprintf("%s:%d", rg.file, rg.begin)
			switch {
			case !mod.defined[a]:
				r.errorf("%s: marker names %s.%s, which %s does not define (renamed or removed?)", at, rg.model, a, mod.spec)
			case mod.mapped[a] == 0:
				r.errorf("%s: marker names %s.%s, which the %s map in %s does not list", at, rg.model, a, rg.model, readmePath)
			case mod.noCode[a] != 0:
				r.errorf("%s: marker names %s.%s, which %s lists as having no Go region", at, rg.model, a, readmePath)
			}
		}
	}
	for _, name := range sortedKeys(models) {
		mod := models[name]
		for _, a := range sortedKeys(mod.mapped) {
			at := fmt.Sprintf("%s:%d", readmePath, mod.mapped[a])
			if !mod.defined[a] {
				r.errorf("%s: the %s map lists %s, which %s does not define (renamed or removed?)", at, name, a, mod.spec)
				continue
			}
			if mod.noCode[a] == 0 && !marked[name][a] {
				r.errorf("%s: the %s map lists %s, but no Go region is marked \"// protocol:%s begin ... %s ...\"", at, name, a, name, a)
			}
		}
		for _, a := range sortedKeys(mod.noCode) {
			if mod.mapped[a] == 0 {
				r.errorf("%s:%d: the %s no-code list names %s, which its map does not list", readmePath, mod.noCode[a], name, a)
			}
		}
		if !mod.hasMap && len(marked[name]) > 0 {
			r.errorf("Go code marks regions of model %s, but %s has no map for it", name, readmePath)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

package plan

import (
	"flag"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var updateOther = flag.Bool("update-other", false, "rewrite plan/testdata/other_findings.txt")

// errorCorpus holds configs that each have one or more of the errors plan reports, to see that every
// error has a finding. n(...) and w(...) build a node list and a wiring list.
func errorCorpus() map[string]string {
	n := func(parts ...string) string { return `"nodes":[` + strings.Join(parts, ",") + `]` }
	nd := func(name, block string, extra ...string) string {
		return `{"name":"` + name + `","block":"` + block + `"` + strings.Join(append([]string{""}, extra...), ",") + `}`
	}
	head := `{"version":1,"flow":"f","entry":"a",`
	return map[string]string{
		"no nodes":                 head + n() + `,"wiring":[]}`,
		"unnamed node":             head + n(nd("a", "s2s"), nd("", "s2s")) + `,"wiring":[]}`,
		"duplicate node":           head + n(nd("a", "s2s"), nd("a", "s2s")) + `,"wiring":[]}`,
		"node without block":       head + n(nd("a", "")) + `,"wiring":[]}`,
		"unknown block":            head + n(nd("a", "nowhere")) + `,"wiring":[]}`,
		"step name with colon":     head + n(nd("a:1", "s2s")) + `,"wiring":[]}`,
		"unknown safety":           head + n(nd("a", "s2s", `"safety":"safe"`)) + `,"wiring":[]}`,
		"safety above":             head + n(nd("a", "s2s", `"safety":"idempotent"`)) + `,"wiring":[]}`,
		"bad approval":             head + n(nd("a", "s2s", `"approval":{"need":0,"approvers":[]}`)) + `,"wiring":[]}`,
		"entry unknown":            `{"version":1,"flow":"f","entry":"zz",` + n(nd("a", "s2s")) + `,"wiring":[]}`,
		"wiring sets none":         head + n(nd("a", "s2s")) + `,"wiring":[{}]}`,
		"wiring sets two":          head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[{"edge":["a","b"],"switch":"a"}]}`,
		"edge not a pair":          head + n(nd("a", "s2s")) + `,"wiring":[{"edge":["a"]}]}`,
		"edge unknown end":         head + n(nd("a", "s2s")) + `,"wiring":[{"edge":["a","ghost"]}]}`,
		"switch unknown":           head + n(nd("a", "s2s")) + `,"wiring":[{"switch":"ghost","when":[]}]}`,
		"arm no predicate":         head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[{"switch":"a","when":[{"to":"b"}]}]}`,
		"arm unknown predicate":    head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[{"switch":"a","when":[{"pred":"nope","to":"b"}]}]}`,
		"arm unknown target":       head + n(nd("a", "s2s")) + `,"wiring":[{"switch":"a","when":[{"pred":"nonEmpty","to":"ghost"}]}]}`,
		"else unknown":             head + n(nd("a", "s2s")) + `,"wiring":[{"switch":"a","when":[],"else":"ghost"}]}`,
		"join no name":             head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[{"join":"","inputs":["a","b"],"merge":"both"}]}`,
		"join no merge":            head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[{"join":"j","inputs":["a","b"]}]}`,
		"join unknown merge":       head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[{"join":"j","inputs":["a","b"],"merge":"nope"}]}`,
		"join unknown input":       head + n(nd("a", "s2s")) + `,"wiring":[{"join":"j","inputs":["a","ghost"],"merge":"both"}]}`,
		"join named as node":       head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[{"join":"a","inputs":["a","b"],"merge":"both"}]}`,
		"two switches":             head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[{"switch":"a","when":[],"else":"b"},{"switch":"a","when":[],"else":"b"}]}`,
		"edge into a join":         head + n(nd("a", "s2s"), nd("b", "s2s"), nd("c", "s2s")) + `,"wiring":[{"edge":["a","b"]},{"edge":["a","c"]},{"join":"j","inputs":["b","c"],"merge":"both"},{"edge":["a","j"]}]}`,
		"edge leaves a later join": head + n(nd("a", "s2s"), nd("b", "s2s"), nd("c", "s2s")) + `,"wiring":[{"edge":["j","c"]},{"edge":["a","b"]},{"join":"j","inputs":["a","b"],"merge":"both"}]}`,
		"switch and edge":          head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[{"switch":"a","when":[],"else":"b"},{"edge":["a","b"]}]}`,
		"fed by several":           head + n(nd("a", "s2s"), nd("b", "s2s"), nd("c", "s2s")) + `,"wiring":[{"edge":["a","c"]},{"edge":["b","c"]}]}`,
		"not reached":              head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[]}`,
		"cycle":                    head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[{"edge":["a","b"]},{"edge":["b","a"]}]}`,
		"loop no exit":             head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[{"edge":["a","b"]},{"switch":"b","when":[{"pred":"nonEmpty","to":"a","loop_max":3}]}]}`,
		"two loop back arms":       head + n(nd("a", "s2s"), nd("b", "s2s"), nd("c", "s2s")) + `,"wiring":[{"edge":["a","b"]},{"switch":"b","when":[{"pred":"nonEmpty","to":"a","loop_max":3},{"pred":"nonEmpty","to":"a","loop_max":2}],"else":"c"}]}`,
		"loop head not ancestor":   head + n(nd("a", "s2s"), nd("b", "s2s"), nd("h", "s2s"), nd("c", "s2s")) + `,"wiring":[{"edge":["a","b"]},{"edge":["a","h"]},{"switch":"b","when":[{"pred":"nonEmpty","to":"h","loop_max":3}],"else":"c"}]}`,
		"loop body not simple":     head + n(nd("a", "s2s"), nd("x", "s2s"), nd("b", "s2s"), nd("c", "s2s")) + `,"wiring":[{"edge":["a","x"]},{"edge":["a","b"]},{"switch":"b","when":[{"pred":"nonEmpty","to":"a","loop_max":3}],"else":"c"}]}`,
		"unreadable":               `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s","safty":"x"}],"wiring":[]}`,
		"unprintable name":         head + n(nd("a\\u202eb", "s2s")) + `,"wiring":[]}`,
		"key in another case":      `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s","Safety":"readonly"}],"wiring":[]}`,
		"another version":          `{"version":2,"flow":"f","entry":"a","nodes":[],"wiring":[]}`,
		"assertion violated":       head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[{"edge":["a","b"]}],"assert":["every path to b passes approval >= 1"]}`,
		"assertion invalid":        head + n(nd("a", "s2s")) + `,"wiring":[],"assert":["this is not a rule"]}`,
		"assertion unknown node":   head + n(nd("a", "s2s")) + `,"wiring":[],"assert":["ghost reachable"]}`,
		"edge types":               head + n(nd("a", "s2i"), nd("b", "s2s")) + `,"wiring":[{"edge":["a","b"]}]}`,
		"arm types":                head + n(nd("a", "s2s"), nd("b", "s2s")) + `,"wiring":[{"switch":"a","when":[{"pred":"isInt","to":"b"}]}]}`,
		"join input types":         head + n(nd("a", "s2s"), nd("x", "s2i"), nd("y", "s2s")) + `,"wiring":[{"edge":["a","x"]},{"edge":["a","y"]},{"join":"j","inputs":["x","y"],"merge":"both"}]}`,
		"terminal types":           head + n(nd("a", "s2s"), nd("b", "s2s"), nd("c", "s2i")) + `,"wiring":[{"switch":"a","when":[{"pred":"nonEmpty","to":"b"}],"else":"c"}]}`,
		"several at once":          head + n(nd("a", "s2i"), nd("b", "s2s"), nd("c", "nowhere"), nd("d", "s2s", `"safety":"idempotent"`)) + `,"wiring":[{"edge":["a","b"]},{"edge":["b","ghost"]}]}`,
	}
}

var (
	quotedRE      = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	numberRE      = regexp.MustCompile(`\d+`)
	registeredRE  = regexp.MustCompile(`\(registered: [^)]*\)`)
	bracketListRE = regexp.MustCompile(`\[[a-z ]+\]`)
)

// messageTemplate reduces a message to its words: the quoted names become %q and the numbers %d.
func messageTemplate(m string) string {
	return numberRE.ReplaceAllString(quotedRE.ReplaceAllString(m, "%q"), "%d")
}

// Every error in a report has exactly one finding with its message, in order, whatever else is wrong with
// the config and however it is checked (the structure alone, against a Registry, against a description);
// warnings have none. The messages whose finding is "other" are listed in testdata/other_findings.txt, so
// the unclassified set is visible, and reviewed, whenever it changes (go test ./plan -run Other -update-other).
func TestFindings_EveryErrorHasExactlyOne(t *testing.T) {
	reg := typedChainRegistry(t)
	desc := reg.Describe()
	others := map[string]bool{}
	hasError := map[string]bool{}
	for name, cfg := range errorCorpus() {
		for mode, run := range map[string]func() ConfigReport{
			"structure":   func() ConfigReport { return InspectConfig([]byte(cfg), nil) },
			"registry":    func() ConfigReport { return InspectConfig([]byte(cfg), reg, AllowUnusedBlocks()) },
			"description": func() ConfigReport { return InspectConfigDescribed([]byte(cfg), desc, AllowUnusedBlocks()) },
		} {
			r := run()
			var errs []string
			for _, p := range r.Problems {
				if p.Severity == "error" {
					errs = append(errs, p.Message)
				}
			}
			var got []string
			for _, f := range r.Findings {
				got = append(got, f.Message)
				if f.Kind == ProblemOther {
					others[messageTemplate(f.Message)] = true
					if len(f.Names) != 0 || f.Got != "" || f.Want != "" {
						t.Errorf("%s/%s: an other finding with names or types: %+v", name, mode, f)
					}
				}
			}
			if strings.Join(errs, "\n") != strings.Join(got, "\n") {
				t.Errorf("%s/%s: the errors and the findings differ\nerrors:   %q\nfindings: %q", name, mode, errs, got)
			}
			if len(errs) > 0 {
				hasError[name] = true
			}
		}
	}
	for name := range errorCorpus() {
		if !hasError[name] {
			t.Errorf("%s: no mode reports an error", name)
		}
	}
	var list []string
	for o := range others {
		list = append(list, o)
	}
	sort.Strings(list)
	text := strings.Join(list, "\n") + "\n"
	const path = "testdata/other_findings.txt"
	if *updateOther {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./plan -run EveryErrorHasExactlyOne -update-other)", err)
	}
	if string(want) != text {
		t.Errorf("the messages of the findings of kind other changed; review them (the set is the promise that is still to make) and run with -update-other:\nwant:\n%s\ngot:\n%s", want, text)
	}
}

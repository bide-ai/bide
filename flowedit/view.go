package flowedit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
)

// ModeLine is what the editor says wherever it shows a verdict: the check is the structural one
// `bide check` makes. Block, predicate and merge names and the types are checked when a program
// loads the flow with its plan.Registry (package plan/planview), and a gate a block declares in Go
// is not visible here.
const ModeLine = "structural check only: blocks and types are checked when a program loads the flow"

// View is what the editor page shows of one version of the config text: the same answers `bide
// check` and `bide view -config` give for it, computed in Go. The page computes nothing about the
// flow itself.
type View struct {
	// Name is the file's name as given on the command line, Format "yaml" or "json".
	Name   string `json:"name"`
	Format string `json:"format"`
	// Text is the config text this view is of.
	Text string `json:"text"`
	// Mode says the check is structural (see ModeLine).
	Mode string `json:"mode"`
	// ParseError is set when the text is not a readable config (invalid YAML, or a second
	// document); the rest of the view is then empty.
	ParseError string `json:"parse_error,omitempty"`
	// Flow is the flow's name, Entry its entry node.
	Flow  string `json:"flow,omitempty"`
	Entry string `json:"entry,omitempty"`
	// In and Out are the config's documentation type names, "" when it has none.
	In  string `json:"in,omitempty"`
	Out string `json:"out,omitempty"`
	// Verdict is the answer `bide view -config` opens with.
	Verdict    Verdict                `json:"verdict"`
	Errors     int                    `json:"errors"`
	Warnings   int                    `json:"warnings"`
	Problems   []plan.ConfigProblem   `json:"problems,omitempty"`
	Assertions []plan.AssertionResult `json:"assertions,omitempty"`
	// AssertionForms are the assert lines read as one of the six forms, aligned with Assertions: null
	// for a line that is none of them, which the page shows without an edit form.
	AssertionForms []*Assertion `json:"assertion_forms,omitempty"`
	// Nodes are the declared nodes in config order (order is data: the digest commits to it) and
	// Wiring the edges, switches and joins in config order.
	Nodes  []Node   `json:"nodes,omitempty"`
	Wiring []string `json:"wiring,omitempty"`
	// Wires are the same elements as data, in the same order: what the page edits wiring from.
	Wires []Wire `json:"wires,omitempty"`
	// Graph is the layered drawing as one SVG document with every label escaped. The page parses
	// it with DOMParser and never assigns it as markup.
	Graph string `json:"graph,omitempty"`
	// Weakening compares the text with the baseline (the committed file, or the file as the editor
	// opened it). The server sets it; BuildView, which sees one text, does not.
	Weakening *Weakening `json:"weakening,omitempty"`
	// Typed is set when the editor knows the program's blocks (WithRegistry, WithBlocks): what the
	// typed check rests on, the digest and the palette.
	Typed *TypedView `json:"typed,omitempty"`
	// Taint is the static taint view of the flow (the same analysis as bide check): the panel's headline,
	// the exposed nodes and what it cannot know. Each node's own verdict is Node.Taint.
	Taint *taintSummary `json:"taint,omitempty"`
}

// Verdict is the banner the page opens with: Class (good, bad, warn, info), the state in capitals
// with a glyph name, and a line of why. A state is never carried by color alone.
type Verdict struct {
	Class   string   `json:"class"`
	Word    string   `json:"word"`
	Icon    string   `json:"icon"`
	Line    string   `json:"line"`
	Details []string `json:"details,omitempty"`
	Stats   []Stat   `json:"stats,omitempty"`
}

// Stat is one key number under the verdict.
type Stat struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Sub   string `json:"sub,omitempty"`
	Class string `json:"class,omitempty"`
}

// Node is one declared node as the outline lists it.
type Node struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Block string `json:"block,omitempty"`
	// Safety is the node's safety as the config states it ("" when it leaves the block's) and Risk
	// the class the check derives ("side effect", "read-only", "idempotent", "safety unknown").
	Safety string `json:"safety,omitempty"`
	Risk   string `json:"risk,omitempty"`
	// Gate is the node's approval gate in words ("2 of 3: ops, finance, risk"), "" for none. It is
	// the effective gate, a gate a block declares in Go included when the editor knows the Registry.
	Gate string `json:"gate,omitempty"`
	// Approval is the gate the config itself declares on the node, which is the one the forms edit;
	// nil when the config declares none.
	Approval *Approval `json:"approval,omitempty"`
	// Problems are this node's problems, as text.
	Problems []string `json:"problems,omitempty"`
	// Taint is what a declared source can do to the node, set when the node declares a source or may read
	// one (see taintNode); nil otherwise, which says nothing about the node.
	Taint *taintNode `json:"taint,omitempty"`
	// In and Out are the node's input and output type names, as the digest writes them; set in typed
	// mode only.
	In  string `json:"in,omitempty"`
	Out string `json:"out,omitempty"`
}

// Doc is the config file as the server read it.
type Doc struct {
	View
	// Hash is the SHA-256 of the file's bytes as read, in hex.
	Hash string `json:"hash"`
}

// formatOf is the config format a file name means: YAML for .yaml and .yml, else JSON.
func formatOf(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml":
		return "yaml"
	}
	return "json"
}

func hashOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// BuildView is the View of text, the contents of the file called name. It never fails: a text that
// is not a config gives a View with ParseError and nothing else.
func BuildView(name string, text []byte) *View { return buildView(name, text, nil) }

// buildView is BuildView for an editor that may know the program's blocks (ty not nil).
func buildView(name string, text []byte, ty *typing) *View {
	v := &View{Name: name, Format: formatOf(name), Text: string(text), Mode: ModeLine}
	if ty != nil {
		v.Mode = ty.label()
	}
	data := text
	if v.Format == "yaml" {
		var err error
		if data, err = ToJSON(text); err != nil {
			return parseFailure(v, "YAML: "+err.Error())
		}
	}
	cfg := runview.Config{Name: name, Data: data}
	r := plan.InspectConfig(data, nil)
	if ty != nil {
		cfg, r = ty.config(name, data), ty.inspect(data)
	}
	cv := runview.BuildConfig(cfg)
	v.Flow, v.Entry, v.In, v.Out = r.Topology.Flow, r.Topology.Entry, r.Topology.In, r.Topology.Out
	v.Errors, v.Warnings, v.Problems, v.Assertions = cv.Errors, cv.Warnings, cv.Problems, cv.Assertions
	v.Verdict = Verdict{Class: cv.Verdict.Class, Word: cv.Verdict.Word, Icon: cv.Verdict.Icon, Line: cv.Verdict.Line, Details: cv.Verdict.Details}
	for _, s := range cv.Verdict.Stats {
		v.Verdict.Stats = append(v.Verdict.Stats, Stat{Value: s.Value, Label: s.Label, Sub: s.Sub, Class: s.Class})
	}
	if cv.Graph != nil {
		// The viewer inlines the SVG in HTML, which needs no namespace; the page parses it as an
		// XML document, which does.
		v.Graph = strings.Replace(cv.Graph.SVG, "<svg ", `<svg xmlns="http://www.w3.org/2000/svg" `, 1)
		// A node name is any string in the config, and JSON allows characters XML 1.0 cannot hold;
		// one such name must not make the whole drawing ill-formed, so each is shown as U+FFFD.
		v.Graph = strings.Map(func(r rune) rune {
			if r == 0x9 || r == 0xA || r == 0xD || r >= 0x20 && r <= 0xD7FF || r >= 0xE000 && r <= 0xFFFD || r >= 0x10000 && r <= 0x10FFFF {
				return r
			}
			return '\uFFFD'
		}, v.Graph)
	}
	var lines []string
	for _, a := range r.Assertions {
		lines = append(lines, a.Text)
	}
	v.AssertionForms = assertionFormsOf(lines)
	v.Nodes = outline(data, r)
	basis := ""
	if ty != nil && ty.source == runview.SourceDescription {
		basis = "per description file, not checked against the program"
	}
	v.Taint = taintViews(v.Nodes, r, cv.Taint, basis)
	v.Wires = wires(data)
	v.Wiring = wiring(v.Wires)
	if ty != nil {
		v.Typed = ty.view(r)
		ty.typeNodes(v.Nodes)
	}
	return v
}

func parseFailure(v *View, msg string) *View {
	v.ParseError = msg
	v.Verdict = Verdict{Class: "bad", Word: "CONFIG DOES NOT PARSE", Icon: "x", Line: msg}
	return v
}

// outline lists the nodes in the order the config declares them. data is the JSON the config
// reads as; r its report.
func outline(data []byte, r plan.ConfigReport) []Node {
	var cfg struct {
		Nodes  []json.RawMessage `json:"nodes"`
		Wiring []json.RawMessage `json:"wiring"`
	}
	_ = json.Unmarshal(data, &cfg) // InspectConfig has read it strictly; a config it could not read has no nodes
	safety := map[string]string{}
	approval := map[string]*Approval{}
	for _, raw := range cfg.Nodes {
		var n struct {
			Name   string `json:"name"`
			Safety string `json:"safety"`
		}
		if json.Unmarshal(raw, &n) != nil {
			continue
		}
		safety[n.Name] = n.Safety
		var a struct {
			Approval *Approval `json:"approval"`
		}
		if json.Unmarshal(raw, &a) == nil { // an approval that is not a mapping is left to the check to report
			approval[n.Name] = a.Approval
		}
	}
	// A join is not a declared node; its safety and gate are keys of its wiring element.
	for _, raw := range cfg.Wiring {
		var j struct {
			Join     string    `json:"join"`
			Safety   string    `json:"safety"`
			Approval *Approval `json:"approval"`
		}
		if json.Unmarshal(raw, &j) != nil || j.Join == "" {
			continue
		}
		safety[j.Join], approval[j.Join] = j.Safety, j.Approval
	}
	problems := map[string][]string{}
	for _, p := range r.Problems {
		if p.Node != "" {
			problems[p.Node] = append(problems[p.Node], p.Severity+": "+p.Message)
		}
	}
	var out []Node
	for _, n := range r.Topology.Nodes {
		out = append(out, Node{
			Name: n.Name, Kind: string(n.Kind), Block: n.Block, Safety: safety[n.Name],
			Risk: string(r.Safety[n.Name]), Gate: runview.GateText(n.Gate), Approval: approval[n.Name], Problems: problems[n.Name],
		})
	}
	return out
}

// Wire is one element of the config's wiring as the page edits it: an edge (From, To), a switch (Over,
// its Arms and Else) or a join (Join, Inputs, Merge).
type Wire struct {
	Kind   string   `json:"kind"` // "edge", "switch", "join", or "other" for an element that is none
	From   string   `json:"from,omitempty"`
	To     string   `json:"to,omitempty"`
	Over   string   `json:"over,omitempty"`
	Arms   []Arm    `json:"arms,omitempty"`
	Else   string   `json:"else,omitempty"`
	Join   string   `json:"join,omitempty"`
	Inputs []string `json:"inputs,omitempty"`
	Merge  string   `json:"merge,omitempty"`
}

// Arm is one arm of a switch; LoopMax is above 0 for a loop-back arm.
type Arm struct {
	Pred    string `json:"pred"`
	To      string `json:"to"`
	LoopMax int    `json:"loop_max,omitempty"`
}

// wires reads the config's wiring elements in the order the config has them: the page addresses an
// element by its position (move_wiring), so the list is the config's order, not the topology's
// grouping by kind. data is the JSON the config reads as; an element it cannot read is skipped by the
// strict check before it gets here.
func wires(data []byte) []Wire {
	var cfg struct {
		Wiring []struct {
			Edge   []string `json:"edge"`
			Switch string   `json:"switch"`
			When   []Arm    `json:"when"`
			Else   string   `json:"else"`
			Join   string   `json:"join"`
			Inputs []string `json:"inputs"`
			Merge  string   `json:"merge"`
		} `json:"wiring"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return nil
	}
	var out []Wire
	for _, w := range cfg.Wiring {
		switch {
		case w.Switch != "":
			out = append(out, Wire{Kind: "switch", Over: w.Switch, Arms: w.When, Else: w.Else})
		case w.Join != "":
			out = append(out, Wire{Kind: "join", Join: w.Join, Inputs: w.Inputs, Merge: w.Merge})
		case len(w.Edge) == 2:
			out = append(out, Wire{Kind: "edge", From: w.Edge[0], To: w.Edge[1]})
		default:
			out = append(out, Wire{Kind: "other"})
		}
	}
	return out
}

// wiring lists the wiring elements as lines of text, one per element, in the config's order.
func wiring(ws []Wire) []string {
	var out []string
	for _, w := range ws {
		switch w.Kind {
		case "switch":
			var arms []string
			for _, a := range w.Arms {
				if a.LoopMax > 0 {
					arms = append(arms, fmt.Sprintf("%s -> %s (loop, at most %d)", a.Pred, a.To, a.LoopMax))
				} else {
					arms = append(arms, a.Pred+" -> "+a.To)
				}
			}
			if w.Else != "" {
				arms = append(arms, "else -> "+w.Else)
			}
			out = append(out, "switch "+w.Over+": "+strings.Join(arms, "; "))
		case "join":
			out = append(out, "join "+w.Join+": "+strings.Join(w.Inputs, ", "))
		case "edge":
			out = append(out, w.From+" -> "+w.To)
		default:
			out = append(out, "(a wiring element that is not an edge, a switch or a join)")
		}
	}
	return out
}

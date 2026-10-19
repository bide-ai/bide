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
	// Graph is the layered drawing as one SVG document with every label escaped. The page parses
	// it with DOMParser and never assigns it as markup.
	Graph string `json:"graph,omitempty"`
	// Weakening compares the text with the baseline (the committed file, or the file as the editor
	// opened it). The server sets it; BuildView, which sees one text, does not.
	Weakening *Weakening `json:"weakening,omitempty"`
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
func BuildView(name string, text []byte) *View {
	v := &View{Name: name, Format: formatOf(name), Text: string(text), Mode: ModeLine}
	data := text
	if v.Format == "yaml" {
		var err error
		if data, err = ToJSON(text); err != nil {
			return parseFailure(v, "YAML: "+err.Error())
		}
	}
	cv := runview.BuildConfig(runview.Config{Name: name, Data: data})
	r := plan.InspectConfig(data, nil)
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
	v.Wiring = wiring(r.Topology)
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
		Nodes []json.RawMessage `json:"nodes"`
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

// wiring lists the edges, switches and joins of the declared graph as lines of text.
func wiring(t plan.Topology) []string {
	var out []string
	for _, e := range t.Edges {
		out = append(out, e.From+" -> "+e.To)
	}
	for _, b := range t.Branches {
		var arms []string
		for _, a := range b.Arms {
			switch {
			case a.Else:
				arms = append(arms, "else -> "+a.Target)
			case a.LoopBack:
				arms = append(arms, fmt.Sprintf("%s -> %s (loop, at most %d)", a.Pred, a.Target, a.LoopMax))
			default:
				arms = append(arms, a.Pred+" -> "+a.Target)
			}
		}
		out = append(out, "switch "+b.Over+": "+strings.Join(arms, "; "))
	}
	for _, j := range t.Joins {
		out = append(out, "join "+j.Name+": "+strings.Join(j.Inputs, ", "))
	}
	return out
}

package flowedit

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bide-ai/bide/internal/planaccess"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
)

// typing is what makes an editor typed: the blocks of the program the config is for, known as its
// plan.Registry (an editor embedded beside it) or as a description of it (bide edit -blocks). It is
// fixed when the Server is made; no request can change it.
type typing struct {
	reg    *plan.Registry
	desc   plan.RegistryDescription // what the palette lists; a copy, so the host cannot change it later
	source string                   // runview.SourceRegistry or runview.SourceDescription
}

// WithRegistry makes the editor typed against the program's Registry: it checks the config as Load
// does (block, predicate and merge names, the types along every edge, safety, the gates the blocks
// declare in Go, the digest), lists the blocks in a palette, and refuses an operation that connects
// what does not fit. Register everything before New; the editor only reads the Registry afterwards.
// New fails for a Registry that reports errors (a duplicate registration), as Load would.
func WithRegistry(reg *plan.Registry) Option {
	return func(o *options) { o.reg, o.regGiven = reg, true }
}

// WithBlocks makes the editor typed against a description of the program's registrations
// (plan.Registry.Describe, read from a file by bide edit -blocks). The checks are the same, by type
// name, and every typed result is labelled as resting on a description, not checked against the
// program. New fails for a description that is not valid (plan.RegistryDescription.Validate). The
// description is copied.
func WithBlocks(d plan.RegistryDescription) Option { return func(o *options) { o.blocks = &d } }

// newTyping resolves the options, or says why they cannot be: both given, a Registry that reports
// errors, a description that is not valid.
func newTyping(o options) (*typing, error) {
	switch {
	case o.regGiven && o.reg == nil:
		return nil, errors.New("flowedit: WithRegistry(nil): there is no Registry to type against")
	case o.reg != nil && o.blocks != nil:
		return nil, errors.New("flowedit: WithRegistry and WithBlocks are alternatives; give one")
	case o.reg != nil:
		d := o.reg.Describe()
		if len(d.Errors) > 0 {
			return nil, fmt.Errorf("flowedit: the Registry reports %d error(s), so Load would refuse it; the first: %s", len(d.Errors), d.Errors[0])
		}
		return &typing{reg: o.reg, desc: d, source: runview.SourceRegistry}, nil
	case o.blocks != nil:
		if err := o.blocks.Validate(); err != nil {
			return nil, fmt.Errorf("flowedit: blocks: %w", err)
		}
		b, err := json.Marshal(o.blocks)
		if err != nil {
			return nil, fmt.Errorf("flowedit: blocks: %w", err)
		}
		var d plan.RegistryDescription
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("flowedit: blocks: %w", err)
		}
		return &typing{desc: d, source: runview.SourceDescription}, nil
	}
	return nil, nil
}

// config is the config as the viewer's checks read it, with what this editor knows of the program.
// A palette lists blocks the config has not placed, so an unused block is no problem. The weakening
// verdict also compares the two texts structurally (see compare), because the program the baseline
// was written for is not the program the editor knows.
func (t *typing) config(name string, data []byte) runview.Config {
	c := runview.Config{Name: name, Data: data, AllowUnusedBlocks: true}
	if t.reg != nil {
		c.Registry = t.reg
	} else {
		c.Description = &t.desc
	}
	return c
}

func (t *typing) inspect(data []byte) plan.ConfigReport {
	opts := []plan.InspectOption{plan.AllowUnusedBlocks()}
	if t.reg != nil {
		return plan.InspectConfig(data, t.reg, opts...)
	}
	return plan.InspectConfigDescribed(data, t.desc, opts...)
}

// findings are the problems the editor compares before and after an edit: plan's findings for the text,
// which are made from the config and the registered types alone, beside whatever else is wrong with it.
func (t *typing) findings(data []byte) []plan.Finding { return t.inspect(data).Findings }

// refusedKind reports whether a finding of this kind makes the editor refuse an edit that adds one: a
// connection that does not fit, or a safety above what the block registers.
func refusedKind(k plan.ProblemKind) bool {
	switch k {
	case plan.ProblemEdgeTypes, plan.ProblemArmTypes, plan.ProblemJoinInputTypes, plan.ProblemTerminalTypes,
		plan.ProblemSafetyAboveRegistration:
		return true
	}
	return false
}

// loopKind reports whether a finding of this kind is a loop shape plan.Load refuses.
func loopKind(k plan.ProblemKind) bool {
	switch k {
	case plan.ProblemLoopNoExitArm, plan.ProblemLoopMoreThanOneBackArm, plan.ProblemLoopHeadNotAncestor,
		plan.ProblemLoopBodyNotSimple, plan.ProblemLoopInnerSwitch, plan.ProblemLoopEdgeLeaves, plan.ProblemLoopEdgeEnters:
		return true
	}
	return false
}

// recordKey is a problem's identity: its class and kind, the full names it is about and the types or
// levels, each Go-quoted so that a name with quotes, brackets or separators cannot read as two.
// rename maps the name of a renamed node to its new one.
func recordKey(r plan.Finding, rename func(string) string) string {
	var b strings.Builder
	b.WriteString(string(r.Kind))
	for _, n := range r.Names {
		b.WriteString(" " + strconv.Quote(rename(n)))
	}
	b.WriteString(" | " + strconv.Quote(r.Got) + " | " + strconv.Quote(r.Want))
	return b.String()
}

// label is what the page says the typed check rests on, wherever it shows a verdict.
func (t *typing) label() string {
	if t.source == runview.SourceDescription {
		return runview.DescriptionNote
	}
	return "typed check against the program's plan.Registry: blocks, predicates, merges and types are checked as Load checks them"
}

// TypedView is what typed mode adds to a View: what the check rests on, the digest, and the blocks
// the program offers. The page computes no types: it lists the palette and filters it by comparing
// type names the server wrote.
type TypedView struct {
	// Source is "registry" or "description" (what the typed result rests on), Label its words.
	Source string `json:"source"`
	Label  string `json:"label"`
	// Digest is the flow:digest a run of this config would journal, empty while the config has errors.
	Digest string `json:"digest,omitempty"`
	// Palette lists every registered block and merge, by name.
	Palette []PaletteBlock `json:"palette"`
}

// PaletteBlock is one registered block or merge as the palette shows it. In and Out are type names
// as the digest writes them (package path included); a merge has Inputs instead of In.
type PaletteBlock struct {
	Name string `json:"name"`
	// Kind is "step", "tool", "model" or "merge".
	Kind   string   `json:"kind"`
	In     string   `json:"in,omitempty"`
	Inputs []string `json:"inputs,omitempty"`
	Out    string   `json:"out"`
	// Safety is the class the block declares ("readonly", "idempotent", "side_effect") and Gate the
	// approval gate it declares in Go, in words ("2 of 3: ops, finance, risk"), "" for none.
	Safety string `json:"safety"`
	Gate   string `json:"gate,omitempty"`
	// Used is whether a node or join of the config uses it. An unused block is not a problem here.
	Used bool `json:"used"`
}

func (t *typing) view(r plan.ConfigReport) *TypedView {
	used := map[string]bool{}
	for _, n := range r.Topology.Nodes {
		used[n.Block] = true
	}
	tv := &TypedView{Source: t.source, Label: t.label(), Digest: r.Digest, Palette: []PaletteBlock{}}
	for _, b := range t.desc.Blocks {
		tv.Palette = append(tv.Palette, PaletteBlock{Name: b.Name, Kind: b.Kind, In: b.In, Out: b.Out, Safety: b.Safety, Gate: gateWords(b.Gate), Used: used[b.Name]})
	}
	for _, m := range t.desc.Merges {
		tv.Palette = append(tv.Palette, PaletteBlock{Name: m.Name, Kind: "merge", Inputs: m.Inputs, Out: m.Out, Safety: m.Safety, Gate: gateWords(m.Gate), Used: used[m.Name]})
	}
	return tv
}

// typeNodes sets each node's In and Out from the program's description of its block (a join's Out from its
// merge). The report's own topology has types only when the whole flow builds, and a flow with a node
// not wired in yet does not, which is exactly when the palette is used; the block's own types are known
// whatever the rest of the config says. A node whose block the program does not describe has none.
func (t *typing) typeNodes(nodes []Node) {
	blocks := map[string]plan.BlockDescription{}
	for _, b := range t.desc.Blocks {
		blocks[b.Name] = b
	}
	merges := map[string]plan.MergeDescription{}
	for _, m := range t.desc.Merges {
		merges[m.Name] = m
	}
	for i := range nodes {
		if nodes[i].Kind == "join" {
			if m, ok := merges[nodes[i].Block]; ok {
				nodes[i].Out = m.Out
			}
			continue
		}
		if b, ok := blocks[nodes[i].Block]; ok {
			nodes[i].In, nodes[i].Out = b.In, b.Out
		}
	}
}

// gateWords writes a described gate as the viewer writes a topology gate.
func gateWords(g *plan.GateDescription) string {
	if g == nil {
		return ""
	}
	tg := &plan.TopologyGate{Single: g.Single, Need: g.Need, Approvers: g.Approvers, ExcludePrincipal: g.ExcludePrincipal}
	if g.Timeout != "" {
		if d, err := time.ParseDuration(g.Timeout); err == nil {
			tg.TimeoutMS = d.Milliseconds()
		}
	}
	return runview.GateText(tg)
}

// allowedWhileUnparsed are the only operations the typed editor lets through while the text before or
// after an edit does not parse: those that cannot change the graph, the blocks, the order of nodes or
// wiring (which decides the implicit entry and which terminal sets the flow's output), the safeties or
// the type names the flow declares. An approval change is caught by the save, which asks the statement
// that the text could not be compared; an assertion is not checked here; set_flow changes the flow's
// name and nothing else (set_in and set_out are not on the list). Any operation not named here, added
// to the editor later too, is refused while the text does not parse.
var allowedWhileUnparsed = map[string]bool{
	"set_approval":     true,
	"clear_approval":   true,
	"add_assertion":    true,
	"set_assertion":    true,
	"remove_assertion": true,
	"set_flow":         true,
}

// typedRefusal is the reason to refuse an operation that took before to after, or "": after has a
// type problem that before did not, so the operation connected what does not fit. A problem the
// text already had is not the operation's, and is not refused. The decision is the server's, from
// the same check that draws the verdict; the page's hints do not decide.
func (s *Server) typedRefusal(before, after []byte, op Operation) string {
	if s.typing == nil {
		return ""
	}
	data := func(b []byte) ([]byte, bool) {
		if formatOf(s.name) != "yaml" {
			return b, true
		}
		j, err := ToJSON(b)
		return j, err == nil
	}
	bd, ok1 := data(before)
	ad, ok2 := data(after)
	if !ok1 || !ok2 {
		return ""
	}
	// A text that does not parse has no records: its problems are unknown, not absent, and decoding it
	// leniently would check a config other than the one written (a misspelt field is silently ignored). So
	// while the text before or after does not parse, an edit that can change what is wrong with the types
	// or the safeties is refused, rather than let through unchecked.
	if !allowedWhileUnparsed[op.Op] {
		if err := cmp.Or(planaccess.ParseError(bd), planaccess.ParseError(ad)); err != nil {
			return "the config does not parse: " + err.Error() + "; fix it first"
		}
	}
	// The problems the text had, counted by identity (class, kind, full names, types), so that two
	// problems whose displayed names look alike are two, and moving an element does not make an old
	// problem new. A rename does not make a problem the text already had a new one: its names are
	// compared under the rename.
	same := func(n string) string { return n }
	renamed := same
	if op.Op == "rename_node" {
		renamed = func(n string) string {
			if n == op.Node {
				return op.Value
			}
			return n
		}
	}
	had := map[string]int{}
	for _, r := range s.typing.findings(bd) {
		if refusedKind(r.Kind) {
			had[recordKey(r, renamed)]++
		}
	}
	for _, r := range s.typing.findings(ad) {
		if !refusedKind(r.Kind) {
			continue
		}
		k := recordKey(r, same)
		if had[k] > 0 {
			had[k]--
			continue
		}
		if r.Kind == plan.ProblemSafetyAboveRegistration {
			return "the safety is above what the program registers: " + r.Message
		}
		return "the types do not fit: " + r.Message
	}
	return ""
}

package plan

import (
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/strictjson"
)

// config is the decoded rung-2 topology: pure topology plus block references.
// Types are not restated here; they come from the Registry at load time. It is
// the one struct both front-ends (stdlib JSON here, an optional YAML adapter
// elsewhere) decode into, so the loader is format-agnostic.
type config struct {
	// Flow is the flow's label, used by RenderMermaid, Conform, and the Digest.
	Flow string `json:"flow"`
	// In and Out are optional type-name strings, documentation only. When present
	// Load cross-checks them against In/Out by reflect.Type identity, never as the
	// type source.
	In  string `json:"in,omitempty"`
	Out string `json:"out,omitempty"`
	// Entry names the entry node explicitly. When empty Load falls back to the first
	// node in Nodes, keeping entry deterministic without a map iteration.
	Entry string `json:"entry,omitempty"`
	// Nodes are the declared steps in array order; Load appends them in this order so
	// the topology Digest is stable across loads of the same bytes.
	Nodes []configNode `json:"nodes"`
	// Wiring is the ordered list of edges and switches. Each element is a
	// discriminated union of exactly one of edge or switch; Load rejects an element
	// that sets both or neither, which encoding/json cannot validate.
	Wiring []configWire `json:"wiring"`
}

// configNode declares one node by its journal name and the registered block it
// resolves to.
type configNode struct {
	Name  string `json:"name"`
	Block string `json:"block"`
	// Safety optionally overrides the node's retry-on-resume classification: one of
	// "readonly", "idempotent", or "retryable" ("retryable" is an alias for
	// "idempotent"). Empty means the node keeps the registered block's Safety (the Go
	// registration default). An explicit value here OVERRIDES that default's retry
	// classification only: an approval gate or IdempotencyKey the block declares is kept.
	// An unknown string is a load error naming the node. It maps to the corresponding NodeOption
	// / agent.Safety, so a loaded node resumes identically to a hand-built one with the
	// same option. Unlike Go registration, Safety CAN be expressed in the config JSON
	// (this field), because it is a per-node authoring choice the config author may
	// want to make without editing Go.
	Safety string `json:"safety,omitempty"`
	// Approval, when present, declares an m-of-n human approval gate
	// that lowers to agent.Safety.Approval on the built node. Absent
	// keeps the node's existing (1-of-1 or none) approval behavior.
	Approval *configApproval `json:"approval,omitempty"`
}

// configApproval is a node's m-of-n approval block: Need decisions required from the
// bounded Approvers set. It lowers to agent.ApprovalPolicy; Load rejects Need outside
// [1, len(Approvers)], an empty approver set, and duplicate approver ids.
type configApproval struct {
	Need      int      `json:"need"`
	Approvers []string `json:"approvers"`
}

// configWire is one wiring element: a discriminated union of EXACTLY ONE of an edge,
// a switch, or a join. encoding/json populates whatever keys are present, so Load
// validates the union explicitly.
type configWire struct {
	// Edge is [from, to] node names when this element is an edge.
	Edge []string `json:"edge,omitempty"`
	// Switch is the switched-over node name when this element is a switch.
	Switch string `json:"switch,omitempty"`
	// When is the ordered list of predicate arms when this element is a switch.
	When []configArm `json:"when,omitempty"`
	// Else is the fallback target node name when this element is a switch; empty means
	// no Else arm.
	Else string `json:"else,omitempty"`
	// Join is the fan-in node's name when this element is a join; it is the kindJoin
	// node assemble creates, wired one input edge per named input.
	Join string `json:"join,omitempty"`
	// Inputs are the ordered upstream producer node names a join fans in, one per
	// merge-block parameter. Their count must equal the merge block's arity.
	Inputs []string `json:"inputs,omitempty"`
	// Merge is the registered merge-block name (a RegisterJoin2/RegisterJoin3) whose
	// arity, ordered input types, and erased merge closure the join node adopts.
	Merge string `json:"merge,omitempty"`
	// Safety optionally sets the join node's retry-on-resume classification (one of
	// "readonly", "idempotent", "retryable"), like configNode.Safety. A join created
	// from wiring is not a declared node, so its Safety is set here. Empty keeps the
	// conservative halt-on-ambiguous-crash default, matching a hand-built Join. An
	// unknown string is a load error naming the join.
	Safety string `json:"safety,omitempty"`
}

// configArm is one When arm of a switch: a registered predicate name and the
// target node it routes to when the predicate holds.
type configArm struct {
	Pred string `json:"pred"`
	To   string `json:"to"`
	// LoopMax, when > 0, marks this arm as a BOUNDED LOOP-BACK rather than a plain
	// forward route: when Pred holds, the switched value routes BACK to To (an earlier
	// loop head, which Build validates is an ancestor of the switch) and the loop body
	// runs again, up to LoopMax iterations before Run declares a runaway loop. It is
	// the config counterpart of LoopBack(max, pred, head). Omitted or 0 means an
	// ordinary forward When arm.
	LoopMax int `json:"loopMax,omitempty"`
}

// isSwitch reports whether this wiring element is a switch. A switch is identified
// by a non-empty switch field, or by carrying When/Else arms. An edge carries an
// Edge slice and a join carries a Join field, neither of which sets these.
func (w configWire) isSwitch() bool {
	return w.Switch != "" || len(w.When) > 0 || w.Else != ""
}

// isEdge reports whether this wiring element is an edge: it carries an Edge slice.
func (w configWire) isEdge() bool {
	return w.Edge != nil
}

// isJoin reports whether this wiring element is a join: it names a Join fan-in node,
// or carries Inputs/Merge. An edge and a switch set none of these.
func (w configWire) isJoin() bool {
	return w.Join != "" || len(w.Inputs) > 0 || w.Merge != ""
}

// Load parses a rung-2 JSON config, resolves every block and predicate against
// reg, validates the whole topology at load time, and returns the built
// *Flow[In, Out]. It is where compile-time typing hands off to load-time
// validation: In and Out are supplied at the Go call site (the caller knows the
// boundary types) and the loader fills the middle from data.
//
// Load reports ALL resolution failures at once, naming every unresolved block or
// predicate (with a near-miss suggestion where one exists) and flagging registered
// blocks the config never uses. It then runs the load-time type checks by
// reflect.Type identity: the entry consumes In, every terminal produces Out, every
// edge's from.outType equals to.inType, and every switch arm's predicate M equals
// the switched node's output type (the improvement over rung 1, which only checks
// a predicate at its compile-time call site). It also enforces two structural
// checks Build does not: a node cannot be both switched-over and have an outgoing
// edge, and a wiring element must set exactly one of edge/switch. On success it
// hands the assembled spec to the existing Build (whole-graph validation) and seal.
func Load[In, Out any](data []byte, reg *Registry, opts ...LoadOption) (*Flow[In, Out], error) {
	var cfg config
	if err := parseConfig(data, &cfg); err != nil {
		return nil, fmt.Errorf("plan: load: parse config: %w", err)
	}

	inType := reflect.TypeFor[In]()
	outType := reflect.TypeFor[Out]()

	// Assemble and check the spec (drift, structural, and intra-graph type checks that
	// do not need In/Out). Validate shares this exact path.
	core, err := assemble(&cfg, reg)
	if err != nil {
		return nil, err
	}

	// Apply load options (for example WithLoadedModel) before Build, so a config that
	// declares a Model node has its model bound in time for Build's binding check.
	for _, opt := range opts {
		if opt != nil {
			opt(core)
		}
	}

	// Boundary checks that need the In/Out type parameters, by reflect.Type identity.
	if err := checkBoundary(&cfg, core, inType, outType); err != nil {
		return nil, err
	}

	// Hand the assembled spec to the existing whole-graph validation and seal, so a
	// loaded flow is validated identically to a hand-built one.
	return (&Builder[In, Out]{core: core}).Build()
}

// LoadOption configures a flow at Load time (before Build). It is the loader's
// counterpart to the Builder's chained options: it mutates the assembled builderCore
// the loaded flow is built from. See WithLoadedModel.
type LoadOption func(*builderCore)

// WithLoadedModel binds an agent.Model to a flow produced by Load or LoadReader, so a
// config that declares a Model node (a RegisterModel block) can render its prompt,
// call the model, and decode the structured result at run time. It is the loader's
// equivalent of Builder.WithModel: pass it to Load and a loaded Model node resumes
// and runs identically to a hand-built one. A config with a Model node loaded without
// a bound model fails Build, naming the node.
func WithLoadedModel(m agent.Model) LoadOption {
	return func(c *builderCore) { c.model = m }
}

// LoadReader is Load reading the config from r (for example an *os.File or an HTTP
// body). It reads r to EOF, then behaves exactly like Load.
func LoadReader[In, Out any](r io.Reader, reg *Registry, opts ...LoadOption) (*Flow[In, Out], error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("plan: load: read config: %w", err)
	}
	return Load[In, Out](data, reg, opts...)
}

// Validate parses and checks a config against reg without producing a Flow, so
// config-vs-registry drift is catchable in a unit test or CI rather than only at
// process start. It runs every check Load runs that does not require the flow's
// In/Out type parameters: the discriminated-union structural check, block and
// predicate resolution (reporting all unknowns at once with near-miss suggestions
// and flagging unused blocks), the switched-vs-edged exclusivity check, and the
// intra-graph type checks (every edge's from.outType == to.inType, every switch
// arm's predicate M == the switched node's output type).
//
// The only checks Validate cannot run are the ones that need In/Out: entry
// consumes In, every terminal produces Out, and the optional in/out documentation
// cross-check. Those run at Load, where the boundary types are known. Validate is
// therefore the drift guard (the stringly-typed footgun re-created at rung 2);
// Load is the full typed check.
func Validate(data []byte, reg *Registry) error {
	var cfg config
	if err := parseConfig(data, &cfg); err != nil {
		return fmt.Errorf("plan: validate: parse config: %w", err)
	}
	_, err := assemble(&cfg, reg)
	return err
}

// parseConfig decodes a config strictly, so the topology loaded is the one the text shows: a name
// that is not a field (a misspelling such as "aproval", or a case variant such as "Safety"), a
// name given twice, data after the value, and invalid UTF-8 are errors. encoding/json would skip
// the first, keep the last of the second, and so load a node without the gate or entry its author
// wrote.
func parseConfig(data []byte, cfg *config) error {
	return strictjson.Unmarshal(data, cfg, nil)
}

// assemble resolves the config against reg and builds a checked builderCore,
// running every check that does not require the flow's In/Out type parameters. It
// is shared by Load (which then adds the In/Out boundary checks and Build) and
// Validate (which stops here). It reports all resolution failures at once.
func assemble(cfg *config, reg *Registry) (*builderCore, error) {
	// 0. Surface any duplicate registrations collected on the Registry: a drifted
	// registry is itself a load failure.
	var problems []string
	for _, e := range reg.errs {
		problems = append(problems, e.Error())
	}

	// 1. Structural union check on every wiring element: EXACTLY ONE of edge/switch/join.
	// encoding/json does not validate a union, so an element that sets more than one, or
	// none, is a load error naming its index.
	for i, w := range cfg.Wiring {
		set := 0
		if w.isEdge() {
			set++
		}
		if w.isSwitch() {
			set++
		}
		if w.isJoin() {
			set++
		}
		switch {
		case set > 1:
			problems = append(problems, fmt.Sprintf("wiring[%d] sets more than one of edge/switch/join; set exactly one", i))
		case set == 0:
			problems = append(problems, fmt.Sprintf("wiring[%d] sets none of edge/switch/join; set exactly one", i))
		case w.isEdge() && len(w.Edge) != 2:
			problems = append(problems, fmt.Sprintf("wiring[%d] edge must be [from, to], got %d element(s)", i, len(w.Edge)))
		}
	}

	// 2. Resolve every node.block against the registry, collecting all unknowns. Track
	// which registered blocks are used so unused ones can be flagged.
	usedBlocks := make(map[string]bool, len(cfg.Nodes))
	resolvedNodes := make([]*node, 0, len(cfg.Nodes))
	seenNodeName := make(map[string]bool, len(cfg.Nodes))
	for _, cn := range cfg.Nodes {
		if seenNodeName[cn.Name] {
			problems = append(problems, fmt.Sprintf("node %q is declared more than once", cn.Name))
		}
		seenNodeName[cn.Name] = true
		if err := checkStepName(cn.Name); err != nil {
			problems = append(problems, err.Error())
		}

		b, ok := reg.blocks[cn.Block]
		if !ok {
			problems = append(problems, fmt.Sprintf("node %q references unknown block %q%s", cn.Name, cn.Block, suggest(cn.Block, blockNames(reg))))
			continue
		}
		usedBlocks[cn.Block] = true
		// Safety default is the registered block's Safety (the Go registration default);
		// an explicit config "safety" overrides it. An unknown safety string is a load
		// error naming the node, collected with the rest of the drift.
		safety := b.safety
		if cn.Safety != "" {
			s, ok := safetyFromConfig(b.safety, cn.Safety)
			if !ok {
				problems = append(problems, fmt.Sprintf("node %q has unknown safety %q; want one of \"readonly\", \"idempotent\", \"retryable\"", cn.Name, cn.Safety))
			} else {
				safety = s
			}
		}
		// An "approval" block lowers to Safety.Approval, applied after the safety string
		// so an explicit "safety" does not discard it. Invalid blocks are load errors
		// naming the node, collected with the rest of the drift.
		if cn.Approval != nil {
			n := len(cn.Approval.Approvers)
			if n == 0 {
				problems = append(problems, fmt.Sprintf("node %q approval.approvers must be non-empty", cn.Name))
			} else if cn.Approval.Need < 1 || cn.Approval.Need > n {
				problems = append(problems, fmt.Sprintf("node %q approval.need %d must be between 1 and %d (the approver count)", cn.Name, cn.Approval.Need, n))
			}
			seenApprover := make(map[string]bool, n)
			for _, id := range cn.Approval.Approvers {
				if id == "" {
					problems = append(problems, fmt.Sprintf("node %q approval.approvers has an empty id", cn.Name))
					continue
				}
				if seenApprover[id] {
					problems = append(problems, fmt.Sprintf("node %q approval.approvers lists %q more than once", cn.Name, id))
				}
				seenApprover[id] = true
			}
			safety.Approval = &agent.ApprovalPolicy{Need: cn.Approval.Need, Approvers: cn.Approval.Approvers}
		}
		resolvedNodes = append(resolvedNodes, &node{
			name:    cn.Name,
			kind:    b.kind,
			inType:  b.inType,
			outType: b.outType,
			run:     b.run,
			// Carry the resolved Safety onto the loaded node, so a loaded flow resumes
			// identically to a hand-built one. The default is the registered block's Safety
			// (recorded in Go at registration, see regBlock.safety); an explicit config
			// "safety" overrides it.
			safety: safety,
			// Carry the model prompt template for a Model block, so a loaded Model node
			// renders identically to a hand-built one (see RegisterModel / runModel).
			prompt: b.prompt,
		})
	}

	// 3. Resolve every switch arm's predicate and every join's merge block against the
	// registry, collecting all unknowns alongside unknown blocks in the single error. A
	// loop-back arm's predicate is an ordinary registered predicate (a When arm with a
	// positive loopMax), so it is resolved by the same walk.
	for i, w := range cfg.Wiring {
		switch {
		case w.isSwitch():
			for j, a := range w.When {
				if _, ok := reg.preds[a.Pred]; !ok {
					problems = append(problems, fmt.Sprintf("wiring[%d].when[%d] references unknown predicate %q%s", i, j, a.Pred, suggest(a.Pred, predNames(reg))))
				}
			}
		case w.isJoin():
			if _, ok := reg.merges[w.Merge]; !ok {
				problems = append(problems, fmt.Sprintf("wiring[%d] join %q references unknown merge %q%s", i, w.Join, w.Merge, suggest(w.Merge, mergeNames(reg))))
			}
		}
	}

	// 4. Flag registered-but-unused blocks (drift the other direction). Reported
	// deterministically (sorted) so a test can assert the message.
	var unused []string
	for name := range reg.blocks {
		if !usedBlocks[name] {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	for _, name := range unused {
		problems = append(problems, fmt.Sprintf("block %q is registered but never used by the config", name))
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("plan: load %q: %s", cfg.Flow, strings.Join(problems, "; "))
	}

	// 5. Assemble the builderCore in config-array order via the existing register, so
	// nodes are appended deterministically (never a map range) and entry is stable.
	core := &builderCore{
		flowName: cfg.Flow,
		byName:   make(map[string]*node),
	}
	for _, n := range resolvedNodes {
		core.register(n)
	}
	// Entry is explicit: the config's entry field wins; the fallback is nodes[0]
	// (which register already set as core.entry).
	if cfg.Entry != "" {
		if core.byName[cfg.Entry] == nil {
			return nil, fmt.Errorf("plan: load %q: entry %q names no declared node", cfg.Flow, cfg.Entry)
		}
		core.entry = cfg.Entry
	}
	if core.entry == "" {
		return nil, fmt.Errorf("plan: load %q: flow has no nodes", cfg.Flow)
	}

	// 6. Add edges, switches, and joins in wiring order, erasing predicates and merge
	// closures. Track which nodes are switched-over and which have an outgoing edge to
	// enforce the structural exclusivity Build does not. A join's input edges count as
	// outgoing edges of the input producers, so a fan-out producer that also feeds a
	// join is tracked here too.
	switchedOver := make(map[string]bool)
	hasOutEdge := make(map[string]bool)
	for i, w := range cfg.Wiring {
		switch {
		case w.isEdge():
			from, to := w.Edge[0], w.Edge[1]
			if core.byName[from] == nil {
				return nil, fmt.Errorf("plan: load %q: wiring[%d] edge from unknown node %q", cfg.Flow, i, from)
			}
			if core.byName[to] == nil {
				return nil, fmt.Errorf("plan: load %q: wiring[%d] edge to unknown node %q", cfg.Flow, i, to)
			}
			hasOutEdge[from] = true
			core.edges = append(core.edges, edge{from: from, to: to})

		case w.isJoin():
			// A join CREATES a kindJoin node named by Join and wires one input edge per
			// named input, mirroring Builder.Join2/Join3 (which registers the join node and
			// appends one edge per input). The merge block (resolved above) supplies the
			// arity, ordered input types, output type, and erased merge closure. The join
			// node is registered here, after its input producers' nodes and the fan-out
			// edges, exactly as Builder.Join2 registers it after the producers and their
			// edges, so a config diamond and a hand-built one share node/edge insertion
			// order and therefore one Digest.
			m := reg.merges[w.Merge] // resolved above
			if err := checkStepName(w.Join); err != nil {
				return nil, fmt.Errorf("plan: load %q: wiring[%d] join: %w", cfg.Flow, i, err)
			}
			if core.byName[w.Join] != nil {
				return nil, fmt.Errorf("plan: load %q: wiring[%d] join names %q, which is already a declared node; a join creates its own node", cfg.Flow, i, w.Join)
			}
			if len(w.Inputs) != m.arity {
				return nil, fmt.Errorf("plan: load %q: wiring[%d] join %q has %d input(s) but merge %q expects %d", cfg.Flow, i, w.Join, len(w.Inputs), w.Merge, m.arity)
			}
			for _, src := range w.Inputs {
				if core.byName[src] == nil {
					return nil, fmt.Errorf("plan: load %q: wiring[%d] join %q reads unknown input %q", cfg.Flow, i, w.Join, src)
				}
			}
			// Register the kindJoin node with its ordered inputs/types, output type, and
			// erased merge closure. inType stays nil like a hand-built join (a join has
			// several inputs wired as edges, not one Edge-consumed input). Safety defaults
			// to the conservative halt; an explicit "safety" on the join element overrides it.
			jsafety := agent.Safety{}
			if w.Safety != "" {
				s, ok := safetyFromConfig(jsafety, w.Safety)
				if !ok {
					return nil, fmt.Errorf("plan: load %q: wiring[%d] join %q has unknown safety %q; want one of \"readonly\", \"idempotent\", \"retryable\"", cfg.Flow, i, w.Join, w.Safety)
				}
				jsafety = s
			}
			core.register(&node{
				name:        w.Join,
				kind:        kindJoin,
				joinInputs:  append([]string(nil), w.Inputs...),
				joinInTypes: append([]reflect.Type(nil), m.inTypes...),
				outType:     m.outType,
				merge:       m.merge,
				safety:      jsafety,
			})
			for _, src := range w.Inputs {
				hasOutEdge[src] = true
				core.edges = append(core.edges, edge{from: src, to: w.Join})
			}

		default:
			// A switch element. Its switched-over node is the switch field.
			over := w.Switch
			if core.byName[over] == nil {
				return nil, fmt.Errorf("plan: load %q: wiring[%d] switch over unknown node %q", cfg.Flow, i, over)
			}
			switchedOver[over] = true
			arms := make([]arm, 0, len(w.When)+1)
			for j, a := range w.When {
				if core.byName[a.To] == nil {
					return nil, fmt.Errorf("plan: load %q: wiring[%d].when[%d] routes to unknown node %q", cfg.Flow, i, j, a.To)
				}
				p := reg.preds[a.Pred] // resolved above
				// A positive loopMax marks the arm as a bounded loop-back to a.To (an earlier
				// loop head), mirroring LoopBack(max, pred, head); Build then validates the
				// ancestor/exit/bound/contiguity requirements. Otherwise it is a plain When arm.
				if a.LoopMax > 0 {
					arms = append(arms, arm{pred: p.pred, target: a.To, loopBack: true, loopMax: a.LoopMax})
				} else {
					arms = append(arms, arm{pred: p.pred, target: a.To})
				}
			}
			if w.Else != "" {
				if core.byName[w.Else] == nil {
					return nil, fmt.Errorf("plan: load %q: wiring[%d] else routes to unknown node %q", cfg.Flow, i, w.Else)
				}
				arms = append(arms, arm{isElse: true, target: w.Else})
			}
			core.branches = append(core.branches, branch{over: over, arms: arms})
		}
	}

	// 7. Structural exclusivity: a node both switched-over and with an outgoing edge is
	// a config bug (Run silently prefers the switch), so reject it explicitly.
	for name := range switchedOver {
		if hasOutEdge[name] {
			return nil, fmt.Errorf("plan: load %q: node %q is both switched-over and has an outgoing edge; a switched node routes only through its arms", cfg.Flow, name)
		}
	}

	// 8. Intra-graph type checks by reflect.Type identity (no In/Out needed): every
	// edge's from.outType == to.inType, and every switch arm's predicate M == the
	// switched node's output type (the improvement over rung 1).
	//
	// A join's INPUT edges are skipped here: a kindJoin node has no single inType (it
	// fans in several producers), so its per-port types are checked by Build.checkJoins
	// against joinInTypes by reflect identity, not by this from.outType == to.inType
	// rule. Comparing against the join's nil inType would spuriously reject every join.
	for _, e := range core.edges {
		from := core.byName[e.from]
		to := core.byName[e.to]
		if to.kind == kindJoin {
			continue // join port types are validated by Build.checkJoins
		}
		if from.outType != to.inType {
			return nil, fmt.Errorf("plan: load %q: edge %q -> %q connects %s to %s (types must match exactly)",
				cfg.Flow, e.from, e.to, typeName(from.outType), typeName(to.inType))
		}
	}
	for i, w := range cfg.Wiring {
		if !w.isSwitch() {
			continue
		}
		over := core.byName[w.Switch]
		for j, a := range w.When {
			p := reg.preds[a.Pred]
			if p.mType != over.outType {
				return nil, fmt.Errorf("plan: load %q: wiring[%d].when[%d] predicate %q tests %s but switched node %q produces %s",
					cfg.Flow, i, j, a.Pred, typeName(p.mType), w.Switch, typeName(over.outType))
			}
		}
	}

	return core, nil
}

// checkBoundary runs the load-time type checks that need the flow's In/Out type
// parameters, by reflect.Type identity: the optional in/out documentation
// cross-check, the entry consuming In, and every terminal producing Out. It runs
// on the already-assembled core, so it can pin inType/outType on it too (Build
// re-uses them). Errors name the offending node and its types.
func checkBoundary(cfg *config, core *builderCore, inType, outType reflect.Type) error {
	// Pin the boundary types on the core so Build re-checks against the same values.
	core.inType = inType
	core.outType = outType

	// Optional in/out documentation cross-check, by reflect.Type identity (never
	// .String() as a type source). A present name not matching In/Out is a load error.
	if cfg.In != "" && cfg.In != inType.String() {
		return fmt.Errorf("plan: load %q: config declares in %q but Load In is %s", cfg.Flow, cfg.In, inType)
	}
	if cfg.Out != "" && cfg.Out != outType.String() {
		return fmt.Errorf("plan: load %q: config declares out %q but Load Out is %s", cfg.Flow, cfg.Out, outType)
	}

	entry := core.byName[core.entry]
	if entry.inType != inType {
		return fmt.Errorf("plan: load %q: entry node %q consumes %s but the flow input is %s",
			cfg.Flow, entry.name, typeName(entry.inType), typeName(inType))
	}

	// A terminal has no outgoing edge and is not switched-over; it must produce Out.
	hasOutEdge := make(map[string]bool, len(core.edges))
	for _, e := range core.edges {
		hasOutEdge[e.from] = true
	}
	switchedOver := make(map[string]bool, len(core.branches))
	for _, br := range core.branches {
		switchedOver[br.over] = true
	}
	for _, n := range core.nodes {
		if hasOutEdge[n.name] || switchedOver[n.name] {
			continue
		}
		if n.outType != outType {
			return fmt.Errorf("plan: load %q: terminal node %q produces %s but the flow output is %s",
				cfg.Flow, n.name, typeName(n.outType), typeName(outType))
		}
	}
	return nil
}

// blockNames returns the registered block names, for near-miss suggestions.
func blockNames(reg *Registry) []string {
	names := make([]string, 0, len(reg.blocks))
	for n := range reg.blocks {
		names = append(names, n)
	}
	return names
}

// predNames returns the registered predicate names, for near-miss suggestions.
func predNames(reg *Registry) []string {
	names := make([]string, 0, len(reg.preds))
	for n := range reg.preds {
		names = append(names, n)
	}
	return names
}

// mergeNames returns the registered merge-block names, for near-miss suggestions.
func mergeNames(reg *Registry) []string {
	names := make([]string, 0, len(reg.merges))
	for n := range reg.merges {
		names = append(names, n)
	}
	return names
}

// safetyFromConfig applies a config safety string to base, the node's Safety before the
// override, mirroring the ReadOnly/Idempotent/Retryable NodeOptions: "readonly" sets
// ReadOnly, "idempotent" and its "retryable" alias set Idempotent. Only that retry
// classification changes: an approval gate (RequiresApproval, Approval) or IdempotencyKey in
// base is kept, so a config file cannot switch off a gate the wrapped tool declares. The bool
// is false for an unknown string so the caller can report it as a load error naming the node.
func safetyFromConfig(base agent.Safety, s string) (agent.Safety, bool) {
	var opt NodeOption
	switch s {
	case "readonly":
		opt = ReadOnly()
	case "idempotent", "retryable":
		opt = Idempotent()
	default:
		return agent.Safety{}, false
	}
	return safetyFromOptions(base, []NodeOption{opt}), true
}

// suggest returns a " (did you mean %q?)" fragment when exactly one candidate is a
// close near-miss of name by Levenshtein distance, else "". It is a small
// no-dependency aid for the common typo; it never guesses when several candidates
// tie, so the message stays trustworthy.
func suggest(name string, candidates []string) string {
	best := ""
	bestDist := 1 << 30
	ties := 0
	for _, c := range candidates {
		d := levenshtein(name, c)
		if d < bestDist {
			bestDist = d
			best = c
			ties = 1
		} else if d == bestDist {
			ties++
		}
	}
	// Only suggest a single, genuinely-close candidate: distance within a third of the
	// longer string's length, and no tie.
	threshold := (max(len(name), len(best)) / 3) + 1
	if best == "" || ties != 1 || bestDist > threshold {
		return ""
	}
	return fmt.Sprintf(" (did you mean %q?)", best)
}

// levenshtein returns the edit distance between a and b using the standard two-row
// dynamic-programming algorithm. It has no dependencies and operates on bytes,
// which is sufficient for the ASCII identifier names a config uses.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

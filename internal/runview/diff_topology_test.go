package runview

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

// runRegistry has the blocks of the three flows below: a loop, a diamond (a fan-in join) and a Switch
// with an else, and one block that declares an approval gate in Go.
func runRegistry(t *testing.T) *plan.Registry {
	t.Helper()
	reg := plan.NewRegistry()
	inc := func(_ context.Context, n int) (int, error) { return n + 1, nil }
	str := func(_ context.Context, n int) (string, error) { return fmt.Sprint(n), nil }
	for _, err := range []error{
		reg.RegisterStep("seed", inc), reg.RegisterStep("refine", inc, plan.ReadOnly()), reg.RegisterStep("check", inc),
		reg.RegisterStep("done", str), reg.RegisterStep("split", inc), reg.RegisterStep("y", inc),
		reg.RegisterStep("w", inc), reg.RegisterStep("z", str),
		reg.RegisterStep("pay", inc, plan.Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "fin"}})),
		reg.RegisterJoin2("sumBlock", func(_ context.Context, a, b int) (string, error) { return fmt.Sprint(a + b), nil }),
		reg.RegisterJoin2("mergeBlock", func(_ context.Context, a int, s string) (string, error) { return s + fmt.Sprint(a), nil }),
		reg.RegisterPredicate("again", func(n int) bool { return n < 3 }),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

const (
	cfgLoop = `{"version":1,"flow":"countdown","in":"int","out":"string","entry":"seed",
 "nodes":[{"name":"seed","block":"seed"},{"name":"refine","block":"refine"},{"name":"check","block":"check"},{"name":"done","block":"done"}],
 "wiring":[{"edge":["seed","refine"]},{"edge":["refine","check"]},{"switch":"check","when":[{"pred":"again","to":"refine","loop_max":10}],"else":"done"}]}`
	cfgDiamond = `{"version":1,"flow":"diamond","in":"int","out":"string","entry":"split",
 "nodes":[{"name":"split","block":"split"},{"name":"y","block":"y"},{"name":"z","block":"z"}],
 "wiring":[{"edge":["split","y"]},{"edge":["split","z"]},{"join":"merge","inputs":["y","z"],"merge":"mergeBlock"}]}`
	cfgSwitch = `{"version":1,"flow":"route","in":"int","out":"int","entry":"seed",
 "nodes":[{"name":"seed","block":"seed"},{"name":"y","block":"y"},{"name":"w","block":"w"},{"name":"extra","block":"y"}],
 "wiring":[{"edge":["seed","extra"]},{"switch":"extra","when":[{"pred":"again","to":"y"}],"else":"w"}]}`
	cfgGated = `{"version":1,"flow":"pays","in":"int","out":"int","entry":"seed",
 "nodes":[{"name":"seed","block":"seed"},{"name":"charge","block":"pay"},{"name":"audit","block":"y","approval":{"need":2,"approvers":["ops","fin","risk"]}}],
 "wiring":[{"edge":["seed","charge"]},{"edge":["charge","audit"]}]}`
)

// cfgSum joins two outputs of one type, so its inputs can be swapped.
const cfgSum = `{"version":1,"flow":"sum","in":"int","out":"string","entry":"split",
 "nodes":[{"name":"split","block":"split"},{"name":"y","block":"y"},{"name":"w","block":"w"}],
 "wiring":[{"edge":["split","y"]},{"edge":["split","w"]},{"join":"total","inputs":["y","w"],"merge":"sumBlock"}]}`

var runConfigs = map[string]string{"sum": cfgSum, "loop": cfgLoop, "diamond": cfgDiamond, "switch": cfgSwitch, "gated": cfgGated}

// runSide is what a run journals for cfg: the topology Load builds, as a report.
func runSide(t *testing.T, cfg string, reg *plan.Registry) plan.ConfigReport {
	t.Helper()
	typed := inspect(cfg, reg)
	if typed.Errors() {
		t.Fatalf("the config has errors: %+v\n%s", typed.Problems, cfg)
	}
	return plan.InspectTopology(typed.Topology)
}

// fileSide is the file the editor holds: checked against the Registry, or structurally.
func fileSide(cfg string, reg *plan.Registry) plan.ConfigReport {
	return inspect(cfg, reg)
}

func diffRun(t *testing.T, cfg, file string, typed bool) *DiffView {
	t.Helper()
	reg := runRegistry(t)
	basis := ""
	var fr *plan.Registry
	if typed {
		basis, fr = SourceRegistry, reg
	}
	v, err := DiffTopologies(TopologyDiff{Old: runSide(t, cfg, reg), New: fileSide(file, fr), OldName: "run r1", NewName: "flow.json", Typed: typed, Basis: basis})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func changeLines(v *DiffView) string {
	var s []string
	for _, c := range v.Changes {
		s = append(s, c.String())
	}
	return strings.Join(s, "\n")
}

// An unchanged file reads as no difference in both modes, for every shape (loop, join, Switch with an
// else, gates), so the structural topology's node, edge and arm order is the order Load builds, and
// its kinds, types and gates do not read as changes. The typed side also has the same digest.
func TestRunDiffUnchangedFileHasNoChanges(t *testing.T) {
	for name, cfg := range runConfigs {
		for _, typed := range []bool{true, false} {
			v := diffRun(t, cfg, cfg, typed)
			if len(v.Changes) != 0 || len(v.Weakened) != 0 || len(v.Strengthened) != 0 || v.Cannot != "" || v.DigestNote != "" {
				t.Errorf("%s typed=%v: changes\n%s\nweakened %v cannot %q note %q", name, typed, changeLines(v), v.Weakened, v.Cannot, v.DigestNote)
			}
			if typed && (v.OldDigest == "" || v.OldDigest != v.NewDigest) {
				t.Errorf("%s: digests %q and %q", name, v.OldDigest, v.NewDigest)
			}
			if !typed && (v.OldDigest != "" || v.NewDigest != "") {
				t.Errorf("%s: a structural comparison holds digests %q, %q", name, v.OldDigest, v.NewDigest)
			}
		}
	}
}

// Safety and assert lines are in neither a journal nor the digest: a file that differs from the run only
// in them has the same digest and shows no change, whichever mode.
func TestRunDiffIgnoresSafetyAndAssertions(t *testing.T) {
	withSafety := strings.Replace(cfgLoop, `{"name":"refine","block":"refine"}`, `{"name":"refine","block":"refine","safety":"side_effect"}`, 1)
	withAssert := strings.Replace(cfgLoop, `"wiring"`, `"assert":["refine reachable"],"wiring"`, 1)
	for _, file := range []string{withSafety, withAssert, withSafety[:len(withSafety)-1] + `,"assert":["done reachable"]}`} {
		if r := inspect(file, runRegistry(t)); r.Errors() {
			t.Fatalf("setup: the variant has errors %+v: %s", r.Problems, file)
		}
		for _, typed := range []bool{true, false} {
			v := diffRun(t, cfgLoop, file, typed)
			if len(v.Changes) != 0 || len(v.Weakened) != 0 {
				t.Errorf("typed=%v: %s\nweakened %v", typed, changeLines(v), v.Weakened)
			}
			if typed && v.OldDigest != v.NewDigest {
				t.Errorf("the digest changed: %s vs %s", v.OldDigest, v.NewDigest)
			}
		}
	}
}

// The digest commits to the order of nodes, edges and arms, and resume refuses a changed order; the
// diff matches by name. Swapping two nodes, two edges, or a join's inputs reads as "order changed", in both
// modes, never as no change.
func TestRunDiffReadsAnOrderChange(t *testing.T) {
	cases := map[string]struct{ cfg, from, to, want string }{
		"nodes":       {cfgDiamond, `{"name":"y","block":"y"},{"name":"z","block":"z"}`, `{"name":"z","block":"z"},{"name":"y","block":"y"}`, "order of the nodes"},
		"edges":       {cfgDiamond, `{"edge":["split","y"]},{"edge":["split","z"]}`, `{"edge":["split","z"]},{"edge":["split","y"]}`, "order of the edges"},
		"join inputs": {cfgSum, `"inputs":["y","w"]`, `"inputs":["w","y"]`, "order of the inputs of join total"},
	}
	for name, c := range cases {
		file := strings.Replace(c.cfg, c.from, c.to, 1)
		if file == c.cfg {
			t.Fatalf("%s: the mutation changed nothing", name)
		}
		for _, typed := range []bool{true, false} {
			if inspect(file, runRegistry(t)).Errors() {
				t.Fatalf("%s: the variant has errors", name)
			}
			v := diffRun(t, c.cfg, file, typed)
			if !strings.Contains(changeLines(v), c.want) {
				t.Errorf("%s typed=%v: changes\n%s\nwant %q", name, typed, changeLines(v), c.want)
			}
			if typed && v.OldDigest == v.NewDigest {
				t.Errorf("%s: the digests are equal for a different order", name)
			}
			if len(v.Weakened) != 0 {
				t.Errorf("%s: an order change is not a weakening: %v", name, v.Weakened)
			}
		}
	}
	// A Switch's arms: two arms in the other order.
	two := `{"version":1,"flow":"route","in":"int","out":"int","entry":"seed",
 "nodes":[{"name":"seed","block":"seed"},{"name":"y","block":"y"},{"name":"w","block":"w"},{"name":"v","block":"y"}],
 "wiring":[{"edge":["seed","v"]},{"switch":"v","when":[{"pred":"again","to":"y"},{"pred":"again","to":"w"}]}]}`
	file := strings.Replace(two, `{"pred":"again","to":"y"},{"pred":"again","to":"w"}`, `{"pred":"again","to":"w"},{"pred":"again","to":"y"}`, 1)
	for _, typed := range []bool{true, false} {
		v := diffRun(t, two, file, typed)
		if !strings.Contains(changeLines(v), "order of the arms of v") {
			t.Errorf("arms typed=%v: changes\n%s", typed, changeLines(v))
		}
	}
}

// A renamed node is a rename, in structural mode too, where the file has no kinds: the pairing that
// the config diff makes needs both sides to say the same of a node's kind.
func TestRunDiffStructuralRenameIsARename(t *testing.T) {
	file := strings.NewReplacer(`"name":"y"`, `"name":"y2"`, `["split","y"]`, `["split","y2"]`, `"inputs":["y","z"]`, `"inputs":["y2","z"]`).Replace(cfgDiamond)
	for _, typed := range []bool{true, false} {
		v := diffRun(t, cfgDiamond, file, typed)
		got := changeLines(v)
		if !strings.Contains(got, "renamed node y2") || strings.Contains(got, "removed node") || strings.Contains(got, "added node") {
			t.Errorf("typed=%v: %s", typed, got)
		}
		if len(v.Weakened) != 0 {
			t.Errorf("typed=%v: a rename is not a weakening: %v", typed, v.Weakened)
		}
	}
}

// A journaled gate is the resolved policy, a Go-declared one included. A structural file cannot show
// it, so it must not read "gate removed"; and no verdict about gates is made structurally.
func TestRunDiffGatesAreComparedOnlyWhenTyped(t *testing.T) {
	// "charge" runs the block pay, which declares its gate in Go; the config states none.
	v := diffRun(t, cfgGated, cfgGated, false)
	if len(v.Changes) != 0 || len(v.Weakened) != 0 {
		t.Errorf("structural: %s %v", changeLines(v), v.Weakened)
	}
	// The config's own gate weakened: typed says so, structural says nothing about gates.
	weaker := strings.Replace(cfgGated, `"need":2,"approvers":["ops","fin","risk"]`, `"need":1,"approvers":["ops","fin","risk"]`, 1)
	if weaker == cfgGated || inspect(weaker, runRegistry(t)).Errors() {
		t.Fatal("setup")
	}
	v = diffRun(t, cfgGated, weaker, true)
	if len(v.Weakened) != 1 || !strings.Contains(v.Weakened[0], "audit") || v.Verdict.Word != "APPROVAL GATE WEAKENED" {
		t.Errorf("typed: weakened %v verdict %q", v.Weakened, v.Verdict.Word)
	}
	v = diffRun(t, cfgGated, weaker, false)
	if len(v.Weakened) != 0 || len(v.Changes) != 0 {
		t.Errorf("structural: %v %s", v.Weakened, changeLines(v))
	}
	// The gate removed from the file.
	gone := strings.Replace(cfgGated, `,"approval":{"need":2,"approvers":["ops","fin","risk"]}`, ``, 1)
	v = diffRun(t, cfgGated, gone, true)
	if len(v.Weakened) != 1 {
		t.Errorf("a removed gate: %v", v.Weakened)
	}
}

// Only a gate can weaken against a run: a journal has no safety, so a node added without a gate, a
// block changed, a Switch changed, are changes and are never "weakened" (the verdicts that need safety
// or reachability through gates are not made).
func TestRunDiffMakesNoVerdictBeyondGates(t *testing.T) {
	for name, file := range map[string]string{
		"new node":      strings.Replace(strings.Replace(cfgGated, `{"name":"audit"`, `{"name":"extra","block":"y"},{"name":"audit"`, 1), `{"edge":["charge","audit"]}`, `{"edge":["charge","extra"]},{"edge":["extra","audit"]}`, 1),
		"block changed": strings.Replace(cfgGated, `"name":"charge","block":"pay"`, `"name":"charge","block":"y"`, 1),
		"entry":         strings.Replace(cfgSwitch, `"entry":"seed"`, `"entry":"extra"`, 1),
	} {
		cfg := cfgGated
		if name == "entry" {
			cfg = cfgSwitch
		}
		for _, typed := range []bool{true, false} {
			if typed && inspect(file, runRegistry(t)).Errors() {
				continue // a variant the Registry refuses cannot be compared typed
			}
			v := diffRun(t, cfg, file, typed)
			if len(v.Changes) == 0 {
				t.Errorf("%s typed=%v: no change", name, typed)
			}
			for _, c := range v.Changes {
				if c.Weakening != "" && c.Weakening != "gate" {
					t.Errorf("%s typed=%v: %s weakens %q", name, typed, c, c.Weakening)
				}
			}
			if !typed && len(v.Weakened) != 0 {
				t.Errorf("%s: structural weakened %v", name, v.Weakened)
			}
		}
	}
}

// A file with errors has no typed digest: nothing is claimed.
func TestRunDiffTypedFileWithErrorsCannotCompare(t *testing.T) {
	broken := strings.Replace(cfgLoop, `"block":"done"`, `"block":"nope"`, 1)
	v := diffRun(t, cfgLoop, broken, true)
	if !strings.Contains(v.Cannot, "the file has errors") || len(v.Changes) != 0 || v.NewDigest != "" {
		t.Errorf("cannot %q changes %s", v.Cannot, changeLines(v))
	}
}

// The digests differ and no change explains it: a note, not "no changes".
func TestRunDiffDigestNote(t *testing.T) {
	reg := runRegistry(t)
	file := fileSide(cfgLoop, reg)
	run := runSide(t, cfgLoop, reg)
	run.Digest = "00" + run.Digest[2:]
	v, err := DiffTopologies(TopologyDiff{Old: run, New: file, OldName: "run", NewName: "f", Typed: true, Basis: SourceRegistry})
	if err != nil {
		t.Fatal(err)
	}
	if v.DigestNote == "" || len(v.Changes) != 0 {
		t.Errorf("note %q changes %s", v.DigestNote, changeLines(v))
	}
}

// A topology a journal supplies is bounded.
func TestRunDiffRefusesAnOversizedTopology(t *testing.T) {
	big := plan.Topology{Flow: "f", Entry: "n0"}
	for i := 0; i <= MaxTopologyNodes; i++ {
		big.Nodes = append(big.Nodes, plan.TopologyNode{Name: fmt.Sprint("n", i), Block: "b"})
	}
	small := fileSide(cfgLoop, nil)
	if _, err := DiffTopologies(TopologyDiff{Old: plan.InspectTopology(big), New: small}); err == nil || !strings.Contains(err.Error(), "1000") {
		t.Errorf("1001 nodes: %v", err)
	}
	edges := plan.Topology{Flow: "f", Entry: "a", Nodes: []plan.TopologyNode{{Name: "a"}, {Name: "b"}}}
	for i := 0; i <= MaxTopologyEdges; i++ {
		edges.Edges = append(edges.Edges, plan.TopologyEdge{From: "a", To: "b"})
	}
	if _, err := DiffTopologies(TopologyDiff{Old: small, New: plan.InspectTopology(edges)}); err == nil {
		t.Error("4001 edges: no error")
	}
	// The arms count toward the bound with the edges.
	arms := plan.Topology{Flow: "f", Entry: "a", Nodes: []plan.TopologyNode{{Name: "a"}, {Name: "b"}}, Edges: []plan.TopologyEdge{{From: "a", To: "b"}}}
	br := plan.TopologyBranch{Over: "a"}
	for i := 0; i < MaxTopologyEdges; i++ {
		br.Arms = append(br.Arms, plan.TopologyBranchArm{Target: "b"})
	}
	arms.Branches = []plan.TopologyBranch{br}
	if _, err := DiffTopologies(TopologyDiff{Old: small, New: plan.InspectTopology(arms)}); err == nil {
		t.Error("4000 arms and an edge: no error")
	}
	ok := plan.Topology{Flow: "f", Entry: "n0"}
	for i := 0; i < MaxTopologyNodes; i++ {
		ok.Nodes = append(ok.Nodes, plan.TopologyNode{Name: fmt.Sprint("n", i), Block: "b"})
	}
	if _, err := DiffTopologies(TopologyDiff{Old: plan.InspectTopology(ok), New: small}); err != nil {
		t.Errorf("exactly 1000 nodes: %v", err)
	}
}

// NormalizeForRun works on a copy: the report it is given (a run's, cached and shared across requests)
// is the same after, in both modes.
func TestNormalizeForRunLeavesItsInputAlone(t *testing.T) {
	reg := runRegistry(t)
	for name, cfg := range runConfigs {
		for _, typed := range []bool{true, false} {
			r := runSide(t, cfg, reg)
			before, _ := json.Marshal(r)
			n := NormalizeForRun(r, typed)
			for i := range n.Topology.Nodes {
				n.Topology.Nodes[i].Name += "!"
				if g := n.Topology.Nodes[i].Gate; g != nil {
					g.Need = 99
					if len(g.Approvers) > 0 {
						g.Approvers[0] = "changed"
					}
				}
			}
			for i := range n.Topology.Edges {
				n.Topology.Edges[i].To += "!"
			}
			for i := range n.Topology.Branches {
				for j := range n.Topology.Branches[i].Arms {
					n.Topology.Branches[i].Arms[j].Target += "!"
				}
			}
			for i := range n.Topology.Joins {
				if len(n.Topology.Joins[i].Inputs) > 0 {
					n.Topology.Joins[i].Inputs[0] += "!"
				}
			}
			if after, _ := json.Marshal(r); string(after) != string(before) {
				t.Errorf("%s typed=%v: the input changed", name, typed)
			}
		}
	}
	n := NormalizeForRun(runSide(t, cfgGated, reg), false)
	for _, x := range n.Topology.Nodes {
		if x.Gate != nil || (x.Kind != "" && x.Kind != plan.NodeKindJoin) || x.In != "" || x.InCanonical != "" {
			t.Errorf("structural normalising left %+v", x)
		}
		if !n.Gated[x.Name] {
			t.Errorf("%s is not counted gated", x.Name)
		}
	}
	if n.Digest != "" {
		t.Errorf("a structural report holds the digest %q", n.Digest)
	}
	if n.Safety != nil || n.Assertions != nil || !n.Typed {
		t.Errorf("safety %v assertions %v typed %v", n.Safety, n.Assertions, n.Typed)
	}
}

// A node renamed in the file and moved is an order change: the order is read under the pairing of
// renamed nodes, so a rename does not hide it.
func TestRunDiffReadsAnOrderChangeBesideARename(t *testing.T) {
	file := strings.NewReplacer(`{"name":"y","block":"y"},{"name":"z","block":"z"}`, `{"name":"z","block":"z"},{"name":"y2","block":"y"}`,
		`["split","y"]`, `["split","y2"]`, `"inputs":["y","z"]`, `"inputs":["y2","z"]`).Replace(cfgDiamond)
	v := diffRun(t, cfgDiamond, file, false)
	if got := changeLines(v); !strings.Contains(got, "order of the nodes") || !strings.Contains(got, "renamed node y2") {
		t.Errorf("changes\n%s", got)
	}
}

// With no digest on the run side nothing is said about the digest.
func TestRunDiffNoNoteWithoutARunDigest(t *testing.T) {
	reg := runRegistry(t)
	run := runSide(t, cfgLoop, reg)
	run.Digest = ""
	v, err := DiffTopologies(TopologyDiff{Old: run, New: fileSide(cfgLoop, reg), OldName: "run", NewName: "f", Typed: true, Basis: SourceRegistry})
	if err != nil || v.DigestNote != "" || v.OldDigest != "" {
		t.Errorf("note %q old digest %q err %v", v.DigestNote, v.OldDigest, err)
	}
}

func TestSameOrder(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"abc", "abc", true}, {"abc", "acb", false}, {"abc", "ac", true}, {"abc", "xbc", true}, {"abc", "cba", false},
		{"", "abc", true}, {"ab", "ba", false}, {"aXb", "bYa", false}, {"aXb", "aYb", true},
	} {
		if got := sameOrder(strings.Split(c.a, ""), strings.Split(c.b, "")); got != c.want {
			t.Errorf("sameOrder(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// inspect checks cfg as the editor does, with the blocks it has not used yet allowed.
func inspect(cfg string, reg *plan.Registry) plan.ConfigReport {
	return plan.InspectConfig([]byte(cfg), reg, plan.AllowUnusedBlocks())
}

func digestStat(v *DiffView) Stat {
	for _, s := range v.Verdict.Stats {
		if s.Label == "digest" {
			return s
		}
	}
	return Stat{}
}

// The headline of a comparison with a run never claims more than it knows: not the config diff's "NO
// CHANGES / the two configs declare the same flow" for a file that was not compared, for a digest that
// differs with nothing itemised, or for a structural comparison that does not see kinds, types or gates.
func TestRunDiffVerdictsSayWhatIsKnown(t *testing.T) {
	reg := runRegistry(t)
	// Same digest, typed.
	v := diffRun(t, cfgLoop, cfgLoop, true)
	if w := v.Verdict; w.Word != "SAME DIGEST" || w.Class != "good" || strings.Contains(w.Line, "same flow") || !strings.Contains(w.Line, "does not say the run followed") || digestStat(v).Value != "same" {
		t.Errorf("typed, same: %+v", w)
	}
	// Nothing seen, structural: a warning that names what is not compared.
	v = diffRun(t, cfgLoop, cfgLoop, false)
	if w := v.Verdict; w.Word != "NO DIFFERENCE SEEN" || w.Class != "warn" || !strings.Contains(w.Line, "Kinds, types and gates are not compared") || strings.Contains(w.Word+w.Line, "NO CHANGES") || strings.Contains(w.Line, "same flow") {
		t.Errorf("structural, nothing seen: %+v", w)
	}
	if b, _ := json.Marshal(v); !strings.Contains(string(b), `"changes":[]`) {
		t.Errorf("no changes is null in the JSON: %s", b)
	}
	// A file with errors was not compared.
	broken := strings.Replace(cfgLoop, `"block":"done"`, `"block":"nope"`, 1)
	v = diffRun(t, cfgLoop, broken, true)
	if w := v.Verdict; w.Word != "CANNOT COMPARE" || w.Class != "warn" || !strings.Contains(w.Line, "the file has errors") || strings.Contains(w.Word+w.Line, "NO CHANGES") || strings.Contains(w.Line, "same flow") {
		t.Errorf("cannot: %+v", w)
	}
	if b, _ := json.Marshal(v); !strings.Contains(string(b), `"changes":[]`) {
		t.Errorf("a refused comparison has null changes: %s", b)
	}
	// A real digest difference nothing itemises: the run's topology with one node's canonical input type
	// spelled with another package path, its digest recomputed.
	run := runSide(t, cfgLoop, reg)
	t0 := run.Topology
	t0.Nodes = append([]plan.TopologyNode(nil), t0.Nodes...)
	t0.Nodes[1].InCanonical = "example.com/other." + t0.Nodes[1].InCanonical
	dr := plan.InspectTopology(t0)
	if dr.Digest == "" || dr.Digest == run.Digest {
		t.Fatalf("setup: digest %q, was %q", dr.Digest, run.Digest)
	}
	v, err := DiffTopologies(TopologyDiff{Old: dr, New: fileSide(cfgLoop, reg), OldName: "run", NewName: "f", Typed: true, Basis: SourceRegistry})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Changes) != 0 || v.DigestNote == "" || !strings.Contains(v.DigestNote, "for example") || v.Verdict.Word != "DIGEST DIFFERS" || v.Verdict.Class != "warn" || strings.Contains(v.Verdict.Line, "same flow") || digestStat(v).Value != "new" {
		t.Errorf("digest differs, nothing itemised: note %q changes %v verdict %+v stat %+v", v.DigestNote, v.Changes, v.Verdict, digestStat(v))
	}
	// A typed run with no computable digest is not "same" and not "no difference".
	nd := runSide(t, cfgLoop, reg)
	nd.Digest = ""
	v, err = DiffTopologies(TopologyDiff{Old: nd, New: fileSide(cfgLoop, reg), OldName: "run", NewName: "f", Typed: true, Basis: SourceRegistry})
	if err != nil || v.Verdict.Word != "NO DIGEST TO COMPARE" {
		t.Errorf("no run digest: %v %+v", err, v.Verdict)
	}
	// Itemised changes keep the config diff's words, and the digest stat is from the view.
	v = diffRun(t, cfgDiamond, strings.Replace(cfgDiamond, `{"name":"y","block":"y"},{"name":"z","block":"z"}`, `{"name":"z","block":"z"},{"name":"y","block":"y"}`, 1), true)
	if w := v.Verdict; w.Word != "1 CHANGE" || w.Class != "warn" || digestStat(v).Value != "new" {
		t.Errorf("changes: %+v stat %+v", w, digestStat(v))
	}
	// A gate weakened against the run is the one verdict made.
	weaker := strings.Replace(cfgGated, `"need":2,"approvers":["ops","fin","risk"]`, `"need":1,"approvers":["ops","fin","risk"]`, 1)
	if w := diffRun(t, cfgGated, weaker, true).Verdict; w.Word != "APPROVAL GATE WEAKENED" || w.Class != "bad" {
		t.Errorf("weakened: %+v", w)
	}
}

// A file over the bound is an error the caller shows as "too large to compare", never as no difference.
func TestRunDiffRefusesAFileOverTheBound(t *testing.T) {
	big := plan.Topology{Flow: "f", Entry: "n0"}
	for i := 0; i <= MaxTopologyNodes; i++ {
		big.Nodes = append(big.Nodes, plan.TopologyNode{Name: fmt.Sprint("n", i), Block: "b"})
	}
	reg := runRegistry(t)
	if _, err := DiffTopologies(TopologyDiff{Old: runSide(t, cfgLoop, reg), New: plan.InspectTopology(big), OldName: "run", NewName: "file.json"}); err == nil || !strings.Contains(err.Error(), "file.json: the topology has") {
		t.Errorf("%v", err)
	}
}

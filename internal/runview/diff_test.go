package runview

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

// diffV1 and diffV2 are the refund flow before and after a change: skip is replaced by review (a
// node added, one removed, the else arm moved), charge's gate goes from 1 of 3 to
// 2 of 3, and validate's safety changes.
const diffV1 = `{"version": 1, "flow": "refund", "entry": "validate",
  "nodes": [{"name": "validate", "block": "validate"},
            {"name": "charge", "block": "charge", "approval": {"need": 1, "approvers": ["ops", "finance", "risk"]}},
            {"name": "skip", "block": "skip"}, {"name": "receipt", "block": "receipt"}],
  "wiring": [{"switch": "validate", "when": [{"pred": "large", "to": "charge"}], "else": "skip"},
             {"edge": ["charge", "receipt"]}]}`

const diffV2 = `{"version": 1, "flow": "refund", "entry": "validate",
  "nodes": [{"name": "validate", "block": "validate", "safety": "side_effect"},
            {"name": "charge", "block": "charge", "approval": {"need": 2, "approvers": ["ops", "finance", "risk"]}},
            {"name": "review", "block": "review"}, {"name": "receipt", "block": "receipt"}],
  "wiring": [{"switch": "validate", "when": [{"pred": "large", "to": "charge"}], "else": "review"},
             {"edge": ["charge", "receipt"]}]}`

func diffOf(t *testing.T, oldData, newData string, oldReg, newReg *plan.Registry) *DiffView {
	t.Helper()
	v, err := BuildDiff(Diff{Old: Config{Name: "old.json", Data: []byte(oldData), Registry: oldReg}, New: Config{Name: "new.json", Data: []byte(newData), Registry: newReg}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func lines(cs []Change) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.String())
	}
	return out
}

// TestDiffChanges: added, removed and changed nodes (block, safety, gate), and added, removed and
// changed edges and arms, each found once, in order, with the gate's verdict.
func TestDiffChanges(t *testing.T) {
	v := diffOf(t, diffV1, diffV2, nil, nil)
	want := []string{
		"changed node validate: safety (the block's) -> side_effect",
		"changed node charge: approval 1 of 3 -> 2 of 3 (strengthened)",
		"added node review: block review",
		"removed node skip: block skip",
		"added edge validate -> review: else",
		"removed edge validate -> skip: else",
	}
	if got := lines(v.Changes); !slices.Equal(got, want) {
		t.Fatalf("changes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if v.Counts != (Counts{NodesAdded: 1, NodesRemoved: 1, NodesChanged: 2, EdgesAdded: 1, EdgesRemoved: 1}) {
		t.Errorf("counts = %+v", v.Counts)
	}
	if len(v.Weakened) != 0 || !slices.Equal(v.Strengthened, []string{"charge: approval 1 of 3 -> 2 of 3"}) {
		t.Errorf("weakened %q, strengthened %q", v.Weakened, v.Strengthened)
	}

	// The reverse diff weakens the gate.
	v = diffOf(t, diffV2, diffV1, nil, nil)
	if !slices.Equal(v.Weakened, []string{"charge: approval 2 of 3 -> 1 of 3"}) {
		t.Errorf("reverse: weakened %q", v.Weakened)
	}

	// A block change, an arm's predicate and a loop bound change, the entry and the flow's name.
	a := `{"version": 1, "flow": "f", "nodes": [{"name": "a", "block": "a"}, {"name": "b", "block": "b"}, {"name": "c", "block": "c"}],
	  "wiring": [{"edge": ["a", "b"]}, {"switch": "b", "when": [{"pred": "again", "to": "a", "loop_max": 3}, {"pred": "p", "to": "c"}]}]}`
	b := `{"version": 1, "flow": "g", "entry": "b", "nodes": [{"name": "a", "block": "a2"}, {"name": "b", "block": "b"}, {"name": "c", "block": "c"}],
	  "wiring": [{"edge": ["a", "b"]}, {"switch": "b", "when": [{"pred": "again", "to": "a", "loop_max": 5}, {"pred": "q", "to": "c"}]}]}`
	want = []string{
		"changed flow: f -> g",
		"changed entry: a -> b",
		"changed node a: block a -> a2",
		"changed edge b -> a: loop again, at most 3 -> loop again, at most 5",
		"changed edge b -> c: when p -> when q",
	}
	if got := lines(diffOf(t, a, b, nil, nil).Changes); !slices.Equal(got, want) {
		t.Fatalf("changes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A removed first node is drawn first, before the nodes both versions have.
	c := `{"version": 1, "flow": "f", "nodes": [{"name": "intake", "block": "intake"}, {"name": "a", "block": "a"}, {"name": "b", "block": "b"}],
	  "wiring": [{"edge": ["intake", "a"]}, {"edge": ["a", "b"]}]}`
	d := `{"version": 1, "flow": "f", "nodes": [{"name": "a", "block": "a"}, {"name": "b", "block": "b"}], "wiring": [{"edge": ["a", "b"]}]}`
	var names []string
	for _, n := range diffOf(t, c, d, nil, nil).Graph.Nodes {
		names = append(names, n.Name+":"+n.Status)
	}
	if want := []string{"intake:removed", "a:unchanged", "b:unchanged"}; !slices.Equal(names, want) {
		t.Errorf("graph nodes %q, want %q", names, want)
	}

	// The same config: no changes.
	if v := diffOf(t, diffV1, diffV1, nil, nil); len(v.Changes) != 0 {
		t.Errorf("a config against itself: %q", lines(v.Changes))
	}
}

// TestDiffGates: each way a gate can change, and whether it weakens the policy.
func TestDiffGates(t *testing.T) {
	withGate := func(gate string) string {
		g := ""
		if gate != "" {
			g = `, "approval": ` + gate
		}
		return `{"version": 1, "flow": "f", "nodes": [{"name": "a", "block": "a"}, {"name": "pay", "block": "pay"` + g + `}],
		  "wiring": [{"edge": ["a", "pay"]}]}`
	}
	g := func(need int, approvers ...string) string {
		b, _ := json.Marshal(map[string]any{"need": need, "approvers": approvers})
		return string(b)
	}
	for _, c := range []struct {
		name, from, to, verdict, line string
	}{
		{"need lowered", g(2, "ops", "finance", "risk"), g(1, "ops", "finance", "risk"), "weakened", "approval 2 of 3 -> 1 of 3"},
		{"need raised", g(1, "ops", "finance", "risk"), g(2, "ops", "finance", "risk"), "strengthened", "approval 1 of 3 -> 2 of 3"},
		{"gate removed", g(2, "ops", "finance"), "", "weakened", "approval 2 of 2: ops, finance -> none"},
		{"gate added", "", g(1, "ops"), "strengthened", "approval none -> 1 of 1: ops"},
		{"approver added, need kept", g(2, "ops", "finance"), g(2, "ops", "finance", "intern"), "weakened", "approval 2 of 2: ops, finance -> 2 of 3: ops, finance, intern"},
		{"approver added, need raised", g(2, "ops", "finance"), g(3, "ops", "finance", "intern"), "strengthened", "approval 2 of 2: ops, finance -> 3 of 3: ops, finance, intern"},
		{"approver removed, need kept", g(2, "ops", "finance", "risk"), g(2, "ops", "finance"), "strengthened", "approval 2 of 3: ops, finance, risk -> 2 of 2: ops, finance"},
		{"approver replaced", g(1, "ops", "finance"), g(1, "ops", "intern"), "weakened", "approval 1 of 2: ops, finance -> 1 of 2: ops, intern"},
		{"reordered", g(2, "ops", "finance"), g(2, "finance", "ops"), "reordered", "approval 2 of 2: ops, finance -> 2 of 2: finance, ops"},
	} {
		v := diffOf(t, withGate(c.from), withGate(c.to), nil, nil)
		if len(v.Changes) != 1 || v.Changes[0].Gate != c.verdict || v.Changes[0].Details[0] != c.line+" ("+c.verdict+")" {
			t.Errorf("%s: changes %q, gate %q; want %q (%s)", c.name, lines(v.Changes), v.Changes[0].Gate, c.line, c.verdict)
		}
		if weak := len(v.Weakened) > 0; weak != (c.verdict == "weakened") {
			t.Errorf("%s: weakened %q", c.name, v.Weakened)
		}
	}

	// A gated node removed takes its gate with it: weakened, as a rename of a gated node is.
	gone := `{"version": 1, "flow": "f", "nodes": [{"name": "a", "block": "a"}, {"name": "pay2", "block": "pay"}], "wiring": [{"edge": ["a", "pay2"]}]}`
	v := diffOf(t, withGate(g(2, "ops", "finance")), gone, nil, nil)
	if !slices.Equal(v.Weakened, []string{"pay: approval 2 of 2: ops, finance -> none"}) {
		t.Errorf("a removed gated node: weakened %q, changes %q", v.Weakened, lines(v.Changes))
	}

	// accepts on single-decision gates, which a config cannot declare but a registered tool can.
	single, two := &plan.TopologyGate{Single: true}, &plan.TopologyGate{Need: 2, Approvers: []string{"a", "b"}}
	if gateChange(two, single) != "weakened" || gateChange(single, two) != "strengthened" || gateChange(single, nil) != "weakened" || gateChange(nil, single) != "strengthened" || gateChange(single, single) != "" {
		t.Error("single-decision gates misclassified")
	}
}

// TestDiffDigest: with the Registry each side's digest is the one plan.Load's flow journals, and a
// changed gate changes it; without one, the digest is not computed and the summary says why.
func TestDiffDigest(t *testing.T) {
	// Load refuses a block registered and never used, so each version has its own Registry, as the
	// program had at that version.
	reg1, reg2 := refundRegistry(t), plan.NewRegistry()
	for _, err := range []error{
		reg2.RegisterStep("validate", func(_ context.Context, o order) (order, error) { return o, nil }),
		reg2.RegisterStep("charge", func(_ context.Context, o order) (order, error) { return o, nil }),
		reg2.RegisterStep("review", func(_ context.Context, o order) (string, error) { return "review " + o.ID, nil }),
		reg2.RegisterStep("receipt", func(_ context.Context, o order) (string, error) { return o.ID, nil }),
		reg2.RegisterPredicate("large", func(o order) bool { return o.Amount > 100 }),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	noKeys := plan.WithLoadedApproverVerifiers(func(string) (agent.ApproverVerifier, bool) { return nil, false })
	f1, err := plan.Load[order, string]([]byte(diffV1), reg1, noKeys)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := plan.Load[order, string]([]byte(diffV2), reg2, noKeys)
	if err != nil {
		t.Fatal(err)
	}
	if f1.Digest() == f2.Digest() {
		t.Fatal("the two flows have one digest")
	}
	v := diffOf(t, diffV1, diffV2, reg1, reg2)
	if !v.Typed || v.OldDigest != f1.Digest() || v.NewDigest != f2.Digest() || v.DigestLine() != "digest "+f1.Digest()+" -> "+f2.Digest() {
		t.Fatalf("typed %v, digests %s -> %s, line %q; want %s -> %s", v.Typed, v.OldDigest, v.NewDigest, v.DigestLine(), f1.Digest(), f2.Digest())
	}
	var out bytes.Buffer
	if weak, err := CheckDiff(&out, Diff{Old: Config{Name: "v1.json", Data: []byte(diffV1), Registry: reg1}, New: Config{Name: "v2.json", Data: []byte(diffV2), Registry: reg2}}); err != nil || weak {
		t.Fatalf("CheckDiff = %v, %v", weak, err)
	}
	if !strings.Contains(out.String(), "v2.json: 6 changes from v1.json (nodes: 1 added, 1 removed, 2 changed; edges: 1 added, 1 removed, 0 changed); digest "+f1.Digest()+" -> "+f2.Digest()+"\n") {
		t.Errorf("summary line missing:\n%s", out.String())
	}
	if v := diffOf(t, diffV1, diffV1, reg1, reg1); v.DigestLine() != "digest unchanged: "+f1.Digest() {
		t.Errorf("unchanged: %q", v.DigestLine())
	}
	// One Registry for both: the old version names a block it lacks (skip) and leaves one unused
	// (review), so it has errors and no digest.
	if v := diffOf(t, diffV1, diffV2, reg2, reg2); v.OldErrors != 2 || v.OldDigest != "" || v.NewDigest != f2.Digest() || v.DigestLine() != "digest not computed: a side has errors" {
		t.Errorf("one Registry: old errors %d, digests %q -> %q, line %q", v.OldErrors, v.OldDigest, v.NewDigest, v.DigestLine())
	}
	if v := diffOf(t, diffV1, diffV2, nil, nil); v.Typed || v.OldDigest != "" || !strings.Contains(v.DigestLine(), "not computed: the check is structural") {
		t.Errorf("structural: typed %v, digest %q, line %q", v.Typed, v.OldDigest, v.DigestLine())
	}
}

// TestDiffPage: the page draws added nodes and edges, removed ones as ghosts placed where they were,
// a changed node amber with its gate's verdict, and the weakened-policy banner; /diff.json is the view.
func TestDiffPage(t *testing.T) {
	h := Handler(Options{Diff: &Diff{Old: Config{Name: "v2.json", Data: []byte(diffV2)}, New: Config{Name: "v1.json", Data: []byte(diffV1)}}})
	code, body := get(t, h, "/")
	if code != 200 {
		t.Fatalf("GET / = %d: %s", code, body)
	}
	for _, want := range []string{
		`<g class="node added"`, `<g class="node removed"`, `<g class="node changed"`, `<g class="node unchanged"`,
		`class="edge added"`, `class="edge removed"`, `marker-end="url(#arrow-removed)"`, `<marker id="arrow-added"`,
		`Approval policy weakened`, `charge: approval 2 of 3 -&gt; 1 of 3`, `>WEAKENED<`, `/assets/diff.css`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	code, body = get(t, h, "/diff.json")
	var v DiffView
	if code != 200 || json.Unmarshal([]byte(body), &v) != nil {
		t.Fatalf("GET /diff.json = %d: %s", code, body)
	}
	var names []string
	for _, n := range v.Graph.Nodes {
		names = append(names, n.Name+":"+n.Status)
	}
	// review (removed) follows charge, the node it followed in the old config.
	if want := []string{"validate:changed", "charge:changed", "review:removed", "skip:added", "receipt:unchanged"}; !slices.Equal(names, want) {
		t.Errorf("graph nodes %q, want %q", names, want)
	}
	if code, _ := get(t, h, "/assets/diff.css"); code != 200 {
		t.Errorf("GET /assets/diff.css = %d", code)
	}

	// A version that is not a flow config: an error, on the page and from BuildDiff.
	bad := Handler(Options{Diff: &Diff{Old: Config{Name: "old.json", Data: []byte(`{`)}, New: Config{Name: "new.json", Data: []byte(diffV1)}}})
	if code, body := get(t, bad, "/"); code != 500 || !strings.Contains(body, "old.json") {
		t.Errorf("an unreadable old config: %d %s", code, body)
	}
}

// TestDiffCLI: -diff -check prints the changes and exits 0, a weakened policy included, unless
// -fail-on-weaken; usage errors exit 2 and an unreadable version 1.
func TestDiffCLI(t *testing.T) {
	dir := t.TempDir()
	v1, v2 := filepath.Join(dir, "v1.json"), filepath.Join(dir, "v2.json")
	for p, s := range map[string]string{v1: diffV1, v2: diffV2} {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(args, &out, &errb, Stores{})
		return code, out.String() + errb.String()
	}
	if code, out := run("-config", v2, "-diff", v1, "-check"); code != 0 || !strings.Contains(out, v2+": changed node charge: approval 1 of 3 -> 2 of 3 (strengthened)\n") {
		t.Errorf("strengthened: exit %d\n%s", code, out)
	}
	if code, out := run("-config", v2, "-diff", v1, "-check", "-fail-on-weaken"); code != 0 {
		t.Errorf("strengthened, -fail-on-weaken: exit %d, want 0\n%s", code, out)
	}
	if code, out := run("-config", v1, "-diff", v2, "-check"); code != 0 || !strings.Contains(out, v1+": approval policy weakened: charge: approval 2 of 3 -> 1 of 3\n") {
		t.Errorf("weakened: exit %d, want 0 and the weakened line\n%s", code, out)
	}
	if code, out := run("-config", v1, "-diff", v2, "-check", "-fail-on-weaken"); code != 1 {
		t.Errorf("weakened, -fail-on-weaken: exit %d, want 1\n%s", code, out)
	}
	for _, args := range [][]string{
		{"-diff", v1, "-check"},                           // no -config
		{"-config", v2, "-fail-on-weaken", "-check"},      // no -diff
		{"-config", v2, "-diff", v1, "-fail-on-weaken"},   // no -check
		{"-config", v2, "-diff", v1, "-check", "-strict"}, // -strict is for one config's problems
	} {
		if code, out := run(args...); code != 2 {
			t.Errorf("%q: exit %d, want 2\n%s", args, code, out)
		}
	}
	if code, out := run("-config", v2, "-diff", filepath.Join(dir, "-nope"), "-check"); code != 1 || !strings.Contains(out, "no such file") {
		t.Errorf("a missing old version: exit %d\n%s", code, out)
	}
}

// TestDiffGitRevision: -diff reads a git revision when no file has its name: <rev>:<path> from the
// working directory, and a bare <rev> as the -config file's own path at that revision. A file of
// that name wins.
func TestDiffGitRevision(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %q: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	flows := filepath.Join(repo, "flows")
	if err := os.Mkdir(flows, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(flows, "refund.json")
	if err := os.WriteFile(cfg, []byte(diffV2), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "v2")
	if err := os.WriteFile(cfg, []byte(diffV1), 0o644); err != nil { // the working copy weakens the gate
		t.Fatal(err)
	}
	t.Chdir(repo)
	for _, rev := range []string{"HEAD", "HEAD:flows/refund.json", "HEAD:./flows/refund.json"} {
		var out, errb bytes.Buffer
		code := Main([]string{"-config", cfg, "-diff", rev, "-check", "-fail-on-weaken"}, &out, &errb, Stores{})
		if code != 1 || !strings.Contains(out.String(), "approval policy weakened: charge: approval 2 of 3 -> 1 of 3") {
			t.Errorf("-diff %s: exit %d, want 1 and the weakened gate\n%s%s", rev, code, out.String(), errb.String())
		}
	}
	var out, errb bytes.Buffer
	if code := Main([]string{"-config", cfg, "-diff", "nosuchrev", "-check"}, &out, &errb, Stores{}); code != 1 || !strings.Contains(errb.String(), "git show nosuchrev:./refund.json failed") {
		t.Errorf("an unknown revision: exit %d\n%s", code, errb.String())
	}
	// A file named like a revision is read as the file.
	if err := os.WriteFile(filepath.Join(repo, "HEAD"), []byte(diffV1), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Main([]string{"-config", cfg, "-diff", "HEAD", "-check"}, &out, &errb, Stores{}); code != 0 || !strings.Contains(out.String(), "no changes from HEAD") {
		t.Errorf("a file named HEAD: exit %d\n%s", code, out.String())
	}
}

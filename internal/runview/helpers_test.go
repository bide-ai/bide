package runview

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/plan"
)

// TestGateWords: a declared gate in each of the phrasings the config and diff pages use.
func TestGateWords(t *testing.T) {
	two := &plan.TopologyGate{Need: 2, Approvers: []string{"ops", "finance", "risk"}}
	one := &plan.TopologyGate{Single: true}
	for _, c := range []struct {
		name                  string
		g                     *plan.TopologyGate
		short, full, topoText string
	}{
		{"2 of 3", two, "2 of 3", "2 of 3: ops, finance, risk", "2 of 3: ops, finance, risk"},
		{"single", one, "one decision", "one decision", "one decision"},
	} {
		if got := gateShort(c.g); got != c.short {
			t.Errorf("%s: gateShort = %q", c.name, got)
		}
		if got := gateFull(c.g); got != c.full {
			t.Errorf("%s: gateFull = %q", c.name, got)
		}
		if got := topoGateText(c.g); got != c.topoText {
			t.Errorf("%s: topoGateText = %q", c.name, got)
		}
	}
	if gateShort(nil) != "none" || gateFull(nil) != "none" {
		t.Error("no gate is not \"none\"")
	}
	one3 := &plan.TopologyGate{Need: 1, Approvers: []string{"ops", "finance", "risk"}}
	if got := gateMove(one3, two); got != "approval 1 of 3 -> 2 of 3" {
		t.Errorf("same approvers: %q", got)
	}
	if got := gateMove(two, &plan.TopologyGate{Need: 2, Approvers: []string{"ops", "legal"}}); got != "approval 2 of 3: ops, finance, risk -> 2 of 2: ops, legal" {
		t.Errorf("different approvers: %q", got)
	}
	if got := gateMove(nil, one); got != "approval none -> one decision" {
		t.Errorf("added single gate: %q", got)
	}
}

// TestGateDetail: the gate item of a change's details, cut before its verdict and its extras.
func TestGateDetail(t *testing.T) {
	c := Change{Details: []string{"safety readonly -> side_effect", "approval 2 of 3 -> 1 of 3 (weakened)"}}
	if got := gateDetail(c); got != "approval 2 of 3 -> 1 of 3" {
		t.Errorf("verdict not cut: %q", got)
	}
	c = Change{Details: []string{"approval none -> 2 of 3, with the node's inputs"}}
	if got := gateDetail(c); got != "approval none -> 2 of 3" {
		t.Errorf("extras not cut: %q", got)
	}
	if got := gateDetail(Change{Details: []string{"safety readonly -> side_effect"}}); got != "" {
		t.Errorf("no gate item: %q", got)
	}
}

// TestNodeKindAndSafetyWords: how a node's kind and its safety are phrased.
func TestNodeKindAndSafetyWords(t *testing.T) {
	for _, c := range []struct {
		n    plan.TopologyNode
		want string
	}{
		{plan.TopologyNode{Block: "charge"}, "block charge"},
		{plan.TopologyNode{Kind: plan.NodeKindJoin, Block: "merge"}, "join, merge merge"},
		{plan.TopologyNode{Kind: plan.NodeKindTool, Block: "lookup"}, "tool lookup"},
	} {
		if got := nodeKind(c.n); got != c.want {
			t.Errorf("nodeKind(%+v) = %q, want %q", c.n, got, c.want)
		}
	}
	if safetyText("") != "(the block's)" || safetyText("side_effect") != "side_effect" {
		t.Error("safetyText")
	}
	r := plan.ConfigReport{Safety: map[string]plan.NodeSafety{"a": plan.SafetyReadOnly}}
	if safetyOf(r, "a") != plan.SafetyReadOnly || safetyOf(r, "b") != plan.SafetyUnknown {
		t.Error("safetyOf: a node the report has no class for is unknown")
	}
}

// TestGlyphsAndBadges: the picture's small pieces: a known icon draws, an unknown one draws
// nothing; a node wears its risk glyph, else its status glyph, else the plain one; a gate badge is
// need/of.
func TestGlyphsAndBadges(t *testing.T) {
	if got := string(icon("halt")); !strings.Contains(got, `class="ic ic-halt"`) || !strings.Contains(got, `href="#ic-halt"`) {
		t.Errorf("icon(halt) = %q", got)
	}
	if got := icon("no-such-glyph"); got != "" {
		t.Errorf("icon(unknown) = %q, want nothing", got)
	}
	for _, c := range []struct {
		n    GraphNode
		want string
	}{
		{GraphNode{Risk: "side effect", Status: "done"}, "side"},
		{GraphNode{Status: "halted"}, "halt"},
		{GraphNode{Status: "something-new"}, "node"},
		{GraphNode{}, "node"},
	} {
		if got := nodeIconName(c.n); got != c.want {
			t.Errorf("nodeIconName(%+v) = %q, want %q", c.n, got, c.want)
		}
	}
	for in, want := range map[string]string{"2 of 3": "2/3", "one decision": "1/1", "10 of 12": "10/12", "whatever": "gate", "": "gate"} {
		if got := gateBadge(in); got != want {
			t.Errorf("gateBadge(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"not-reached": "not reached", "undeclared": "not declared", "done": "done"} {
		if got := statusWords(in); got != want {
			t.Errorf("statusWords(%q) = %q, want %q", in, got, want)
		}
	}
	if got := clip("abcdef", 4); got != "abc…" {
		t.Errorf("clip = %q", got)
	}
	if got := clip("abcd", 4); got != "abcd" {
		t.Errorf("clip at the limit = %q", got)
	}
}

// TestTextHelpers: the short renderings of hashes, records and JSON in the timeline.
func TestTextHelpers(t *testing.T) {
	if short8("0123456789abcdef") != "01234567" || short8("abc") != "abc" {
		t.Error("short8 keeps the first 8 hex digits and leaves a shorter hash alone")
	}
	if compact(nil) != "" || compact(json.RawMessage(`{"a":1}`)) != `{"a":1}` {
		t.Error("compact")
	}
	if short(nil) != "" || short(json.RawMessage(`"text"`)) != "text" || short(json.RawMessage(`{"a":1}`)) != `{"a":1}` {
		t.Error("short: a JSON string is its text, anything else its JSON")
	}
	long := json.RawMessage(`"` + strings.Repeat("x", 500) + `"`)
	if got := short(long); len(got) != 403 || !strings.HasSuffix(got, "...") {
		t.Errorf("short of 500 chars = %d long, want 400 and an ellipsis", len(got))
	}
	for _, c := range []struct {
		r    agent.Record
		want string
	}{
		{agent.Record{}, "side effect (resolved)"},
		{agent.Record{Safety: &agent.Safety{ReadOnly: true}}, "read-only"},
		{agent.Record{Safety: &agent.Safety{Idempotent: true}}, "idempotent"},
		{agent.Record{Safety: &agent.Safety{}}, "side effect"},
	} {
		if got := safetyLabel(c.r); got != c.want {
			t.Errorf("safetyLabel(%+v) = %q, want %q", c.r.Safety, got, c.want)
		}
	}
	if cmpName(Gate{ToolName: "refund", ToolUseID: "c1"}) != "refund" || cmpName(Gate{ToolUseID: "c1"}) != "c1" {
		t.Error("cmpName names a gate by its tool, else its call")
	}
}

// TestJournalSourceWithoutLister: a store that cannot list its runs lists none rather than failing.
func TestJournalSourceWithoutLister(t *testing.T) {
	src := JournalSource{J: noListJournal(t)}
	ids, err := src.Runs(t.Context())
	if err != nil || len(ids) != 0 {
		t.Errorf("Runs = %v, %v, want none and no error", ids, err)
	}
}

// noListStore hides the optional listing capability of the store it wraps.
type noListStore struct{ s agent.Store }

func (n noListStore) Insert(ctx context.Context, r, name string, d []byte) (agent.Entry, bool, error) {
	return n.s.Insert(ctx, r, name, d)
}
func (n noListStore) Get(ctx context.Context, r, name string) (agent.Entry, bool, error) {
	return n.s.Get(ctx, r, name)
}
func (n noListStore) Load(ctx context.Context, r string, after int64) iter.Seq2[agent.Entry, error] {
	return n.s.Load(ctx, r, after)
}

func noListJournal(t *testing.T) *agent.Journal {
	t.Helper()
	return agenttest.MustJournal(noListStore{agent.NewMemStore()})
}

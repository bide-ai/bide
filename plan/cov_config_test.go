package plan

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/bide-ai/bide/agent"
)

// LoadReader reads the whole config and loads it, and reports a read failure as one.
func TestLoadReader(t *testing.T) {
	reg := triageRegistry(t)
	flow, err := LoadReader[cfgOrder, cfgReceipt](strings.NewReader(triageConfig), reg)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := Load[cfgOrder, cfgReceipt]([]byte(triageConfig), triageRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	if flow.Digest() != direct.Digest() {
		t.Errorf("LoadReader digest %s != Load digest %s", flow.Digest(), direct.Digest())
	}
	_, err = LoadReader[cfgOrder, cfgReceipt](iotest.ErrReader(errors.New("disk gone")), reg)
	if err == nil || !strings.Contains(err.Error(), "read config: disk gone") {
		t.Errorf("a failing reader = %v, want a read-config error", err)
	}
}

// A config that does not describe a flow is refused with an error naming what is wrong, and
// errors.Is(agent.ErrConfig) where the config (not the loader's inputs) is at fault.
func TestLoad_RefusesMalformedWiring(t *testing.T) {
	const nodes = `"nodes": [
    {"name": "classify", "block": "classify"},
    {"name": "reserve",  "block": "reserve"},
    {"name": "finalize", "block": "finalize"},
    {"name": "decline",  "block": "decline"}
  ]`
	cfg := func(entry, wiring string) string {
		e := ""
		if entry != "" {
			e = `"entry": "` + entry + `",`
		}
		return `{"version": 1, "flow": "triage", "in": "plan.cfgOrder", "out": "plan.cfgReceipt", ` + e + nodes + `, "wiring": [` + wiring + `]}`
	}
	good := `{"switch": "classify", "when": [{"pred": "rush", "to": "reserve"}], "else": "decline"}, {"edge": ["reserve", "finalize"]}`
	for name, tc := range map[string]struct {
		config string
		want   string
	}{
		"entry names no node":          {cfg("nowhere", good), `entry "nowhere" names no declared node`},
		"edge from an unknown node":    {cfg("", `{"edge": ["ghost", "finalize"]}`), `wiring[0] edge from unknown node "ghost"`},
		"edge to an unknown node":      {cfg("", `{"edge": ["reserve", "ghost"]}`), `wiring[0] edge to unknown node "ghost"`},
		"switch over an unknown node":  {cfg("", `{"switch": "ghost", "else": "decline"}`), `wiring[0] switch over unknown node "ghost"`},
		"arm to an unknown node":       {cfg("", `{"switch": "classify", "when": [{"pred": "rush", "to": "ghost"}], "else": "decline"}`), `wiring[0].when[0] routes to unknown node "ghost"`},
		"else to an unknown node":      {cfg("", `{"switch": "classify", "when": [{"pred": "rush", "to": "reserve"}], "else": "ghost"}`), `wiring[0] else routes to unknown node "ghost"`},
		"switched node with an edge":   {cfg("", good+`, {"edge": ["classify", "decline"]}`), `node "classify" is both switched-over and has an outgoing edge`},
		"in type disagrees with Load":  {strings.Replace(cfg("", good), `"in": "plan.cfgOrder"`, `"in": "plan.cfgReceipt"`, 1), `config declares in "plan.cfgReceipt" but Load In is plan.cfgOrder`},
		"out type disagrees with Load": {strings.Replace(cfg("", good), `"out": "plan.cfgReceipt"`, `"out": "plan.cfgOrder"`, 1), `config declares out "plan.cfgOrder" but Load Out is plan.cfgReceipt`},
	} {
		_, err := Load[cfgOrder, cfgReceipt]([]byte(tc.config), triageRegistry(t))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
		}
	}
}

func TestLoad_RefusesDuplicateNodeNames(t *testing.T) {
	config := `{"version": 1, "flow": "dup", "nodes": [
    {"name": "classify", "block": "classify"},
    {"name": "classify", "block": "classify"}
  ], "wiring": []}`
	_, err := Load[cfgOrder, cfgAssessment]([]byte(config), triageRegistry(t))
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `node "classify" is declared more than once`) {
		t.Errorf("err = %v, want ErrConfig naming the duplicate", err)
	}
}

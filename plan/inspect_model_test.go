package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A config with a Model block is a valid config: Load accepts it once a model is bound, which
// the program does and a check cannot. InspectConfig must not report the missing binding as a
// problem with the config.
func TestInspectConfigAcceptsAModelBlock(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterModel[ticket, review]("assessBlock", "Subject: {{.Subject}}"); err != nil {
		t.Fatal(err)
	}
	cfg := `{"version":1,"flow":"triage","entry":"assess","nodes":[{"name":"assess","block":"assessBlock"}],"wiring":[]}`
	r := InspectConfig([]byte(cfg), reg)
	if r.Errors() || r.Digest == "" || len(r.Topology.Nodes) != 1 {
		t.Fatalf("problems %+v, digest %q, nodes %d", r.Problems, r.Digest, len(r.Topology.Nodes))
	}
	if r.Topology.Nodes[0].Kind != NodeKindModel {
		t.Errorf("kind %s", r.Topology.Nodes[0].Kind)
	}
}

// loadErrors reports whether rep has an error Load would also report: assert lines are checked, not
// loaded.
func loadErrors(rep ConfigReport) bool {
	for _, p := range rep.Problems {
		if p.Severity == "error" && !strings.HasPrefix(p.Message, "assert[") {
			return true
		}
	}
	return false
}

func modelRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, err := range []error{
		reg.RegisterStep("intake", func(_ context.Context, k ticket) (ticket, error) { return k, nil }, ReadOnly()),
		reg.RegisterModel[ticket, review]("assessBlock", "Subject: {{.Subject}}"),
		reg.RegisterStep("publish", func(_ context.Context, r review) (review, error) { return r, nil }),
		reg.RegisterStep("escalate", func(_ context.Context, r review) (review, error) { return r, nil }),
		reg.RegisterPredicate("bad", func(r review) bool { return r.Score < 3 }),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

const modelFlowConfig = `{"version":1,"flow":"triage","entry":"intake",
  "nodes":[{"name":"intake","block":"intake"},{"name":"assess","block":"assessBlock"},{"name":"publish","block":"publish"},{"name":"escalate","block":"escalate"}],
  "wiring":[{"edge":["intake","assess"]},{"switch":"assess","when":[{"pred":"bad","to":"escalate"}],"else":"publish"}]}`

// The stand-in model InspectConfig binds must not make it more lenient than Load. For a config with a
// Model block and each of its variants, a config Load accepts with a real model is clean, with
// Load's own digest; a config InspectConfig finds clean is one Load accepts, unless only the flow's
// boundary types (which InspectConfig takes from the entry and terminals) differ; and no report ever
// says a model is unbound. The variants are genuinely misconfigured in the ways variants() breaks a
// config, and most of them are reported.
func TestInspectConfigStandInModelIsNoMoreLenientThanLoad(t *testing.T) {
	reg := modelRegistry(t)
	var loads, clean, reported int
	for _, cfg := range variants(t, modelFlowConfig, reg) {
		flow, loadErr := Load[ticket, review]([]byte(cfg), modelRegistry(t), WithLoadedModel(&fakeModel{reply: `{}`}),
			WithLoadedApproverVerifiers(func(string) (agent.ApproverVerifier, bool) { return nil, false }))
		rep := InspectConfig([]byte(cfg), reg)
		for _, p := range rep.Problems {
			if strings.Contains(p.Message, "no bound model") || strings.Contains(p.Message, "WithModel") {
				t.Fatalf("a binding problem for\n%s\n%+v", cfg, p)
			}
		}
		if loadErr == nil {
			loads++
			if loadErrors(rep) || rep.Digest != flow.Digest() {
				t.Fatalf("Load accepts but the check reports %+v (digest %q, Load's %q) for\n%s", rep.Problems, rep.Digest, flow.Digest(), cfg)
			}
		}
		if !loadErrors(rep) {
			clean++
			if loadErr != nil && !strings.Contains(loadErr.Error(), "flow input") && !strings.Contains(loadErr.Error(), "flow output") && !strings.Contains(loadErr.Error(), "declares") {
				t.Fatalf("the check is clean but Load refuses (%v) for\n%s", loadErr, cfg)
			}
		} else {
			reported++
		}
	}
	if loads == 0 || clean == 0 || reported < 20 {
		t.Errorf("the corpus is too thin: %d Load accepts, %d clean, %d reported", loads, clean, reported)
	}
}

// A Model block that is wrong in itself is still an error: its block unknown, its input not what
// feeds it, its output not what the next node takes.
func TestInspectConfigStillReportsAMisconfiguredModelBlock(t *testing.T) {
	reg := modelRegistry(t)
	for name, c := range map[string]struct{ from, to, want string }{
		"unknown block":      {`"block":"assessBlock"`, `"block":"assessBlok"`, "unknown block"},
		"fed the wrong type": {`{"edge":["intake","assess"]}`, `{"edge":["publish","assess"]}`, "connects"},
	} {
		cfg := strings.Replace(modelFlowConfig, c.from, c.to, 1)
		rep := InspectConfig([]byte(cfg), reg)
		var msgs []string
		for _, p := range rep.Problems {
			msgs = append(msgs, p.Message)
		}
		if !rep.Errors() || !strings.Contains(strings.Join(msgs, "\n"), c.want) {
			t.Errorf("%s: problems %q", name, msgs)
		}
	}
}

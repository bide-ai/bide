package plan

import "testing"

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

package runview

import (
	"encoding/json"
	"strings"
	"testing"
)

// The JSON a page is served for a config carries the problems and no findings: no page uses them, and
// they can be long. A Go host that marshals plan.ConfigReport gets them.
func TestConfigViewJSONHasNoFindings(t *testing.T) {
	v := BuildConfig(Config{Name: "f.json", Data: []byte(`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"x"}],"wiring":[{"edge":["a","ghost"]}]}`)})
	if len(v.Problems) == 0 {
		t.Fatal("the config has no problem")
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"findings"`) {
		t.Errorf("the page JSON carries findings: %.300s", b)
	}
}

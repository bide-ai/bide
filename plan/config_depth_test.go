package plan

import (
	"runtime"
	"strings"
	"testing"
)

// A config nested far past any real topology is refused with an error naming the depth limit, in
// bounded time and memory.
func TestInspectConfig_RefusesDeepNesting(t *testing.T) {
	data := []byte(`{"version":1,"x":` + strings.Repeat("[", 20_000))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := InspectConfig(data, nil)
	runtime.ReadMemStats(&after)
	var found bool
	for _, p := range r.Problems {
		found = found || strings.Contains(p.Message, "nesting deeper than")
	}
	if !found {
		t.Fatalf("no nesting depth problem in %+v", r.Problems)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 4<<20 {
		t.Fatalf("allocated %d bytes for 20 KB of config", got)
	}
}

// Messages that quote names from a config are bounded: a 5,000-character node name or key gives a
// problem of a few hundred characters, in InspectConfig and in Load's error.
func TestConfigErrors_LongNamesAreBounded(t *testing.T) {
	long := strings.Repeat("n", 5000)
	for name, cfg := range map[string]string{
		"node name":    `{"version":1,"flow":"f","entry":"` + long + `","nodes":[{"name":"` + long + `","block":"missing"},{"name":"` + long + `","block":"missing"}]}`,
		"unknown safe": `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"b","safety":"` + long + `"}]}`,
		"entry":        `{"version":1,"flow":"` + long + `","entry":"` + long + `","nodes":[]}`,
	} {
		r := InspectConfig([]byte(cfg), nil)
		if len(r.Problems) == 0 {
			t.Errorf("%s: no problem reported", name)
		}
		for _, p := range r.Problems {
			if len(p.Message) > 1600 || strings.Contains(p.Message, strings.Repeat("n", 700)) {
				t.Errorf("%s: a problem is %d bytes: %.120q", name, len(p.Message), p.Message)
			}
		}
		if _, err := Load[string, string]([]byte(cfg), NewRegistry()); err == nil {
			t.Errorf("%s: loaded", name)
		} else if len(err.Error()) > 8300 || strings.Contains(err.Error(), strings.Repeat("n", 9000)) {
			t.Errorf("%s: Load error is %d bytes", name, len(err.Error()))
		}
	}
}

func TestBoundMessage(t *testing.T) {
	long := strings.Repeat("n", 300)
	for in, want := range map[string]string{
		`node "a" has a problem`: `node "a" has a problem`,
		`node "` + long + `" connects "b" (types must match exactly)`: `node "` + long[:64] + `..." connects "b" (types must match exactly)`,
		`key "a\"` + long + `" end`:                                   `key "a\"` + long[:61] + `..." end`,
		`unclosed "` + long:                                           `unclosed "` + long,
	} {
		if got := boundMessage(in); got != want {
			t.Errorf("boundMessage(%.40q...) = %.80q, want %.80q", in, got, want)
		}
	}
	if got := boundMessage(strings.Repeat("x", 5000)); len(got) > 2100 || !strings.Contains(got, " ... ") {
		t.Errorf("a long message with no names keeps its head and tail: %d bytes", len(got))
	}
}

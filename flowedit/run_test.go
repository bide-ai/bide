package flowedit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/editrun"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
)

const runCfg = `{"version":1,"flow":"pay","in":"int","out":"int","entry":"a",
 "nodes":[{"name":"a","block":"inc"},{"name":"b","block":"dbl"}],
 "wiring":[{"edge":["a","b"]}]}`

func runRegistry(t *testing.T) *plan.Registry {
	t.Helper()
	reg := plan.NewRegistry()
	for name, f := range map[string]func(context.Context, int) (int, error){
		"inc": func(_ context.Context, n int) (int, error) { return n + 1, nil },
		"dbl": func(_ context.Context, n int) (int, error) { return n * 2, nil },
	} {
		if err := reg.RegisterStep(name, f); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// ranRun runs cfg once under run ID "r1" in a memory journal, as a program would, and returns the
// journal and the flow's digest.
func ranRun(t *testing.T, cfg string) (*agent.Journal, string) {
	t.Helper()
	flow, err := plan.Load[int, int]([]byte(cfg), runRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Run(context.Background(), j, "r1", 1); err != nil {
		t.Fatal(err)
	}
	return j, flow.Digest()
}

// runServer is an editor on a copy of file that compares with run id from src, typed when typed.
func runServer(t *testing.T, file string, src runview.Source, id string, typed bool) *Server {
	t.Helper()
	p := filepath.Join(t.TempDir(), "flow.json")
	if err := os.WriteFile(p, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := []Option{editrun.NewOption(editrun.Run{ID: id, Source: src}).(Option)}
	if typed {
		opts = append(opts, WithRegistry(runRegistry(t)))
	}
	s, err := newServer(t, p, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func askRun(t *testing.T, s *Server, text string, renames []Rename, reload bool) *runReply {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"text": text, "renames": renames, "reload": reload})
	w := call(s.Handler(), s, "/api/run", string(body), nil)
	if w.Code != 200 {
		t.Fatalf("/api/run: %d %s", w.Code, w.Body)
	}
	var r runReply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

// The constructor is registered once flowedit is linked, and builds a real Option: a build that forgot to
// link the package would otherwise lose the feature without an error.
func TestRunOptionIsRegistered(t *testing.T) {
	if editrun.NewOption == nil {
		t.Fatal("editrun.NewOption is not set: flowedit's init did not run")
	}
	if _, ok := editrun.NewOption(editrun.Run{ID: "x"}).(Option); !ok {
		t.Errorf("NewOption returned %T, not an Option", editrun.NewOption(editrun.Run{}))
	}
}

// Two editors in one process keep their own runs.
func TestTwoEditorsKeepSeparateRuns(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	src := runview.JournalSource{J: j}
	a := runServer(t, runCfg, src, "r1", true)
	b := runServer(t, runCfg, src, "other", true)
	ra, rb := askRun(t, a, runCfg, nil, false), askRun(t, b, runCfg, nil, false)
	if ra.Run.ID != "r1" || ra.Status != "same" || rb.Run.ID != "other" || rb.Status != "not_found" {
		t.Errorf("a: %+v %s; b: %+v %s", ra.Run, ra.Status, rb.Run, rb.Status)
	}
}

func TestNoRunConfigured(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	body := call(s.Handler(), s, "/api/run", `{"text":""}`, nil).Body.String()
	if strings.TrimSpace(body) != `{"configured":false}` {
		t.Errorf("%s", body)
	}
}

func TestRunSameDigest(t *testing.T) {
	j, digest := ranRun(t, runCfg)
	s := runServer(t, runCfg, runview.JournalSource{J: j}, "r1", true)
	r := askRun(t, s, runCfg, nil, false)
	if r.Status != "same" || r.RunDigest != digest || r.FileDigest != digest || len(r.Changes) != 0 {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"the same digest run r1 journaled", digest, "does not say the run followed this file", "conformance"} {
		if !strings.Contains(r.Claim, want) {
			t.Errorf("claim %q lacks %q", r.Claim, want)
		}
	}
	if !strings.Contains(r.Claim, "rests on the program's Registry") || r.Basis == "" {
		t.Errorf("basis missing: %q", r.Claim)
	}
	if r.Run.Flow != "pay" || r.Run.Digest != digest || r.Run.ReadAt == "" || r.Run.Records == 0 {
		t.Errorf("run %+v", r.Run)
	}
}

func TestRunStructuralSaysNoDifferenceSeenAndNeverEqual(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	s := runServer(t, runCfg, runview.JournalSource{J: j}, "r1", false)
	r := askRun(t, s, runCfg, nil, false)
	if r.Status != "none_seen" || r.FileDigest != "" || r.RunDigest != "" {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"No difference in flow name, entry, node names", "not compared in this mode", "-blocks"} {
		if !strings.Contains(r.Claim, want) {
			t.Errorf("claim %q lacks %q", r.Claim, want)
		}
	}
	for _, bad := range []string{"equal", "same digest", "identical"} {
		if strings.Contains(strings.ToLower(r.Claim), bad) {
			t.Errorf("a structural comparison must not say %q: %q", bad, r.Claim)
		}
	}
	if !slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "Gates are compared only when") }) {
		t.Errorf("notes %q", r.Notes)
	}
}

func TestRunDiffers(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	src := runview.JournalSource{J: j}
	moved := strings.NewReplacer(`{"name":"b","block":"dbl"}]`, `{"name":"b","block":"dbl"},{"name":"c","block":"inc"}]`, `{"edge":["a","b"]}`, `{"edge":["a","b"]},{"edge":["b","c"]}`).Replace(runCfg)
	swapped := strings.Replace(runCfg, `{"name":"a","block":"inc"},{"name":"b","block":"dbl"}`, `{"name":"b","block":"dbl"},{"name":"a","block":"inc"}`, 1)
	for _, typed := range []bool{true, false} {
		s := runServer(t, runCfg, src, "r1", typed)
		for name, c := range map[string]struct{ text, change string }{
			"block changed": {strings.Replace(runCfg, `"block":"dbl"`, `"block":"inc"`, 1), "changed node b"},
			"edge added":    {moved, "added edge b -> c"},
			"order":         {swapped, "order of the nodes"},
		} {
			r := askRun(t, s, c.text, nil, false)
			if r.Status != "differs" || !slices.ContainsFunc(r.Changes, func(l string) bool { return strings.Contains(l, c.change) }) {
				t.Errorf("%s typed=%v: %s changes %q", name, typed, r.Status, r.Changes)
			}
			for _, want := range []string{"differs from the topology run r1 journaled", "will not resume under this file"} {
				if !strings.Contains(r.Claim, want) {
					t.Errorf("%s: claim %q lacks %q", name, r.Claim, want)
				}
			}
			if strings.Contains(r.Claim, "0 changes") {
				t.Errorf("%s: a difference read as 0 changes: %q", name, r.Claim)
			}
		}
	}
}

// A rename made in this editing session is matched to the run's node, so it is not a removal and an
// addition; the digest still differs (names are in it), and the claim says so, never "no difference".
func TestRunRenamesOfTheSession(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	src := runview.JournalSource{J: j}
	renamed := strings.NewReplacer(`"name":"b"`, `"name":"c"`, `["a","b"]`, `["a","c"]`).Replace(runCfg)
	for _, typed := range []bool{true, false} {
		s := runServer(t, runCfg, src, "r1", typed)
		r := askRun(t, s, renamed, []Rename{{From: "b", To: "c"}}, false)
		if r.Status != "differs" || !strings.Contains(r.Claim, "the renames made in this session") || !slices.Equal(r.Renamed, []string{"b -> c"}) {
			t.Errorf("typed=%v with the rename: %s %q renamed %q", typed, r.Status, r.Claim, r.Renamed)
		}
		for _, l := range r.Changes {
			if strings.Contains(l, "removed node") || strings.Contains(l, "added node") {
				t.Errorf("typed=%v: the rename read as %q", typed, l)
			}
		}
		// Without the rename list the same text is a removed node and an added one.
		r = askRun(t, s, renamed, nil, false)
		if r.Status != "differs" || len(r.Renamed) != 0 {
			t.Errorf("typed=%v without it: %s renamed %q", typed, r.Status, r.Renamed)
		}
	}
}

// A typed file with errors has no digest, so nothing is claimed.
func TestRunTypedFileWithErrorsCannotCompare(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	s := runServer(t, runCfg, runview.JournalSource{J: j}, "r1", true)
	r := askRun(t, s, strings.Replace(runCfg, `"block":"dbl"`, `"block":"nope"`, 1), nil, false)
	if r.Status != "cannot" || !strings.Contains(r.Claim, "the file has errors") || len(r.Changes) != 0 || r.FileDigest != "" {
		t.Errorf("%+v", r)
	}
	for _, bad := range []string{"differs", "refused", "same digest"} {
		if strings.Contains(r.Claim, bad) {
			t.Errorf("claim %q says %q", r.Claim, bad)
		}
	}
	r = askRun(t, s, "{", nil, false)
	if r.Status != "cannot" {
		t.Errorf("unparsable text: %+v", r)
	}
}

// The journal is data someone else wrote. Each way it can be wrong reads as a state in words, no topology is
// drawn from it, and nothing of a record reaches the reply.
func TestRunHostileJournals(t *testing.T) {
	j, digest := ranRun(t, runCfg)
	recs, err := j.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	mod := func(f func(r agent.Record) (agent.Record, bool)) runview.Exports {
		var out []agent.Record
		for _, r := range recs {
			if n, keep := f(r); keep {
				out = append(out, n)
			}
		}
		return runview.Exports{"r1": out}
	}
	str := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	set := func(name string, res json.RawMessage) runview.Exports {
		return mod(func(r agent.Record) (agent.Record, bool) {
			if r.Name == name {
				r.Result = res
			}
			return r, true
		})
	}
	drop := func(name string) runview.Exports {
		return mod(func(r agent.Record) (agent.Record, bool) { return r, r.Name != name })
	}
	var topo plan.Topology
	for _, r := range recs {
		if r.Name == plan.TopologyStep {
			_ = json.Unmarshal(r.Result, &topo)
		}
	}
	bidi := topo
	bidi.Nodes = slices.Clone(topo.Nodes)
	bidi.Nodes[0].Name = "a‮\x00"
	big := topo
	big.Nodes = slices.Clone(topo.Nodes)
	big.Nodes[0].Name = strings.Repeat("n", MaxFileSize)
	for name, c := range map[string]struct {
		src    runview.Source
		status string
		want   string
	}{
		"no topology":          {drop(plan.TopologyStep), "no_topology", "journaled no topology"},
		"topology, no digest":  {drop("flow:digest"), "no_digest", "has not recorded its digest yet"},
		"digest, not a string": {set("flow:digest", str(7)), "no_digest", "has not recorded its digest yet"},
		"digest not hex":       {set("flow:digest", str(strings.Repeat("z", 64))), "mismatch", "not a SHA-256 digest"},
		"digest too long":      {set("flow:digest", str(digest+"00")), "mismatch", "not a SHA-256 digest"},
		"digest of another":    {set("flow:digest", str(strings.Repeat("0", 64))), "mismatch", "does not match its digest"},
		"topology tampered":    {set(plan.TopologyStep, str(bidi)), "mismatch", "does not match its digest"},
		"topology not JSON":    {set(plan.TopologyStep, json.RawMessage(`"x"`)), "mismatch", "does not match its digest"},
		"oversized topology":   {set(plan.TopologyStep, str(big)), "too_large", "more than"},
		"no such run":          {runview.Exports{}, "not_found", "has no records"},
	} {
		s := runServer(t, runCfg, c.src, "r1", true)
		r := askRun(t, s, runCfg, nil, false)
		if r.Status != c.status || !strings.Contains(r.Claim, c.want) || len(r.Changes) != 0 || r.FileDigest != "" {
			t.Errorf("%s: %s %q", name, r.Status, r.Claim)
		}
	}
	// A run started as one flow whose topology names another: compared, with a note.
	other := topo
	other.Flow = "elsewhere"
	d2, err := plan.TopologyDigest(other)
	if err != nil {
		t.Fatal(err)
	}
	src := set(plan.TopologyStep, str(other))
	src["r1"] = slices.Clone(src["r1"])
	for i := range src["r1"] {
		if src["r1"][i].Name == "flow:digest" {
			src["r1"][i].Result = str(d2)
		}
	}
	r := askRun(t, runServer(t, runCfg, src, "r1", true), runCfg, nil, false)
	if r.Status != "differs" || !slices.ContainsFunc(r.Notes, func(n string) bool {
		return strings.Contains(n, "started the flow pay but the topology it journaled names the flow elsewhere")
	}) {
		t.Errorf("flow mismatch: %s %q", r.Status, r.Notes)
	}
}

// A reserved run ID is not read, and a store that fails is named by its class, not its text (which may hold a
// path, a DSN or a record).
func TestRunNeverEchoesTheStore(t *testing.T) {
	const secret = "postgres://admin:hunter2@db.internal/orders"
	s := runServer(t, runCfg, failingSource{errors.New("dial " + secret)}, "r1", true)
	r := askRun(t, s, runCfg, nil, false)
	b, _ := json.Marshal(r)
	if r.Status != "unreadable" || strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "db.internal") {
		t.Errorf("%s\n%s", r.Status, b)
	}
	j, _ := ranRun(t, runCfg)
	r = askRun(t, runServer(t, runCfg, runview.JournalSource{J: j}, agent.StopControlRun, true), runCfg, nil, false)
	if r.Status != "unreadable" || !strings.Contains(r.Claim, "reserved") {
		t.Errorf("reserved run: %s %q", r.Status, r.Claim)
	}
}

type failingSource struct{ err error }

func (f failingSource) Runs(context.Context) ([]string, error) { return nil, f.err }
func (f failingSource) History(context.Context, string) ([]agent.Record, error) {
	return nil, f.err
}

// No payload of the run reaches a reply: the run's input and results hold a recognisable secret, and
// no endpoint's answer contains it.
func TestRunRepliesHoldNoPayload(t *testing.T) {
	const secret = "SECRET-PAYLOAD-8f3a"
	flow, err := plan.Load[string, string]([]byte(`{"version":1,"flow":"echo","in":"string","out":"string","entry":"e","nodes":[{"name":"e","block":"echo"}],"wiring":[]}`), func() *plan.Registry {
		reg := plan.NewRegistry()
		if err := reg.RegisterStep("echo", func(_ context.Context, s string) (string, error) { return s + "!", nil }); err != nil {
			t.Fatal(err)
		}
		return reg
	}())
	if err != nil {
		t.Fatal(err)
	}
	j, _ := agent.NewJournal(agent.NewMemStore())
	if _, err := flow.Run(context.Background(), j, "r1", secret); err != nil {
		t.Fatal(err)
	}
	cfg := `{"version":1,"flow":"echo","in":"string","out":"string","entry":"e","nodes":[{"name":"e","block":"echo"}],"wiring":[]}`
	p := filepath.Join(t.TempDir(), "flow.json")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(t, p, editrun.NewOption(editrun.Run{ID: "r1", Source: runview.JournalSource{J: j}}).(Option))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{cfg, strings.Replace(cfg, `"echo","block"`, `"x","block"`, 1), "{"} {
		body, _ := json.Marshal(map[string]any{"text": text})
		if out := call(s.Handler(), s, "/api/run", string(body), nil).Body.String(); strings.Contains(out, secret) {
			t.Errorf("the reply holds the run's payload: %s", out)
		}
	}
}

// The run is read once and again only on request, so a baseline does not move under an edit.
func TestRunSnapshotIsReadOnceUntilReload(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	cs := &countingSource{Source: runview.JournalSource{J: j}}
	s := runServer(t, runCfg, cs, "r1", true)
	for i := 0; i < 3; i++ {
		askRun(t, s, runCfg, nil, false)
	}
	if n := cs.n.Load(); n != 1 {
		t.Errorf("3 requests read the run %d times, want 1", n)
	}
	askRun(t, s, runCfg, nil, true)
	if n := cs.n.Load(); n != 2 {
		t.Errorf("a reload: %d reads, want 2", n)
	}
}

type countingSource struct {
	runview.Source
	n atomic.Int32
}

func (c *countingSource) History(ctx context.Context, id string) ([]agent.Record, error) {
	c.n.Add(1)
	return c.Source.History(ctx, id)
}

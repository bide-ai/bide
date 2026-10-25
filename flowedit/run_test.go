package flowedit

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/editrun"
	"github.com/bide-ai/bide/internal/printable"
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
	if r.Run.Flow != "pay" || r.Run.Digest != digest || r.Run.ReadAt == "" || r.Run.State != "done" {
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

// A file of more nodes than the comparison reads is refused in words, not compared (the loader bounds a
// config by bytes only, and the diff costs more than linear time).
func TestRunRefusesAFileOfTooManyNodes(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	s := runServer(t, runCfg, runview.JournalSource{J: j}, "r1", false)
	var b strings.Builder
	b.WriteString(`{"version":1,"flow":"pay","entry":"n0","nodes":[`)
	for i := 0; i <= runview.MaxTopologyNodes; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"n` + strconv.Itoa(i) + `","block":"inc"}`)
	}
	b.WriteString(`],"wiring":[]}`)
	r := askRun(t, s, b.String(), nil, false)
	if r.Status != "too_large" || !strings.Contains(r.Claim, "Cannot compare") || len(r.Changes) != 0 {
		t.Errorf("%s %q", r.Status, r.Claim)
	}
}

// The text is bounded as /api/report bounds it.
func TestRunRefusesOversizedText(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	s := runServer(t, runCfg, runview.JournalSource{J: j}, "r1", true)
	body, _ := json.Marshal(map[string]any{"text": strings.Repeat("x", MaxFileSize+1)})
	if w := call(s.Handler(), s, "/api/run", string(body), nil); w.Code != 413 {
		t.Errorf("%d", w.Code)
	}
}

// With a description file in place of the Registry the claim says the digest rests on the file, as
// every typed result does.
func TestRunClaimNamesTheDescriptionBasis(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	p := filepath.Join(t.TempDir(), "flow.json")
	if err := os.WriteFile(p, []byte(runCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(t, p, WithBlocks(runRegistry(t).Describe()), editrun.NewOption(editrun.Run{ID: "r1", Source: runview.JournalSource{J: j}}).(Option))
	if err != nil {
		t.Fatal(err)
	}
	r := askRun(t, s, runCfg, nil, false)
	if r.Status != "same" || !strings.Contains(r.Claim, "rests on the description file (-blocks), not checked against the program") || !strings.Contains(r.Basis, "description file") {
		t.Errorf("%s %q basis %q", r.Status, r.Claim, r.Basis)
	}
}

// flakySource fails its first read, as a store does when a client leaves, a deadline passes or a connection drops.
type flakySource struct {
	runview.Source
	calls atomic.Int32
}

func (f *flakySource) History(ctx context.Context, id string) ([]agent.Record, error) {
	if f.calls.Add(1) == 1 {
		return nil, errors.New("connection reset")
	}
	return f.Source.History(ctx, id)
}

// A read that failed is not kept: the next request reads the run again. Otherwise one dropped connection
// would leave the page saying the run cannot be read until Reload.
func TestRunFailedReadIsNotKept(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	s := runServer(t, runCfg, &flakySource{Source: runview.JournalSource{J: j}}, "r1", true)
	if r := askRun(t, s, runCfg, nil, false); r.Status != "unreadable" || !strings.Contains(r.Claim, "read error") {
		t.Fatalf("first: %s %q", r.Status, r.Claim)
	}
	if r := askRun(t, s, runCfg, nil, false); r.Status != "same" {
		t.Errorf("second: %s %q: the failed read was kept", r.Status, r.Claim)
	}
	// A definitive answer is kept: the run is read once more only on request.
	cs := &countingSource{Source: runview.Exports{}}
	s = runServer(t, runCfg, cs, "r1", true)
	askRun(t, s, runCfg, nil, false)
	askRun(t, s, runCfg, nil, false)
	if n := cs.n.Load(); n != 1 {
		t.Errorf("a run with no records was read %d times, want 1 (not_found is definitive)", n)
	}
}

// blockingSource holds its read until released or cancelled: a store that hangs.
type blockingSource struct {
	runview.Source
	release chan struct{}
}

func (b *blockingSource) History(ctx context.Context, id string) ([]agent.Record, error) {
	select {
	case <-b.release:
		return b.Source.History(ctx, id)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// A hung store does not hold a request past its own context, and a request whose client left does not
// fail the read for the next one.
func TestRunHungStoreDoesNotHoldRequests(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	b := &blockingSource{Source: runview.JournalSource{J: j}, release: make(chan struct{})}
	s := runServer(t, runCfg, b, "r1", true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := make(chan *runReply, 1)
	go func() { got <- s.compareRun(ctx, []byte(runCfg), nil, false) }()
	select {
	case r := <-got:
		if r.Status != "unreadable" || !strings.Contains(r.Claim, "timed out") {
			t.Fatalf("a request past its deadline: %s %q", r.Status, r.Claim)
		}
	case <-time.After(5 * time.Second):
		close(b.release)
		t.Fatalf("a request past its deadline was still waiting for the store after %v", time.Since(start))
	}
	close(b.release)
	if r := askRun(t, s, runCfg, nil, false); r.Status != "same" {
		t.Errorf("after the store answered: %s %q", r.Status, r.Claim)
	}
}

// With the renames of the session the run's names are the file's, but the digests are still compared as
// they are: a type difference is not hidden behind "only the renames".
func TestRunRenameDoesNotHideATypeDifference(t *testing.T) {
	j, digest := ranRun(t, runCfg)
	recs, err := j.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	var topo plan.Topology
	for _, r := range recs {
		if r.Name == plan.TopologyStep {
			_ = json.Unmarshal(r.Result, &topo)
		}
	}
	renamed := strings.NewReplacer(`"name":"b"`, `"name":"c"`, `["a","b"]`, `["a","c"]`).Replace(runCfg)
	asked := func(src runview.Source) *runReply {
		return askRun(t, runServer(t, runCfg, src, "r1", true), renamed, []Rename{{From: "b", To: "c"}}, false)
	}
	// Only the rename: the renamed run topology has the file's digest.
	if r := asked(runview.JournalSource{J: j}); r.Status != "differs" || !strings.Contains(r.Claim, "only the renames made in this session") {
		t.Errorf("only a rename: %s %q", r.Status, r.Claim)
	}
	// The rename and a type spelled with another package path in the run: the digest differs for more.
	t0 := topo
	t0.Nodes = slices.Clone(topo.Nodes)
	t0.Nodes[1].InCanonical = "example.com/other." + t0.Nodes[1].InCanonical
	d0, err := plan.TopologyDigest(t0)
	if err != nil || d0 == digest {
		t.Fatalf("setup: %v %q", err, d0)
	}
	b, _ := json.Marshal(t0)
	db, _ := json.Marshal(d0)
	forged := runview.Exports{"r1": slices.Clone(recs)}
	for i := range forged["r1"] {
		switch forged["r1"][i].Name {
		case plan.TopologyStep:
			forged["r1"][i].Result = b
		case "flow:digest":
			forged["r1"][i].Result = db
		}
	}
	r := asked(forged)
	if r.Status != "differs" || !strings.Contains(r.Claim, "the renames made in this session, and the digest differs in order or types as well") || strings.Contains(r.Claim, "only the renames") {
		t.Errorf("a rename and a type: %s %q", r.Status, r.Claim)
	}
}

// A journal's names reach the reply written out: a topology that passes the digest check but names a
// node with a direction override, a NUL and an escape sequence shows none of them raw in any field.
func TestRunConsistentHostileTopologyIsWrittenOut(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	recs, err := j.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	var topo plan.Topology
	for _, r := range recs {
		if r.Name == plan.TopologyStep {
			_ = json.Unmarshal(r.Result, &topo)
		}
	}
	raw, _ := json.Marshal(topo)
	evil, _ := json.Marshal("b‮evil\x00\x1b[31m")
	hostile := strings.ReplaceAll(string(raw), `"b"`, string(evil))
	var ht plan.Topology
	if err := json.Unmarshal([]byte(hostile), &ht); err != nil {
		t.Fatal(err)
	}
	ht.Flow = "pay\u202e\x00"
	hostile2, _ := json.Marshal(ht)
	d, err := plan.TopologyDigest(ht)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := json.Marshal(d)
	const evilID = "r\u202e1\x1b"
	forged := runview.Exports{evilID: slices.Clone(recs)}
	for i := range forged[evilID] {
		switch forged[evilID][i].Name {
		case plan.TopologyStep:
			forged[evilID][i].Result = hostile2
		case "flow:digest":
			forged[evilID][i].Result = db
		case "run:start":
			var st agent.RunStart
			if err := json.Unmarshal(forged[evilID][i].Result, &st); err != nil || st.Flow == nil {
				t.Fatalf("run:start: %v", err)
			}
			st.Flow.Name = "start\u202eflow"
			forged[evilID][i].Result, _ = json.Marshal(st)
		}
	}
	for _, typed := range []bool{true, false} {
		s := runServer(t, runCfg, forged, evilID, typed)
		r := askRun(t, s, runCfg, nil, false)
		if r.Status != "differs" || len(r.Changes) == 0 {
			t.Fatalf("typed=%v: the hostile topology was not compared: %s %q", typed, r.Status, r.Claim)
		}
		b, _ := json.Marshal(r)
		var m any
		_ = json.Unmarshal(b, &m)
		var walk func(string, any)
		walk = func(path string, v any) {
			switch x := v.(type) {
			case string:
				if c, bad := printable.Bad(strings.ReplaceAll(strings.ReplaceAll(x, "\n", ""), "\t", "")); bad {
					t.Errorf("typed=%v: %s holds %U raw: %q", typed, path, c, x)
				}
			case []any:
				for i, e := range x {
					walk(path+"[]"+strconv.Itoa(i), e)
				}
			case map[string]any:
				for k, e := range x {
					walk(path+"."+k, e)
				}
			}
		}
		walk("reply", m)
		for name, field := range map[string]string{"changes": strings.Join(r.Changes, "\n"), "claim": r.Claim, "run id": r.Run.ID, "flow": r.Run.Flow, "notes": strings.Join(r.Notes, "\n")} {
			if !strings.Contains(field, `\u{202E}`) {
				t.Errorf("typed=%v: the override is not written out in %s: %q", typed, name, field)
			}
		}
	}
}

// countingStore counts the entries a reader pulls from a store and the point reads it makes.
type countingStore struct {
	agent.Store
	yielded, gets atomic.Int32
}

func (c *countingStore) Get(ctx context.Context, run, name string) (agent.Entry, bool, error) {
	c.gets.Add(1)
	return c.Store.Get(ctx, run, name)
}

func (c *countingStore) Load(ctx context.Context, run string, after int64) iter.Seq2[agent.Entry, error] {
	return func(yield func(agent.Entry, error) bool) {
		for e, err := range c.Store.Load(ctx, run, after) {
			c.yielded.Add(1)
			if !yield(e, err) {
				return
			}
		}
	}
}

// The panel needs the run's topology, its digest and whether it has ended, so it reads those by point reads:
// a run of a hundred records is not streamed, not held, and no View is built of it. The end of the run is
// read from its end markers, and a run without one is said to have none.
func TestRunIsReadByPointReadsNotByHistory(t *testing.T) {
	flow, err := plan.Load[int, int]([]byte(runCfg), runRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	cs := &countingStore{Store: agent.NewMemStore()}
	j, err := agent.NewJournal(cs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Run(context.Background(), j, "r1", 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, _, err := cs.Store.Insert(context.Background(), "r1", "extra:"+strconv.Itoa(i), []byte(`{"name":"extra:`+strconv.Itoa(i)+`","kind":"value","result":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	cs.yielded.Store(0)
	cs.gets.Store(0)
	s := runServer(t, runCfg, runview.JournalSource{J: j}, "r1", true)
	r := askRun(t, s, runCfg, nil, false)
	if r.Status != "same" || r.Run.State != "done" || r.Run.StateText != "completed" || r.Run.Flow != "pay" || r.Run.Records != 0 {
		t.Errorf("%s run %+v", r.Status, r.Run)
	}
	if y, g := cs.yielded.Load(), cs.gets.Load(); y > 1 || g > 3 {
		t.Errorf("reading a run of more than 100 records pulled %d entries (want 1, the first) and made %d point reads (want 3: topology, digest, the end marker)", y, g)
	}
	// A run with no end marker.
	open, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	for e, err := range cs.Store.Load(context.Background(), "r1", -1) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Name == "run:complete" || strings.HasPrefix(e.Name, "extra:") {
			continue
		}
		if _, _, err := open.Store().Insert(context.Background(), "r1", e.Name, e.Data); err != nil {
			t.Fatal(err)
		}
	}
	r = askRun(t, runServer(t, runCfg, runview.JournalSource{J: open}, "r1", true), runCfg, nil, false)
	if r.Status != "same" || r.Run.State != "unfinished" || r.Run.StateText != "no end marker" {
		t.Errorf("no end marker: %s run %+v", r.Status, r.Run)
	}
	// An aborted and a cancelled run, by their end markers.
	for marker, want := range map[string][2]string{"run:aborted": {"aborted", "aborted"}, "run:cancelled": {"cancelled", "cancelled"}} {
		ended, err := agent.NewJournal(agent.NewMemStore())
		if err != nil {
			t.Fatal(err)
		}
		for e, err := range open.Store().Load(context.Background(), "r1", -1) {
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := ended.Store().Insert(context.Background(), "r1", e.Name, e.Data); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := ended.Store().Insert(context.Background(), "r1", marker, []byte(`{"name":"`+marker+`","kind":"value","result":1}`)); err != nil {
			t.Fatal(err)
		}
		r := askRun(t, runServer(t, runCfg, runview.JournalSource{J: ended}, "r1", true), runCfg, nil, false)
		if r.Run.State != want[0] || r.Run.StateText != want[1] {
			t.Errorf("%s: run %+v", marker, r.Run)
		}
	}
	// A run with no records.
	if r := askRun(t, runServer(t, runCfg, runview.JournalSource{J: open}, "nope", true), runCfg, nil, false); r.Status != "not_found" {
		t.Errorf("no such run: %s", r.Status)
	}
}

// A file with errors is compared on what reads, and the panel says it has errors, so a structural "no
// difference seen" is not read as a clean bill.
func TestRunFileWithErrorsIsSaidSo(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	s := runServer(t, runCfg, runview.JournalSource{J: j}, "r1", false)
	broken := strings.Replace(runCfg, `"wiring":[{"edge":["a","b"]}]`, `"wiring":[{"edge":["a","b"]},{"edge":["b","nowhere"]}]`, 1)
	r := askRun(t, s, broken, nil, false)
	if !slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "The file has 1 error (see Problems)") }) {
		t.Errorf("%s %q notes %q", r.Status, r.Claim, r.Notes)
	}
	if r := askRun(t, s, runCfg, nil, false); slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "The file has") }) {
		t.Errorf("a clean file: notes %q", r.Notes)
	}
}

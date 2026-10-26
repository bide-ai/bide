package flowedit

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/editrun"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
)

// flowCfg is a two-node flow of the given name and second block; flows of different names (or blocks) have
// different digests.
func flowCfg(name, block string) string {
	return `{"version":1,"flow":"` + name + `","in":"int","out":"int","entry":"a","nodes":[{"name":"a","block":"inc"},{"name":"b","block":"` + block + `"}],"wiring":[{"edge":["a","b"]}]}`
}

// regFor registers the blocks cfg uses and no others (Load refuses a block nothing uses).
func regFor(t *testing.T, cfg string) *plan.Registry {
	t.Helper()
	reg := plan.NewRegistry()
	for name, f := range map[string]func(context.Context, int) (int, error){
		"inc": func(_ context.Context, n int) (int, error) { return n + 1, nil },
		"dbl": func(_ context.Context, n int) (int, error) { return n * 2, nil },
	} {
		if strings.Contains(cfg, `"block":"`+name+`"`) {
			if err := reg.RegisterStep(name, f); err != nil {
				t.Fatal(err)
			}
		}
	}
	return reg
}

// templateRun runs cfg once and returns its journal, to be cloned into a store as many runs as a test needs.
func templateRun(t *testing.T, cfg string) *agent.Journal {
	t.Helper()
	flow, err := plan.Load[int, int]([]byte(cfg), regFor(t, cfg))
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
	return j
}

// cloneRun writes the entries of template's run r1 into st under id, leaving out those drop names; an entry can
// be replaced by with.
func cloneRun(t *testing.T, template *agent.Journal, st agent.Store, id string, drop func(name string) bool, with map[string][]byte) {
	t.Helper()
	for e, err := range template.Store().Load(context.Background(), "r1", -1) {
		if err != nil {
			t.Fatal(err)
		}
		if drop != nil && drop(e.Name) {
			continue
		}
		data := e.Data
		if w, ok := with[e.Name]; ok {
			data = w
		}
		if _, _, err := st.Insert(context.Background(), id, e.Name, data); err != nil {
			t.Fatal(err)
		}
	}
}

func unfinished(name string) bool { return name == "run:complete" }

// storeServer is an editor over file with a store and no run (the list of unfinished runs only).
func storeServer(t *testing.T, file string, st agent.Store, typed bool) (*Server, *agent.Journal) {
	t.Helper()
	j, err := agent.NewJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "flow.json")
	if err := os.WriteFile(p, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := []Option{editrun.NewOption(editrun.Run{Source: runview.JournalSource{J: j}}).(Option)}
	if typed {
		opts = append(opts, WithRegistry(runRegistry(t)))
	}
	s, err := newServer(t, p, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s, j
}

func askAffected(t *testing.T, s *Server, text string, check, reload bool) *affectedReply {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"text": text, "check": check, "reload": reload})
	w := call(s.Handler(), s, "/api/affected", string(body), nil)
	if w.Code != 200 {
		t.Fatalf("/api/affected: %d %s", w.Code, w.Body)
	}
	var r affectedReply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func groupOf(r *affectedReply, digest string) *affectedGroup {
	for i := range r.Groups {
		if r.Groups[i].Digest == digest {
			return &r.Groups[i]
		}
	}
	return nil
}

func digestOf(t *testing.T, cfg string) string {
	t.Helper()
	flow, err := plan.Load[int, int]([]byte(cfg), regFor(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	return flow.Digest()
}

// The list of unfinished runs a file would refuse: the runs of the file's flow under another digest are refused,
// those under the file's digest are not, runs of another flow are counted apart, and finished, cancelled, aborted
// and reserved runs are not asked about.
func TestAffectedListsWhatTheFileWouldRefuse(t *testing.T) {
	v1, v2, other := flowCfg("pay", "dbl"), flowCfg("pay", "inc"), flowCfg("elsewhere", "dbl")
	tv1, tv2, tother := templateRun(t, v1), templateRun(t, v2), templateRun(t, other)
	st := agent.NewMemStore()
	for i := 0; i < 3; i++ {
		cloneRun(t, tv1, st, "same-"+strconv.Itoa(i), unfinished, nil)
	}
	for i := 0; i < 2; i++ {
		cloneRun(t, tv2, st, "zz-changed-"+strconv.Itoa(i), unfinished, nil)
	}
	cloneRun(t, tother, st, "foreign-0", unfinished, nil)
	cloneRun(t, tv2, st, "done", nil, nil) // finished: run:complete
	for _, marker := range []string{"run:aborted", "run:cancelled"} {
		id := strings.TrimPrefix(marker, "run:")
		cloneRun(t, tv2, st, id, unfinished, nil)
		if _, _, err := st.Insert(context.Background(), id, marker, []byte(`{"name":"`+marker+`","kind":"value","result":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	cloneRun(t, tv2, st, agent.StopControlRun, unfinished, nil) // reserved: not a run
	s, _ := storeServer(t, v1, st, true)
	r := askAffected(t, s, v1, true, false)
	if r.Status != "ok" || r.Listed != 6 || r.Refused != 2 || r.OtherFlow != 1 || r.More {
		t.Fatalf("%+v", r)
	}
	if g := groupOf(r, digestOf(t, v1)); g == nil || g.Status != "same" || g.Runs != 3 {
		t.Errorf("same: %+v", g)
	}
	if g := groupOf(r, digestOf(t, v2)); g == nil || g.Status != "refused" || g.Runs != 2 || !slices.Equal(g.IDs, []string{"zz-changed-0", "zz-changed-1"}) {
		t.Errorf("refused: %+v", g)
	}
	if g := groupOf(r, digestOf(t, other)); g == nil || g.Status != "other_flow" {
		t.Errorf("other flow: %+v", g)
	}
	if r.Groups[0].Status != "refused" {
		t.Errorf("refused groups come first: %+v", r.Groups)
	}
	if want := "Unfinished runs that would be refused on their next drive (resume, recovery or restart) with this file: 2 of 6 checked"; r.Heading != want {
		t.Errorf("heading %q", r.Heading)
	}
	notes := strings.Join(r.Notes, "\n")
	for _, want := range []string{"runs in other stores are not seen", "keeps the flow it started with", "of another flow"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	for _, bad := range []string{"done", "aborted", "cancelled", agent.StopControlRun} {
		for _, g := range r.Groups {
			if slices.Contains(g.IDs, bad) {
				t.Errorf("%s was asked about", bad)
			}
		}
	}
}

// A run with no flow:digest is not refused by the digest rule and is counted apart; a digest that is not a SHA-256
// digest is unreadable; neither makes a group.
func TestAffectedCountsNoDigestAndUnreadableApart(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	tv1 := templateRun(t, v1)
	st := agent.NewMemStore()
	cloneRun(t, tv1, st, "ok", unfinished, nil)
	for i := 0; i < 3; i++ {
		cloneRun(t, tv1, st, "nodigest-"+strconv.Itoa(i), func(n string) bool { return unfinished(n) || n == "flow:digest" }, nil)
	}
	cloneRun(t, tv1, st, "crashwindow", func(n string) bool { return unfinished(n) || n == "flow:digest" }, nil) // topology but no digest
	// A run that is not a flow run at all: no digest, no topology, no flow in its start.
	cloneRun(t, tv1, st, "agentrun", func(n string) bool {
		return unfinished(n) || n == "flow:digest" || n == plan.TopologyStep
	}, nil)
	cloneRun(t, tv1, st, "garbled", unfinished, map[string][]byte{"flow:digest": []byte(`{"name":"flow:digest","kind":"value","result":"not-a-digest"}`)})
	s, _ := storeServer(t, v1, st, true)
	r := askAffected(t, s, v1, true, false)
	if r.NoDigest != 5 || r.Unreadable != 1 || r.Refused != 0 || len(r.Groups) != 1 || r.Groups[0].Runs != 1 {
		t.Fatalf("%+v", r)
	}
	want := `5 unfinished runs have no flow:digest. 4 of them wrote a flow:topology, so they are flow runs that have not recorded their digest yet (4 of the flow "pay"); resume records the digest of whichever flow resumes them, so they are not counted. 1 of them wrote no flow:topology: runs that are not flow runs, or flow runs that stopped before writing it; they are not attributed to a flow. None of these is counted.`
	if !slices.Contains(r.Notes, want) {
		t.Errorf("the note is not the split of the runs with no digest:\n%q\nwant\n%q", r.Notes, want)
	}
}

// The runs with no digest are told apart by one read of their topology each, for the first maxNoDigestReads only: 500 of them
// cost 500 digest reads and maxNoDigestReads topology reads, however long they are, and the note says the rest were not
// looked at.
func TestAffectedToldApartNoDigestRunsAreBounded(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	tv1 := templateRun(t, v1)
	count := func(extra int) (gets int32, note string) {
		cs := &countStore{Store: agent.NewMemStore()}
		for i := 0; i < 500; i++ {
			drop := func(n string) bool { return unfinished(n) || n == "flow:digest" }
			if i%2 == 1 { // not a flow run: neither a topology
				drop = func(n string) bool { return unfinished(n) || n == "flow:digest" || n == plan.TopologyStep }
			}
			id := fmt.Sprintf("r%04d", i)
			cloneRun(t, tv1, cs.Store, id, drop, nil)
			for j := 0; j < extra; j++ {
				n := "extra:" + strconv.Itoa(j)
				if _, _, err := cs.Store.Insert(context.Background(), id, n, []byte(`{"name":"`+n+`","kind":"value","result":1}`)); err != nil {
					t.Fatal(err)
				}
			}
		}
		s, _ := storeServer(t, v1, cs, true)
		cs.gets.Store(0)
		r := askAffected(t, s, v1, true, false)
		if r.NoDigest != 500 {
			t.Fatalf("%+v", r)
		}
		for _, n := range r.Notes {
			if strings.HasPrefix(n, "500 unfinished runs have no flow:digest.") {
				note = n
			}
		}
		return cs.gets.Load(), note
	}
	short, note := count(0)
	long, _ := count(30)
	if short != 500+maxNoDigestReads || long != short {
		t.Errorf("reads: %d for short runs, %d for long ones; want %d (a digest read each, and a topology read for the first %d)", short, long, 500+maxNoDigestReads, maxNoDigestReads)
	}
	if !strings.Contains(note, "10 of them wrote a flow:topology") || !strings.Contains(note, "10 of them wrote no flow:topology") || !strings.Contains(note, "Only the first 20 were looked at; the other 480 are not told apart.") {
		t.Errorf("note %q", note)
	}
}

// A digest that is not the digest of the run's own topology (a run recorded under digest v1) is refused by a
// resume whatever the file says, so it counts, in both modes, and says why.
func TestAffectedCountsARunRecordedUnderAnotherDigestVersion(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	tv1 := templateRun(t, v1)
	old := strings.Repeat("ab", 32)
	st := agent.NewMemStore()
	cloneRun(t, tv1, st, "old", unfinished, map[string][]byte{"flow:digest": []byte(`{"name":"flow:digest","kind":"value","result":"` + old + `"}`)})
	for _, typed := range []bool{true, false} {
		s, _ := storeServer(t, v1, st, typed)
		r := askAffected(t, s, v1, true, false)
		g := groupOf(r, old)
		if r.Refused != 1 || g == nil || g.Status != "refused" || !strings.Contains(g.Reason, "recorded under digest v1") {
			t.Errorf("typed=%v: %+v", typed, r)
		}
	}
}

// A flow renamed in the editor still lists the runs of the flow it was: they are asked about by the flow's name when the
// file was opened.
func TestAffectedKeysOnTheFlowNameTheFileWasOpenedWith(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	tv1 := templateRun(t, v1)
	st := agent.NewMemStore()
	cloneRun(t, tv1, st, "r", unfinished, nil)
	renamed := flowCfg("pay2", "dbl")
	for _, typed := range []bool{true, false} {
		s, _ := storeServer(t, v1, st, typed)
		r := askAffected(t, s, renamed, true, false)
		if r.Refused != 1 || r.OtherFlow != 0 || len(r.Groups) != 1 || r.Groups[0].Status != "refused" {
			t.Fatalf("typed=%v: %+v", typed, r)
		}
		if want := `this file names the flow "pay2"; these runs started the flow "pay", and a drive of a run under another flow is refused`; !strings.Contains(r.Groups[0].Reason, want) {
			t.Errorf("typed=%v: the reason is %q, want it to say %q", typed, r.Groups[0].Reason, want)
		}
		// Without a rename there is no such reason.
		if g := askAffected(t, s, flowCfg("pay", "inc"), true, false).Groups[0]; strings.Contains(g.Reason, "this file names the flow") {
			t.Errorf("typed=%v: a reason about a rename with none: %q", typed, g.Reason)
		}
	}
}

// A typed file with errors has no digest: nothing is claimed.
func TestAffectedTypedFileWithErrorsCannotCompare(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	st := agent.NewMemStore()
	cloneRun(t, templateRun(t, v1), st, "r", unfinished, nil)
	s, _ := storeServer(t, v1, st, true)
	r := askAffected(t, s, flowCfg("pay", "nope"), true, false)
	if r.Status != "cannot" || !strings.Contains(r.Summary, "Cannot compare: the file has errors (") || r.Refused != 0 || len(r.Groups) != 0 {
		t.Errorf("%+v", r)
	}
	for _, bad := range []string{"refused", "same"} {
		if strings.Contains(r.Summary, bad) {
			t.Errorf("summary says %q", bad)
		}
	}
}

// Structural mode compares each group's topology with the file by what the digest covers.
func TestAffectedStructuralComparesTopologies(t *testing.T) {
	v1, v2 := flowCfg("pay", "dbl"), flowCfg("pay", "inc")
	tv1, tv2 := templateRun(t, v1), templateRun(t, v2)
	st := agent.NewMemStore()
	cloneRun(t, tv1, st, "same", unfinished, nil)
	cloneRun(t, tv2, st, "changed", unfinished, nil)
	cloneRun(t, tv2, st, "notopology", func(n string) bool { return unfinished(n) || n == plan.TopologyStep }, map[string][]byte{"flow:digest": []byte(`{"name":"flow:digest","kind":"value","result":"` + strings.Repeat("cd", 32) + `"}`)})
	s, _ := storeServer(t, v1, st, false)
	r := askAffected(t, s, v1, true, false)
	if r.Refused != 1 {
		t.Fatalf("%+v", r)
	}
	if g := groupOf(r, digestOf(t, v1)); g == nil || g.Status != "none_seen" {
		t.Errorf("same: %+v", g)
	}
	if g := groupOf(r, digestOf(t, v2)); g == nil || g.Status != "refused" || len(g.Changes) == 0 || !strings.Contains(g.Changes[0], "block") {
		t.Errorf("changed: %+v", g)
	}
	if g := groupOf(r, strings.Repeat("cd", 32)); g == nil || g.Status != "not_compared" || g.Reason != "the run recorded no topology" {
		t.Errorf("no topology: %+v", g)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "Kinds, types and gates are not compared") {
		t.Errorf("notes %q", r.Notes)
	}
}

// Only the first maxGroups digests are followed up; the runs under the others are counted.
func TestAffectedBoundsTheGroups(t *testing.T) {
	st := agent.NewMemStore()
	for i := 0; i < maxGroups+5; i++ {
		cloneRun(t, templateRun(t, flowCfg("pay"+strconv.Itoa(i), "dbl")), st, "r"+strconv.Itoa(i), unfinished, nil)
	}
	s, _ := storeServer(t, flowCfg("pay0", "dbl"), st, true)
	r := askAffected(t, s, flowCfg("pay0", "dbl"), true, false)
	if len(r.Groups) != maxGroups || r.Overflow != 5 || !strings.Contains(strings.Join(r.Notes, "\n"), "5 more digests (5 runs) not compared: only the first 20 are.") {
		t.Errorf("%d groups, overflow %d, notes %q", len(r.Groups), r.Overflow, r.Notes)
	}
}

// Past maxListed runs the page says "at least", and that only the first by run ID were read.
func TestAffectedSaysAtLeast(t *testing.T) {
	old := maxListed
	maxListed = 10
	t.Cleanup(func() { maxListed = old })
	v1 := flowCfg("pay", "dbl")
	tv1 := templateRun(t, v1)
	st := agent.NewMemStore()
	for i := 0; i < 25; i++ {
		cloneRun(t, tv1, st, fmt.Sprintf("r%03d", i), unfinished, nil)
	}
	s, _ := storeServer(t, v1, st, true)
	r := askAffected(t, s, v1, true, false)
	if !r.More || r.Listed != 10 || !strings.Contains(r.Heading, "of at least 10 checked") || !strings.Contains(strings.Join(r.Notes, "\n"), "Only the first 10 unfinished runs by run ID were read; there may be more.") {
		t.Errorf("%+v", r)
	}
	if g := r.Groups[0]; !slices.Equal(g.IDs[:3], []string{"r000", "r001", "r002"}) {
		t.Errorf("not the first by run ID: %q", g.IDs)
	}
}

// countStore counts the reads a scan makes of a store.
type countStore struct {
	agent.Store
	gets, yielded, runs atomic.Int32
	delay               time.Duration
}

func (c *countStore) Get(ctx context.Context, run, name string) (agent.Entry, bool, error) {
	c.gets.Add(1)
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	return c.Store.Get(ctx, run, name)
}

func (c *countStore) Load(ctx context.Context, run string, after int64) iter.Seq2[agent.Entry, error] {
	return func(yield func(agent.Entry, error) bool) {
		for e, err := range c.Store.Load(ctx, run, after) {
			c.yielded.Add(1)
			if !yield(e, err) {
				return
			}
		}
	}
}

func (c *countStore) Runs(ctx context.Context, f agent.RunFilter) iter.Seq2[string, error] {
	c.runs.Add(1)
	l, _ := agent.Capability[agent.Lister](c.Store)
	return l.Runs(ctx, f)
}

// A big synthetic store: the scan reads one record per run plus a constant per group, whatever the length of the
// runs, and never loads a history.
func TestAffectedReadsAreBoundedOnABigStore(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	tv1 := templateRun(t, v1)
	count := func(runs, extra int) (gets, yielded int32) {
		cs := &countStore{Store: agent.NewMemStore()}
		for i := 0; i < runs; i++ {
			id := fmt.Sprintf("r%05d", i)
			cloneRun(t, tv1, cs.Store, id, unfinished, nil)
			for j := 0; j < extra; j++ {
				n := "extra:" + strconv.Itoa(j)
				if _, _, err := cs.Store.Insert(context.Background(), id, n, []byte(`{"name":"`+n+`","kind":"value","result":1}`)); err != nil {
					t.Fatal(err)
				}
			}
		}
		s, _ := storeServer(t, v1, cs, true)
		cs.gets.Store(0)
		cs.yielded.Store(0)
		r := askAffected(t, s, v1, true, false)
		if r.Listed != runs || len(r.Groups) != 1 || r.Groups[0].Runs != runs {
			t.Fatalf("%+v", r)
		}
		return cs.gets.Load(), cs.yielded.Load()
	}
	shortGets, shortLoaded := count(2000, 0)
	longGets, longLoaded := count(2000, 30)
	// One flow:digest per run, one run:start and one topology; and the Journal's header check pulls the first entry
	// of each run it reads from, so one entry per run, however long the run.
	if shortGets != 2000+2 || longGets != shortGets || shortLoaded != 2000 || longLoaded != 2000 {
		t.Errorf("reads: %d gets and %d entries pulled for short runs, %d and %d for long ones; want %d gets and %d entries", shortGets, shortLoaded, longGets, longLoaded, 2002, 2000)
	}
}

// Requests that arrive together share one scan, and the scan is kept until the text's reload: a second
// request reads nothing; Reload reads again.
func TestAffectedIsSingleFlightAndCached(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	tv1 := templateRun(t, v1)
	cs := &countStore{Store: agent.NewMemStore(), delay: time.Millisecond}
	for i := 0; i < 50; i++ {
		cloneRun(t, tv1, cs.Store, fmt.Sprintf("r%02d", i), unfinished, nil)
	}
	s, _ := storeServer(t, v1, cs, true)
	cs.gets.Store(0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); askAffected(t, s, v1, true, false) }()
	}
	waitOrFail(t, &wg, 20*time.Second)
	once := cs.gets.Load()
	if cs.runs.Load() != 1 || once != 52 {
		t.Errorf("8 concurrent requests made %d gets and %d listings, want one scan's 52 and 1", once, cs.runs.Load())
	}
	askAffected(t, s, flowCfg("pay", "inc"), true, false) // other text, same scan
	if cs.gets.Load() != once {
		t.Errorf("a second request read the store again: %d gets", cs.gets.Load())
	}
	askAffected(t, s, v1, true, true)
	if cs.gets.Load() != 2*once {
		t.Errorf("reload: %d gets, want %d", cs.gets.Load(), 2*once)
	}
	// Asking whether there is a store reads nothing.
	cs.gets.Store(0)
	if r := askAffected(t, s, v1, false, false); !r.Configured || r.Checked || cs.gets.Load() != 0 {
		t.Errorf("probe: %+v, %d gets", r, cs.gets.Load())
	}
}

// A scan that runs out of time says so and keeps what it read.
func TestAffectedStopsAtItsTimeLimit(t *testing.T) {
	old := scanTimeout
	scanTimeout = 30 * time.Millisecond
	t.Cleanup(func() { scanTimeout = old })
	v1 := flowCfg("pay", "dbl")
	tv1 := templateRun(t, v1)
	cs := &countStore{Store: agent.NewMemStore(), delay: 2 * time.Millisecond}
	for i := 0; i < 200; i++ {
		cloneRun(t, tv1, cs.Store, fmt.Sprintf("r%03d", i), unfinished, nil)
	}
	s, _ := storeServer(t, v1, cs, true)
	r := askAffected(t, s, v1, true, false)
	if r.Listed >= 200 || r.Listed == 0 || !strings.Contains(strings.Join(r.Notes, "\n"), "The scan stopped at the time limit after reading") || !strings.Contains(r.Heading, "of at least") {
		t.Errorf("listed %d heading %q notes %q", r.Listed, r.Heading, r.Notes)
	}
}

// Structural diffs share a time budget; groups past it are listed as not compared.
func TestAffectedStructuralDiffsShareABudget(t *testing.T) {
	old := diffBudget
	diffBudget = -time.Nanosecond
	t.Cleanup(func() { diffBudget = old })
	st := agent.NewMemStore()
	for i := 0; i < 3; i++ {
		cloneRun(t, templateRun(t, flowCfg("pay", []string{"dbl", "inc", "dbl"}[i])), st, "r"+strconv.Itoa(i), unfinished, nil)
	}
	s, _ := storeServer(t, flowCfg("pay", "dbl"), st, false)
	r := askAffected(t, s, flowCfg("pay", "dbl"), true, false)
	for _, g := range r.Groups {
		if g.Status != "not_compared" || g.Reason != "the time budget of the structural comparison was used" {
			t.Errorf("%+v", g)
		}
	}
}

// A run ID is the journal's: shown written out, in a command only when it is safe to paste, quoted for a shell.
func TestAffectedCommandAndIDs(t *testing.T) {
	v1, v2 := flowCfg("pay", "dbl"), flowCfg("pay", "inc")
	st := agent.NewMemStore()
	cloneRun(t, templateRun(t, v2), st, "has space", unfinished, nil)
	cloneRun(t, templateRun(t, flowCfg("pay", "dbl")), st, "evil\u202e;rm", unfinished, nil)
	cloneRun(t, templateRun(t, flowCfg("pay3", "dbl")), st, "quote'd", unfinished, nil) // another digest
	s, _ := storeServer(t, v1, st, false)
	r := askAffected(t, s, v1, true, false)
	var cmds []string
	for _, g := range r.Groups {
		cmds = append(cmds, g.Command)
		if strings.ContainsAny(strings.Join(g.IDs, ""), "\u202e\x00\x1b") {
			t.Errorf("an ID is not written out: %q", g.IDs)
		}
	}
	if !slices.Contains(cmds, "bide edit -run 'has space' <store flags> <file>") {
		t.Errorf("commands %q", cmds)
	}
	for _, c := range cmds {
		if strings.Contains(c, "evil") {
			t.Errorf("a command holds an unsafe ID: %q", c)
		}
	}
}

// hidden is a store that cannot list its runs.
type hidden struct{ agent.Store }

// A store that cannot list says so; an exported journal lists the runs given and leaves out a finished one.
func TestAffectedStoresWithoutAListerAndExports(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	st := agent.NewMemStore()
	cloneRun(t, templateRun(t, v1), st, "r", unfinished, nil)
	s, _ := storeServer(t, v1, hidden{Store: st}, true)
	if r := askAffected(t, s, v1, true, false); r.Status != "nolister" || !strings.Contains(r.Summary, "cannot list its runs") {
		t.Errorf("%+v", r)
	}
	tv1 := templateRun(t, v1)
	recs, err := tv1.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	open := slices.DeleteFunc(slices.Clone(recs), func(r agent.Record) bool { return r.Name == "run:complete" })
	p := filepath.Join(t.TempDir(), "flow.json")
	if err := os.WriteFile(p, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	ex, err := newServer(t, p, editrun.NewOption(editrun.Run{Source: runview.Exports{"open": open, "finished": recs}}).(Option), WithRegistry(runRegistry(t)))
	if err != nil {
		t.Fatal(err)
	}
	r := askAffected(t, ex, v1, true, false)
	if r.Status != "ok" || r.Listed != 1 || len(r.Groups) != 1 || r.Groups[0].Status != "same" || !slices.Equal(r.Groups[0].IDs, []string{"open"}) {
		t.Errorf("%+v", r)
	}
}

// With a store and no run the editor lists unfinished runs and has no run panel; with no store it has neither.
func TestAffectedWithoutARunAndWithoutAStore(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	st := agent.NewMemStore()
	cloneRun(t, templateRun(t, v1), st, "r", unfinished, nil)
	s, _ := storeServer(t, v1, st, true)
	if r := askRun(t, s, v1, nil, false); r.Configured {
		t.Errorf("a store alone has a run panel: %+v", r)
	}
	if r := askAffected(t, s, v1, false, false); !r.Configured {
		t.Errorf("a store alone has no list: %+v", r)
	}
	plain, _ := newTestServer(t, "refund.yaml", testFlow)
	body := call(plain.Handler(), plain, "/api/affected", `{"text":"","check":true}`, nil).Body.String()
	if !strings.Contains(body, `"configured":false`) {
		t.Errorf("no store: %s", body)
	}
}

// Requests that arrive while a scan runs join it: eight, started one after the other while the first is reading, make
// one listing and one scan's reads, not eight.
func TestAffectedRequestsDuringAScanJoinIt(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	tv1 := templateRun(t, v1)
	cs := &countStore{Store: agent.NewMemStore(), delay: 2 * time.Millisecond}
	for i := 0; i < 60; i++ {
		cloneRun(t, tv1, cs.Store, fmt.Sprintf("r%02d", i), unfinished, nil)
	}
	s, _ := storeServer(t, v1, cs, true)
	cs.gets.Store(0)
	var wg sync.WaitGroup
	start := func() {
		wg.Add(1)
		go func() { defer wg.Done(); askAffected(t, s, v1, true, false) }()
	}
	start()
	for cs.gets.Load() < 10 { // the first scan is reading
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 7; i++ {
		start()
		time.Sleep(3 * time.Millisecond)
	}
	waitOrFail(t, &wg, 20*time.Second)
	if cs.runs.Load() != 1 || cs.gets.Load() != 62 {
		t.Errorf("8 requests during a scan made %d listings and %d gets, want 1 and 62", cs.runs.Load(), cs.gets.Load())
	}
}

// ctxStore blocks the listing until its context ends the first time, and says so.
type ctxStore struct {
	agent.Store
	started, cancelled chan struct{}
	n                  atomic.Int32
}

func (c *ctxStore) Runs(ctx context.Context, f agent.RunFilter) iter.Seq2[string, error] {
	if c.n.Add(1) == 1 {
		return func(yield func(string, error) bool) {
			c.started <- struct{}{}
			<-ctx.Done()
			close(c.cancelled)
			yield("", ctx.Err())
		}
	}
	l, _ := agent.Capability[agent.Lister](c.Store)
	return l.Runs(ctx, f)
}

// Reading the store again cancels the scan it replaces, and a request that waited on the replaced scan gets the new
// scan's answer, not "canceled".
func TestAffectedReloadCancelsTheSupersededScan(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	st := agent.NewMemStore()
	cloneRun(t, templateRun(t, v1), st, "r", unfinished, nil)
	cx := &ctxStore{Store: st, started: make(chan struct{}, 1), cancelled: make(chan struct{})}
	s, _ := storeServer(t, v1, cx, true)
	a := make(chan *affectedReply, 1)
	go func() { a <- s.affected(context.Background(), []byte(v1), false) }()
	<-cx.started
	rb := s.affected(context.Background(), []byte(v1), true)
	if rb.Status != "ok" || rb.Listed != 1 {
		t.Fatalf("the reload: %+v", rb)
	}
	select {
	case <-cx.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced scan was not cancelled")
	}
	select {
	case r := <-a:
		if r.Status != "ok" || r.Listed != 1 {
			t.Errorf("the request that waited on the replaced scan: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting request did not finish")
	}
}

// An error from the store never reaches the list as text, whichever read fails (the listing, any record, the
// first entry), and a command to open a run never holds a store's address: it says <store flags>.
func TestAffectedReplyNeverHoldsAStoreError(t *testing.T) {
	v1, v2 := flowCfg("pay", "dbl"), flowCfg("pay", "inc")
	base := agent.NewMemStore()
	cloneRun(t, templateRun(t, v1), base, "a", unfinished, nil)
	cloneRun(t, templateRun(t, v2), base, "b", unfinished, nil)
	cases := map[string]func(op, name string) bool{
		"the listing":  func(op, n string) bool { return op == "runs" },
		"every get":    func(op, n string) bool { return op == "get" },
		"the digest":   func(op, n string) bool { return op == "get" && n == "flow:digest" },
		"run:start":    func(op, n string) bool { return op == "get" && n == "run:start" },
		"the topology": func(op, n string) bool { return op == "get" && n == plan.TopologyStep },
		"first entry":  func(op, n string) bool { return op == "load" },
		"nothing":      func(op, n string) bool { return false },
	}
	for name, fail := range cases {
		for _, typed := range []bool{true, false} {
			s, _ := storeServer(t, v1, poisonStore{Store: base, failOn: fail}, typed)
			for _, reload := range []bool{false, true} {
				body, _ := json.Marshal(map[string]any{"text": v1, "check": true, "reload": reload})
				out := call(s.Handler(), s, "/api/affected", string(body), nil).Body.String()
				for _, w := range leakWords {
					if strings.Contains(out, w) {
						t.Errorf("%s (typed=%v): the reply holds %q: %s", name, typed, w, out)
					}
				}
				var r affectedReply
				if err := json.Unmarshal([]byte(out), &r); err != nil {
					t.Fatal(err)
				}
				for _, g := range r.Groups {
					if g.Command != "" && !strings.HasSuffix(g.Command, " <store flags> <file>") {
						t.Errorf("%s: a command holds more than a placeholder for the store: %q", name, g.Command)
					}
				}
			}
		}
	}
	// A failing listing says so in words and with the class.
	s, _ := storeServer(t, v1, poisonStore{Store: base, failOn: cases["the listing"]}, true)
	if r := askAffected(t, s, v1, true, false); r.Status != "error" || r.Summary != "The runs could not be listed (read error)." {
		t.Errorf("%+v", r)
	}
}

// waitOrFail waits for wg, and fails the test (not by hanging) if it does not finish in d.
func waitOrFail(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("the requests did not finish in %v", d)
	}
}

// A structural file with errors is compared on what reads, and the list says it has errors, as the run panel does.
func TestAffectedStructuralFileWithErrorsIsSaidSo(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	st := agent.NewMemStore()
	cloneRun(t, templateRun(t, v1), st, "r", unfinished, nil)
	s, _ := storeServer(t, v1, st, false)
	broken := strings.Replace(v1, `"wiring":[{"edge":["a","b"]}]`, `"wiring":[{"edge":["a","b"]},{"edge":["b","nowhere"]}]`, 1)
	r := askAffected(t, s, broken, true, false)
	if !slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "The file has 1 error (see Problems)") }) {
		t.Errorf("%+v", r)
	}
	if r := askAffected(t, s, v1, true, false); slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "The file has") }) {
		t.Errorf("a clean file: %q", r.Notes)
	}
}

// The comparison of a text with a scan is made once: the same text asked again, at once or later, is answered from the
// last comparison; another text, or another scan, is compared anew.
func TestAffectedComparesATextOnce(t *testing.T) {
	st := agent.NewMemStore()
	for i := 0; i < 3; i++ {
		cloneRun(t, templateRun(t, flowCfg("pay", []string{"dbl", "inc", "dbl"}[i])), st, "r"+strconv.Itoa(i), unfinished, nil)
	}
	v1 := flowCfg("pay", "dbl")
	s, _ := storeServer(t, v1, st, false)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); askAffected(t, s, v1, true, false) }()
	}
	waitOrFail(t, &wg, 20*time.Second)
	askAffected(t, s, v1, true, false)
	if n := s.affEvals.Load(); n != 1 {
		t.Errorf("9 requests for one text made %d comparisons, want 1", n)
	}
	askAffected(t, s, flowCfg("pay", "inc"), true, false)
	if n := s.affEvals.Load(); n != 2 {
		t.Errorf("another text: %d comparisons, want 2", n)
	}
	askAffected(t, s, flowCfg("pay", "inc"), true, true) // reading the store again is a new scan
	if n := s.affEvals.Load(); n != 3 {
		t.Errorf("a new scan: %d comparisons, want 3", n)
	}
}

// A run with no digest whose topology is larger than the comparison reads, or cannot be read, is not told apart and the
// note says so.
func TestAffectedNoDigestRunWithAnUnreadableTopologyIsNotToldApart(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	tv1 := templateRun(t, v1)
	st := agent.NewMemStore()
	big := `{"name":"` + plan.TopologyStep + `","kind":"value","result":{"flow":"` + strings.Repeat("x", MaxFileSize) + `"}}`
	cloneRun(t, tv1, st, "big", func(n string) bool { return unfinished(n) || n == "flow:digest" }, map[string][]byte{plan.TopologyStep: []byte(big)})
	cloneRun(t, tv1, st, "junk", func(n string) bool { return unfinished(n) || n == "flow:digest" }, map[string][]byte{plan.TopologyStep: []byte(`{"name":"` + plan.TopologyStep + `","kind":"value","result":7}`)})
	s, _ := storeServer(t, v1, st, true)
	r := askAffected(t, s, v1, true, false)
	if !slices.ContainsFunc(r.Notes, func(n string) bool {
		return strings.HasPrefix(n, "2 unfinished runs have no flow:digest.") && strings.Contains(n, "2 could not be told apart (their topology could not be read).") && !strings.Contains(n, "wrote a flow:topology")
	}) {
		t.Errorf("%q", r.Notes)
	}
}

// With exactly one run that wrote a topology and one that wrote none, the note reads in the singular throughout.
func TestAffectedNoDigestNoteInTheSingular(t *testing.T) {
	v1 := flowCfg("pay", "dbl")
	tv1 := templateRun(t, v1)
	st := agent.NewMemStore()
	cloneRun(t, tv1, st, "flowrun", func(n string) bool { return unfinished(n) || n == "flow:digest" }, nil)
	cloneRun(t, tv1, st, "agentrun", func(n string) bool { return unfinished(n) || n == "flow:digest" || n == plan.TopologyStep }, nil)
	s, _ := storeServer(t, v1, st, true)
	r := askAffected(t, s, v1, true, false)
	want := `2 unfinished runs have no flow:digest. 1 of them wrote a flow:topology, so it is a flow run that has not recorded its digest yet (1 of the flow "pay"); resume records the digest of whichever flow resumes it, so it is not counted. 1 of them wrote no flow:topology: runs that are not flow runs, or flow runs that stopped before writing it; they are not attributed to a flow. None of these is counted.`
	if !slices.Contains(r.Notes, want) {
		t.Errorf("%q\nwant\n%q", r.Notes, want)
	}
}

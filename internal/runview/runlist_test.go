package runview

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/runview/runlistdemo"
)

// demoHandler serves the run list over a store holding the demo runs.
func demoHandler(t *testing.T, prefix string) (h http.Handler, j *agent.Journal, runs []runlistdemo.Run) {
	t.Helper()
	s := agent.NewMemStore()
	runs, err := runlistdemo.Write(context.Background(), s, prefix)
	if err != nil {
		t.Fatal(err)
	}
	j, err = agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	return Handler(Options{Source: JournalSource{J: j}}), j, runs
}

func listOf(t *testing.T, h http.Handler, query string) RunList {
	t.Helper()
	code, body := get(t, h, "/runs.json"+query)
	if code != 200 {
		t.Fatalf("GET /runs.json%s = %d: %s", query, code, body)
	}
	var l RunList
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func ids(l RunList) []string {
	var out []string
	for _, r := range l.Rows {
		out = append(out, r.ID)
	}
	return out
}

// TestRunListStates: each demo run is listed in the state group its journal puts it in, with its
// kind, and its state as the run page shows it.
func TestRunListStates(t *testing.T) {
	h, _, runs := demoHandler(t, "")
	l := listOf(t, h, "")
	if l.Err != "" || l.Listed != len(runs) || l.Summarized != len(runs) || len(l.Rows) != len(runs) {
		t.Fatalf("list: err %q, listed %d, read %d, rows %d; want %d runs", l.Err, l.Listed, l.Summarized, len(l.Rows), len(runs))
	}
	text := map[string]string{"completed": "completed", "halted": "halted: outcome unknown", "waiting": "waiting for approval", "in-progress": "in progress"}
	for _, want := range runs {
		i := slices.IndexFunc(l.Rows, func(r RunRow) bool { return r.ID == want.ID })
		if i < 0 {
			t.Fatalf("%s is not listed", want.ID)
		}
		r := l.Rows[i]
		if r.Group != want.Group || r.Kind != want.Kind {
			t.Errorf("%s: group %q kind %q, want %q %q", r.ID, r.Group, r.Kind, want.Group, want.Kind)
		}
		if tx, ok := text[want.Group]; ok && r.StateText != tx {
			t.Errorf("%s: state text %q, want %q", r.ID, r.StateText, tx)
		}
		if r.Records == 0 {
			t.Errorf("%s: no records counted", r.ID)
		}
	}
	// The failed group holds the cancelled run and the aborted saga, each with its own state.
	for id, state := range map[string]string{"refund-1045": "cancelled", "ship-2001": "aborted"} {
		if i := slices.IndexFunc(l.Rows, func(r RunRow) bool { return r.ID == id }); i < 0 || l.Rows[i].State != state || l.Rows[i].StateText != state {
			t.Errorf("%s: want state %q, rows %+v", id, state, l.Rows)
		}
	}
	// Every run shows when it started but the one journaled with no start time, which comes last.
	for i, r := range l.Rows {
		if old := r.ID == "refund-0999"; r.Started.IsZero() != old || old != (i == len(l.Rows)-1) {
			t.Errorf("%s (row %d): started %v", r.ID, i, r.Started)
		}
	}
	want := map[string]int{"completed": 4, "halted": 2, "waiting": 1, "in-progress": 1, "failed": 2}
	for g, n := range want {
		if l.Counts[g] != n {
			t.Errorf("count %s = %d, want %d", g, l.Counts[g], n)
		}
	}
	if l.All != len(runs) {
		t.Errorf("all = %d, want %d", l.All, len(runs))
	}
}

// TestRunListFilters: the state, kind and run ID filters, alone and together.
func TestRunListFilters(t *testing.T) {
	h, _, runs := demoHandler(t, "")
	of := func(pred func(runlistdemo.Run) bool) []string {
		var out []string
		for _, r := range runs {
			if pred(r) {
				out = append(out, r.ID)
			}
		}
		slices.Sort(out)
		return out
	}
	sorted := func(l RunList) []string { s := ids(l); slices.Sort(s); return s }
	for _, g := range []string{"completed", "halted", "waiting", "in-progress", "failed"} {
		if got, want := sorted(listOf(t, h, "?state="+g)), of(func(r runlistdemo.Run) bool { return r.Group == g }); !slices.Equal(got, want) {
			t.Errorf("state=%s: %v, want %v", g, got, want)
		}
	}
	if got, want := sorted(listOf(t, h, "?kind=flow")), of(func(r runlistdemo.Run) bool { return r.Kind == "flow" }); !slices.Equal(got, want) || len(got) != 3 {
		t.Errorf("kind=flow: %v, want %v", got, want)
	}
	if got := sorted(listOf(t, h, "?q=REFUND-104")); len(got) != 5 || !slices.Equal(got, of(func(r runlistdemo.Run) bool { return strings.HasPrefix(r.ID, "refund-104") })) {
		t.Errorf("q=REFUND-104 (any case): %v", got)
	}
	l := listOf(t, h, "?state=halted&kind=flow")
	if got := ids(l); !slices.Equal(got, []string{"triage-3003"}) {
		t.Errorf("halted flows: %v", got)
	}
	// The state counts follow the kind filter, not the state filter.
	if l.Counts["completed"] != 2 || l.Counts["halted"] != 1 || l.All != 3 {
		t.Errorf("counts under kind=flow: %v, all %d", l.Counts, l.All)
	}
	if got := ids(listOf(t, h, "?q=no-such-run")); len(got) != 0 {
		t.Errorf("q=no-such-run: %v", got)
	}
	if code, body := get(t, h, "/?q=no-such-run"); code != 200 || !strings.Contains(body, "No run matches the filter.") {
		t.Errorf("page for no match = %d", code)
	}
	if !slices.Equal(listOf(t, h, "?kind=flow").Kinds, []string{"agent", "flow"}) {
		t.Error("the kind options narrow to the filter")
	}
}

// TestRunListOrder: newest first by the start time run:start records, then the runs that record
// none (an earlier version's, one whose time is not a time, one with no run:start) by run ID.
func TestRunListOrder(t *testing.T) {
	start := func(at string) []agent.Record {
		res := `{"input":"go","kind":"agent"}`
		if at != "" {
			res = `{"input":"go","kind":"agent","started_at":"` + at + `"}`
		}
		return []agent.Record{{Name: "run:start", Kind: agent.StepValue, Result: json.RawMessage(res)}}
	}
	x := Exports{
		"old":      start("2026-10-01T09:00:00.000Z"),
		"new":      start("2026-10-12T23:59:59.999Z"),
		"mid":      start("2026-10-12T08:00:00.500+02:00"),
		"c-legacy": start(""),
		"b-junk":   start("yesterday"),
		"a-none":   {{Name: "x", Kind: agent.StepValue}},
	}
	l := Options{Source: x}.runList(context.Background(), ListFilter{Limit: listPage})
	if got, want := ids(l), []string{"new", "mid", "old", "a-none", "b-junk", "c-legacy"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if got := l.Rows[1]; got.StartedText() != "2026-10-12 06:00:00 UTC" {
		t.Fatalf("mid started %q, want its time in UTC", got.StartedText())
	}
	for _, r := range l.Rows[3:] {
		if !r.Started.IsZero() || r.StartedText() != "" {
			t.Fatalf("%s: started %v", r.ID, r.Started)
		}
	}
	// A journal with records and no run:start is a run not started.
	if r := l.Rows[3]; r.Group != "not-started" || r.StateText != "not started" || r.Kind != "" || r.KindText() != "unknown" {
		t.Fatalf("a-none = %+v", r)
	}
	if code, body := get(t, Handler(Options{Source: x}), "/"); code != 200 || !strings.Contains(body, "2026-10-12 23:59:59 UTC") || strings.Count(body, `title="journaled by a version that recorded no start time">none<`) != 3 {
		t.Fatalf("page = %d; want the start times, and none for the three runs that record none", code)
	}
}

// TestRunListMore: the list shows Limit rows and links to a page with more, the filters kept.
func TestRunListMore(t *testing.T) {
	x := Exports{}
	for i := range listPage + 7 {
		x["run-"+strconv.Itoa(1000+i)] = []agent.Record{{Name: "x", Kind: agent.StepValue}}
	}
	x["other"] = []agent.Record{{Name: "x", Kind: agent.StepValue}}
	h := Handler(Options{Source: x})
	l := listOf(t, h, "?q=run-")
	if len(l.Rows) != listPage || l.Matched != listPage+7 || l.More == "" {
		t.Fatalf("rows %d, matched %d, more %q", len(l.Rows), l.Matched, l.More)
	}
	u, err := url.Parse(l.More)
	if err != nil || u.Path != "/" || u.Query().Get("q") != "run-" || u.Query().Get("limit") != strconv.Itoa(2*listPage) {
		t.Fatalf("more link %q", l.More)
	}
	code, body := get(t, h, l.More)
	if code != 200 || !strings.Contains(body, "run-1056") || strings.Contains(body, "Show more") {
		t.Fatalf("GET %s = %d; want every run and no further link", l.More, code)
	}
	// The boundary: a page that holds every match has no link.
	if l := listOf(t, h, "?q=run-&limit="+strconv.Itoa(listPage+7)); len(l.Rows) != listPage+7 || l.More != "" {
		t.Fatalf("limit = matches: rows %d, more %q", len(l.Rows), l.More)
	}
	if l := listOf(t, h, "?q=run-&limit="+strconv.Itoa(listPage+6)); len(l.Rows) != listPage+6 || l.More == "" {
		t.Fatalf("limit = matches-1: rows %d, more %q", len(l.Rows), l.More)
	}
	if code, body := get(t, h, "/?q=run-"); code != 200 || !strings.Contains(body, `href="`+html.EscapeString(l.More)+`"`) || strings.Contains(body, "run-1056") {
		t.Fatalf("first page = %d; want the show more link and not the 57th run", code)
	}
}

// TestRunListCap: a store of more runs than one load reads says so, and the run ID filter reaches
// the others.
func TestRunListCap(t *testing.T) {
	x := Exports{}
	for i := range maxSummarized {
		x[strconv.Itoa(100000+i)] = []agent.Record{{Name: "x", Kind: agent.StepValue}}
	}
	if l := (Options{Source: x}).runList(context.Background(), ListFilter{Limit: listPage}); l.Summarized != maxSummarized || len(l.Notes) != 0 {
		t.Fatalf("at the cap: read %d, notes %q", l.Summarized, l.Notes)
	}
	for i := maxSummarized; i < maxSummarized+1; i++ {
		x[strconv.Itoa(100000+i)] = []agent.Record{{Name: "x", Kind: agent.StepValue}}
	}
	l := Options{Source: x}.runList(context.Background(), ListFilter{Limit: listPage})
	if l.Summarized != maxSummarized || l.Listed != maxSummarized+1 || len(l.Notes) != 1 || !strings.Contains(l.Notes[0], "Only the first 5000 of 5001") {
		t.Fatalf("read %d of %d, notes %q", l.Summarized, l.Listed, l.Notes)
	}
	last := strconv.Itoa(100000 + maxSummarized)
	if l := (Options{Source: x}).runList(context.Background(), ListFilter{Q: last, Limit: listPage}); !slices.Equal(ids(l), []string{last}) || len(l.Notes) != 0 {
		t.Fatalf("q=%s: %v, notes %q", last, ids(l), l.Notes)
	}
}

// TestRunListLink: a row links to its run's page, the run ID escaped, and the link opens it.
func TestRunListLink(t *testing.T) {
	const prefix = "t&x y/é?"
	h, _, _ := demoHandler(t, prefix)
	code, body := get(t, h, "/?q="+url.QueryEscape("x y/é?triage-3003"))
	if code != 200 {
		t.Fatalf("GET / = %d", code)
	}
	m := regexp.MustCompile(`<td class="rid"><a href="([^"]*)">([^<]*)</a>`).FindAllStringSubmatch(body, -1)
	if len(m) != 1 || html.UnescapeString(m[0][2]) != prefix+"triage-3003" {
		t.Fatalf("rows = %q", m)
	}
	link := html.UnescapeString(m[0][1])
	if want := "/run?id=" + url.QueryEscape(prefix+"triage-3003"); link != want {
		t.Fatalf("link = %q, want %q", link, want)
	}
	var v View
	if code, body := get(t, h, "/run.json"+strings.TrimPrefix(link, "/run")); code != 200 || json.Unmarshal([]byte(body), &v) != nil || v.RunID != prefix+"triage-3003" || v.State != "halted" {
		t.Fatalf("the link's run = %d %q %q", code, v.RunID, v.State)
	}
	code, page := get(t, h, link)
	if code != 200 || !strings.Contains(page, "halted: outcome unknown") {
		t.Fatalf("GET %s = %d", link, code)
	}
}

// TestRunListLoopbackOnly: the run list, page and JSON, answers only a loopback host.
func TestRunListLoopbackOnly(t *testing.T) {
	h, _, _ := demoHandler(t, "")
	for _, target := range []string{"/", "/runs.json", "/?state=halted"} {
		for host, want := range map[string]int{"127.0.0.1:8484": 200, "[::1]:8484": 200, "localhost:8484": 200, "attacker.example:8484": 403, "10.0.0.5:8484": 403} {
			req := httptest.NewRequest("GET", target, nil)
			req.Host = host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != want {
				t.Errorf("GET %s with Host %s = %d, want %d", target, host, rec.Code, want)
			}
			if want == 403 && strings.Contains(rec.Body.String(), "refund-1041") {
				t.Errorf("GET %s with Host %s leaked a run ID", target, host)
			}
		}
	}
}

// TestRunListOneExport: an exported journal holds one run, and the list says so.
func TestRunListOneExport(t *testing.T) {
	ctx := context.Background()
	_, j, _ := demoHandler(t, "")
	exp, err := audit.ExportJournal(ctx, j, "refund-1042")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "refund-1042.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	o, _, err := options(flags{journals: multi{path}}, Stores{})
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(o)
	l := listOf(t, h, "")
	if !slices.Equal(ids(l), []string{"refund-1042"}) || l.Rows[0].Group != "halted" || len(l.Notes) != 1 || !strings.Contains(l.Notes[0], "holds one run") {
		t.Fatalf("rows %v, notes %q", ids(l), l.Notes)
	}
	if code, body := get(t, h, "/"); code != 200 || !strings.Contains(body, "An exported journal holds one run.") {
		t.Fatalf("GET / = %d; the page does not say the export holds one run", code)
	}
	// Two exports list two runs, with no note.
	o.Source.(Exports)["refund-1041"] = o.Source.(Exports)["refund-1042"]
	if l := listOf(t, Handler(o), ""); len(l.Rows) != 2 || len(l.Notes) != 0 {
		t.Fatalf("two exports: rows %v, notes %q", ids(l), l.Notes)
	}
}

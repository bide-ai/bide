package runview

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// brokenSource lists the ids it is given and fails History for the ids in bad.
type brokenSource struct {
	Source
	ids     []string
	listErr error
	bad     map[string]bool
}

func (s brokenSource) Runs(context.Context) ([]string, error) { return s.ids, s.listErr }

func (s brokenSource) History(ctx context.Context, id string) ([]agent.Record, error) {
	if s.bad[id] {
		return nil, errors.New("unreadable " + id)
	}
	return s.Source.History(ctx, id)
}

func oneRun(t *testing.T, id string) Exports {
	t.Helper()
	j := agenttest.MemJournal()
	m := agenttest.NewScriptedModel(agenttest.TextTurn("hi"))
	if _, err := agenttest.MustNew(m, j).Run(context.Background(), id, agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	return Exports{id: recordsOf(t, j, id)}
}

// TestRunListErrors: a source that fails to list, or to read one run, still lists the runs it can
// and says what failed, on the page and in the JSON; it never drops the failure silently.
func TestRunListErrors(t *testing.T) {
	ok := oneRun(t, "good")
	src := brokenSource{Source: ok, ids: []string{"bad", "good", "empty"}, bad: map[string]bool{"bad": true}, listErr: errors.New("listing cut short")}
	h := Handler(Options{Source: src})
	l := listOf(t, h, "")
	if l.Err != "listing failed (read error)" || l.Unread < 1 {
		t.Errorf("Err = %q, unread %d, want the listing failure first, by its class and not the store's text", l.Err, l.Unread)
	}
	if v := l.Verdict(); v.Status != StatusUnknown || v.Word == "ALL CLEAR" {
		t.Errorf("a list with a failed listing says %q (%s)", v.Word, v.Status)
	}
	if got := ids(l); len(got) != 1 || got[0] != "good" {
		t.Errorf("rows = %q, want only the readable run with records (the unreadable and the empty are not listed)", got)
	}
	if l.Listed != 3 || l.Summarized != 1 {
		t.Errorf("listed %d summarized %d, want 3 and 1", l.Listed, l.Summarized)
	}
	src.listErr = nil
	l = listOf(t, Handler(Options{Source: src}), "")
	if l.Err != "bad could not be read (read error)" || l.Unread != 1 || l.Empty != 1 {
		t.Errorf("Err = %q, unread %d, empty %d, want the unreadable run named by class, one unread and one empty", l.Err, l.Unread, l.Empty)
	}
	code, page := get(t, Handler(Options{Source: src}), "/")
	if code != 200 || !strings.Contains(page, "bad could not be read (read error)") || strings.Contains(page, "unreadable bad") {
		t.Errorf("GET / = %d, page lacks the failure's class or shows the store's text", code)
	}
}

// TestRunListNotesAndFilter: a single export says how to list several; the run ID filter is
// case-insensitive; past the cap the page says how many were read and how to narrow.
func TestRunListNotesAndFilter(t *testing.T) {
	x := oneRun(t, "Refund-1")
	l := listOf(t, Handler(Options{Source: x}), "")
	if len(l.Notes) != 1 || !strings.Contains(l.Notes[0], "An exported journal holds one run") {
		t.Errorf("notes = %q", l.Notes)
	}
	if got := ids(listOf(t, Handler(Options{Source: x}), "?q=REFUND")); len(got) != 1 {
		t.Errorf("case-insensitive filter matched %q", got)
	}
	if got := ids(listOf(t, Handler(Options{Source: x}), "?q=zzz")); len(got) != 0 {
		t.Errorf("filter zzz matched %q", got)
	}

	var many []string
	for i := 0; i < maxSummarized+3; i++ {
		many = append(many, fmt.Sprintf("r%05d", i))
	}
	src := brokenSource{Source: oneRun(t, "r00000"), ids: many}
	l = listOf(t, Handler(Options{Source: src}), "")
	want := fmt.Sprintf("Only the first %d of %d matching runs (by run ID) are read.", maxSummarized, maxSummarized+3)
	if len(l.Notes) < 1 || !strings.Contains(l.Notes[0], want) || l.Listed != maxSummarized+3 || l.Truncated != 3 {
		t.Errorf("notes %q, listed %d, truncated %d; want %q and 3 truncated", l.Notes, l.Listed, l.Truncated, want)
	}
	if v := l.Verdict(); v.Status == StatusClear {
		t.Errorf("a list cut short at the cap says %q", v.Word)
	}
}

// TestRunPageStoreFailures: /run and /run.json over a source that fails answer 500 with the
// reason; an unknown run is 404 on both.
func TestRunPageStoreFailures(t *testing.T) {
	src := brokenSource{Source: oneRun(t, "good"), ids: []string{"bad", "good"}, bad: map[string]bool{"bad": true}}
	h := Handler(Options{Source: src})
	for _, path := range []string{"/run?id=bad", "/run.json?id=bad"} {
		if code, body := get(t, h, path); code != http.StatusInternalServerError || !strings.Contains(body, "unreadable bad") {
			t.Errorf("GET %s = %d %q", path, code, body)
		}
	}
	for _, path := range []string{"/run?id=nosuch", "/run.json?id=nosuch"} {
		if code, _ := get(t, h, path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
}

// TestKeyHelpers: the segments of journal record names the viewer parses.
func TestKeyHelpers(t *testing.T) {
	for _, c := range []struct {
		in, node string
		iter     int
	}{
		{"iter:3:charge", "charge", 3},
		{"charge", "charge", -1},
		{"iter:x:charge", "iter:x:charge", -1},
		{"iter:3", "iter:3", -1},
	} {
		if node, iter := splitIter(c.in); node != c.node || iter != c.iter {
			t.Errorf("splitIter(%q) = %q, %d; want %q, %d", c.in, node, iter, c.node, c.iter)
		}
	}
	for in, want := range map[string]string{
		"attempt:retry:2:tool:c1": "attempt:tool:c1",
		"attempt:tool:c1":         "attempt:tool:c1",
		"attempt:retry:2":         "attempt:retry:2",
	} {
		if got := attemptBase(in); got != want {
			t.Errorf("attemptBase(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"attempt:not-started:claim9:attempt:tool:c1": "attempt:tool:c1",
		"attempt:not-started:claim9":                 "claim9",
	} {
		if got := notStartedMarker(in); got != want {
			t.Errorf("notStartedMarker(%q) = %q, want %q", in, got, want)
		}
	}
}

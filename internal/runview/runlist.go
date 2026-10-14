package runview

import (
	"cmp"
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// The run list's states, by the group its state filter names. A run's group is its View state (see
// Build), so the list and the run page always agree.
var stateGroups = []struct{ Value, Label string }{
	{"completed", "completed"},
	{"halted", "halted: outcome unknown"},
	{"waiting", "waiting for approval"},
	{"in-progress", "in progress"},
	{"failed", "failed or cancelled"},
	{"not-started", "not started"},
}

// group is the state filter group of a View state.
func group(state string) string {
	switch state {
	case string(agent.RunCompleted), "resolved", "done":
		return "completed"
	case "halted":
		return "halted"
	case "waiting":
		return "waiting"
	case string(agent.RunStarted):
		return "in-progress"
	case string(agent.RunAborted), string(agent.RunCancelled):
		return "failed"
	}
	return "not-started"
}

const (
	// listPage is how many rows the run list shows, and how many more each "show more" adds.
	listPage = 50
	// maxSummarized caps the runs one load of the list reads: every listed run's journal is read to
	// classify it, so a store of many runs is summarized up to this many (by run ID), and the page
	// says so.
	maxSummarized = 5000
)

// ListFilter is what the run list shows: the runs in a state group and of a kind, whose ID
// contains Q (ignoring case), at most Limit of them. Empty fields admit every run.
type ListFilter struct {
	State string `json:"state,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Q     string `json:"q,omitempty"`
	Limit int    `json:"limit"`
}

// listFilter reads a ListFilter from the run list's query.
func listFilter(q url.Values) ListFilter {
	f := ListFilter{State: q.Get("state"), Kind: q.Get("kind"), Q: strings.TrimSpace(q.Get("q")), Limit: listPage}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		f.Limit = n
	}
	return f
}

// RunRow is one run as the run list shows it.
type RunRow struct {
	ID        string `json:"run_id"`
	Kind      string `json:"kind"`  // agent.RunKind, or "" when the run has no run:start
	State     string `json:"state"` // the View state (see Build)
	StateText string `json:"state_text"`
	Group     string `json:"group"` // the state filter group
	Flow      string `json:"flow,omitempty"`
	Records   int    `json:"records"`
	// Started is when the run's first drive started, as its run:start records it
	// (agent.RunStart.Started). Zero for a run journaled by a version that recorded none.
	Started time.Time `json:"started,omitzero"`
}

// Link is the run's page.
func (r RunRow) Link() string { return "/run?id=" + url.QueryEscape(r.ID) }

// StartedText is Started as the list shows it, or "" when the run records no start time.
func (r RunRow) StartedText() string {
	if r.Started.IsZero() {
		return ""
	}
	return r.Started.UTC().Format("2006-01-02 15:04:05 UTC")
}

// KindText is the run's kind as the list shows it.
func (r RunRow) KindText() string { return kindText(r.Kind) }

func kindText(k string) string {
	switch k {
	case "":
		return "unknown"
	case KindSteps:
		return "steps only (no run:start)"
	}
	return strings.ReplaceAll(k, "_", " ")
}

// RunList is the run list page: the runs the filter admits, newest first by start time, then the
// runs that record none by run ID.
type RunList struct {
	Filter ListFilter `json:"filter"`
	Rows   []RunRow   `json:"runs"`
	// Matched is how many runs the filter admits; Rows holds the first Filter.Limit of them.
	Matched int `json:"matched"`
	// Summarized is how many runs were read; Listed how many the source lists. They differ when
	// more than maxSummarized runs match the ID filter.
	Summarized int      `json:"summarized"`
	Listed     int      `json:"listed"`
	Kinds      []string `json:"kinds"` // the kinds of the runs read, for the kind filter
	// Counts are the runs of each state group the ID and kind filters admit, and All their sum:
	// the state filter's counts.
	Counts map[string]int `json:"counts"`
	All    int            `json:"all"`
	// Recovering is how many of the runs counted have an open recovery failure episode in the
	// notification trail and have not ended.
	Recovering int      `json:"recovering,omitempty"`
	More       string   `json:"more,omitempty"` // the page with listPage more rows, when there are more
	Notes      []string `json:"notes,omitempty"`
	// Status is what the headline says, for a script: attention, clear or unknown. Coverage says what
	// was read; the headline is ALL CLEAR only when it is complete (see Coverage.nothingFound).
	Status string `json:"status"`
	Coverage
}

// StateOption is one of the state filter's options.
type StateOption struct {
	Value, Label string
	Count        int
	Link         string // the list with this option chosen, the other filters kept
	On           bool   // the option is the filter's
}

// States are the state filter's options: every run, then each state group that has a run or is
// chosen.
func (l RunList) States() []StateOption {
	opts := []StateOption{{Label: "all", Count: l.All, Link: l.link(""), On: l.Filter.State == ""}}
	for _, g := range stateGroups {
		if l.Counts[g.Value] > 0 || l.Filter.State == g.Value {
			opts = append(opts, StateOption{Value: g.Value, Label: g.Label, Count: l.Counts[g.Value], Link: l.link(g.Value), On: l.Filter.State == g.Value})
		}
	}
	return opts
}

// link is the run list with the state filter set to state, the kind and ID filters kept and the
// limit reset.
func (l RunList) link(state string) string {
	return query(ListFilter{State: state, Kind: l.Filter.Kind, Q: l.Filter.Q})
}

// query is the run list's URL for f, naming only the fields set.
func query(f ListFilter) string {
	v := url.Values{}
	for k, s := range map[string]string{"state": f.State, "kind": f.Kind, "q": f.Q} {
		if s != "" {
			v.Set(k, s)
		}
	}
	if f.Limit > 0 && f.Limit != listPage {
		v.Set("limit", strconv.Itoa(f.Limit))
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

// runList reads the source's runs and summarizes those the filter admits.
func (o Options) runList(ctx context.Context, f ListFilter) RunList {
	l := RunList{Filter: f, Counts: map[string]int{}}
	l.Coverage = Coverage{Listable: canList(o.Source), Trail: o.Events != nil, Filtered: f.Kind != "" || f.Q != ""}
	var ids []string
	if l.Listable {
		var err error
		if ids, err = o.Source.Runs(ctx); err != nil {
			l.Unread++
			l.Err = "listing failed (" + errClass(err) + ")"
		}
	}
	l.Listed = len(ids)
	if x, ok := o.Source.(Exports); ok && len(x) == 1 {
		l.Notes = append(l.Notes, "An exported journal holds one run. Give -journal once per export to list several.")
	}
	if f.Q != "" {
		q := strings.ToLower(f.Q)
		ids = slices.DeleteFunc(ids, func(id string) bool { return !strings.Contains(strings.ToLower(id), q) })
	}
	slices.Sort(ids)
	if len(ids) > maxSummarized {
		l.Truncated = len(ids) - maxSummarized
		l.Notes = append(l.Notes, "Only the first "+strconv.Itoa(maxSummarized)+" of "+strconv.Itoa(len(ids))+
			" matching runs (by run ID) are read. Narrow the run ID filter to see the others.")
		ids = ids[:maxSummarized]
	}
	var tr *trailReader
	if o.Events != nil {
		tr = newTrailReader(ctx, o.Events)
	}
	kinds := map[string]bool{}
	var rows []RunRow
	for _, id := range ids {
		recs, err := o.Source.History(ctx, id)
		switch {
		case err != nil:
			l.Unread++
			l.Err = cmp.Or(l.Err, id+" could not be read ("+errClass(err)+")")
			continue
		case len(recs) == 0:
			l.Empty++
			continue
		}
		r := summarize(ctx, id, recs)
		l.Summarized++
		kinds[r.Kind] = true
		if f.Kind != "" && r.Kind != f.Kind {
			continue
		}
		l.Counts[r.Group]++
		l.All++
		// The run's own trail is read, for a run that has not ended: recovery is recorded under the run it
		// was run for, and a finished run has none to fail.
		if tr != nil && !ended(r.State) {
			if fold := tr.of(id); fold.err != nil {
				l.TrailUnread++
			} else if fold.st.FailingSince != nil {
				l.Recovering++
			}
		}
		if f.State == "" || r.Group == f.State {
			rows = append(rows, r)
		}
	}
	if tr != nil && len(ids) > 0 && !tr.probe(ids[0]) {
		l.TrailUnread++ // a store that cannot be read is noticed even when no run needed its trail
	}
	for k := range kinds {
		if k != "" {
			l.Kinds = append(l.Kinds, k)
		}
	}
	slices.Sort(l.Kinds)
	slices.SortStableFunc(rows, func(a, b RunRow) int {
		if c := b.Started.Compare(a.Started); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	if l.TrailUnread > 0 {
		l.Notes = append(l.Notes, "The notify trail could not be read for "+count(l.TrailUnread, "run")+" ("+errClassNote+"), so the list cannot say whether a recovery is failing.")
	}
	if l.Unread > 0 || l.Err != "" {
		l.Notes = append(l.Notes, count(l.Unread, "run")+" could not be read ("+cmp.Or(l.Err, "read error")+"): the counts above leave them out.")
	}
	if l.Empty > 0 {
		l.Notes = append(l.Notes, count(l.Empty, "listed run")+" hold no records and are not shown.")
	}
	l.Matched = len(rows)
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		l.More = query(ListFilter{State: f.State, Kind: f.Kind, Q: f.Q, Limit: f.Limit + listPage})
	}
	l.Rows = rows
	l.Status = l.Verdict().Status
	return l
}

// summarize is the run's row, from the View the run page shows.
func summarize(ctx context.Context, runID string, recs []agent.Record) RunRow {
	v := Build(ctx, runID, recs, nil)
	r := RunRow{ID: runID, Kind: v.Kind, State: v.State, StateText: v.StateText, Group: group(v.State), Flow: v.Flow, Records: v.Records}
	if r.Group == "in-progress" || r.Group == "not-started" {
		r.StateText = strings.ReplaceAll(r.Group, "-", " ")
	}
	for _, rec := range recs {
		if rec.Kind == agent.StepValue && rec.Name == "run:start" {
			var st agent.RunStart
			if json.Unmarshal(rec.Result, &st) == nil {
				r.Started, _ = st.Started()
			}
			break
		}
	}
	return r
}

// KindOption is one of the kind filter's options.
type KindOption struct {
	Value, Label string
	On           bool
}

// KindOptions are the kind filter's options: the kinds of the runs read, and the chosen one.
func (l RunList) KindOptions() []KindOption {
	kinds := l.Kinds
	if l.Filter.Kind != "" && !slices.Contains(kinds, l.Filter.Kind) {
		kinds = append(slices.Clone(kinds), l.Filter.Kind)
	}
	opts := make([]KindOption, 0, len(kinds))
	for _, k := range kinds {
		opts = append(opts, KindOption{Value: k, Label: kindText(k), On: k == l.Filter.Kind})
	}
	return opts
}

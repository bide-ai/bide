package runview

import (
	"cmp"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// Coverage says how much of the source a page read, and so what its headline may claim. A page is
// clear only when it read everything it should have: a headline that is silent about a run it never
// read would be a false all-clear. It is in the JSON of the run list and the Needs attention list.
type Coverage struct {
	// Listable: the source can enumerate its runs. A store without an agent.Lister cannot, and a page
	// over it knows nothing about its runs.
	Listable bool `json:"listable"`
	// Truncated is how many listed runs were not read because a load reads at most maxSummarized.
	Truncated int `json:"truncated,omitempty"`
	// Unread is how many runs could not be read (their listing or their journal failed); Err says
	// the class of the first failure, never the store's own text.
	Unread int    `json:"unread,omitempty"`
	Err    string `json:"error,omitempty"`
	// Empty is how many listed runs hold no records.
	Empty int `json:"empty,omitempty"`
	// Trail: the notification trail was given (Options.Events). TrailUnread is how many runs' trails
	// could not be read.
	Trail       bool `json:"notify_trail"`
	TrailUnread int  `json:"trail_unread,omitempty"`
	// Filtered: the page shows a filter's runs only, so its headline cannot speak for the store.
	Filtered bool `json:"filtered,omitempty"`
}

// journalsComplete reports whether every run the source lists was read.
func (c Coverage) journalsComplete() bool {
	return c.Listable && c.Err == "" && c.Unread == 0 && c.Truncated == 0 && !c.Filtered
}

// complete reports whether the journals and the notification trail were read in full: the only
// state in which a page may say nothing needs a person.
func (c Coverage) complete() bool { return c.journalsComplete() && c.Trail && c.TrailUnread == 0 }

// Gap is a line saying what the page did not read, or "" when it read everything it should have.
func (c Coverage) Gap() string {
	switch {
	case !c.Listable:
		return "This store cannot enumerate its runs, so no run was read."
	case c.Filtered:
		return "A filter is set: this speaks for the runs the filter admits, not for the store."
	}
	var parts []string
	if c.Unread > 0 || c.Err != "" {
		parts = append(parts, count(c.Unread, "run")+" could not be read ("+cmp.Or(c.Err, "read error")+")")
	}
	if c.Truncated > 0 {
		parts = append(parts, count(c.Truncated, "run")+" not read: a load reads at most "+strconv.Itoa(maxSummarized))
	}
	if c.TrailUnread > 0 {
		parts = append(parts, "the notify trail could not be read for "+count(c.TrailUnread, "run"))
	}
	out := strings.Join(parts, "; ")
	if out != "" {
		out += "."
	}
	return out
}

// nothingFound is the verdict of a page that found nothing needing a person: clear only with
// complete coverage, else what it cannot rule out. none says the source lists no run at all.
// clearWord is the headline of a clear page.
func (c Coverage) nothingFound(none bool, clearWord, clearLine string) Verdict {
	switch {
	case !c.Listable:
		return Verdict{Class: "warn", Status: StatusUnknown, Word: "CANNOT LIST", Icon: "x", Line: "This store cannot enumerate its runs, so nothing can be said about them. Open one with -run."}
	case c.Filtered:
		return Verdict{Class: "info", Status: StatusUnknown, Word: "NOTHING IN THIS FILTER NEEDS A PERSON", Icon: "idle", Line: c.Gap()}
	case !c.journalsComplete():
		return Verdict{Class: "warn", Status: StatusUnknown, Word: "NOT EVERY RUN WAS READ", Icon: "x", Line: c.Gap() + " Nothing in the runs that were read needs a person."}
	case none:
		return Verdict{Class: "info", Status: StatusClear, Word: "NO RUNS", Icon: "idle", Line: "Nothing to list."}
	case c.complete():
		return Verdict{Class: "good", Status: StatusClear, Word: clearWord, Icon: "ok", Line: clearLine}
	case c.Trail:
		return Verdict{Class: "info", Status: StatusUnknown, Word: "NOTHING IN THE JOURNALS NEEDS A PERSON", Icon: "idle", Line: "The notify trail could not be read for every run. Recovery failures are not visible without it."}
	}
	return Verdict{Class: "info", Status: StatusUnknown, Word: "NOTHING IN THE JOURNALS NEEDS A PERSON", Icon: "idle", Line: "Recovery failures are not visible without the notify trail (it is read from -db or -postgres)."}
}

// A page's status in the JSON, from the same Verdict as its headline: attention when something needs
// a person, clear when the page read everything and found nothing, unknown when it cannot say.
const (
	StatusAttention = "attention"
	StatusClear     = "clear"
	StatusUnknown   = "unknown"
)

// errClass is what a page says of a source error: its class, never the store's text, which can carry
// a connection string or a query.
func errClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, agent.ErrStorage):
		return "store error"
	}
	return "read error"
}

// Listable is a Source that can say whether it can enumerate its runs. A Source that does not
// implement it is taken to.
type Listable interface{ CanList() bool }

// UnfinishedLister is a Source that can list the runs without a run:complete marker alone, so the
// Needs attention list reads no finished run.
type UnfinishedLister interface {
	Unfinished(ctx context.Context) ([]string, error)
}

func canList(s Source) bool {
	if l, ok := s.(Listable); ok {
		return l.CanList()
	}
	return true
}

// trailFold is one run's notification trail, folded, or why it could not be read.
type trailFold struct {
	st  agent.NotifyState
	err error
}

// trailReader reads notification trails through an audit.EventStore, each once per page load.
type trailReader struct {
	ctx   context.Context
	trail *audit.NotifyTrail
	folds map[string]*trailFold
}

func newTrailReader(ctx context.Context, es audit.EventStore) *trailReader {
	return &trailReader{ctx: ctx, trail: audit.NewNotifyTrail(es), folds: map[string]*trailFold{}}
}

// of is the fold of root's trail.
func (t *trailReader) of(root string) *trailFold {
	f, ok := t.folds[root]
	if !ok {
		f = &trailFold{}
		recs, err := t.trail.Load(t.ctx, agent.NotifyTrailName(root))
		f.st, f.err = agent.FoldNotify(recs), err
		t.folds[root] = f
	}
	return f
}

// probe reads one trail if none was read, so a store that cannot be read is noticed even when no run
// needed its trail. It reports whether the store answered.
func (t *trailReader) probe(name string) bool {
	if len(t.folds) > 0 {
		return true
	}
	return t.of(name).err == nil
}

// ttlCache holds a computed page for a moment, so many viewers (or one page refreshing itself) cost
// the store one read per ttl, not one per request. A request for a key being computed waits for it.
type ttlCache struct {
	mu sync.Mutex
	m  map[string]*cacheEntry
}

type cacheEntry struct {
	mu sync.Mutex
	at time.Time
	v  any
}

// cacheTTL is how long a computed page is served again.
const cacheTTL = 2 * time.Second

const maxCacheKeys = 64

func (c *ttlCache) get(key string, build func() any) any {
	c.mu.Lock()
	if c.m == nil || len(c.m) >= maxCacheKeys {
		c.m = map[string]*cacheEntry{} // the keys come from filters in the URL: bound them
	}
	e, ok := c.m[key]
	if !ok {
		e = &cacheEntry{}
		c.m[key] = e
	}
	c.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.v == nil || time.Since(e.at) > cacheTTL {
		e.v, e.at = build(), time.Now()
	}
	return e.v
}

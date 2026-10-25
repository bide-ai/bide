package runview

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"iter"
	"net"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	safetext "github.com/bide-ai/bide/internal/printable"
	"github.com/bide-ai/bide/plan"
	"github.com/bide-ai/bide/theme"
)

//go:embed assets
var assets embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"svg":       func(s string) template.HTML { return template.HTML(s) }, // built by svg, every label escaped
	"icon":      icon,
	"head":      head,
	"headr":     headRefresh,
	"sprite":    sprite,
	"json":      highlightJSON,
	"min":       func(a, b int) int { return min(a, b) },
	"join":      strings.Join,
	"count":     func(items []AttentionItem, noun string) string { return count(len(items), noun) },
	"hasPrefix": func(prefix, s string) bool { return strings.HasPrefix(s, prefix) },
}).ParseFS(assets, "assets/*.html"))

// Source is what the viewer reads: the runs it lists and each run's records. Both are reads.
type Source interface {
	Runs(ctx context.Context) ([]string, error)
	History(ctx context.Context, runID string) ([]agent.Record, error)
}

// JournalSource reads a store through a Journal. Runs lists through the store's agent.Lister,
// when it has one.
type JournalSource struct{ J *agent.Journal }

// Runs lists the store's runs, or none when the store cannot list them.
func (s JournalSource) Runs(ctx context.Context) ([]string, error) {
	l, ok := agent.Capability[agent.Lister](s.J.Store())
	if !ok {
		return nil, nil
	}
	var ids []string
	for id, err := range l.Runs(ctx, agent.RunFilter{}) {
		if err != nil {
			return ids, err
		}
		if !agent.IsReservedRun(id) { // the kill switch's control run and a cap ledger are not runs
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// CanList reports whether the store can enumerate its runs (it has an agent.Lister). A page over one
// that cannot knows nothing about its runs, and says so.
func (s JournalSource) CanList() bool {
	_, ok := agent.Capability[agent.Lister](s.J.Store())
	return ok
}

// Unfinished lists the runs that have no run:complete marker, in the store's own query: a run that
// completed has nothing a person must do, so the Needs attention list reads none. An aborted or
// cancelled run is listed (a halt outlives either).
func (s JournalSource) Unfinished(ctx context.Context) ([]string, error) {
	return s.UnfinishedLimit(ctx, 0)
}

// UnfinishedLimit is Unfinished that stops listing after limit runs (no limit when limit is 0), so a store
// with a very large number of unfinished runs is not loaded whole to read the first few.
func (s JournalSource) UnfinishedLimit(ctx context.Context, limit int) ([]string, error) {
	l, ok := agent.Capability[agent.Lister](s.J.Store())
	if !ok {
		return nil, nil
	}
	var ids []string
	for id, err := range l.Runs(ctx, agent.RunFilter{ExcludeHolding: []string{"run:complete"}}) {
		if err != nil {
			return ids, err
		}
		if !agent.IsReservedRun(id) {
			ids = append(ids, id)
			if limit > 0 && len(ids) >= limit {
				break
			}
		}
	}
	return ids, nil
}

// History reads the run's records.
func (s JournalSource) History(ctx context.Context, runID string) ([]agent.Record, error) {
	return s.J.History(ctx, runID)
}

// Exports is a Source over exported journals (audit.JournalExport), by run ID.
type Exports map[string][]agent.Record

// Runs lists the exported runs.
func (x Exports) Runs(context.Context) ([]string, error) {
	ids := make([]string, 0, len(x))
	for id := range x {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

// History returns the exported run's records.
func (x Exports) History(_ context.Context, runID string) ([]agent.Record, error) {
	return x[runID], nil
}

// Options configure Handler.
type Options struct {
	Source Source
	// Flows are the declared flows runs are drawn against, by flow name.
	Flows map[string]*Flow
	// Provers check runs against audit artifacts; the first to return a verdict for a run wins.
	Provers []Prover
	// Config, when set, makes the handler show this flow definition instead of runs: the page at /
	// and its view as JSON at /config.json. Source is then not read.
	Config *Config
	// Diff, when set, makes the handler show a flow definition compared with an earlier one
	// instead of runs: the page at / and its view as JSON at /diff.json (see BuildDiff).
	Diff *Diff
	// Events, when set, is the notification trail's store (the audit.EventStore beside the
	// journal): the Needs attention list then says for each item whether it was notified and
	// acknowledged, and lists a run whose recovery keeps failing.
	Events audit.EventStore
	// ApproveCommand, when set, writes the bide approve command line that records a decision on a
	// call of a run (single: a one-decision gate). The Needs attention list shows it with each
	// pending approval; without it the list says it has none.
	ApproveCommand func(runID, toolUseID string, single bool) (string, error)
	// ApproveCertCommand, when set, writes the bide approve -cert command line for a gate with a
	// certificate-bound approver: a bare-key decision does not count for that approver. The Needs
	// attention list shows it beside (a gate with bare-key approvers too) or instead of (all approvers
	// certificate-bound) ApproveCommand's line.
	ApproveCertCommand func(runID, toolUseID string) (string, error)
	// Stops, when set, is the kill switch's control run of the journal: every page then opens with a banner
	// while a stop is in force (or the control run cannot be judged, or holds rejected records), a run's page
	// and the run list say which runs a stop holds, and /stops.json serves the same state.
	Stops *StopFeed
	// Themes are custom themes (see the theme package) the pages offer beside the shipped light, dark
	// and high-contrast; DefaultTheme, when not empty, names the one a reader starts in. Handler
	// panics on a DefaultTheme that names no theme and on two themes with one name: they are a host
	// program's mistake, and bide view checks both before it calls Handler.
	Themes       []*theme.Theme
	DefaultTheme string
	// ThemeLock, when not empty, names the one theme the pages have: no switch, no remembered
	// choice (the system's request for more contrast or forced colors still wins). Handler panics
	// on a ThemeLock that names no theme or that differs from a DefaultTheme.
	ThemeLock string
	// Now is the clock the Needs attention list measures waits by; nil is time.Now.
	Now func() time.Time

	cache *ttlCache // set by Handler: the run list and the Needs attention list are served again for cacheTTL
}

// Handler serves the run list at / (as JSON at /runs.json; see RunList), the Needs attention list at
// /attention (as JSON at /attention.json; see AttentionList) and a run at /run?id=<run>
// (its View as JSON at /run.json?id=<run>, and, for a run in progress in a store, its live updates
// at /run/live?id=<run>&after=<cursor>).
// The run list and the Needs attention list are built at most once per cacheTTL (2 seconds) per query, and
// a build is cut off after buildTimeout (30 seconds), so many viewers cost the store one read of its runs.
// It answers only GET and HEAD, and only requests addressed to a loopback host, so a page on
// another site cannot read it through a rebound DNS name.
func Handler(o Options) http.Handler {
	o.cache = &ttlCache{}
	mux := http.NewServeMux()
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", typed(http.FileServerFS(AssetsWith(o.themeSet())))))
	if o.Config != nil {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { render(w, "config.html", BuildConfig(*o.Config)) })
		mux.HandleFunc("GET /config.json", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, BuildConfig(*o.Config)) })
		return guard(mux)
	}
	if o.Diff != nil {
		diffRoutes(mux, *o.Diff)
		return guard(mux)
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		o.render(w, r, "runs.html", o.cachedRunList(r.Context(), listFilter(r.URL.Query())))
	})
	mux.HandleFunc("GET /stops.json", o.serveStops)
	mux.HandleFunc("GET /runs.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, o.cachedRunList(r.Context(), listFilter(r.URL.Query())))
	})
	serveRun := func(asJSON bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			v, err := o.view(r.Context(), r.URL.Query().Get("id"))
			switch {
			case err != nil:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			case v == nil:
				http.NotFound(w, r)
			case asJSON:
				writeJSON(w, v)
			default:
				o.render(w, r, "run.html", v)
			}
		}
	}
	mux.HandleFunc("GET /attention", o.serveAttention(false))
	mux.HandleFunc("GET /attention.json", o.serveAttention(true))
	mux.HandleFunc("GET /run", serveRun(false))
	mux.HandleFunc("GET /run.json", serveRun(true))
	mux.HandleFunc("GET /run/live", o.serveLive)
	return guard(mux)
}

// guard answers only requests addressed to a loopback host, with a strict content policy.
func guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "bide view answers only requests to a loopback address", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; img-src 'self' data:")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Every page is written with each character that does not print written out (\u{XXXX}), so a
		// name from a journal (a topology, a step or tool name: any string) cannot be drawn as another
		// name. The pages carry no script data, so nothing parses the escape; the JSON endpoints are
		// for programs and are left as they are.
		ew := &escapingWriter{ResponseWriter: w}
		defer ew.close()
		h.ServeHTTP(ew, r)
	})
}

// escapingWriter writes an HTML response through safetext.Writer, decided at the first write by the
// content type the handler set (text/html pages, and the text/plain bodies of an error).
type escapingWriter struct {
	http.ResponseWriter
	out     *safetext.EscapingWriter
	decided bool
}

func isText(ct string) bool {
	return strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "text/plain")
}

func (e *escapingWriter) decide() {
	if e.decided {
		return
	}
	e.decided = true
	if isText(e.Header().Get("Content-Type")) {
		e.out = safetext.Writer(e.ResponseWriter)
	}
}

// close writes what the escaping writer still holds.
func (e *escapingWriter) close() {
	if e.out != nil {
		_ = e.out.Close()
	}
}

func (e *escapingWriter) WriteHeader(code int) {
	e.decide()
	e.ResponseWriter.WriteHeader(code)
}

func (e *escapingWriter) Write(p []byte) (int, error) {
	e.decide()
	if e.out != nil {
		return e.out.Write(p)
	}
	return e.ResponseWriter.Write(p)
}

// Flush passes a flush on, so a streaming response still streams.
func (e *escapingWriter) Flush() {
	if f, ok := e.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (e *escapingWriter) Unwrap() http.ResponseWriter { return e.ResponseWriter }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// now is the viewer's clock (Options.Now, else time.Now).
func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// view builds the run's View, or nil for a run with no records.
func (o Options) view(ctx context.Context, runID string) (*View, error) {
	recs, seqs, err := o.history(ctx, runID)
	if err != nil {
		return nil, err
	}
	return o.viewOf(ctx, runID, recs, seqs), nil
}

// viewOf builds the View of the run's records, or nil for none. seqs, when given, are the records'
// store sequences: the page of a run with no end marker then follows the run (see follow).
func (o Options) viewOf(ctx context.Context, runID string, recs []agent.Record, seqs []int64) *View {
	if len(recs) == 0 {
		return nil
	}
	var flow *Flow
	for _, r := range recs {
		if r.Name == "run:start" {
			var st agent.RunStart
			if json.Unmarshal(r.Result, &st) == nil && st.Flow != nil {
				flow = o.Flows[st.Flow.Name]
			}
			break
		}
	}
	// Without an override (-topology), a flow run is drawn from the topology it journaled, once its
	// digest is checked against the journaled flow:digest (plan.JournaledTopology).
	var topoErr error
	if flow == nil {
		if t, d, ok, err := plan.JournaledTopology(recs); ok && err == nil {
			flow = &Flow{Topology: t, Digest: d, Source: "from journal (digest verified)"}
		} else if ok {
			topoErr = err
		}
	}
	v := Build(ctx, runID, recs, flow)
	if topoErr != nil {
		v.Notes = append(v.Notes, "the journaled topology is not drawn: "+topoErr.Error())
	}
	declined := &otherRuns{}
	pctx := context.WithValue(ctx, otherRunsKey{}, declined)
	for _, p := range o.Provers {
		if v.Proof = p(pctx, runID, recs); v.Proof != nil {
			break
		}
	}
	for i := range v.Gates {
		signatures(&v.Gates[i], v.Proof, declined)
		if chk, ok := v.Proof.approval(v.Gates[i].ToolUseID); ok {
			applyIdentityAudit(&v.Gates[i], chk.Identity)
		}
		v.Gates[i].Cards = cards(v.Gates[i], v.Proof)
	}
	MarkDeadlines(v.Gates, o.now())
	markGates(v.Graph, v.Gates) // the graph's gated nodes say whether their signatures verified
	follow(v, seqs)
	v.Verdict = runVerdict(v)
	if r, ok := o.stopState(ctx); ok && r.err == nil {
		v.Held = heldBy(r.st, v)
	}
	return v
}

// signatures sets g's signature verdict from the proof's approval checks, or says why its
// signatures were not checked. declined are the artifacts the provers declined for being another
// run's; they only change the wording when no proof was made.
func signatures(g *Gate, p *Proof, declined *otherRuns) {
	switch c, ok := p.approval(g.ToolUseID); {
	case g.Single:
		g.Signatures = "one decision (agent.Approve), which is not signed"
	case ok:
		g.Signatures, g.SignaturesOK = c.Text, c.OK
	case p != nil && p.Approvals != nil:
		g.Signatures = "signatures not checked: the gate has not journaled its tally yet"
	case p != nil && p.OK:
		g.Signatures = "included in the signed log; signatures not checked (pass -approver-keys)"
	case p == nil && declined.text() != "":
		g.Signatures = declined.text()
	default:
		g.Signatures = "signatures not checked (pass -approver-keys, with -sth or -evidence)"
	}
}

// approval is the signature check of the gate on toolUseID, if the proof made one.
func (p *Proof) approval(toolUseID string) (ApprovalCheck, bool) {
	if p == nil {
		return ApprovalCheck{}, false
	}
	c, ok := p.Approvals[toolUseID]
	return c, ok
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// Listen listens on addr, which must name a loopback IP (127.0.0.1 or ::1) or "localhost"
// (listened on as 127.0.0.1). Any other host, an empty one included, is refused: the viewer shows
// a journal's contents and must not be reachable from another machine.
func Listen(addr string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("runview: %q is not a loopback address; the viewer listens on 127.0.0.1 or ::1 only", addr)
	}
	return net.Listen("tcp", net.JoinHostPort(host, port))
}

// loopbackHost reports whether an HTTP Host names a loopback address or localhost.
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// staticStore is a read-only agent.Store over one run's records, so the agent and audit readers
// (Status, Conform, ProveRecord) run over records already read.
type staticStore struct{ recs []agent.Record }

var errReadOnly = errors.New("runview: the viewer is read-only")

func (staticStore) Insert(context.Context, string, string, []byte) (agent.Entry, bool, error) {
	return agent.Entry{}, false, errReadOnly
}

func (s staticStore) Get(_ context.Context, _, name string) (agent.Entry, bool, error) {
	for i, r := range s.recs {
		if r.Name == name {
			return agent.Entry{Seq: int64(i + 1), Name: name, Data: r.Raw()}, true, nil
		}
	}
	return agent.Entry{}, false, nil
}

func (s staticStore) Load(_ context.Context, _ string, after int64) iter.Seq2[agent.Entry, error] {
	return func(yield func(agent.Entry, error) bool) {
		for i, r := range s.recs {
			e := agent.Entry{Seq: int64(i + 1), Name: r.Name, Data: r.Raw()}
			if e.Seq > after && !yield(e, nil) {
				return
			}
		}
	}
}

// assetTypes is the Content-Type of each kind of file /assets/ serves, by extension, and the list of
// kinds it serves at all: the pages' scripts and styles. It is a fixed table and not the operating
// system's: Go reads the type of an extension from the platform (on Windows, the registry), so a
// machine that maps .js or .css to text/plain, by mistake or on purpose, would with nosniff leave
// every page without its script and style. The page templates (.html) are in the same directory and
// are not served.
var assetTypes = map[string]string{
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".json": "application/json",
	".svg":  "image/svg+xml",
}

// typed serves only the extensions in assetTypes, each with its table type set before the file
// server writes it. Anything else, a template, a directory listing or an unknown extension, is 404.
func typed(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, ok := assetTypes[path.Ext(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", t)
		h.ServeHTTP(w, r)
	})
}

// themeSet is the themes the options name.
func (o Options) themeSet() ThemeSet {
	return ThemeSet{Extra: o.Themes, Default: o.DefaultTheme, Lock: o.ThemeLock}
}

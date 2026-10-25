package flowedit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/approveline"
	"github.com/bide-ai/bide/internal/editrun"
	"github.com/bide-ai/bide/internal/printable"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
)

// The bounds of the list of unfinished runs a change would refuse. Reads of a store are bounded by all of them,
// and a result that stopped at one says so.
const (
	// maxGroups is how many distinct digests are followed up; runs under further digests are counted.
	maxGroups = 20
	// maxGroupIDs is how many run IDs a group shows.
	maxGroupIDs = 20
)

// The bounds a test may change: how many unfinished runs are read (the first by run ID; the listing asks
// for one more to know whether there are others), how long one scan of the store may take, and the total time
// structural diffs of the groups may take together.
var (
	maxListed   = 5000
	scanTimeout = 60 * time.Second
	diffBudget  = 10 * time.Second
)

// scanGroup is the unfinished runs that recorded one digest: how many, the first IDs, and what the
// scan read of one of them (the flow it started, and its topology) so that nothing more is read from the
// store when the text changes.
type scanGroup struct {
	digest string
	count  int
	ids    []string
	// flow is the flow name run:start of the group's first run records; flowErr says why it is not known.
	flow    string
	flowErr string
	// topo is that run's journaled topology, nil with topoNote saying why, and consistent says its digest
	// is the recorded digest (a run recorded under digest v1 is not consistent).
	topo       *plan.Topology
	topoNote   string
	consistent bool
}

// scanResult is one scan of the store: the unfinished runs, by digest.
type scanResult struct {
	readAt time.Time
	// status is "ok", "nolister" (the store cannot list its runs) or "error".
	status string
	note   string
	// listed is how many runs were read, and more says there are others past maxListed.
	listed int
	more   bool
	// stopped says the scan ended early ("the time limit"), and unreadable how many runs could not be read.
	stopped    string
	unreadable int
	noDigest   int
	groups     []*scanGroup
	// overflowRuns and overflowDigests count the runs and the distinct digests past maxGroups.
	overflowRuns, overflowDigests int
	transient                     bool
}

// scanState is the server's one scan, and the scan in flight to make it, as runState is for one run.
type scanState struct {
	mu       sync.Mutex
	res      *scanResult
	until    time.Time
	inflight *scanFlight
	gen      int
}

type scanFlight struct {
	done       chan struct{}
	res        *scanResult
	gen        int
	cancel     context.CancelFunc
	superseded bool
}

// scan returns the scan already made unless reload, sharing a scan in flight, with a context of its own, and
// keeping a failed one for runBackoff only: the same rules as snapshot.
func (s *Server) scan(ctx context.Context, reload bool) *scanResult {
	s.aff.mu.Lock()
	if s.aff.res != nil && !reload && (s.aff.until.IsZero() || time.Now().Before(s.aff.until)) {
		r := s.aff.res
		s.aff.mu.Unlock()
		return r
	}
	f := s.aff.inflight
	if f == nil || reload {
		if f != nil {
			f.superseded = true
			f.cancel()
		}
		s.aff.gen++
		rctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
		f = &scanFlight{done: make(chan struct{}), gen: s.aff.gen, cancel: cancel}
		s.aff.inflight = f
		run := *s.run
		go func() {
			defer cancel()
			res := scanRuns(rctx, run)
			s.aff.mu.Lock()
			if f.gen == s.aff.gen {
				s.aff.res, s.aff.until = res, time.Time{}
				if res.transient {
					s.aff.until = time.Now().Add(runBackoff)
				}
				s.aff.inflight = nil
			}
			s.aff.mu.Unlock()
			f.res = res
			close(f.done)
		}()
	}
	s.aff.mu.Unlock()
	select {
	case <-f.done:
		s.aff.mu.Lock()
		superseded := f.superseded
		s.aff.mu.Unlock()
		if superseded {
			return s.scan(ctx, false)
		}
		return f.res
	case <-ctx.Done():
		return &scanResult{readAt: time.Now().UTC(), status: "error", note: storeFailure("The store could not be read", ctx.Err()), transient: true}
	}
}

// endNames are the records that end a run: a run holding one is not unfinished, whatever flow it is of.
var endNames = []string{"run:complete", "run:aborted", "run:cancelled"}

// scanRuns lists the store's unfinished runs and groups them by the digest each recorded, reading one small
// record per run (flow:digest) and, for the first run of each of at most maxGroups digests, its run:start
// (for the flow's name) and flow:topology. It never reads a run's history. The list is the recovery pass's:
// runs with no end marker, reserved runs left out.
func scanRuns(ctx context.Context, run editrun.Run) *scanResult {
	res := &scanResult{readAt: time.Now().UTC(), status: "ok"}
	var ids []string
	var get func(id, name string) (agent.Record, bool, error)
	switch src := run.Source.(type) {
	case runview.JournalSource:
		l, ok := agent.Capability[agent.Lister](src.J.Store())
		if !ok {
			res.status, res.note = "nolister", "This store cannot list its runs, so the unfinished runs cannot be checked."
			return res
		}
		for id, err := range l.Runs(ctx, agent.RunFilter{ExcludeHolding: endNames}) {
			if err != nil {
				res.status, res.note, res.transient = "error", storeFailure("The runs could not be listed", err), true
				return res
			}
			if agent.IsReservedRun(id) {
				continue
			}
			if len(ids) == maxListed {
				res.more = true
				break
			}
			ids = append(ids, id)
		}
		get = func(id, name string) (agent.Record, bool, error) { return src.J.Get(ctx, id, name) }
	default:
		all, err := run.Source.Runs(ctx)
		if err != nil {
			res.status, res.note, res.transient = "error", storeFailure("The runs could not be listed", err), true
			return res
		}
		slices.Sort(all)
		hist := map[string]getFunc{}
		for _, id := range all {
			if agent.IsReservedRun(id) {
				continue
			}
			recs, err := run.Source.History(ctx, id)
			if err != nil {
				res.unreadable++
				continue
			}
			g := getOf(recs)
			ended := false
			for _, n := range endNames {
				if _, ok, _ := g(n); ok {
					ended = true
				}
			}
			if ended {
				continue
			}
			if len(ids) == maxListed {
				res.more = true
				break
			}
			ids = append(ids, id)
			hist[id] = g
		}
		get = func(id, name string) (agent.Record, bool, error) { return hist[id](name) }
	}
	by := map[string]*scanGroup{}
	overflow := map[string]bool{}
	for _, id := range ids {
		if ctx.Err() != nil {
			res.stopped = "the time limit"
			break
		}
		res.listed++
		rec, ok, err := get(id, "flow:digest")
		if err != nil {
			res.unreadable++
			continue
		}
		if !ok {
			res.noDigest++
			continue
		}
		var d string
		if json.Unmarshal(rec.Result, &d) != nil || !sixtyFourHex(d) {
			res.unreadable++
			continue
		}
		g := by[d]
		if g == nil {
			if len(res.groups) == maxGroups {
				res.overflowRuns++
				overflow[d] = true
				continue
			}
			g = &scanGroup{digest: d}
			by[d] = g
			res.groups = append(res.groups, g)
			followUp(g, id, get)
		}
		g.count++
		if len(g.ids) < maxGroupIDs {
			g.ids = append(g.ids, id)
		}
	}
	res.overflowDigests = len(overflow)
	return res
}

// editrun0 is the run (or the store alone) the scan reads.

// followUp reads what one run says of its group: the flow it started, and its topology.
func followUp(g *scanGroup, id string, get func(id, name string) (agent.Record, bool, error)) {
	if rec, ok, err := get(id, "run:start"); err != nil {
		g.flowErr = strings.TrimSuffix(storeFailure("run:start could not be read", err), ".")
	} else if !ok {
		g.flowErr = "the run has no run:start"
	} else {
		var st agent.RunStart
		if json.Unmarshal(rec.Result, &st) != nil || st.Flow == nil {
			g.flowErr = "run:start names no flow"
		} else {
			g.flow = st.Flow.Name
		}
	}
	rec, ok, err := get(id, plan.TopologyStep)
	switch {
	case err != nil:
		g.topoNote = strings.TrimSuffix(storeFailure("its topology could not be read", err), ".")
	case !ok:
		g.topoNote = "the run recorded no topology"
	case len(rec.Result) > MaxFileSize:
		g.topoNote = "its topology is larger than the comparison reads"
	default:
		var t plan.Topology
		if json.Unmarshal(rec.Result, &t) != nil {
			g.topoNote = "its topology cannot be read"
			return
		}
		if d, err := plan.TopologyDigest(t); err == nil && d == g.digest {
			g.consistent = true
		}
		g.topo = &t
	}
}

// affectedGroup is what the page lists for one digest.
type affectedGroup struct {
	Digest string `json:"digest"`
	Runs   int    `json:"runs"`
	// IDs are the first run IDs of the group, written out as the rest of a reply is.
	IDs []string `json:"ids"`
	// Command is the command to open the first run in the editor, with the IDs of unsafe characters left
	// out; empty when its ID cannot be put in a command line.
	Command string `json:"command,omitempty"`
	// Status is "refused" (a resume under this file would be refused), "same" (the digests are equal), "none_seen"
	// (structural: no difference seen), "other_flow" (the runs are of another flow, so this file is not theirs) or
	// "not_compared" (with Reason).
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	// Changes are the first changes of a structural comparison.
	Changes []string `json:"changes,omitempty"`
}

// affectedReply answers /api/affected. Without a store only Configured is false.
type affectedReply struct {
	Configured bool `json:"configured"`
	// Checked says a scan was asked for (and made) for this reply.
	Checked bool   `json:"checked"`
	Status  string `json:"status,omitempty"`
	// Heading and Summary are the sentences the page shows.
	Heading string `json:"heading,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Refused is the number of runs in groups a resume under this file would refuse, and Listed how many unfinished
	// runs were read.
	Refused    int  `json:"refused"`
	Listed     int  `json:"listed"`
	More       bool `json:"more"`
	NoDigest   int  `json:"no_digest"`
	Unreadable int  `json:"unreadable"`
	OtherFlow  int  `json:"other_flow"`
	Overflow   int  `json:"overflow_runs"`
	// Groups are the digests, refused ones first.
	Groups []affectedGroup `json:"groups"`
	Notes  []string        `json:"notes,omitempty"`
	ReadAt string          `json:"read_at,omitempty"`
}

// serveAffected answers /api/affected: with check, the unfinished runs of the store compared with the text.
func (s *Server) serveAffected(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text   string `json:"text"`
		Check  bool   `json:"check"`
		Reload bool   `json:"reload"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if len(req.Text) > MaxFileSize {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the text is larger than %d bytes", MaxFileSize))
		return
	}
	if s.run == nil || s.run.Source == nil {
		reply(w, &affectedReply{Groups: []affectedGroup{}})
		return
	}
	if !req.Check {
		reply(w, &affectedReply{Configured: true, Groups: []affectedGroup{}})
		return
	}
	reply(w, s.affected(r.Context(), []byte(req.Text), req.Reload))
}

// affected compares the text with the digests of the unfinished runs the store holds.
func (s *Server) affected(ctx context.Context, text []byte, reload bool) *affectedReply {
	sc := s.scan(ctx, reload)
	rep := &affectedReply{Configured: true, Checked: true, Status: sc.status, Groups: []affectedGroup{},
		Listed: sc.listed, More: sc.more, NoDigest: sc.noDigest, Unreadable: sc.unreadable, Overflow: sc.overflowRuns, ReadAt: sc.readAt.Format(time.RFC3339)}
	if sc.status != "ok" {
		rep.Summary = sc.note
		return rep.clean()
	}
	typed := s.typing != nil
	file, err := s.fileConfig(text)
	if err != nil {
		rep.Status, rep.Summary = "cannot", "Cannot compare: the text is not a readable config ("+firstLine(err.Error())+")." // text of the file, not of a store
		return rep.clean()
	}
	frep := file.Inspect()
	if typed && frep.Digest == "" {
		rep.Status = "cannot"
		rep.Summary = fmt.Sprintf("Cannot compare: the file has errors (%d), so it has no digest. Fix them to compare with the unfinished runs.", errorsOf(frep))
		return rep.clean()
	}
	baseFlow := flowNameOf(s.opened, s.name)
	start := time.Now()
	for _, g := range sc.groups {
		ag := affectedGroup{Digest: g.digest, Runs: g.count, IDs: slices.Clone(g.ids)}
		if cmd, err := approveline.Word(g.ids[0]); err == nil {
			ag.Command = "bide edit -run " + cmd + " <store flags> <file>"
		}
		switch {
		case g.flowErr != "":
			ag.Status, ag.Reason = "not_compared", "which flow it is of is not known: "+g.flowErr
		case baseFlow == "":
			ag.Status, ag.Reason = "not_compared", "the file's flow name is not known"
		case g.flow != baseFlow:
			ag.Status = "other_flow"
			rep.OtherFlow += g.count
		case typed && g.digest == frep.Digest:
			ag.Status = "same"
		case typed:
			ag.Status = "refused"
			rep.Refused += g.count
			if g.topo != nil && !g.consistent {
				ag.Reason = "its digest is not the digest of its own topology (a run recorded under digest v1, or a journal that does not match itself)"
			}
		case g.topo != nil && !g.consistent:
			// Structural: the recorded digest is not its topology's, so a resume under any flow of this version
			// refuses it for that reason alone.
			ag.Status = "refused"
			ag.Reason = "its digest is not the digest of its own topology (a run recorded under digest v1, or a journal that does not match itself)"
			rep.Refused += g.count
		case g.topo == nil:
			ag.Status, ag.Reason = "not_compared", g.topoNote
		case time.Since(start) > diffBudget:
			ag.Status, ag.Reason = "not_compared", "the time budget of the structural comparison was used"
		default:
			old := plan.InspectTopology(*g.topo)
			old.Digest = g.digest
			v, err := runview.DiffTopologies(runview.TopologyDiff{Old: old, New: frep, OldName: "the runs", NewName: s.name})
			if err != nil {
				ag.Status, ag.Reason = "not_compared", firstLine(err.Error()) // text of the file, not of a store
				break
			}
			if len(v.Changes) == 0 {
				ag.Status = "none_seen"
				break
			}
			ag.Status = "refused"
			rep.Refused += g.count
			for i, c := range v.Changes {
				if i == 5 {
					break
				}
				ag.Changes = append(ag.Changes, c.String())
			}
		}
		rep.Groups = append(rep.Groups, ag)
	}
	slices.SortStableFunc(rep.Groups, func(a, b affectedGroup) int {
		ra, rb := 0, 0
		if a.Status == "refused" {
			ra = -1
		}
		if b.Status == "refused" {
			rb = -1
		}
		return ra - rb
	})
	words := "at least "
	if !sc.more && sc.stopped == "" {
		words = ""
	}
	rep.Heading = fmt.Sprintf("Unfinished runs that would be refused on their next drive (resume, recovery or restart) with this file: %d of %s%d checked", rep.Refused, words, sc.listed)
	var notes []string
	if sc.more {
		notes = append(notes, fmt.Sprintf("Only the first %d unfinished runs by run ID were read; there may be more.", maxListed))
	}
	if sc.stopped != "" {
		notes = append(notes, "The scan stopped at "+sc.stopped+" after reading "+fmt.Sprint(sc.listed)+" runs.")
	}
	if sc.noDigest > 0 {
		notes = append(notes, fmt.Sprintf("%d unfinished %s no digest yet (all flows, not attributed); resume records the digest of whichever flow resumes %s, so %s not counted.", sc.noDigest, verb(sc.noDigest, "run records", "runs record"), pronoun(sc.noDigest), verb(sc.noDigest, "it is", "they are")))
	}
	if sc.unreadable > 0 {
		notes = append(notes, fmt.Sprintf("%d %s be read, or recorded a digest that is not a SHA-256 digest.", sc.unreadable, verb(sc.unreadable, "run could not", "runs could not")))
	}
	if rep.OtherFlow > 0 {
		notes = append(notes, fmt.Sprintf("%d %s of another flow than %q, which this file is not.", rep.OtherFlow, verb(rep.OtherFlow, "run is", "runs are"), baseFlow))
	}
	if sc.overflowDigests > 0 {
		notes = append(notes, fmt.Sprintf("%d more digest%s (%d runs) not compared: only the first %d are.", sc.overflowDigests, plural(sc.overflowDigests), sc.overflowRuns, maxGroups))
	}
	if !typed {
		notes = append(notes, "Kinds, types and gates are not compared without the program's blocks (-blocks), so a change in them would not show.")
	} else if s.typing.source == runview.SourceDescription {
		notes = append(notes, "The digest rests on the description file (-blocks), not checked against the program.")
	}
	notes = append(notes, "This lists unfinished runs in the store you gave; runs in other stores are not seen. A running process keeps the flow it started with; this is about the next drive.")
	rep.Notes = notes
	rep.Summary = rep.Heading + "."
	return rep.clean()
}

// verb picks the singular or plural words for n: one when n is 1.
func verb(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func pronoun(n int) string { return verb(n, "it", "them") }

func errorsOf(r plan.ConfigReport) int {
	n := 0
	for _, p := range r.Problems {
		if p.Severity == "error" {
			n++
		}
	}
	return n
}

// flowNameOf is the flow's name in the config text as the editor opened it, "" when it does not read.
func flowNameOf(text []byte, name string) string {
	data := text
	if formatOf(name) == "yaml" {
		var err error
		if data, err = ToJSON(text); err != nil {
			return ""
		}
	}
	var c struct {
		Flow string `json:"flow"`
	}
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	return c.Flow
}

// clean writes out the characters that do not print in every string of the reply (a run ID is the journal's).
func (r *affectedReply) clean() *affectedReply {
	fix := func(s *string) { *s = printable.Escape(*s) }
	fix(&r.Heading)
	fix(&r.Summary)
	for i := range r.Notes {
		fix(&r.Notes[i])
	}
	for i := range r.Groups {
		g := &r.Groups[i]
		fix(&g.Reason)
		for j := range g.IDs {
			fix(&g.IDs[j])
		}
		for j := range g.Changes {
			fix(&g.Changes[j])
		}
	}
	return r
}

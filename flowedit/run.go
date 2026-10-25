package flowedit

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/editrun"
	"github.com/bide-ai/bide/internal/printable"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
)

// init registers the constructor bide edit gives an editor its run with (see package editrun): no
// exported name of this package carries it.
func init() {
	editrun.NewOption = func(r editrun.Run) any {
		return Option(func(o *options) { o.run = &r })
	}
}

// runTimeout bounds one read of a run's journal.
const runTimeout = 30 * time.Second

// maxRunLines bounds each list of change lines a reply carries.
const maxRunLines = 200

// runSnap is what the editor read of the run, once: the run's facts and, when it journaled a topology
// that checks out, the topology and its digest. It is a snapshot, re-read only when the page asks, so a
// baseline never moves under an edit. It holds no record.
type runSnap struct {
	readAt time.Time
	info   runInfo
	// status is "ok" when topo is usable, and otherwise why it is not (see runReply.Status), with the
	// sentence that says so in claim.
	status, claim string
	topo          plan.Topology
	digest        string
	notes         []string
	// transient is set when the read failed in a way a later try may not (a cancelled or timed-out read,
	// a store error): such a result is returned and never kept.
	transient bool
}

// runInfo is what the page shows of the run.
type runInfo struct {
	ID        string `json:"id"`
	Flow      string `json:"flow,omitempty"`
	State     string `json:"state,omitempty"`
	StateText string `json:"state_text,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Records   int    `json:"records,omitempty"`
	// ReadAt is when the snapshot was read (RFC 3339, UTC).
	ReadAt string `json:"read_at,omitempty"`
}

// runReply answers /api/run. Without a run on the command line only Configured is false and the page
// shows no panel.
type runReply struct {
	Configured bool     `json:"configured"`
	Run        *runInfo `json:"run,omitempty"`
	// Status is "same", "none_seen", "differs" or "cannot" for a comparison made, and for none:
	// "no_topology", "no_digest", "mismatch", "too_large", "unreadable" or "not_found".
	Status string `json:"status,omitempty"`
	// Claim is the sentence the page shows, the whole of what the editor says about the run.
	Claim string `json:"claim,omitempty"`
	// Basis says what the digests rest on, in typed mode.
	Basis      string   `json:"basis,omitempty"`
	RunDigest  string   `json:"run_digest,omitempty"`
	FileDigest string   `json:"file_digest,omitempty"`
	Changes    []string `json:"changes,omitempty"`
	Weakened   []string `json:"weakened,omitempty"`
	// Strengthened lists each way the file's gates are stronger than the run's.
	Strengthened []string `json:"strengthened,omitempty"`
	// Renamed lists the session's renames the comparison honoured.
	Renamed []string `json:"renamed,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// serveRun answers /api/run: the file's text compared with the topology the run journaled.
func (s *Server) serveRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text    string   `json:"text"`
		Renames []Rename `json:"renames"`
		Reload  bool     `json:"reload"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if len(req.Text) > MaxFileSize {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the text is larger than %d bytes", MaxFileSize))
		return
	}
	if s.run == nil {
		reply(w, &runReply{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runTimeout)
	defer cancel()
	reply(w, s.compareRun(ctx, []byte(req.Text), req.Renames, req.Reload))
}

// runBackoff is how long a failed read is kept: a store that is down, or a journal in a format this version
// cannot read, is not read again on every keystroke, and Reload run reads at once. A var so a test can set it.
var runBackoff = 3 * time.Second

// runState is the server's one snapshot of its run, and the read in flight to make it.
type runState struct {
	mu       sync.Mutex
	snap     *runSnap
	until    time.Time // when a failed snapshot stops being kept; zero for a definitive one
	inflight *runFlight
	gen      int // the newest read started; an older read that finishes late is dropped
}

// runFlight is one read of the run, shared by every request that arrives while it is the newest.
type runFlight struct {
	done chan struct{}
	snap *runSnap
	gen  int
}

// snapshot returns the snapshot already read unless reload (or, for a failed read, unless runBackoff has
// passed). A read runs under a context of its own (not a request's: the first client leaving must not fail
// the others) and is shared by the requests that arrive while it runs; Reload run starts a fresh read
// and does not join an older one, whose answer is then dropped. A definitive result is kept; a failed
// one (the store, a cancelled or timed-out read) is kept for runBackoff only. A request that gives up
// waiting returns at once, whatever the store is doing.
func (s *Server) snapshot(ctx context.Context, reload bool) *runSnap {
	s.runs.mu.Lock()
	if s.runs.snap != nil && !reload && (s.runs.until.IsZero() || time.Now().Before(s.runs.until)) {
		sn := s.runs.snap
		s.runs.mu.Unlock()
		return sn
	}
	f := s.runs.inflight
	if f == nil || reload {
		s.runs.gen++
		f = &runFlight{done: make(chan struct{}), gen: s.runs.gen}
		s.runs.inflight = f
		run := *s.run
		go func() {
			rctx, cancel := context.WithTimeout(context.Background(), runTimeout)
			defer cancel()
			sn := readRun(rctx, run)
			s.runs.mu.Lock()
			if f.gen == s.runs.gen {
				s.runs.snap, s.runs.until = sn, time.Time{}
				if sn.transient {
					s.runs.until = time.Now().Add(runBackoff)
				}
				s.runs.inflight = nil
			}
			s.runs.mu.Unlock()
			f.snap = sn
			close(f.done)
		}()
	}
	s.runs.mu.Unlock()
	select {
	case <-f.done:
		return f.snap
	case <-ctx.Done():
		sn := &runSnap{readAt: time.Now().UTC(), info: runInfo{ID: s.run.ID}, transient: true}
		sn.status, sn.claim = "unreadable", "Run "+s.run.ID+" could not be read ("+runview.ErrClass(ctx.Err())+")."
		return sn
	}
}

// sixtyFourHex reports whether s is a SHA-256 digest as hex.
func sixtyFourHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}

// readRun reads the run's journal and what the editor needs of it. The journal is data someone else
// wrote: every refusal here names a state, never echoes a record.
func readRun(ctx context.Context, run editrun.Run) *runSnap {
	id := run.ID
	sn := &runSnap{readAt: time.Now().UTC(), info: runInfo{ID: id}}
	sn.info.ReadAt = sn.readAt.Format(time.RFC3339)
	refuse := func(status, claim string) *runSnap { sn.status, sn.claim = status, claim; return sn }
	if agent.IsReservedRun(id) {
		return refuse("unreadable", "Run "+id+" is not a run: the ID is reserved by the engine.")
	}
	recs, found, err := readFacts(ctx, run, &sn.info)
	if err != nil {
		sn.transient = true
		return refuse("unreadable", "Run "+id+" could not be read ("+runview.ErrClass(err)+").")
	}
	if !found {
		return refuse("not_found", "Run "+id+" has no records in this store.")
	}
	var topo, dig *agent.Record
	for i := range recs {
		switch recs[i].Name {
		case plan.TopologyStep:
			topo = &recs[i]
		case "flow:digest":
			dig = &recs[i]
		}
	}
	switch {
	case topo == nil:
		return refuse("no_topology", "Run "+id+" has no flow:topology record (it is not a flow run, or it started before that record was written), so there is nothing to compare.")
	case len(topo.Result) > MaxFileSize:
		return refuse("too_large", fmt.Sprintf("Run %s journaled a topology of more than %d bytes; it is not read.", id, MaxFileSize))
	}
	var digest string
	if dig == nil || json.Unmarshal(dig.Result, &digest) != nil {
		return refuse("no_digest", "Run "+id+" has not recorded its digest yet, so there is nothing to compare; resume will record whichever flow resumes it.")
	}
	if !sixtyFourHex(digest) {
		// The string is the journal's, of any length and content: it is not shown, only that it is not a digest.
		return refuse("mismatch", "Run "+id+" journaled a digest that is not a SHA-256 digest; its topology is not shown.")
	}
	sn.info.Digest = digest
	t, d, _, err := plan.JournaledTopology(recs)
	if err != nil {
		return refuse("mismatch", "Run "+id+" journaled a topology that does not match its digest; it is not shown.")
	}
	sn.topo, sn.digest, sn.status = t, d, "ok"
	if sn.info.Flow == "" {
		sn.info.Flow = t.Flow // the flow the topology names, which its digest commits to
	} else if sn.info.Flow != t.Flow {
		sn.notes = append(sn.notes, "The run started the flow "+sn.info.Flow+" but the topology it journaled names the flow "+t.Flow+".")
	}
	return sn
}

// endMarkers are the records that end a run, in the words the panel shows for each.
var endMarkers = []struct{ name, state, text string }{
	{"run:complete", "done", "completed"},
	{"run:aborted", "aborted", "aborted"},
	{"run:cancelled", "cancelled", "cancelled"},
}

// readFacts reads what the panel needs of the run: its flow:topology and flow:digest records, and, for the
// facts line, whether it has ended. From a store these are point reads (the run's first entry to tell it
// exists, the two records, and at most three end markers): no run:start, whose input can be large, and no
// history. The run's flow is then the topology's, which the digest commits to, and the record count is not
// known. An exported journal is already in memory, so its state and flow are read as the viewer reads them.
func readFacts(ctx context.Context, run editrun.Run, info *runInfo) (recs []agent.Record, found bool, err error) {
	js, ok := run.Source.(runview.JournalSource)
	if !ok {
		recs, err = run.Source.History(ctx, run.ID)
		if err != nil || len(recs) == 0 {
			return nil, false, err
		}
		v := runview.Build(ctx, run.ID, recs, nil)
		info.Flow, info.State, info.StateText, info.Records = v.Flow, v.State, v.StateText, v.Records
		return recs, true, nil
	}
	any := false
	for _, err := range js.J.Store().Load(ctx, run.ID, -1) {
		if err != nil {
			return nil, false, err
		}
		any = true
		break
	}
	if !any {
		return nil, false, nil
	}
	for _, name := range []string{plan.TopologyStep, "flow:digest"} {
		rec, ok, err := js.J.Get(ctx, run.ID, name)
		if err != nil {
			return nil, false, err
		}
		if ok {
			recs = append(recs, rec)
		}
	}
	info.State, info.StateText = "unfinished", "no end marker"
	for _, m := range endMarkers {
		_, ok, err := js.J.Get(ctx, run.ID, m.name)
		if err != nil {
			return nil, false, err
		}
		if ok {
			info.State, info.StateText = m.state, m.text
			break
		}
	}
	return recs, true, nil
}

// fileConfig is the text as the runview package reads a config: converted from YAML, and typed when the
// editor knows the program's blocks.
func (s *Server) fileConfig(text []byte) (runview.Config, error) {
	data := text
	if formatOf(s.name) == "yaml" {
		var err error
		if data, err = ToJSON(text); err != nil {
			return runview.Config{}, err
		}
	}
	if s.typing != nil {
		return s.typing.config(s.name, data), nil
	}
	return runview.Config{Name: s.name, Data: data}, nil
}

// compareRun compares the text with the run's journaled topology, by what the digest covers (see
// runview.DiffTopologies), and says what that does and does not show.
func (s *Server) compareRun(ctx context.Context, text []byte, renames []Rename, reload bool) *runReply {
	sn := s.snapshot(ctx, reload)
	info := sn.info
	rep := &runReply{Configured: true, Run: &info, Status: sn.status, Claim: sn.claim, Notes: slicesClone(sn.notes)}
	if sn.status != "ok" {
		return rep.clean()
	}
	id := info.ID
	file, err := s.fileConfig(text)
	if err != nil {
		rep.Status, rep.Claim = "cannot", "Cannot compare: the text is not a readable config ("+firstLine(err.Error())+")."
		return rep.clean()
	}
	topo, made := renameTopology(sn.topo, renames)
	old := plan.InspectTopology(topo)
	old.Digest = sn.digest // the digest the run journaled, not the renamed topology's
	typed := s.typing != nil
	v, err := runview.DiffTopologies(runview.TopologyDiff{Old: old, New: file.Inspect(), OldName: "run " + id, NewName: s.name, Typed: typed, Basis: file.Source()})
	if err != nil {
		rep.Status, rep.Claim = "cannot", "Cannot compare: "+firstLine(err.Error())+"."
		var big *runview.TooLargeError
		if errors.As(err, &big) {
			rep.Status = "too_large"
		}
		return rep.clean()
	}
	for _, m := range made {
		rep.Renamed = append(rep.Renamed, m.From+" -> "+m.To)
	}
	rep.RunDigest, rep.FileDigest = v.OldDigest, v.NewDigest
	for _, c := range v.Changes {
		if len(rep.Changes) < maxRunLines {
			rep.Changes = append(rep.Changes, c.String())
		}
	}
	rep.Weakened, rep.Strengthened = capLines(v.Weakened), capLines(v.Strengthened)
	basis := ""
	if typed {
		basis = " The digest rests on the program's Registry."
		if v.Basis == runview.SourceDescription {
			basis = " The digest rests on the description file (-blocks), not checked against the program."
		}
		rep.Basis = strings.TrimSpace(basis)
	}
	n := len(v.Changes)
	switch {
	case v.Cannot != "":
		rep.Status = "cannot"
		rep.Claim = "Cannot compare: " + v.Cannot + ". Fix them to compare with run " + id + "."
	case typed && v.OldDigest == v.NewDigest:
		rep.Status = "same"
		rep.Claim = "This file has the topology digest " + v.NewDigest + ", the same digest run " + id + " journaled. This does not say the run followed this file or what any step did; that is conformance (bide view)." + basis
	case !typed && n == 0 && len(made) == 0:
		rep.Status = "none_seen"
		rep.Claim = "No difference in flow name, entry, node names, blocks, edges, switch arms or their order between this file and the topology run " + id + " journaled. Kinds, types and gates are not compared in this mode, so a change in them would not show. Load the program's blocks (-blocks) to compare digests."
	default:
		rep.Status = "differs"
		what := fmt.Sprintf("%d change%s the digest covers", n, plural(n))
		switch {
		case n == 0 && len(made) > 0:
			what = "the renames made in this session"
			// With the renames undone the digests are compared as they are: if they still differ, the
			// renames are not the whole difference.
			if typed {
				if rd, err := plan.TopologyDigest(topo); err != nil || rd != v.NewDigest {
					what += ", and the digest differs in order or types as well"
				} else {
					what = "only the renames made in this session"
				}
			}
		case n == 0:
			what = "the digest differs in order or types"
		}
		rep.Claim = "This file differs from the topology run " + id + " journaled (" + what + "). The digest is part of what resume checks, so a run paused under that topology will not resume under this file." + basis
	}
	if v.NewErrors > 0 && rep.Status != "cannot" {
		rep.Notes = append(rep.Notes, fmt.Sprintf("The file has %d error%s (see Problems); the comparison was made on what could be read.", v.NewErrors, plural(v.NewErrors)))
	}
	rep.Notes = append(rep.Notes,
		"Safety and assert lines are not in a journal or in the digest, so they are not compared.",
		"The journal is read as it records itself: the digest check shows the topology is the one its digest names, not that the run ran it (bide view -sth proves the journal).")
	if !typed {
		rep.Notes = append(rep.Notes, "Gates are compared only when the editor knows the program's blocks (-blocks), because a journaled gate is the resolved policy, which includes gates a block declares in Go.")
	}
	return rep.clean()
}

// clean writes out every character that does not print (a control or direction-changing character, a
// zero-width one) in every string of the reply: names, blocks and flow names come from a journal someone
// else wrote, and a name must not read as another in a change line, or carry a control character into a copy.
// The HTML is already safe (the page uses text); this is the same rule bide view and bide check apply.
func (r *runReply) clean() *runReply {
	fix := func(s *string) { *s = printable.Escape(*s) }
	fixAll := func(l []string) {
		for i := range l {
			fix(&l[i])
		}
	}
	fix(&r.Claim)
	fix(&r.Basis)
	fixAll(r.Changes)
	fixAll(r.Weakened)
	fixAll(r.Strengthened)
	fixAll(r.Renamed)
	fixAll(r.Notes)
	if r.Run != nil {
		fix(&r.Run.ID)
		fix(&r.Run.Digest)
		fix(&r.Run.Flow)
		fix(&r.Run.State)
		fix(&r.Run.StateText)
	}
	return r
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func capLines(l []string) []string {
	if len(l) > maxRunLines {
		return l[:maxRunLines]
	}
	return l
}

func slicesClone(l []string) []string { return append([]string(nil), l...) }

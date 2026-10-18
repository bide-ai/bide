package runview

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// StopFeed is how the viewer reads the kill switch: the control run of the journal it shows, folded by an
// agent.StopControl in the mode the viewer was given (a stop policy makes it audited and its signatures
// checked; without one it reads as a basic control, and says that signatures are not verified). Reading is
// all it does: the viewer never sets or lifts a stop.
type StopFeed struct {
	Control *agent.StopControl
	// Plain, set with an audited Control, is a basic control over the same journal: the stops it sees that
	// are not signed are stops a basic process enforces and an audited one ignores, which the banner says
	// (a deployment that has not upgraded, read by a viewer given the stop policy).
	Plain *agent.StopControl
	// Verified is true when the control checks signatures: the viewer was given the deployment's stop policy.
	Verified bool
}

// maxBannerStops bounds the stops the banner lists; the count says how many more there are.
const maxBannerStops = 10

// maxBannerRejected bounds the rejected records the banner names.
const maxBannerRejected = 5

// StopLine is one active stop as the banner shows it.
type StopLine struct {
	ID           string `json:"id"`
	Scope        string `json:"scope"`
	Reason       string `json:"reason"`
	By           string `json:"by"`
	Host         string `json:"host,omitempty"`
	At           string `json:"at"`
	Age          string `json:"age"`
	Signed       bool   `json:"signed"`
	Compensation string `json:"compensation"`
	LiftsHeld    int    `json:"lifts_held"`
	LiftsNeeded  int    `json:"lifts_needed"`
}

// StopBanner is the kill switch's state as every page of the viewer shows it, and /stops.json serves it.
type StopBanner struct {
	// Mode is the control's mode, basic or audited. Verified says whether signatures were checked.
	Mode     string `json:"mode"`
	Verified bool   `json:"signatures_verified"`
	// Active are the stops in force (the first maxBannerStops), MoreActive how many more there are.
	Active     []StopLine `json:"active"`
	MoreActive int        `json:"more_active,omitempty"`
	// Unknowable says why the stops cannot be judged (a basic viewer on a signed deployment, a policy chain it
	// cannot walk): the engine holds side-effect calls a stop could cover while that is so.
	Unknowable string `json:"unknowable,omitempty"`
	// Unverified counts signed records a viewer with no stop policy cannot check.
	Unverified int `json:"unverified,omitempty"`
	// Rejected are records in the control run that have no effect (a forged record, a signer the policy does
	// not enrol); RejectedTotal counts them all.
	Rejected      []string `json:"rejected,omitempty"`
	RejectedTotal int      `json:"rejected_total,omitempty"`
	// ReadErr says the control run could not be read, by class: the banner never claims "no stop" then.
	ReadErr string `json:"read_error,omitempty"`
	// PlainIgnored counts plain (unsigned) stops in force for basic processes that this audited view ignores:
	// a deployment that has not moved to audited mode, or a process still configured basic, is stopped while
	// the audited processes are not.
	PlainIgnored int `json:"plain_ignored,omitempty"`
	// ReadAt is when the control run was read (the banner is served again for a couple of seconds).
	ReadAt string `json:"read_at,omitempty"`
}

// Show reports whether the banner has anything to say. A control run with no stop, no rejected record and
// nothing unreadable shows no banner.
func (b StopBanner) Show() bool {
	return len(b.Active) > 0 || b.MoreActive > 0 || b.Unknowable != "" || b.Unverified > 0 || b.RejectedTotal > 0 || b.ReadErr != "" || b.PlainIgnored > 0
}

// Stopped reports whether a stop is in force.
func (b StopBanner) Stopped() bool { return len(b.Active) > 0 || b.MoreActive > 0 }

// stopRead is one read of the control run, or why it failed.
type stopRead struct {
	st  agent.StopState
	err error
	// plain counts the unsigned stops in force in a basic reading of the same journal (see StopFeed.Plain).
	plain int
	at    time.Time
}

// stopState reads the control run, served again for cacheTTL, so many viewers and a refreshing page cost the
// store one read.
func (o Options) stopState(ctx context.Context) (stopRead, bool) {
	if o.Stops == nil || o.Stops.Control == nil {
		return stopRead{}, false
	}
	read := func() stopRead {
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildTimeout)
		defer cancel()
		st, err := o.Stops.Control.State(bctx)
		r := stopRead{st: st, err: err, at: time.Now()}
		if err == nil && o.Stops.Plain != nil {
			if ps, perr := o.Stops.Plain.State(bctx); perr == nil {
				for _, s := range ps.Active() {
					if !s.Signed {
						r.plain++
					}
				}
			}
		}
		return r
	}
	if o.cache == nil {
		return read(), true
	}
	return o.cache.get("stops", func() any { return read() }).(stopRead), true
}

// banner is the page banner for r.
func (o Options) banner(ctx context.Context) (StopBanner, bool) {
	r, ok := o.stopState(ctx)
	if !ok {
		return StopBanner{}, false
	}
	return buildStopBanner(r, o.Stops.Verified, o.now()), true
}

// buildStopBanner folds one read of the control run into the banner. Text that came from the store (a
// reason, a signer, a host) is cleaned of control and bidirectional characters and cut, so a record cannot
// dress the banner up.
func buildStopBanner(r stopRead, verified bool, now time.Time) StopBanner {
	b := StopBanner{Mode: string(r.st.Mode), Verified: verified, Active: []StopLine{}}
	if !r.at.IsZero() {
		b.ReadAt = r.at.UTC().Format(time.RFC3339)
	}
	if r.err != nil {
		b.ReadErr = errClass(r.err)
		b.Mode = ""
		return b
	}
	if err := r.st.UnknowableErr(); err != nil {
		b.Unknowable = cleanLine(err.Error(), 300)
	}
	b.Unverified = r.st.Unverified
	b.PlainIgnored = r.plain
	for _, s := range r.st.Active() {
		if len(b.Active) == maxBannerStops {
			b.MoreActive++
			continue
		}
		b.Active = append(b.Active, StopLine{ID: s.ID, Scope: cleanLine(s.Scope.String(), 300), Reason: cleanLine(s.Reason, agent.MaxStopReason), By: cleanLine(s.By, 100), Host: cleanLine(s.Host, 100),
			At: cleanLine(s.At, 40), Age: stopAge(s.At, now), Signed: s.Signed, Compensation: s.Compensation, LiftsHeld: s.LiftsHeld, LiftsNeeded: s.LiftsNeeded})
	}
	b.RejectedTotal = r.st.RejectedTotal
	for _, rj := range r.st.Rejected {
		if len(b.Rejected) == maxBannerRejected {
			break
		}
		b.Rejected = append(b.Rejected, cleanLine(rj.Name+": "+rj.Reason, 300))
	}
	return b
}

// cleanLine drops control, bidirectional and unprintable characters from a store-supplied string and cuts it
// to n runes.
func cleanLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '‪' || r == '‫' || r == '‬' || r == '‭' || r == '‮' || (r >= '⁦' && r <= '⁩') || r == '‎' || r == '‏' || r == '؜':
			return -1
		}
		return r
	}, printable(s))
	return shorten(s, n)
}

func stopAge(at string, now time.Time) string {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return "an unknown time"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// render writes a page, with the kill switch banner as the first thing in <main> when the viewer reads a
// control run that has something to say. The banner is built by the same template engine as the page (so
// every store-supplied word in it is escaped) and placed after the layout's own <main>, which is the first
// one in the output: the page's content is escaped, so it cannot hold another.
func (o Options) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	var page bytes.Buffer
	if err := pages.ExecuteTemplate(&page, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := page.Bytes()
	if b, ok := o.banner(r.Context()); ok && b.Show() {
		var ban bytes.Buffer
		if err := pages.ExecuteTemplate(&ban, "stopbanner", b); err == nil {
			if i := bytes.Index(out, []byte("<main>")); i >= 0 {
				i += len("<main>")
				out = append(append(append([]byte{}, out[:i]...), ban.Bytes()...), out[i:]...)
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(out)
}

// serveStops answers GET /stops.json: the banner as JSON, or 404 when the viewer reads no control run.
func (o Options) serveStops(w http.ResponseWriter, r *http.Request) {
	b, ok := o.banner(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, b)
}

// StopHold says a stop holds, or may hold, a run.
type StopHold struct {
	Stop  string `json:"stop"` // the stop's ID
	Scope string `json:"scope"`
	// May is true when the journal cannot say: an agent stop covers the runs of an agent identity, which the
	// journal does not record, so any unfinished run may be one of them.
	May bool `json:"may,omitempty"`
}

// Text is the hold as the page says it.
func (h StopHold) Text() string {
	if h.May {
		return "may be held by stop " + short8(h.Stop) + " (" + h.Scope + ")"
	}
	return "held by stop " + short8(h.Stop) + " (" + h.Scope + ")"
}

// heldBy lists the active stops that hold or may hold v at its next drive. A stop never holds a run that
// has ended, or one halted for an unknown outcome (a stop never fires a call again, and a halted run waits for
// a person), nor a run of durable steps with no run:start (Journal.Step calls are not driven by an agent or a
// flow, so no check point sees them). A global stop holds every other run; a flow stop the runs of that flow; a tool stop a run with a
// call to that tool the journal shows no result for; an agent stop may hold any of them, since the journal does
// not record the agent identity that drives a run (agent.Identity.Actor is the deployment's, not journaled).
func heldBy(st agent.StopState, v *View) []StopHold {
	if v == nil || v.Kind == KindSteps || ended(v.State) || v.State == "halted" {
		return nil
	}
	var out []StopHold
	for _, s := range st.Active() {
		hold := StopHold{Stop: s.ID, Scope: s.Scope.String()}
		switch s.Scope.Kind {
		case agent.StopAgent:
			hold.May = true
		default:
			held := s.Covers(agent.StopQuery{Flow: v.Flow})
			for _, t := range v.openTools {
				held = held || s.Covers(agent.StopQuery{Tool: t})
			}
			if !held {
				continue
			}
			// A saga that is rolling back keeps compensating under a stop that does not block compensation:
			// its forward calls are held, its rollback is not.
			if v.rollingBack && s.Compensation != agent.CompensationBlock {
				hold.May = true
			}
		}
		out = append(out, hold)
	}
	return out
}

// maxHeldRead bounds how many unfinished runs HeldCount reads.
var maxHeldRead = 2000

// HeldCounts is how many of a store's unfinished runs one stop holds (see HeldCount).
type HeldCounts struct {
	// Held runs are held by the stop for certain; May runs may be (an agent stop: the journal does not record
	// the agent identity that drives a run). Read is how many unfinished runs were read, Unread how many could
	// not be, and Truncated says more than maxHeldRead were listed and only the first were read.
	Held, May, Read, Unread int
	Truncated               bool
	// ListErr is set when the runs could not be listed, so nothing was counted. Incomplete is set when the count
	// was cut off (its context ended) after counting some: the counts are then lower bounds.
	ListErr    string
	Incomplete bool
}

// HeldCount reads the unfinished runs of src and counts those the stop e holds, or may hold: what a person
// lifting a stop is about to let carry on. It is a read of every unfinished run, bounded by maxHeldRead.
func HeldCount(ctx context.Context, src JournalSource, e agent.StopEntry) HeldCounts {
	var c HeldCounts
	ids, err := src.UnfinishedLimit(ctx, maxHeldRead+1)
	if err != nil {
		c.ListErr = errClass(err)
		return c
	}
	if len(ids) > maxHeldRead {
		ids, c.Truncated = ids[:maxHeldRead], true
	}
	e.Lifted = false
	st := agent.StopState{Mode: agent.StopBasic, Stops: []agent.StopEntry{e}}
	for _, id := range ids {
		if ctx.Err() != nil {
			c.Incomplete = true
			c.ListErr = errClass(ctx.Err())
			return c
		}
		recs, err := src.History(ctx, id)
		if err != nil {
			c.Unread++
			continue
		}
		c.Read++
		for _, h := range heldBy(st, Build(ctx, id, recs, nil)) {
			if h.May {
				c.May++
			} else {
				c.Held++
			}
		}
	}
	return c
}

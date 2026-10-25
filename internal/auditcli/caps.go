package auditcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/capbridge"
	"github.com/bide-ai/bide/internal/capledger"
	"github.com/bide-ai/bide/internal/journalhook"
)

// Finding kinds of bide audit caps. Each is a reason the audit fails.
const (
	capCorrupt       = "CORRUPT"                    // the ledger breaks its own accept rules
	capUnreadable    = "UNREADABLE"                 // a ledger or a run that cannot be decoded
	capTotal         = "TOTAL"                      // the recomputed window total is not the last cum_after
	capDouble        = "DOUBLE"                     // one attempt holds two live reserves
	capDeleted       = "DELETED"                    // a cap-reserve record names a slot the ledger does not hold
	capChanged       = "CHANGED"                    // a slot differs from the cap-reserve record that names it
	capUnrecorded    = "UNRECORDED"                 // a reserve slot with no cap-reserve record in its run
	capBadHead       = "BAD_HEAD"                   // a signed head that does not verify
	capDivergent     = "DIVERGENT_HEAD"             // a signed head the ledger run does not match
	capNoHead        = "NOT_ATTESTED"               // entries no signed head covers
	capHeadOnly      = "LEDGER_MISSING"             // a signed head or a cap-reserve record for a ledger the store does not hold
	capIntentBad     = "INTENT_UNREADABLE"          // a cap-intent record that does not decode, or sits under another attempt's name
	capIntentNone    = "INTENT_MISSING"             // a reserve slot whose attempt has no cap-intent
	capIntentDup     = "INTENT_DUPLICATE"           // two cap-intents for one attempt
	capIntentDiff    = "INTENT_MISMATCH"            // an intent that differs from the slot it led to (rule, key, amount, currency, marking, window)
	capIndexNone     = "INDEX_MISSING"              // an intent whose window the rule's index run does not hold
	capIntentOld     = "INTENT_CLOCK"               // an intent whose window is earlier than the newest window the index held when it was written
	capIntentWin     = "INTENT_WINDOW"              // an intent whose window is not the window of the call's own time
	capIntentUndated = "INTENT_UNDATED"             // warning: an intent whose call has no start time to date it by
	capRunHead       = "RUN_NOT_ATTESTED"           // cap records of a run that no signed head of that run covers
	capReleaseRan    = "RELEASE_OF_A_CALL_THAT_RAN" // a release of a reservation whose call has a result the tool produced
	capReleaseTail   = "RELEASE_UNATTESTED"         // a release whose verdict rests on records past the run's newest signed head
	capReleaseUnk    = "RELEASE_UNCONFIRMED"        // warning: a release whose attempt is neither voided nor answered
)

// The markers that end a run (agent's runCompleteStep, runAbortedStep and runCancelledStep, which are not
// exported): a run with none of them is still going, or died, and a record it has not yet written may be
// merely late.
var capEndMarkers = []string{"run:complete", "run:aborted", "run:cancelled"}

// capsFinding is one problem bide audit caps found.
type capsFinding struct {
	Kind   string `json:"kind"`
	Ledger string `json:"ledger"`
	Slot   int    `json:"slot"` // -1 when the finding is not about one slot
	Detail string `json:"detail"`
	// Warn marks a finding that is reported and does not fail the audit.
	Warn bool `json:"warn,omitempty"`
}

// capsOver is a reservation the ledger counted over the rule's limit under a waiver.
type capsOver struct {
	Slot     int    `json:"slot"`
	Attempt  string `json:"attempt"`
	CumAfter int64  `json:"cum_after"`
	Limit    int64  `json:"limit"`
}

// capsLedger is the report of one ledger run.
type capsLedger struct {
	Ledger     string     `json:"ledger"`
	Window     string     `json:"window"`
	Slots      int        `json:"slots"`
	Live       int        `json:"live"`
	Total      int64      `json:"total"`
	Currency   string     `json:"currency,omitempty"`
	Limit      int64      `json:"limit"`
	Over       []capsOver `json:"over,omitempty"`
	Entries    int        `json:"entries"`
	Attested   int        `json:"attested"`
	Tail       int        `json:"tail"`
	Exposed    int        `json:"exposed"`
	ExposedSum int64      `json:"exposed_amount"`
}

// capsReport is what bide audit caps found; -json embeds it as the artifact.
type capsReport struct {
	Anchored   bool          `json:"anchored"`
	Ledgers    []capsLedger  `json:"ledgers"`
	Findings   []capsFinding `json:"findings"`
	NotChecked []string      `json:"not_checked,omitempty"`
	// Ignored says which signed heads the audit did not use (information, not a limit).
	Ignored string `json:"ignored,omitempty"`
	// OrphanIntents are the attempts with a cap-intent and no slot: interrupted before the slot (information).
	OrphanIntents []string `json:"orphan_intents,omitempty"`
}

// capsFilter limits what bide audit caps reads. The segments are as encoded in a ledger run's ID.
type capsFilter struct {
	ns, rule, window string // encoded segments, "" for any
	run              string // a run: only its calls and its sub-runs' calls
}

// matchesRule is matches without the window filter.
func (f capsFilter) matchesRule(ledger string) bool {
	g := f
	g.window = ""
	return g.matches(ledger)
}

func (f capsFilter) any() bool { return f.ns != "" || f.rule != "" || f.window != "" || f.run != "" }

// matches reports whether a ledger run passes the namespace, rule and window filters.
func (f capsFilter) matches(ledger string) bool {
	ns, rule, _, window, _, ok := capledger.LedgerSegments(ledger)
	return ok && (f.ns == "" || f.ns == ns) && (f.rule == "" || f.rule == rule) && (f.window == "" || f.window == window)
}

// inRun reports whether the calls of run id are in scope.
func (f capsFilter) inRun(id string) bool {
	return f.run == "" || id == f.run || strings.HasPrefix(id, f.run+">") && agent.IsSubRun(id)
}

// caps audits the cap ledgers of a store: that each reads under the ledger's own rules, that its
// window total follows from its slots, that its slots and the cap-reserve records of the runs that
// made the calls agree, and, with signed heads, that the ledger is the one they sign. It reads only.
func (c *cli) caps(args []string) {
	fs := c.flagSet("caps")
	var heads stringList
	fs.Var(&heads, "sth", "a signed tree head over a ledger run, as JSON (an entry's sth from the anchor log of the AuditedStore that anchors the store; a file may hold the heads of many runs, one JSON value after another); repeat for as many as you have: each must verify and match a prefix of its ledger, the largest is the newest")
	pubkey := fs.String("pubkey", "", "the "+pubkeyHelp+"; it verifies every -sth")
	unanchored := fs.Bool("unanchored", false, "read the ledgers with no signed head: whoever can write the store can delete a slot or append a release undetected, and this audit only finds inconsistencies it can see")
	db := fs.String("db", "", "read the ledgers and the runs from this SQLite journal (opened read-only)")
	pgEnv := fs.String("postgres-env", "", "read them from the Postgres journal whose DSN is in this environment variable (never given on the command line)")
	ns := fs.String("namespace", "", "only the ledgers of this cap namespace (agent.WithCapNamespace)")
	rule := fs.String("rule", "", "only the ledgers of this cap rule")
	window := fs.String("window", "", "only the ledgers of this window (a day 2006-01-02, an hour 2006-01-02T15, or a root run's ID)")
	run := fs.String("run", "", "only the calls of this run and its sub-runs: their slots and cap-reserve records, and the ledgers that hold them; the other ledgers and runs are not checked, and the report says so")
	c.parse(fs, args)

	haveStore := *db != "" || *pgEnv != ""
	switch {
	case *db != "" && *pgEnv != "":
		c.usageLine(fs.Name() + ": give one of -db and -postgres-env, not both; run `" + fs.Name() + " -h` for usage")
	case !haveStore:
		c.usageLine(fs.Name() + ": missing -db or -postgres-env; run `" + fs.Name() + " -h` for usage")
	case c.storeOpeners() == nil:
		c.usageLine(fs.Name() + ": this build reads no store; -db and -postgres-env are in the full bide command")
	case *unanchored && len(heads) > 0:
		c.usageLine(fs.Name() + ": give signed heads (-sth, with -pubkey) or -unanchored, not both")
	case !*unanchored && len(heads) == 0:
		c.usageLine(fs.Name() + ": missing -sth (and -pubkey); give -unanchored to read ledgers nothing attests")
	case len(heads) > 0 && *pubkey == "":
		c.usageLine(fs.Name() + ": -sth needs -pubkey")
	case *pubkey != "" && len(heads) == 0:
		c.usageLine(fs.Name() + ": -pubkey verifies -sth; there is none")
	}
	if *unanchored {
		c.println("UNANCHORED: whoever can write the store can delete a slot or append a release undetected; this audit only finds inconsistencies it can see")
	}

	var in capsInput
	ok := true
	if *pubkey != "" {
		v, err := readPubKey(*pubkey)
		ok = c.note(err) && ok
		in.verifier = v
	}
	for _, p := range heads {
		hs, err := c.readHeadFile(p)
		ok = c.note(err) && ok
		in.heads = append(in.heads, hs...)
	}
	if !ok {
		return
	}
	in.anchored = len(heads) > 0

	var s NotifyStores
	var err error
	if *db != "" {
		s, err = c.storeOpeners().SQLite(*db)
	} else {
		dsn := os.Getenv(*pgEnv)
		if dsn == "" {
			c.note(unusable(fmt.Errorf("-postgres-env %s: the variable is empty or not set", *pgEnv)))
			return
		}
		s, err = c.storeOpeners().Postgres(dsn)
	}
	if err != nil {
		c.note(unusable(fmt.Errorf("open the store: %w", err)))
		return
	}
	defer s.Close()

	j, err := agent.NewJournal(s.Journal)
	if err != nil {
		c.note(unusable(err))
		return
	}
	led := journalhook.CapLedger(j, time.Now, 0)
	in.filter = capsFilter{run: *run}
	if *ns != "" {
		in.filter.ns = led.Segment(*ns)
	}
	if *rule != "" {
		in.filter.rule = led.Segment(*rule)
	}
	if *window != "" {
		in.filter.window = led.Segment(*window)
	}
	rep, err := auditCaps(context.Background(), s.Journal, j, led, capbridge.Voided(), in)
	if err != nil {
		c.note(unusable(err))
		return
	}
	c.capsReport(rep)
	if b, merr := json.Marshal(rep); merr == nil {
		c.artifact = b
	}
	if in.filter.any() && len(rep.Ledgers) == 0 && len(rep.Findings) == 0 {
		c.note(unusable(errors.New("no cap ledger matches -namespace, -rule, -window and -run: nothing was checked")))
		return
	}
	if n := rep.failures(); n > 0 {
		c.fail("%d problem(s): the cap ledgers do not hold together", n)
	}
	slots, live := 0, 0
	for _, l := range rep.Ledgers {
		slots += l.Slots
		live += l.Live
	}
	unchecked := ""
	warnings := len(rep.Findings) - rep.failures()
	switch n := len(rep.NotChecked); {
	case n > 0 && warnings > 0 && !in.filter.any():
		unchecked = fmt.Sprintf(" (with %d NOT CHECKED note(s) and %d warning(s) above)", n, warnings)
	case n > 0 && !in.filter.any():
		unchecked = fmt.Sprintf(" (with %d NOT CHECKED note(s) above)", n)
	case warnings > 0:
		unchecked = fmt.Sprintf(" (with %d warning(s) above)", warnings)
	}
	switch {
	case in.filter.any():
		c.printf("PASS (filtered): the %d ledger(s) checked hold together (%d slots, %d live); the checks that were NOT made are listed above\n", len(rep.Ledgers), slots, live)
	case in.anchored:
		c.printf("PASS: %d ledger(s), %d slot(s), %d live: each reads under the ledger's rules, agrees with the runs' cap-reserve records, and is unchanged since its newest signed head, and so are the cap records of its runs (a signature covers what the store was given at signing, not that it was legitimate)%s\n", len(rep.Ledgers), slots, live, unchecked)
	default:
		c.printf("PASS: %d ledger(s), %d slot(s), %d live: each reads under the ledger's rules and agrees with the runs' cap-reserve records; nothing attests that the store is complete (UNANCHORED)%s\n", len(rep.Ledgers), slots, live, unchecked)
	}
}

// capsInput is what the audit reads besides the store.
type capsInput struct {
	filter   capsFilter
	anchored bool
	verifier audit.Verifier
	heads    []audit.SignedTreeHead
}

// capClaim is a cap-reserve record: the run that holds it and what it says of a slot.
type capClaim struct {
	run, name string
	rec       capledger.Reserve
}

// auditCaps reads every cap ledger of the store and every run's cap-reserve records, and compares them.
// An error is a store it could not read; everything it finds wrong with what it read is a finding.
func auditCaps(ctx context.Context, store agent.Store, j *agent.Journal, led *capledger.Ledger, voided capbridge.VoidedFunc, in capsInput) (capsReport, error) {
	rep := capsReport{Anchored: in.anchored, Ledgers: []capsLedger{}, Findings: []capsFinding{}}
	if voided == nil {
		return rep, errors.New("internal error: the reconcile bridge of package agent is not registered")
	}
	lister, ok := agent.Capability[agent.Lister](store)
	if !ok {
		return rep, errors.New("the store cannot list its runs")
	}
	var ledgers, runs []string
	for id, err := range lister.Runs(ctx, agent.RunFilter{}) {
		if err != nil {
			return rep, fmt.Errorf("list runs: %w", err)
		}
		switch {
		case agent.IsCapRun(id):
			if _, _, _, _, _, isLedger := capledger.LedgerSegments(id); isLedger && in.filter.matches(id) {
				ledgers = append(ledgers, id)
			}
		case agent.IsReservedRun(id):
		case in.filter.inRun(id):
			runs = append(runs, id)
		}
	}
	slices.Sort(ledgers)
	slices.Sort(runs)
	find := func(kind, ledger string, slot int, format string, args ...any) {
		rep.Findings = append(rep.Findings, capsFinding{Kind: kind, Ledger: ledger, Slot: slot, Detail: fmt.Sprintf(format, args...)})
	}
	warn := func(kind, ledger string, slot int, format string, args ...any) {
		rep.Findings = append(rep.Findings, capsFinding{Kind: kind, Ledger: ledger, Slot: slot, Detail: fmt.Sprintf(format, args...), Warn: true})
	}

	// The cap-reserve records of the runs in scope, by the ledger they name.
	claims := map[string][]capClaim{}
	intents := map[string][]capIntent{} // by attempt
	rs := capRunFacts{results: map[string]map[string]bool{}, notes: map[string]map[string][]byte{}, ended: map[string]bool{}, unreadable: map[string]bool{}, covered: map[string]bool{}, started: map[string]time.Time{},
		attempted: map[string]map[string]time.Time{}, capRecs: map[string]int{}, records: map[string]int{}, unmarked: map[string]bool{}, oldReleases: map[string]bool{}}
	for _, id := range runs {
		recs, err := j.History(ctx, id)
		if err != nil {
			// One run that cannot be read (a corrupt row, a record that does not decode) is a finding on
			// that run; the audit goes on with the rest. Only the store itself being unreadable (it cannot
			// be opened or its runs cannot be listed) ends the audit with exit 4.
			rs.unreadable[id] = true
			find(capUnreadable, "", -1, "run %s cannot be read, so its cap-reserve, cap-intent and cap-release records are not cross-checked: %v", cleanText(id), err)
			continue
		}
		rs.records[id] = len(recs)
		for i, r := range recs {
			if capledger.IsIntentName(r.Name) || capledger.IsReserveName(r.Name) {
				rs.capRecs[id] = i + 1
			}
			if r.Name == "run:start" && r.Kind == agent.StepValue {
				var st struct {
					StartedAt string `json:"started_at"`
				}
				if json.Unmarshal(r.Result, &st) == nil {
					if t, err := agent.ParseTimestamp(st.StartedAt); err == nil {
						rs.started[id] = t
					}
				}
			}
			if r.Kind == agent.StepAttempt {
				// The marker's recorded_at is the claim on the run's own clock (its AttemptedAt is the
				// wall clock, which a run driven with WithClock does not share); a marker written before
				// the field has none and dates nothing.
				if t, err := agent.ParseTimestamp(r.RecordedAt()); err == nil {
					if rs.attempted[id] == nil {
						rs.attempted[id] = map[string]time.Time{}
					}
					rs.attempted[id][r.Name] = t
				}
			}
			if slices.Contains(capEndMarkers, r.Name) {
				rs.ended[id] = true
			}
			if r.Kind == agent.StepToolResult {
				if rs.results[id] == nil {
					rs.results[id] = map[string]bool{}
				}
				rs.results[id][r.ToolUseID] = r.IsError
			}
			if r.Kind == agent.StepValue && capledger.IsReleaseName(r.Name) {
				if rs.notes[id] == nil {
					rs.notes[id] = map[string][]byte{}
				}
				rs.notes[id][r.Name] = []byte(r.Result)
			}
			if r.Kind == agent.StepValue && capledger.IsIntentName(r.Name) {
				in, err := capledger.DecodeIntent(r.Result)
				switch {
				case err != nil:
					find(capIntentBad, "", -1, "run %s, %s: %v", id, r.Name, err)
				case r.Name != led.IntentName(in.Attempt):
					find(capIntentBad, "", -1, "run %s holds %s, which names the attempt %s: not the name of that attempt's intent", id, r.Name, cleanText(in.Attempt))
				default:
					intents[in.Attempt] = append(intents[in.Attempt], capIntent{run: id, in: in})
				}
				continue
			}
			if r.Kind != agent.StepValue || !capledger.IsReserveName(r.Name) {
				continue
			}
			res, err := capledger.DecodeReserve(r.Result)
			if err != nil {
				find(capChanged, "", -1, "run %s, %s: %v", id, r.Name, err)
				continue
			}
			claims[res.Ledger] = append(claims[res.Ledger], capClaim{run: id, name: r.Name, rec: res})
		}
	}

	// The signed heads, by the ledger run they sign.
	byRun := map[string][]audit.SignedTreeHead{}
	otherHeads := 0
	for _, h := range in.heads {
		if h.Kind == audit.TreeJournal && rs.capRecs[h.RunID] > 0 {
			continue // a head of a run that holds cap records: read below, with the run
		}
		if !agent.IsCapRun(h.RunID) || h.Kind != audit.TreeJournal {
			otherHeads++
			continue
		}
		if !in.filter.matches(h.RunID) {
			continue
		}
		if err := h.Verify(in.verifier); err != nil {
			find(capBadHead, h.RunID, -1, "a signed head of size %d does not verify under -pubkey: %v", h.Size, err)
			continue
		}
		byRun[h.RunID] = append(byRun[h.RunID], h)
	}

	slotted := map[string]bool{} // the attempts that hold a reserve slot in the ledgers read
	if in.anchored {
		for _, id := range runs {
			if rs.capRecs[id] == 0 {
				continue
			}
			recs, err := j.History(ctx, id)
			if err != nil {
				continue // already reported when it was read for its records
			}
			var runHeads []audit.SignedTreeHead
			for _, h := range in.heads {
				if h.Kind != audit.TreeJournal || h.RunID != id {
					continue
				}
				if err := h.Verify(in.verifier); err != nil {
					find(capBadHead, id, -1, "a signed head of size %d does not verify under -pubkey: %v", h.Size, err)
					continue
				}
				runHeads = append(runHeads, h)
			}
			cov, err := audit.CheckDecisionCoverage(id, recs, runHeads, in.verifier, audit.CoverageBound{})
			if err != nil {
				return rep, err
			}
			for _, d := range cov.DivergentHeads {
				find(capDivergent, id, -1, "run %s: the signed head of size %d does not match the run (a record was changed or deleted): %s", cleanText(id), d.Size, d.Reason)
			}
			rs.covered[id] = cov.Heads > 0 && cov.LargestHead >= len(recs)
			switch {
			case cov.Heads == 0:
				find(capRunHead, id, -1, "no signed head covers run %s, whose cap-intent and cap-reserve records the audit relies on: NOT ATTESTED", cleanText(id))
			case cov.LargestHead < rs.capRecs[id]:
				find(capRunHead, id, -1, "run %s: its cap records run to entry %d and the newest signed head covers %d: NOT ATTESTED", cleanText(id), rs.capRecs[id], cov.LargestHead)
			}
		}
	}
	for _, run := range ledgers {
		lr, err := auditLedger(ctx, j, led, voided, in, run, claims[run], byRun[run], intents, slotted, rs, find, warn)
		if err != nil {
			return rep, err
		}
		if lr != nil {
			rep.Ledgers = append(rep.Ledgers, *lr)
		}
	}

	// Ledgers the store does not hold but a record or a signed head names.
	rep.OrphanIntents = auditIntents(ctx, j, led, in, intents, slotted, rs, find, warn, &rep)
	listed := map[string]bool{}
	for _, l := range ledgers {
		listed[l] = true
	}
	for _, name := range sortedKeys(claims) {
		if listed[name] || !in.filter.matches(name) {
			continue
		}
		for _, cl := range claims[name] {
			find(capHeadOnly, name, cl.rec.Slot, "run %s records a reservation in this ledger, and the store holds no such ledger: the ledger was deleted", cl.run)
		}
	}
	for _, name := range sortedKeys(byRun) {
		if !listed[name] {
			find(capHeadOnly, name, -1, "a signed head covers this ledger, and the store holds no such ledger: the ledger was deleted")
		}
	}

	if in.anchored && otherHeads > 0 {
		rep.Ignored = fmt.Sprintf("%d signed head(s) of runs that hold no cap records were ignored", otherHeads)
	}
	if in.filter.any() {
		rep.NotChecked = append(rep.NotChecked, in.filter.describe()...)
	}
	return rep, nil
}

// describe lists what a filtered audit does not check.
func (f capsFilter) describe() []string {
	var out []string
	if f.ns != "" || f.rule != "" || f.window != "" {
		out = append(out, "ledgers outside -namespace, -rule and -window were not read, and cap-reserve records that name them were not compared")
	}
	if f.run != "" {
		out = append(out,
			"-run: the cap-reserve records of other runs were not read, so slots of other runs were not cross-checked and a slot deleted from another run's record was not seen",
			"-run: ledgers with no slot or record of this run were not checked at all",
			"-run: a ledger that holds this run's calls was checked whole against its own rules and its signed heads, but only this run's slots against the runs")
	}
	return out
}

// sortedKeys is the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// auditLedger checks one ledger run. It returns nil when a run filter leaves the ledger out because
// none of the run's calls touch it.
func auditLedger(ctx context.Context, j *agent.Journal, led *capledger.Ledger, voided capbridge.VoidedFunc, in capsInput, run string,
	claims []capClaim, heads []audit.SignedTreeHead, intents map[string][]capIntent, slotted map[string]bool, rs capRunFacts,
	find, warn func(kind, ledger string, slot int, format string, args ...any)) (*capsLedger, error) {
	_, _, _, window, _, _ := capledger.LedgerSegments(run)
	lr := &capsLedger{Ledger: run, Window: window}

	slots, err := led.Slots(ctx, run)
	corrupt := false
	if err != nil {
		switch {
		case errors.Is(err, capledger.ErrCorrupt):
			find(capCorrupt, run, -1, "%v", err)
		default:
			find(capUnreadable, run, -1, "%v", err)
		}
		corrupt = true
	}
	touched := in.filter.run == "" || len(claims) > 0
	for _, s := range slots {
		if in.filter.inRun(s.Run) {
			touched = true
		}
	}
	if !touched && !corrupt {
		return nil, nil
	}

	// Heads: the records of the run, as the AuditedStore signed them, against the heads we hold.
	if in.anchored {
		recs, err := j.History(ctx, run)
		if err != nil {
			find(capUnreadable, run, -1, "the ledger's journal cannot be read for its signed heads: %v", err)
		} else {
			// CheckDecisionCoverage classifies heads against a journal with the one rule every verb
			// uses (a head counts if it verifies, is a journal head of the run and is the root of the
			// run's prefix of its size); with no decisions to judge it reports only the heads.
			cov, err := audit.CheckDecisionCoverage(run, recs, heads, in.verifier, audit.CoverageBound{})
			if err != nil {
				return nil, err
			}
			for _, d := range cov.DivergentHeads {
				find(capDivergent, run, -1, "the signed head of size %d does not match the ledger (a slot was changed or deleted, or the ledger is shorter): %s", d.Size, d.Reason)
			}
			// cov.RejectedHeads is empty here: every head in byRun verified, is a journal head and is of this run.
			lr.Entries, lr.Attested = len(recs), cov.LargestHead
			lr.Tail = lr.Entries - lr.Attested
			switch {
			case cov.Heads == 0:
				find(capNoHead, run, -1, "no signed head covers this ledger: all %d entries are NOT ATTESTED", lr.Entries)
			case lr.Tail > 0:
				find(capNoHead, run, -1, "the newest signed head covers the first %d of %d entries: the last %d are NOT ATTESTED (a slot deleted or a release appended there would not be seen)", lr.Attested, lr.Entries, lr.Tail)
			}
		}
	}
	if corrupt {
		return lr, nil
	}

	// The window total, recomputed from the live reserves.
	live, total, sums := recomputeWindow(slots)
	released := map[int]bool{}
	for _, s := range slots {
		if s.Kind == "release" {
			released[s.Ref] = true
		}
	}
	for _, f := range sums {
		find(f.Kind, run, f.Slot, "%s", f.Detail)
	}
	lr.Slots, lr.Live, lr.Total = len(slots), live, total
	if len(slots) > 0 {
		lr.Currency, lr.Limit = slots[len(slots)-1].Currency, slots[len(slots)-1].Limit
	}
	for _, s := range slots {
		if s.Kind == "reserve" && s.Over {
			lr.Over = append(lr.Over, capsOver{Slot: s.N, Attempt: s.Attempt, CumAfter: s.CumAfter, Limit: s.Limit})
		}
	}

	// The slots against the cap-reserve records of the calls.
	claimed := map[int][]capClaim{}
	for _, cl := range claims {
		claimed[cl.rec.Slot] = append(claimed[cl.rec.Slot], cl)
		switch n := cl.rec.Slot; {
		case n < 0 || n >= len(slots):
			find(capDeleted, run, n, "run %s records a reservation in slot %d, and the ledger holds %d slot(s)", cl.run, n, len(slots))
		case slots[n].Kind != "reserve":
			find(capChanged, run, n, "run %s records a reservation in slot %d, which is a %s", cl.run, n, slots[n].Kind)
		case slots[n].CumAfter != cl.rec.Cum:
			find(capChanged, run, n, "run %s records cum_after %d for slot %d, and the ledger holds %d", cl.run, cl.rec.Cum, n, slots[n].CumAfter)
		case slots[n].Run != cl.run || led.ReserveName(slots[n].Attempt) != cl.name:
			find(capChanged, run, n, "run %s holds the record %s of slot %d, which belongs to another attempt (%s)", cl.run, cl.name, n, cleanText(slots[n].Attempt))
		}
	}
	firstOf := map[string]int{} // the first reserve slot of an attempt: the one its cap-reserve names
	for _, s := range slots {
		if s.Kind != "reserve" {
			continue
		}
		if _, seen := firstOf[s.Attempt]; seen {
			continue // a re-reserve after a release: the attempt's record names its first slot
		}
		firstOf[s.Attempt] = s.N
		slotted[s.Attempt] = true
		if in.filter.inRun(s.Run) && !rs.unreadable[s.Run] {
			checkIntentOfSlot(led, run, s, intents[s.Attempt], find)
		}
		if in.filter.inRun(s.Run) && !rs.unreadable[s.Run] && len(claimed[s.N]) == 0 {
			if rs.ended[s.Run] {
				find(capUnrecorded, run, s.N, "slot %d (attempt %s) has no cap-reserve record in run %s, which has ended: a slot inserted from outside, or a record deleted", s.N, cleanText(s.Attempt), cleanText(s.Run))
			} else {
				warn(capUnrecorded, run, s.N, "slot %d (attempt %s) has no cap-reserve record in run %s, which has not ended: a reservation interrupted before its record (a re-drive of the run writes it), or a slot inserted from outside", s.N, cleanText(s.Attempt), cleanText(s.Run))
			}
		}
	}

	// A release is the loop's, and the loop releases only a call it proved never began, after it journals
	// why (cap-release:). A release is explained by that record, or by the attempt's void (the reconcile
	// pass releases a voided attempt and writes no record). A release of a call whose result the tool
	// produced is the loop's never, note or not. A release neither explained nor refuted fails when
	// anchored (the justification is missing from a journal whose writer cannot be shown to have left it
	// out) and warns when not; a journal written before the record existed cannot be told from a forgery,
	// so its releases warn and are counted. A release whose verdict rests on records after the run's
	// newest signed head (anchored) is not attested, and fails.
	for _, rel := range slots {
		if rel.Kind != "release" || rel.Ref < 0 || rel.Ref >= len(slots) {
			continue
		}
		r := slots[rel.Ref]
		if !in.filter.inRun(r.Run) || rs.unreadable[r.Run] {
			continue
		}
		isErr, answered := rs.results[r.Run][r.Tool]
		switch {
		case in.anchored && !rs.covered[r.Run]:
			find(capReleaseTail, run, rel.N, "slot %d releases slot %d (attempt %s), and what explains it lies in run %s after the newest signed head (or no head covers the run): NOT ATTESTED", rel.N, r.N, cleanText(r.Attempt), cleanText(r.Run))
		case answered && !isErr:
			find(capReleaseRan, run, rel.N, "slot %d releases slot %d (attempt %s), and run %s holds a result of call %s that the tool produced: the call ran, so it was never released by the loop", rel.N, r.N, cleanText(r.Attempt), cleanText(r.Run), cleanText(r.Tool))
		default:
			explained, why, err := releaseExplained(ctx, j, led, voided, rs, r)
			if err != nil {
				return nil, err
			}
			switch {
			case explained:
			case why != "":
				find(capReleaseUnk, run, rel.N, "slot %d releases slot %d (attempt %s): %s", rel.N, r.N, cleanText(r.Attempt), why)
			case rs.dated(r.Run, r.Att):
				if in.anchored {
					find(capReleaseUnk, run, rel.N, "slot %d releases slot %d (attempt %s), and run %s holds neither a cap-release record nor a void of the attempt: nothing shows the call never began", rel.N, r.N, cleanText(r.Attempt), cleanText(r.Run))
				} else {
					warn(capReleaseUnk, run, rel.N, "slot %d releases slot %d (attempt %s), and run %s holds neither a cap-release record nor a void of the attempt: nothing shows the call never began (a crash between the release and the void looks the same, and a re-drive voids it; unanchored, a writer who can edit the store can leave exactly this)", rel.N, r.N, cleanText(r.Attempt), cleanText(r.Run))
				}
			default:
				rs.oldReleases[r.Attempt] = true
				warn(capReleaseUnk, run, rel.N, "slot %d releases slot %d (attempt %s), a call journaled before the cap-release record existed: nothing shows the call never began", rel.N, r.N, cleanText(r.Attempt))
			}
		}
	}

	// Reconcile exposure: the live slots whose attempt the journal records as never started (an unmarked
	// attempt has no marker, so it is never one).
	for _, s := range slots {
		if s.Kind != "reserve" || released[s.N] || !in.filter.inRun(s.Run) {
			continue
		}
		if rs.unreadable[s.Run] {
			continue
		}
		v, err := voided(ctx, j, s)
		if err != nil {
			find(capUnreadable, run, s.N, "slot %d: reading whether its attempt was voided failed: %v", s.N, err)
			continue
		}
		if v {
			lr.Exposed++
			lr.ExposedSum += s.Amount
		}
	}
	return lr, nil
}

// capsReport prints the report.
func (c *cli) capsReport(rep capsReport) {
	if rep.Anchored {
		c.println("INFO  anchored: each ledger is compared with its signed heads")
	}
	for _, l := range rep.Ledgers {
		line := fmt.Sprintf("INFO  ledger %s: %d slot(s), %d live, window total %d %s", cleanText(l.Ledger), l.Slots, l.Live, l.Total, cleanText(l.Currency))
		if l.Slots > 0 {
			line += fmt.Sprintf(" (limit %d; amounts are in minor units)", l.Limit)
		}
		if rep.Anchored && l.Entries > 0 {
			line += fmt.Sprintf("; signed head covers %d of %d entries", l.Attested, l.Entries)
		}
		c.println(line)
		for _, o := range l.Over {
			c.printf("WARN  OVER LIMIT slot %d (attempt %s): cum_after %d exceeds the limit %d, counted under a waiver\n", o.Slot, cleanText(o.Attempt), o.CumAfter, o.Limit)
		}
		if l.Exposed > 0 {
			c.printf("INFO  reconcile exposure, window %s: %d slot(s) worth %d %s (minor units) are counted for attempts the journal records as never started (the next recovery releases them)\n", cleanText(l.Window), l.Exposed, l.ExposedSum, cleanText(l.Currency))
		}
	}
	if n := len(rep.OrphanIntents); n > 0 {
		c.printf("INFO  %d cap-intent(s) have no slot: the attempt was interrupted before its slot, and counts nothing\n", n)
	}
	if rep.Ignored != "" {
		c.printf("INFO  %s\n", rep.Ignored)
	}
	for _, n := range rep.NotChecked {
		c.printf("INFO  NOT CHECKED: %s\n", n)
	}
	for _, f := range rep.Findings {
		where := cleanText(f.Ledger)
		if f.Slot >= 0 {
			where += fmt.Sprintf(" slot %d", f.Slot)
		}
		label := "FAIL"
		if f.Warn {
			label = "WARN"
		}
		c.printf("%s  %s %s: %s\n", label, f.Kind, where, cleanText(f.Detail))
	}
}

// recomputeWindow sums the live reserves of a ledger's slots (those no release voids) and compares the
// sum with the last cum_after. It finds an attempt that holds two live reserves, which the window counts
// twice. The ledger's own rules make both impossible for a ledger it reads; the audit recomputes them
// rather than trusting that.
func recomputeWindow(slots []capledger.Slot) (live int, total int64, finds []capsFinding) {
	released := map[int]bool{}
	for _, s := range slots {
		if s.Kind == "release" {
			released[s.Ref] = true
		}
	}
	liveAttempt := map[string]int{}
	for _, s := range slots {
		if s.Kind != "reserve" || released[s.N] {
			continue
		}
		live++
		total += s.Amount
		if first, dup := liveAttempt[s.Attempt]; dup {
			finds = append(finds, capsFinding{Kind: capDouble, Slot: s.N, Detail: fmt.Sprintf("attempt %s holds a live reserve in slot %d and in slot %d: the window counts it twice", cleanText(s.Attempt), first, s.N)})
		}
		liveAttempt[s.Attempt] = s.N
	}
	if n := len(slots); n > 0 && total != slots[n-1].CumAfter {
		finds = append(finds, capsFinding{Kind: capTotal, Slot: slots[n-1].N, Detail: fmt.Sprintf("the live reserves sum to %d and the last cum_after is %d", total, slots[n-1].CumAfter)})
	}
	return live, total, finds
}

// capIntent is a cap-intent record and the run that holds it.
type capIntent struct {
	run string
	in  capledger.Intent
}

// checkIntentOfSlot compares a reserve slot with the cap-intent of its attempt: there must be exactly one,
// in the run that made the call, and it must send the attempt to this ledger with this amount, currency
// and marking. A planted intent with another window is the window-pinning attack: the attempt's slot
// would sit in a ledger of the writer's choosing.
func checkIntentOfSlot(led *capledger.Ledger, ledger string, s capledger.Slot, ins []capIntent, find func(kind, ledger string, slot int, format string, args ...any)) {
	switch len(ins) {
	case 0:
		find(capIntentNone, ledger, s.N, "slot %d (attempt %s) has no cap-intent in run %s: the window it was counted in was never fixed by an intent", s.N, cleanText(s.Attempt), cleanText(s.Run))
		return
	case 1:
	default:
		find(capIntentDup, ledger, s.N, "attempt %s has %d cap-intents (in runs %s): the window it belongs to is not fixed", cleanText(s.Attempt), len(ins), cleanText(runsOf(ins)))
		return
	}
	ci := ins[0]
	var diff []string
	if ci.run != s.Run {
		diff = append(diff, fmt.Sprintf("the intent is in run %s, the slot's call is in run %s", cleanText(ci.run), cleanText(s.Run)))
	}
	if want := led.IntentLedger(ci.in); want != ledger {
		_, _, _, wantWindow, _, _ := capledger.LedgerSegments(want)
		_, _, _, haveWindow, _, _ := capledger.LedgerSegments(ledger)
		if wantWindow != haveWindow {
			diff = append(diff, fmt.Sprintf("the intent fixes window %s, the slot is in window %s", cleanText(ci.in.Window), cleanText(haveWindow)))
		} else {
			diff = append(diff, fmt.Sprintf("the intent names rule %s, key %s, currency %s in namespace %s: its ledger is %s", cleanText(ci.in.Rule), cleanText(ci.in.Key), cleanText(ci.in.Currency), cleanText(ci.in.Namespace), cleanText(want)))
		}
	}
	if ci.in.Amount != s.Amount {
		diff = append(diff, fmt.Sprintf("the intent reserves %d, the slot holds %d", ci.in.Amount, s.Amount))
	}
	if ci.in.Currency != s.Currency {
		diff = append(diff, fmt.Sprintf("the intent's currency is %s, the slot's is %s", cleanText(ci.in.Currency), cleanText(s.Currency)))
	}
	if ci.in.Marked != s.Marked {
		diff = append(diff, fmt.Sprintf("the intent's marking is %t, the slot's is %t", ci.in.Marked, s.Marked))
	}
	for _, d := range diff {
		find(capIntentDiff, ledger, s.N, "slot %d (attempt %s): %s", s.N, cleanText(s.Attempt), d)
	}
}

func runsOf(ins []capIntent) string {
	var rs []string
	for _, ci := range ins {
		rs = append(rs, ci.run)
	}
	slices.Sort(rs)
	return strings.Join(rs, ", ")
}

// auditIntents checks what the slots do not: an attempt with two intents (even with no slot), an intent
// whose window the rule's index run does not hold or is earlier than the newest window the index held when
// it was written, and the intents with no slot, which it returns (an attempt interrupted before its slot
// leaves one; information).
func auditIntents(ctx context.Context, j *agent.Journal, led *capledger.Ledger, in capsInput, intents map[string][]capIntent, slotted map[string]bool, rs capRunFacts,
	find, warn func(kind, ledger string, slot int, format string, args ...any), rep *capsReport) []string {
	var orphans []string
	outside := 0                          // intents whose ledger the window filter left unread
	index := map[string]map[string]bool{} // index run -> window ids it holds
	for _, att := range sortedKeys(intents) {
		ins := intents[att]
		if len(ins) > 1 && !slotted[att] {
			find(capIntentDup, "", -1, "attempt %s has %d cap-intents (in runs %s) and no slot", cleanText(att), len(ins), cleanText(runsOf(ins)))
		}
		for _, ci := range ins {
			ledger := led.IntentLedger(ci.in)
			// The window an intent fixes must be the window of its call's own time, whatever ledger it
			// names: an intent planted with an old window moves the attempt's slot into a ledger with room.
			// Only the namespace and rule filters (not -window, which would hide the pinned window) skip it.
			if in.filter.matchesRule(ledger) {
				checkIntentTime(led, att, ci, rs, find, warn)
			}
			if !in.filter.matches(ledger) {
				if in.filter.matchesRule(ledger) {
					outside++
				}
				continue
			}
			if !slotted[att] && len(ins) == 1 {
				orphans = append(orphans, att)
			}
			if ci.in.Kind == capledger.Run {
				continue
			}
			idx := led.IndexRun(ci.in.IntentRule())
			held, ok := index[idx]
			if !ok {
				held = map[string]bool{}
				recs, err := j.History(ctx, idx)
				if err == nil {
					for _, r := range recs {
						if w, found := strings.CutPrefix(r.Name, "win:"); found {
							held[w] = true
						}
					}
				}
				index[idx] = held
			}
			if !held[ci.in.Window] {
				find(capIndexNone, idx, -1, "the cap-intent of %s in run %s fixes window %s, and the index run holds no such window: the intent was planted, or the index entry was deleted", cleanText(att), cleanText(ci.run), cleanText(ci.in.Window))
			}
			if ci.in.Floor != "" && ci.in.Window < ci.in.Floor {
				find(capIntentOld, ledger, -1, "the cap-intent of %s fixes window %s, earlier than the newest window %s the index held when it was written", cleanText(att), cleanText(ci.in.Window), cleanText(ci.in.Floor))
			}
		}
	}
	if n := len(rs.oldReleases); n > 0 {
		rep.NotChecked = append(rep.NotChecked, fmt.Sprintf("%d release(s) are of calls journaled before the cap-release record existed: nothing shows those calls never began", n))
	}
	if n := len(rs.unmarked); n > 0 {
		rep.NotChecked = append(rep.NotChecked, fmt.Sprintf("%d cap-intent(s) belong to attempts with no dated marker (a retry-safe tool, or a marker written before recorded_at existed): their window was held only to the start of their run, so a long-lived run could pin one to any later window", n))
	}
	if outside > 0 {
		rep.NotChecked = append(rep.NotChecked, fmt.Sprintf("%d cap-intent(s) fix a window the filter left out: their dates were checked, but their slots, and the ledgers they name, were not read", outside))
	}
	return orphans
}

// capRunFacts is what the audit read of the runs besides their cap records: which tool-use IDs of a run
// have a result (and whether it is an error), and which runs have ended.
type capRunFacts struct {
	results        map[string]map[string]bool   // run -> tool-use ID -> the result is an error
	notes          map[string]map[string][]byte // run -> cap-release record name -> its data
	covered        map[string]bool              // run -> its newest signed head covers the whole run (anchored audits)
	unreadable     map[string]bool              // runs that could not be read
	ended          map[string]bool
	started        map[string]time.Time            // run -> the start time its run:start records (the run's clock)
	attempted      map[string]map[string]time.Time // run -> attempt marker name -> recorded_at (the run clock)
	capRecs        map[string]int                  // run -> entries up to and including its last cap-intent or cap-reserve
	records        map[string]int                  // run -> number of entries
	undatedRelease int                             // releases of attempts journaled before the cap-release record existed
	unmarked       map[string]bool                 // the intents dated by their run's start only (no attempt marker)
	oldReleases    map[string]bool                 // releases of attempts journaled before the cap-release record existed
}

// failures counts the findings that fail the audit (not the warnings).
func (r capsReport) failures() int {
	n := 0
	for _, f := range r.Findings {
		if !f.Warn {
			n++
		}
	}
	return n
}

// checkIntentTime compares the window an intent fixes with the time of its call. The window of a day or hour
// rule must not be earlier than the window of the run's start (the run's own clock, recorded by its
// run:start), nor later than the window after the one the attempt marker's time falls in (the marker is
// stamped by the store when the attempt is claimed, a moment before the first intent): a boundary may fall
// between the claim and the intent, so one window forward is allowed, and never one back, which is how a
// planted intent gains room. A run window must be the call's root run.
func checkIntentTime(led *capledger.Ledger, att string, ci capIntent, rs capRunFacts, find, warn func(kind, ledger string, slot int, format string, args ...any)) {
	in := ci.in
	if in.Kind == capledger.Run {
		parts := strings.Split(in.Attempt, "/")
		if len(parts) < 2 || parts[1] != led.Segment(in.Window) {
			find(capIntentWin, "", -1, "the cap-intent of %s in run %s fixes the run window %s, which is not the root run of the call", cleanText(att), cleanText(ci.run), cleanText(in.Window))
		}
		return
	}
	start, haveStart := rs.started[ci.run]
	var marked time.Time
	if t, ok := rs.attempted[ci.run][markerOf(in.Attempt)]; ok {
		marked = t
	}
	if marked.IsZero() && haveStart {
		rs.unmarked[ci.run+"\x00"+att] = true // dated by the run's start only
	}
	if !haveStart && marked.IsZero() {
		warn(capIntentUndated, "", -1, "the cap-intent of %s in run %s fixes window %s, and the run records neither a start time nor an attempt marker to date the call by", cleanText(att), cleanText(ci.run), cleanText(in.Window))
		return
	}
	if why := windowOutOfTime(in.Kind, in.Window, start, haveStart, marked); why != "" {
		find(capIntentWin, "", -1, "the cap-intent of %s in run %s fixes window %s: %s", cleanText(att), cleanText(ci.run), cleanText(in.Window), why)
	}
}

// windowOutOfTime says why window (of kind) cannot hold a call, or "" if it can. A call with an attempt
// marker is dated by it: the engine writes recorded_at on the run's clock when it claims the attempt, a
// moment before the first intent, so the window must be the one containing it or the one after (a boundary
// may fall between the claim and the intent). A call with no marker (an unmarked attempt) is dated only by
// the start of its run: the window must not be earlier than the window of the start, and a run that lives
// long can pin to any later window; that is a limit, and the report counts such intents.
func windowOutOfTime(kind capledger.Kind, window string, start time.Time, haveStart bool, marked time.Time) string {
	dur := 24 * time.Hour
	if kind == capledger.Hour {
		dur = time.Hour
	}
	if !marked.IsZero() {
		lo, err := capledger.WindowID(kind, marked, "")
		if err != nil {
			return err.Error()
		}
		hi, err := capledger.WindowID(kind, marked.Add(dur), "")
		if err != nil {
			return err.Error()
		}
		switch {
		case window < lo:
			return fmt.Sprintf("the attempt was claimed in window %s: a call cannot be counted in an earlier window", lo)
		case window > hi:
			return fmt.Sprintf("the attempt was claimed in window %s: a call cannot be counted in a window past the next one (%s)", lo, hi)
		}
		return ""
	}
	if haveStart {
		w, err := capledger.WindowID(kind, start, "")
		if err != nil {
			return err.Error()
		}
		if window < w {
			return fmt.Sprintf("the run started in window %s: a call cannot be counted in an earlier window", w)
		}
	}
	return ""
}

// markerOf is the attempt marker's name inside an attempt key: the encoded "<marker key>#<rule>" is its
// last component, and the marker key is what precedes the last (encoded) '#'.
func markerOf(attempt string) string {
	parts := strings.Split(attempt, "/")
	last := parts[len(parts)-1]
	i := strings.LastIndex(last, "%23")
	if i <= 0 {
		return ""
	}
	if name, err := url.PathUnescape(last[:i]); err == nil {
		return name
	}
	return ""
}

// releaseExplained reports whether the release of reserve r is explained: the run holds the loop's
// cap-release record for the attempt (and it names that attempt), or the attempt is voided. why is set when a
// record is there and does not hold.
func releaseExplained(ctx context.Context, j *agent.Journal, led *capledger.Ledger, voided capbridge.VoidedFunc, rs capRunFacts, r capledger.Slot) (explained bool, why string, err error) {
	if data, ok := rs.notes[r.Run][led.ReleaseName(r.Attempt)]; ok {
		note, derr := capledger.DecodeRelease(data)
		switch {
		case derr != nil:
			return false, fmt.Sprintf("the cap-release record of the attempt does not read: %v", derr), nil
		case note.Attempt != r.Attempt:
			return false, fmt.Sprintf("the cap-release record under this attempt's name names another attempt (%s)", cleanText(note.Attempt)), nil
		}
		return true, "", nil
	}
	v, err := voided(ctx, j, r)
	if err != nil {
		return false, fmt.Sprintf("reading whether the attempt was voided failed: %v", err), nil
	}
	return v, "", nil
}

// dated reports whether the attempt of a slot (its Att, "<marker key>#<rule>") has a marker with a
// recorded_at in run: a journal written by a version that writes cap-release records.
func (rs capRunFacts) dated(run, att string) bool {
	i := strings.LastIndexByte(att, '#')
	if i <= 0 {
		return false
	}
	_, ok := rs.attempted[run][att[:i]]
	return ok
}

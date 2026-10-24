package auditcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	capCorrupt    = "CORRUPT"        // the ledger breaks its own accept rules
	capUnreadable = "UNREADABLE"     // a ledger or a run that cannot be decoded
	capTotal      = "TOTAL"          // the recomputed window total is not the last cum_after
	capDouble     = "DOUBLE"         // one attempt holds two live reserves
	capDeleted    = "DELETED"        // a cap-reserve record names a slot the ledger does not hold
	capChanged    = "CHANGED"        // a slot differs from the cap-reserve record that names it
	capUnrecorded = "UNRECORDED"     // a reserve slot with no cap-reserve record in its run
	capBadHead    = "BAD_HEAD"       // a signed head that does not verify
	capDivergent  = "DIVERGENT_HEAD" // a signed head the ledger run does not match
	capNoHead     = "NOT_ATTESTED"   // entries no signed head covers
	capHeadOnly   = "LEDGER_MISSING" // a signed head or a cap-reserve record for a ledger the store does not hold
)

// capsFinding is one problem bide audit caps found.
type capsFinding struct {
	Kind   string `json:"kind"`
	Ledger string `json:"ledger"`
	Slot   int    `json:"slot"` // -1 when the finding is not about one slot
	Detail string `json:"detail"`
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
}

// capsFilter limits what bide audit caps reads. The segments are as encoded in a ledger run's ID.
type capsFilter struct {
	ns, rule, window string // encoded segments, "" for any
	run              string // a run: only its calls and its sub-runs' calls
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
	rep, err := auditCaps(context.Background(), s.Journal, j, led, in)
	if err != nil {
		c.note(unusable(err))
		return
	}
	c.capsReport(rep)
	if b, merr := json.Marshal(rep); merr == nil {
		c.artifact = b
	}
	if len(rep.Findings) > 0 {
		c.fail("%d problem(s): the cap ledgers do not hold together", len(rep.Findings))
	}
	slots, live := 0, 0
	for _, l := range rep.Ledgers {
		slots += l.Slots
		live += l.Live
	}
	switch {
	case in.filter.any():
		c.printf("PASS (filtered): the %d ledger(s) checked hold together (%d slots, %d live); the checks that were NOT made are listed above\n", len(rep.Ledgers), slots, live)
	case in.anchored:
		c.printf("PASS: %d ledger(s), %d slot(s), %d live: each reads under the ledger's rules, agrees with the runs' cap-reserve records, and is attested by a signed head\n", len(rep.Ledgers), slots, live)
	default:
		c.printf("PASS: %d ledger(s), %d slot(s), %d live: each reads under the ledger's rules and agrees with the runs' cap-reserve records; nothing attests that the store is complete (UNANCHORED)\n", len(rep.Ledgers), slots, live)
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
func auditCaps(ctx context.Context, store agent.Store, j *agent.Journal, led *capledger.Ledger, in capsInput) (capsReport, error) {
	rep := capsReport{Anchored: in.anchored, Ledgers: []capsLedger{}, Findings: []capsFinding{}}
	voided := capbridge.Voided()
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

	// The cap-reserve records of the runs in scope, by the ledger they name.
	claims := map[string][]capClaim{}
	for _, id := range runs {
		recs, err := j.History(ctx, id)
		if err != nil {
			if errors.Is(err, agent.ErrStorage) {
				return rep, fmt.Errorf("read run %s: %w", id, err)
			}
			find(capUnreadable, "", -1, "run %s cannot be read, so its cap-reserve records are not cross-checked: %v", id, err)
			continue
		}
		for _, r := range recs {
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

	for _, run := range ledgers {
		lr, err := auditLedger(ctx, j, led, voided, in, run, claims[run], byRun[run], find)
		if err != nil {
			return rep, err
		}
		if lr != nil {
			rep.Ledgers = append(rep.Ledgers, *lr)
		}
	}

	// Ledgers the store does not hold but a record or a signed head names.
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
		rep.NotChecked = append(rep.NotChecked, fmt.Sprintf("%d signed head(s) of runs that are not cap ledgers were ignored", otherHeads))
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
	claims []capClaim, heads []audit.SignedTreeHead, find func(kind, ledger string, slot int, format string, args ...any)) (*capsLedger, error) {
	_, _, _, window, _, _ := capledger.LedgerSegments(run)
	lr := &capsLedger{Ledger: run, Window: window}

	slots, err := led.Slots(ctx, run)
	corrupt := false
	if err != nil {
		switch {
		case errors.Is(err, capledger.ErrCorrupt):
			find(capCorrupt, run, -1, "%v", err)
		case errors.Is(err, agent.ErrStorage):
			return nil, fmt.Errorf("read ledger %s: %w", run, err)
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
			if errors.Is(err, agent.ErrStorage) {
				return nil, fmt.Errorf("read ledger %s: %w", run, err)
			}
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
			for _, d := range cov.RejectedHeads {
				find(capBadHead, run, -1, "a signed head of size %d was not counted: %s", d.Size, d.Reason)
			}
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
		if in.filter.inRun(s.Run) && len(claimed[s.N]) == 0 {
			find(capUnrecorded, run, s.N, "slot %d (attempt %s) has no cap-reserve record in run %s: a slot inserted from outside, or a reservation interrupted before its record (a re-drive of the run writes it)", s.N, cleanText(s.Attempt), s.Run)
		}
	}

	// Reconcile exposure: the live slots whose attempt the journal records as never started (an unmarked
	// attempt has no marker, so it is never one).
	for _, s := range slots {
		if s.Kind != "reserve" || released[s.N] || !in.filter.inRun(s.Run) {
			continue
		}
		v, err := voided(ctx, j, s)
		if err != nil {
			return nil, fmt.Errorf("read whether %s was voided: %w", s.Attempt, err)
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
			line += fmt.Sprintf(" (limit %d)", l.Limit)
		}
		if rep.Anchored && l.Entries > 0 {
			line += fmt.Sprintf("; signed head covers %d of %d entries", l.Attested, l.Entries)
		}
		c.println(line)
		for _, o := range l.Over {
			c.printf("WARN  OVER LIMIT slot %d (attempt %s): cum_after %d exceeds the limit %d, counted under a waiver\n", o.Slot, cleanText(o.Attempt), o.CumAfter, o.Limit)
		}
		if l.Exposed > 0 {
			c.printf("INFO  reconcile exposure, window %s: %d slot(s) worth %d %s are counted for attempts the journal records as never started (the next recovery releases them)\n", cleanText(l.Window), l.Exposed, l.ExposedSum, cleanText(l.Currency))
		}
	}
	for _, n := range rep.NotChecked {
		c.printf("INFO  NOT CHECKED: %s\n", n)
	}
	for _, f := range rep.Findings {
		where := cleanText(f.Ledger)
		if f.Slot >= 0 {
			where += fmt.Sprintf(" slot %d", f.Slot)
		}
		c.printf("FAIL  %s %s: %s\n", f.Kind, where, cleanText(f.Detail))
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

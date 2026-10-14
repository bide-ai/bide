package auditcli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// NotifyStores is a journal and an event store opened for bide audit notifications, read-only.
type NotifyStores struct {
	Journal agent.Store      // the run journals, which must list their runs (agent.Lister)
	Events  audit.EventStore // the notification trails
	Close   func() error     // releases both
}

// StoreOpeners open the databases `bide audit notifications -db` and `-postgres-env` read. The core
// module carries no store driver, so the full bide command sets them (cmd/bide); in an audit-only
// build they are nil and those flags are a usage error: the verb then reads exported files only.
type StoreOpeners struct {
	SQLite   func(path string) (NotifyStores, error)
	Postgres func(dsn string) (NotifyStores, error)
}

// Stores is set by the program that links the store drivers; nil if there are none.
var Stores *StoreOpeners

// notifications verifies that the events that needed a person were notified and acknowledged
// (audit.VerifyNotifications), from exported trails and journals or read from a store.
func (c *cli) notifications(args []string) {
	fs := c.flagSet("notifications")
	var trails, heads, journals stringList
	fs.Var(&trails, "trail", "a notification trail export (audit.ExportNotifyTrail as JSON); repeat once per trail")
	fs.Var(&heads, "sth", "a signed tree head over a trail (audit.SignNotifyTrail as JSON); repeat once per trail")
	fs.Var(&journals, "journal", "a run's journal export (audit.ExportJournal as JSON) to cross-check against; repeat once per run")
	pubkey := fs.String("pubkey", "", "the "+pubkeyHelp+"; it verifies every -sth")
	unanchored := fs.Bool("unanchored", false, "accept a trail with no signed head: it is read, and nothing attests that it is complete or unaltered")
	db := fs.String("db", "", "read the trails and journals from this SQLite journal (opened read-only) instead of from files")
	pgEnv := fs.String("postgres-env", "", "read them from the Postgres journal whose DSN is in this environment variable (never given on the command line)")
	run := fs.String("run", "", "with -db or -postgres-env: only this run and its sub-runs; default every run")
	maxAck := fs.Duration("max-ack", 0, "fail an event not acknowledged within this long of when the run entered its state (0: only each event's recorded deadline)")
	grace := fs.Duration("grace", 5*time.Minute, "how long after an event began that a missing observation is not yet a failure (the notifier's detection window)")
	recovery := fs.Duration("recovery-failing-after", 15*time.Minute, "how long a recovery failure episode may be open before a recovery_failing event is owed (the notifier's setting)")
	acceptTail := fs.Bool("accept-tail", false, "accept verdicts that rest on records after the newest signed head (a live trail between heads); without it such an event is unattested and fails")
	routingPath := fs.String("routing", "", "the notifier's routing as JSON (agent.RoutingOf, from the options the notifier was built with): every event must have been delivered to the receivers it routes to, and each due escalation step to its receiver; without it per-receiver completeness is not checked")
	strict := fs.Bool("strict", false, "also fail (exit 1) on a warning: a decided gate no notifier observed (unobserved_resolved), or an all-clear owed that cannot be timed (all_clear_untimed)")
	asOf := fs.String("as-of", "", "judge pending events as of this RFC 3339 time (default now)")
	c.parse(fs, args)

	haveFiles := len(trails) > 0 || len(journals) > 0
	haveStore := *db != "" || *pgEnv != ""
	switch {
	case *db != "" && *pgEnv != "":
		c.usageLine(fs.Name() + ": give one of -db and -postgres-env, not both; run `" + fs.Name() + " -h` for usage")
	case haveFiles && haveStore:
		c.usageLine(fs.Name() + ": give exported files (-trail, -journal) or a store (-db, -postgres-env), not both")
	case !haveFiles && !haveStore:
		c.usageLine(fs.Name() + ": missing -trail and -journal, or -db, or -postgres-env; run `" + fs.Name() + " -h` for usage")
	case *run != "" && !haveStore:
		c.usageLine(fs.Name() + ": -run reads one run from a store; it needs -db or -postgres-env")
	case haveStore && Stores == nil:
		c.usageLine(fs.Name() + ": this build reads exported files only (-trail, -journal); -db and -postgres-env are in the full bide command")
	case !*unanchored && *pubkey == "":
		c.usageLine(fs.Name() + ": missing -pubkey (and an -sth for each trail); give -unanchored to read a trail nothing attests")
	case len(heads) > 0 && *pubkey == "":
		c.usageLine(fs.Name() + ": -sth needs -pubkey")
	case *maxAck < 0 || *grace < 0 || *recovery < 0:
		c.usageLine(fs.Name() + ": -max-ack, -grace and -recovery-failing-after must not be negative")
	}
	now := time.Now()
	if *asOf != "" {
		t, err := time.Parse(time.RFC3339, *asOf)
		if err != nil {
			c.usageLine(fmt.Sprintf("%s: -as-of: %v", fs.Name(), err))
		}
		now = t
	}

	in := audit.NotifyVerifyInput{Unanchored: *unanchored, Now: now, MaxAck: *maxAck, Grace: *grace, RecoveryFailingAfter: *recovery, MaxClockSkew: maxClockSkew, Strict: *strict, AcceptTail: *acceptTail}
	ok := true
	if *routingPath != "" {
		var rt agent.NotifyRouting
		ok = c.note(c.readJSON(*routingPath, &rt)) && ok
		in.Routing = &rt
	}
	if *pubkey != "" {
		v, err := readPubKey(*pubkey)
		ok = c.note(err) && ok
		in.Verifier = v
	}
	for _, p := range trails {
		var x audit.NotifyTrailExport
		ok = c.note(c.readJSON(p, &x)) && ok
		in.Trails = append(in.Trails, x)
	}
	for _, p := range heads {
		var h audit.SignedTreeHead
		ok = c.note(c.readJSON(p, &h)) && ok
		in.Heads = append(in.Heads, h)
	}
	for _, p := range journals {
		var x audit.JournalExport
		ok = c.note(c.readJSON(p, &x)) && ok
		in.Journals = append(in.Journals, x)
	}
	if haveStore {
		ok = c.note(c.readStore(&in, *db, *pgEnv, *run)) && ok
	}
	if !ok {
		return
	}

	rep, err := audit.VerifyNotifications(in)
	if err != nil && !errors.Is(err, audit.ErrNotVerified) {
		c.note(unusable(err))
		return
	}
	c.report(rep, *strict)
	if b, merr := json.Marshal(rep); merr == nil {
		c.artifact = b
	}
	if err != nil {
		c.fail("%d problem(s): an event that needed a person was not notified, not acknowledged in time, or the trail does not hold together", len(rep.Problems))
	}
	if !c.clean() {
		return
	}
	c.printf("PASS: %d event(s) in %d trail(s): every event that needed a person was notified, and none is late or overdue\n", len(rep.Events), len(rep.Trails))
}

// readStore opens the store and loads into in the trails and journals of run (or of every run).
func (c *cli) readStore(in *audit.NotifyVerifyInput, db, pgEnv, run string) error {
	var s NotifyStores
	var err error
	if db != "" {
		s, err = Stores.SQLite(db)
	} else {
		dsn := os.Getenv(pgEnv)
		if dsn == "" {
			return unusable(fmt.Errorf("-postgres-env %s: the variable is empty or not set", pgEnv))
		}
		s, err = Stores.Postgres(dsn)
	}
	if err != nil {
		return unusable(fmt.Errorf("open the store: %w", err))
	}
	defer s.Close()
	ctx := context.Background()
	lister, ok := agent.Capability[agent.Lister](s.Journal)
	if !ok {
		return unusable(errors.New("the store cannot list its runs"))
	}
	j, err := agent.NewJournal(s.Journal)
	if err != nil {
		return unusable(err)
	}
	var ids []string
	for id, err := range lister.Runs(ctx, agent.RunFilter{}) {
		if err != nil {
			return unusable(fmt.Errorf("list runs: %w", err))
		}
		if agent.IsSessionRun(id) {
			continue // the session drives it, and the notifier skips it
		}
		if run == "" || id == run || strings.HasPrefix(id, run+">") && agent.IsSubRun(id) {
			ids = append(ids, id)
		}
	}
	if run != "" && !slices.Contains(ids, run) {
		return unusable(fmt.Errorf("-run %s: the store holds no such run", run))
	}
	roots := map[string]bool{} // the runs whose trails may hold events: roots, and the roots events name (a flow run is its own)
	for _, id := range ids {
		x, err := audit.ExportJournal(ctx, j, id)
		if err != nil {
			return unusable(err)
		}
		in.Journals = append(in.Journals, x)
		if !agent.IsSubRun(id) {
			roots[id] = true
		}
		recs, err := x.Journal()
		if err != nil {
			return unusable(err)
		}
		for _, ev := range agent.JournalNotifyEvents(id, recs) {
			roots[ev.RunID] = true
		}
	}
	add := func(name string) ([]agent.NotifyRecord, error) {
		x, err := audit.ExportNotifyTrail(ctx, s.Events, name)
		if err != nil {
			return nil, err
		}
		if len(x.Leaves) == 0 {
			return nil, nil
		}
		in.Trails = append(in.Trails, x)
		recs, err := audit.NewNotifyTrail(memLeaves{x.Leaves}).Load(ctx, name)
		return recs, err
	}
	for _, id := range slices.Sorted(mapKeys(roots)) {
		if _, err := add(agent.NotifyTrailName(id)); err != nil {
			return unusable(err)
		}
	}
	// The open trail: every yearly index, the segments they name, and the segments those seal into.
	now := time.Now().UTC()
	segs := map[string]bool{}
	var queue []string
	for y := now.Year() + 1; y >= now.Year()-50; y-- {
		recs, err := add(agent.OpenIndex(y))
		if err != nil {
			return unusable(err)
		}
		for _, r := range recs {
			if r.Kind == agent.NotifySegment && !segs[r.Next] {
				segs[r.Next] = true
				queue = append(queue, r.Next)
			}
		}
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		recs, err := add(name)
		if err != nil {
			return unusable(err)
		}
		if next := agent.FoldOpen(recs).Sealed; next != "" && !segs[next] {
			segs[next] = true
			queue = append(queue, next)
		}
	}
	return nil
}

// memLeaves is an audit.EventStore over the leaves of one trail, to decode them as records.
type memLeaves struct{ leaves [][]byte }

func (m memLeaves) Append(context.Context, string, int, []byte) error {
	return errors.New("read-only")
}
func (m memLeaves) Load(context.Context, string) ([][]byte, error) { return m.leaves, nil }

func mapKeys(m map[string]bool) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// report prints the trails and the verdict on each event.
func (c *cli) report(rep audit.NotifyReport, strict bool) {
	for _, tr := range rep.Trails {
		switch {
		case tr.Anchored:
			line := fmt.Sprintf("INFO  trail %s: %d records; signed head over the first %d (signed %s)", tr.Trail, tr.Records, tr.HeadSize, tr.HeadTime.Format(time.RFC3339))
			if tr.Tail > 0 {
				line += fmt.Sprintf("; the last %d (%s to %s) are not attested by it", tr.Tail, tr.TailFirstAt.Format(time.RFC3339), tr.TailLastAt.Format(time.RFC3339))
			}
			c.println(line)
		default:
			c.printf("INFO  trail %s: %d records; UNANCHORED, nothing attests that it is complete or unaltered\n", tr.Trail, tr.Records)
		}
		for _, v := range tr.Violations {
			c.printf("FAIL  trail %s: %s\n", tr.Trail, cleanText(v))
		}
	}
	events := slices.Clone(rep.Events)
	slices.SortStableFunc(events, func(a, b audit.NotifyEventVerdict) int {
		return cmp.Or(cmp.Compare(a.Trail, b.Trail), a.EventAt.Compare(b.EventAt))
	})
	for _, e := range events {
		mark := "INFO  "
		switch {
		case e.Status.Problem() || strict && e.Status.Warning():
			mark = "FAIL  "
		case e.Status.Warning():
			mark = "note: WARNING "
		case e.Status == audit.NotifyStatusAcked || e.Status == audit.NotifyStatusClearedBeforeNotification:
			mark = "PASS  "
		}
		line := fmt.Sprintf("%s%s  %s %s", mark, e.Status, e.Kind, cleanText(e.Key))
		if !e.EventAt.IsZero() {
			line += "  began " + e.EventAt.UTC().Format(time.RFC3339)
		}
		if !e.ObservedAt.IsZero() {
			line += "  observed " + e.ObservedAt.UTC().Format(time.RFC3339)
		}
		if !e.AckedAt.IsZero() {
			line += fmt.Sprintf("  acknowledged %s after %d attempt(s), %s after it began", e.AckedAt.UTC().Format(time.RFC3339), e.Attempts, e.Total.Round(time.Millisecond))
		}
		if e.Receiver != "" && e.Receiver != agent.DefaultReceiver {
			line += "  receiver " + e.Receiver
		}
		if e.Level > 0 {
			line += fmt.Sprintf("  level %d", e.Level)
		}
		if e.Why != "" {
			line += "  (" + cleanText(e.Why) + ")"
		}
		if e.Auth != nil {
			line += "  [" + authText(e.Auth) + "]"
		}
		c.println(line)
	}
	counts := rep.Counts()
	var parts []string
	for _, s := range slices.Sorted(mapKeysStatus(counts)) {
		parts = append(parts, fmt.Sprintf("%d %s", counts[audit.NotifyStatus(s)], s))
	}
	if len(parts) > 0 {
		c.printf("INFO  %s\n", strings.Join(parts, ", "))
	}
	// With more than one receiver, the answer to "was every event delivered to each of them".
	if by := rep.CountsByReceiver(); len(by) > 1 {
		for _, name := range slices.Sorted(maps.Keys(by)) {
			var ps []string
			for _, s := range slices.Sorted(mapKeysStatus(by[name])) {
				ps = append(ps, fmt.Sprintf("%d %s", by[name][audit.NotifyStatus(s)], s))
			}
			c.printf("INFO  %s: %s\n", name, strings.Join(ps, ", "))
		}
	}
}

func mapKeysStatus(m map[audit.NotifyStatus]int) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(string(k)) {
				return
			}
		}
	}
}

// authText is how an acknowledged delivery was authenticated, in a few words.
func authText(a *agent.NotifyAuth) string {
	var p []string
	if a.Signature != "" {
		p = append(p, a.Signature+" "+a.SecretSlot)
	}
	if a.MTLS {
		p = append(p, "mTLS")
	}
	if a.ServerPinned {
		p = append(p, "pinned")
	}
	if a.TLSVersion != "" {
		p = append(p, "TLS "+a.TLSVersion)
	}
	if len(p) == 0 {
		return "unauthenticated"
	}
	return strings.Join(p, ", ")
}

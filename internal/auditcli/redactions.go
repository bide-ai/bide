package auditcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// redactions verifies the redaction trail (audit.VerifyRedactions): that it is complete under its
// signed heads, that every tombstone in the journals is named by a trail record with the same name
// and leaf hash (so a redaction made by hand is UNACCOUNTED, and a tombstone's name, which the
// journal's own hashes do not attest, is attested by the signed trail), that every record the trail
// lists as erased is erased, and that no pass is unfinished.
func (c *cli) redactions(args []string) {
	fs := c.flagSet("redactions")
	var heads, journals stringList
	trailPath := fs.String("trail", "", "the redaction trail export (audit.ExportNotifyTrail of "+audit.RedactionTrailName+", as JSON)")
	fs.Var(&heads, "sth", "a signed tree head over the redaction trail, as JSON; repeat for as many as you have: each must verify and match a prefix of the trail")
	fs.Var(&journals, "journal", "a run's journal export (audit.ExportJournal as JSON) to check the tombstones of; repeat once per run")
	pubkey := fs.String("pubkey", "", "the "+pubkeyHelp+"; it verifies every -sth")
	unanchored := fs.Bool("unanchored", false, "accept a trail with no signed head: it is read, and nothing attests that it is complete or unaltered")
	db := fs.String("db", "", "read the trail and the journals from this SQLite journal (opened read-only) instead of from files")
	pgEnv := fs.String("postgres-env", "", "read them from the Postgres journal whose DSN is in this environment variable (never given on the command line)")
	run := fs.String("run", "", "with -db or -postgres-env: only this run and its sub-runs; default every run")
	c.parse(fs, args)

	haveFiles := *trailPath != "" || len(journals) > 0
	haveStore := *db != "" || *pgEnv != ""
	switch {
	case *db != "" && *pgEnv != "":
		c.usageLine(fs.Name() + ": give one of -db and -postgres-env, not both; run `" + fs.Name() + " -h` for usage")
	case haveFiles && haveStore:
		c.usageLine(fs.Name() + ": give exported files (-trail, -journal) or a store (-db, -postgres-env), not both")
	case !haveFiles && !haveStore:
		c.usageLine(fs.Name() + ": missing -trail, or -db, or -postgres-env; run `" + fs.Name() + " -h` for usage")
	case haveFiles && *trailPath == "":
		c.usageLine(fs.Name() + ": missing -trail")
	case *run != "" && !haveStore:
		c.usageLine(fs.Name() + ": -run reads one run from a store; it needs -db or -postgres-env")
	case haveStore && Stores == nil:
		c.usageLine(fs.Name() + ": this build reads exported files only (-trail, -journal); -db and -postgres-env are in the full bide command")
	case !*unanchored && *pubkey == "":
		c.usageLine(fs.Name() + ": missing -pubkey (and an -sth for the trail); give -unanchored to read a trail nothing attests")
	case len(heads) > 0 && *pubkey == "":
		c.usageLine(fs.Name() + ": -sth needs -pubkey")
	}
	in := audit.RedactionVerifyInput{Unanchored: *unanchored, Journals: map[string][]agent.Record{}}
	ok := true
	if *pubkey != "" {
		v, err := readPubKey(*pubkey)
		ok = c.note(err) && ok
		in.Verifier = v
	}
	for _, p := range heads {
		var h audit.SignedTreeHead
		ok = c.note(c.readJSON(p, &h)) && ok
		in.Heads = append(in.Heads, h)
	}
	if haveFiles {
		var x audit.NotifyTrailExport
		ok = c.note(c.readJSON(*trailPath, &x)) && ok
		if ok && x.Trail != audit.RedactionTrailName {
			ok = c.note(unusable(fmt.Errorf("-trail %s: this is the trail %q, not %q", *trailPath, x.Trail, audit.RedactionTrailName)))
		}
		in.Trail = x.Leaves
		for _, p := range journals {
			var jx audit.JournalExport
			ok = c.note(c.readJSON(p, &jx)) && ok
			if recs, err := jx.Journal(); err == nil {
				in.Journals[jx.RunID] = recs
			} else {
				ok = c.note(unusable(err)) && ok
			}
		}
	} else {
		ok = c.note(c.readRedactionStore(&in, *db, *pgEnv, *run)) && ok
	}
	if !ok {
		return
	}
	rep, err := audit.VerifyRedactions(in)
	if err != nil && !errors.Is(err, audit.ErrNotVerified) {
		c.note(unusable(err))
		return
	}
	switch {
	case rep.Anchored:
		c.printf("INFO  redaction trail: %d record(s), %d redaction(s); signed head over the first %d\n", rep.Records, rep.Redactions, rep.HeadSize)
	default:
		c.printf("INFO  redaction trail: %d record(s), %d redaction(s); UNANCHORED, nothing attests that it is complete or unaltered\n", rep.Records, rep.Redactions)
	}
	if b, merr := json.Marshal(rep); merr == nil {
		c.artifact = b
	}
	for _, f := range rep.Findings {
		run := ""
		if f.RunID != "" {
			run = " run " + cleanText(f.RunID) + ":"
		}
		c.printf("FAIL  %s%s %s\n", f.Kind, run, cleanText(f.Detail))
	}
	if err != nil {
		c.fail("%d finding(s): an erasure is not accounted for, not finished, or the trail does not hold together", len(rep.Findings))
	}
	if !c.clean() {
		return
	}
	c.printf("PASS: %d redaction(s) in %d trail record(s); every tombstone in the journals checked is named by the trail, and every listed record is erased\n", rep.Redactions, rep.Records)
}

// readRedactionStore loads the redaction trail and the journals of run (or of every run) from a store.
func (c *cli) readRedactionStore(in *audit.RedactionVerifyInput, db, pgEnv, run string) error {
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
	x, err := audit.ExportNotifyTrail(ctx, s.Events, audit.RedactionTrailName)
	if err != nil {
		return unusable(err)
	}
	in.Trail = x.Leaves
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
		if run == "" || id == run || len(id) > len(run) && id[:len(run)] == run && id[len(run)] == '>' {
			ids = append(ids, id)
		}
	}
	if run != "" && !slices.Contains(ids, run) {
		return unusable(fmt.Errorf("-run %s: the store holds no such run", run))
	}
	for _, id := range ids {
		recs, err := j.History(ctx, id)
		if err != nil {
			return unusable(fmt.Errorf("read run %s: %w", id, err))
		}
		in.Journals[id] = recs
	}
	return nil
}

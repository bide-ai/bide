package auditcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

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
	deployment := fs.String("deployment", "", "the deployment name every trail record must be signed for (required; RedactionOptions.Deployment): a record copied from another deployment's trail under the same key is BAD_SIGNATURE")
	trailDB := fs.String("trail-db", "", "read the redaction trail from this SQLite file (opened read-only), when it is kept apart from the journal; the journals then come from -db or -postgres-env, or -journal")
	trailEnv := fs.String("trail-postgres-env", "", "read the redaction trail from the Postgres database whose DSN is in this environment variable (never given on the command line)")
	run := fs.String("run", "", "with -db or -postgres-env: only this run and its sub-runs; default every run")
	c.parse(fs, args)

	haveFiles := *trailPath != "" || len(journals) > 0
	haveTrailStore := *trailDB != "" || *trailEnv != ""
	haveStore := *db != "" || *pgEnv != "" || haveTrailStore
	switch {
	case *trailDB != "" && *trailEnv != "":
		c.usageLine(fs.Name() + ": give one of -trail-db and -trail-postgres-env, not both")
	case haveTrailStore && *trailPath != "":
		c.usageLine(fs.Name() + ": the trail is a file (-trail) or a store (-trail-db, -trail-postgres-env), not both")
	case *db != "" && *pgEnv != "":
		c.usageLine(fs.Name() + ": give one of -db and -postgres-env, not both; run `" + fs.Name() + " -h` for usage")
	case !haveStore && !haveFiles:
		c.usageLine(fs.Name() + ": missing -trail, or -db, or -postgres-env; run `" + fs.Name() + " -h` for usage")
	case *trailPath == "" && !haveStore:
		c.usageLine(fs.Name() + ": missing -trail")
	case (*db != "" || *pgEnv != "") && len(journals) > 0:
		c.usageLine(fs.Name() + ": the journals are files (-journal) or a store (-db, -postgres-env), not both")
	case *run != "" && *db == "" && *pgEnv == "":
		c.usageLine(fs.Name() + ": -run reads one run from a store; it needs -db or -postgres-env")
	case haveStore && Stores == nil:
		c.usageLine(fs.Name() + ": this build reads exported files only (-trail, -journal); -db and -postgres-env are in the full bide command")
	case strings.TrimSpace(*deployment) == "":
		c.usageLine(fs.Name() + ": missing -deployment: every trail record is signed for one deployment, and the check verifies it")
	case *pubkey == "":
		c.usageLine(fs.Name() + ": missing -pubkey: every trail record is signed with the redaction key, and the check verifies each (give -unanchored to read a trail with no signed head)")
	}
	in := audit.RedactionVerifyInput{Unanchored: *unanchored, Deployment: strings.TrimSpace(*deployment), Journals: map[string][]agent.Record{}}
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
	if *trailPath != "" {
		var x audit.NotifyTrailExport
		ok = c.note(c.readJSON(*trailPath, &x)) && ok
		if ok && x.Trail != audit.RedactionTrailName {
			ok = c.note(unusable(fmt.Errorf("-trail %s: this is the trail %q, not %q", *trailPath, x.Trail, audit.RedactionTrailName)))
		}
		in.Trail = x.Leaves
	}
	for _, p := range journals {
		var jx audit.JournalExport
		ok = c.note(c.readJSON(p, &jx)) && ok
		if recs, err := jx.Journal(); err == nil {
			in.Journals[jx.RunID] = recs
		} else {
			ok = c.note(unusable(err)) && ok
		}
	}
	if haveStore {
		ok = c.note(c.readRedactionStore(&in, *db, *pgEnv, *run, *trailDB, *trailEnv, *trailPath != "")) && ok
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
	for _, n := range rep.Notes {
		c.printf("INFO  %s\n", cleanText(n))
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

// openNotifyStores opens the SQLite file or the Postgres database named by path or env.
func openNotifyStores(path, env string) (NotifyStores, error) {
	if path != "" {
		s, err := Stores.SQLite(path)
		if err != nil {
			return NotifyStores{}, unusable(fmt.Errorf("open the store: %w", err))
		}
		return s, nil
	}
	dsn := os.Getenv(env)
	if dsn == "" {
		return NotifyStores{}, unusable(fmt.Errorf("%s: the variable is empty or not set", env))
	}
	s, err := Stores.Postgres(dsn)
	if err != nil {
		return NotifyStores{}, unusable(fmt.Errorf("open the store: %w", err))
	}
	return s, nil
}

// readRedactionStore loads the redaction trail (from the trail store if one is named, else from the
// journal's store, unless the trail came from a file) and the journals of run (or of every run) from
// the journal store.
func (c *cli) readRedactionStore(in *audit.RedactionVerifyInput, db, pgEnv, run, trailDB, trailEnv string, trailFromFile bool) error {
	ctx := context.Background()
	var journalStore NotifyStores
	haveJournals := db != "" || pgEnv != ""
	if haveJournals {
		s, err := openNotifyStores(db, pgEnv)
		if err != nil {
			return err
		}
		defer s.Close()
		journalStore = s
	}
	if !trailFromFile {
		events := journalStore.Events
		if trailDB != "" || trailEnv != "" {
			ts, err := openNotifyStores(trailDB, trailEnv)
			if err != nil {
				return err
			}
			defer ts.Close()
			events = ts.Events
		}
		x, err := audit.ExportNotifyTrail(ctx, events, audit.RedactionTrailName)
		if err != nil {
			return unusable(err)
		}
		in.Trail = x.Leaves
	}
	if !haveJournals {
		return nil
	}
	lister, ok := agent.Capability[agent.Lister](journalStore.Journal)
	if !ok {
		return unusable(errors.New("the store cannot list its runs"))
	}
	j, err := agent.NewJournal(journalStore.Journal)
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

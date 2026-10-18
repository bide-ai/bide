// Package journalcmd is bide journal: the commands that act on a journal's contents. Today there is
// one, bide journal redact, which erases personal data from finished runs accountably:
//
//	bide journal redact -db journal.db -run r1 -payloads                          # a dry run: lists what would be replaced
//	bide journal redact -db journal.db -run r1 -payloads -execute -plan-digest <d> \
//	    -reason "customer request" -ticket PRIV-42 -sign-key log.key -head-out heads.jsonl -pubkey log.pub -sth r1.sth
//
// It never prints record content. A database DSN is read from an environment variable, and a signing
// key from a file, never from a flag value.
package journalcmd

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/internal/approvecmd"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/store/postgres"
	"github.com/bide-ai/bide/store/sqlite"
)

// Verbs are the verbs of bide journal.
func Verbs() []string { return []string{"redact", "acknowledge", "hold"} }

// getenv is replaced by tests.
var getenv = os.Getenv

// Main runs bide journal with args (after "journal") and returns the exit status: 0 when the plan was
// listed or the erasure finished, 1 when it was refused or failed, 2 for a usage error.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "-help" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stderr, usage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "redact":
		return redact(args[1:], stdout, stderr)
	case "acknowledge":
		return acknowledge(args[1:], stdout, stderr)
	case "hold":
		return hold(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "%sbide journal: unknown verb %q\n\n%s", term.For(stderr, term.Auto).ErrorPrefix(), args[0], usage)
	return 2
}

const usage = `usage: bide journal redact (-db <file> | -postgres-env <var> [-schema <name>]) -run <id> <selection>
                           [-execute -plan-digest <hex> -reason <text> -ticket <id> -sign-key <file> -head-out <file> ...]

  redact  replace the content of records of a finished run with tombstones that keep each record's
          position, name and leaf hash, so the run's signed heads and every other record's proof
          still verify, and record the erasure in a signed trail. Run bide journal redact -h.
  hold    place, lift and list legal holds: the store refuses to erase a held run, and the signed
          redaction trail records who placed and lifted each hold. Run bide journal hold -h.
  acknowledge  sign a note that a leaf in the redaction trail that the tool did not write (a planted
          one that bide audit redactions reports) has been investigated; the audit then reports it as
          acknowledged. Run bide journal acknowledge -h.
`

// stringList is a repeatable flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// copiesNotReached names what an erasure in the journal does not reach; the report prints it.
var copiesNotReached = []string{
	"exported journals, evidence packages, proofs and approver receipts made earlier (they hold the bytes they were made from)",
	"the event store (bide_events), a copy of event content with no tombstone for an event leaf",
	"Postgres dead tuples until VACUUM, SQLite free pages and the write-ahead log until a checkpoint and VACUUM, replicas, WAL archives, base backups and dumps",
	"logs, traces, the application's own tables, and the model provider's retention of what was sent to it",
	"govern event logs, which hold application-chosen strings",
}

func redact(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide journal redact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", "the SQLite journal to erase from")
	pgEnv := fs.String("postgres-env", "", "the environment variable that holds the Postgres DSN of a role that can read the journal (never given on the command line)")
	redactEnv := fs.String("redact-postgres-env", "", "the environment variable that holds the DSN of the role that may erase (EXECUTE on bide_redact_v3 and no table privilege); default: the -postgres-env role")
	schema := fs.String("schema", "", "with -postgres-env: the schema that holds the journal (recommended; without it the DSN's search path decides)")
	run := fs.String("run", "", "the run to erase from")
	var names, prefixes, kinds stringList
	fs.Var(&names, "name", "erase the record with this key; repeat")
	fs.Var(&prefixes, "prefix", "erase the records whose key starts with this; repeat")
	fs.Var(&kinds, "kind", "erase the records of this kind (model, tool_result, value, signal, approval); repeat")
	payloads := fs.Bool("payloads", false, "erase every record whose content is payload: the input and principal, model turns, tool results, user steps, signals, approval requests (never an end marker, which is erased only by naming it; never a protected record, never an approval decision unless -include-approvals)")
	approvals := fs.Bool("include-approvals", false, "let the selection also erase approval decisions, which verify-approvals and decision coverage read")
	subruns := fs.Bool("subruns", false, "also erase from the run's sub-agent runs (each is planned and recorded on its own)")
	execute := fs.Bool("execute", false, "erase; without it the command only lists the plan")
	digest := fs.String("plan-digest", "", "with -execute: the plan digest a dry run printed; the erasure runs only if the plan is still exactly that")
	reason := fs.String("reason", "", "with -execute: why (recorded in the trail)")
	ticket := fs.String("ticket", "", "with -execute: the ticket or authority (recorded in the trail)")
	signKey := fs.String("sign-key", "", "with -execute: the log signing key file that signs the redaction trail's head (never printed)")
	deployment := fs.String("deployment", "", "with -execute: the deployment name signed into every trail record (required): records cannot be copied between deployments that share a key")
	acceptUnheaded := fs.Bool("accept-unheaded-trail", false, "with -execute: sign the trail although it already holds records and no earlier head (-head-out) attests them, so they are attested only from this head on; never needed on a trail's first erasure")
	headOut := fs.String("head-out", "", "with -execute: append the trail's new signed head, as one JSON line, to this file, for your anchor")
	trailDB := fs.String("trail-db", "", "keep the redaction trail in this SQLite file instead of the journal's (recommended: a store the erasing role cannot rewrite)")
	trailEnv := fs.String("trail-postgres-env", "", "keep the redaction trail in the Postgres database whose DSN is in this variable, instead of the journal's")
	pubkey := fs.String("pubkey", "", "with -execute: the log's public key (ed25519:<hex>, or a file) that verifies the -sth heads")
	var sths stringList
	fs.Var(&sths, "sth", "with -execute: a signed journal head of the run; one that verifies, matches the journal and covers all of it is required; repeat")
	allowUncovered := fs.Bool("allow-uncovered", false, "with -execute: erase although no signed head covers the run (recorded in the trail as UNCOVERED, and reported by bide audit redactions)")
	keepEvents := fs.Bool("keep-event-copies", false, "with -execute: erase although the event store holds events of the run, which stay (recorded)")
	asJSON := fs.Bool("json", false, "print the plan and result as one JSON document")
	color := term.Flag(fs)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: bide journal redact (-db <file> | -postgres-env <var>) -run <id> (-name <key> | -prefix <p> | -kind <k> | -payloads)...

A dry run (the default) prints, for each run, whether it can be erased and the records it would
replace: position, key, kind, length and leaf hash, never content, and a plan digest. Review it, then
run the same command with -execute -plan-digest <digest> -reason ... -ticket ... -sign-key ... -head-out ...

Before erasing, make every piece of evidence you will need: a journal that holds a tombstone can no
longer produce a new absence proof, run certificate or event stream. The command refuses to erase a
run that no signed journal head (-sth, verified under -pubkey) covers, unless -allow-uncovered.

It refuses a run that is not over, whose lease is live (probed by taking the lease and giving it back; a role that cannot take a lease skips the probe and the store's own refusal applies), whose parent is not over, that belongs to a
session, that holds protected records (they are kept and listed), and a run the event store holds
events of (unless -keep-event-copies). It writes a "begin" record to the redaction trail before it
erases and an "end" record after; if it stops partway, run the same plan again.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	st := term.For(stdout, *color)
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "%sbide journal redact: %s\n", term.For(stderr, *color).ErrorPrefix(), fmt.Sprintf(format, a...))
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "%sbide journal redact: %s\n", term.For(stderr, *color).ErrorPrefix(), fmt.Sprintf(format, a...))
		return 1
	}
	switch {
	case fs.NArg() > 0:
		return usageErr("unexpected argument: every input is a flag, and a DSN is never an argument (it is read from the environment variable -postgres-env names)")
	case (*db == "") == (*pgEnv == ""):
		return usageErr("give one of -db and -postgres-env")
	case *run == "":
		return usageErr("-run is required")
	case len(names)+len(prefixes)+len(kinds) == 0 && !*payloads:
		return usageErr("select records: -name, -prefix, -kind or -payloads")
	case *redactEnv != "" && *pgEnv == "":
		return usageErr("-redact-postgres-env goes with -postgres-env")
	case *trailDB != "" && *trailEnv != "":
		return usageErr("give one of -trail-db and -trail-postgres-env")
	case *execute && (*reason == "" || *ticket == "" || *digest == "" || *signKey == "" || *headOut == "" || strings.TrimSpace(*deployment) == ""):
		return usageErr("-execute needs -plan-digest, -reason, -ticket, -deployment, -sign-key and -head-out")
	case !*execute && (*digest != "" || *reason != "" || *ticket != "" || *signKey != "" || *headOut != "" || *deployment != "" || *acceptUnheaded):
		return usageErr("-plan-digest, -reason, -ticket, -deployment, -sign-key and -head-out go with -execute")
	case *execute && !*allowUncovered && (len(sths) == 0 || *pubkey == ""):
		return usageErr("-execute needs a signed journal head (-sth and -pubkey) that covers the run, or -allow-uncovered")
	}
	ctx, stop := signalContext()
	defer stop()

	rd, err := openJournal(ctx, *db, *pgEnv, *schema, *redactEnv)
	if err != nil {
		return fail("%v", err)
	}
	defer rd.close()
	sel := agent.RedactionSelector{Names: names, Prefixes: prefixes, Payloads: *payloads, IncludeApprovals: *approvals}
	for _, k := range kinds {
		sel.Kinds = append(sel.Kinds, agent.StepKind(k))
	}
	runs := []string{*run}
	if *subruns {
		sub, err := subRuns(ctx, rd.journal, *run)
		if err != nil {
			return fail("%v", err)
		}
		runs = append(runs, sub...)
	}
	var plans []agent.RedactionPlan
	for _, id := range runs {
		p, err := agent.PlanRedaction(ctx, rd.journal, id, sel)
		if err != nil {
			return fail("plan run %s: %v", id, err)
		}
		plans = append(plans, p)
	}
	combined := planDigest(plans)
	rep := report{PlanDigest: combined, Plans: plans, CopiesNotReached: copiesNotReached}
	var blockers []string
	for _, p := range plans {
		if p.Refusal != "" {
			blockers = append(blockers, fmt.Sprintf("run %s: %s", p.RunID, p.Refusal))
		}
	}

	if !*execute {
		rep.DryRun = true
		rep.Refusals = blockers
		return emit(stdout, rep, *asJSON, st, 0)
	}

	// Execute: every refusal is checked before anything is written.
	if len(blockers) > 0 {
		return fail("refused: %s", strings.Join(blockers, "; "))
	}
	if !strings.EqualFold(*digest, combined) {
		return fail("refused: the plan is not the one reviewed (digest %s, not %s): the journal changed, or the selection did; list the plan again", combined, *digest)
	}
	signer, err := approvecmd.LoadSigner(*signKey)
	if err != nil {
		return fail("%v", err)
	}
	var verifier audit.Verifier
	var heads []audit.SignedTreeHead
	if *pubkey != "" {
		if verifier, err = readPubKey(*pubkey); err != nil {
			return fail("%v", err)
		}
	}
	for _, p := range sths {
		var h audit.SignedTreeHead
		if err := readJSON(p, &h); err != nil {
			return fail("%v", err)
		}
		heads = append(heads, h)
	}
	trail, closeTrail, err := openTrail(ctx, rd, *trailDB, *trailEnv, *schema)
	if err != nil {
		return fail("%v", err)
	}
	defer closeTrail()
	trailVerifier, err := audit.NewVerifier(signer.Alg(), signer.PublicKey())
	if err != nil {
		return fail("the signing key: %v", err)
	}
	// The heads the trail has had so far: the -head-out file the earlier passes wrote. The trail is
	// checked against them before anything is erased, and a head is signed only over a trail they
	// cover; a trail that already holds records and has no earlier head is not attested by anything.
	prior, err := readHeads(*headOut)
	if err != nil {
		return fail("%v", err)
	}
	existing, err := trail.Load(ctx, audit.RedactionTrailName)
	if err != nil {
		return fail("read the redaction trail: %v", err)
	}
	if len(prior) == 0 && len(existing) > 0 && !*acceptUnheaded {
		return fail("refused: the redaction trail already holds %d record(s) and %s holds no earlier signed head, so nothing attests them (a trail in the wrong database looks like this, and so does a plant): give the earlier heads in that file, or -accept-unheaded-trail to attest the trail as it stands from this head on", len(existing), *headOut)
	}
	if !*keepEvents {
		for _, p := range plans {
			if len(p.Records) == 0 {
				continue
			}
			leaves, err := rd.events.Load(ctx, p.RunID)
			if err != nil {
				return fail("read the event store: %v", err)
			}
			if len(leaves) > 0 {
				return fail("refused: the event store holds %d event(s) of run %s, a copy of what the journal held that this command cannot erase (give -keep-event-copies to erase the journal anyway, which is recorded)", len(leaves), p.RunID)
			}
		}
	}
	// A run with a live lease is refused before the trail is written: the store would refuse it
	// after a begin record, which leaves a failed pass in the trail for nothing.
	for _, p := range plans {
		if len(p.Records) == 0 {
			continue
		}
		if leased(ctx, rd.journal, p.RunID) {
			return fail("refused: a lease on run %s is live, so a driver may still be running it; nothing was written", p.RunID)
		}
	}
	now := time.Now()
	exec := executorName(signer)
	var passErr error
	for _, p := range plans {
		copies := 0
		if *keepEvents {
			if leaves, err := rd.events.Load(ctx, p.RunID); err == nil {
				copies = len(leaves)
			}
		}
		res, err := audit.RedactJournal(ctx, rd.journal, trail, p, audit.RedactionOptions{
			Reason: *reason, Ticket: *ticket, Executor: exec, Now: now, Version: version(), Signer: signer,
			Deployment: *deployment, TrailHeads: prior, EventCopiesKept: copies,
			Heads: heads, Verifier: verifier, AllowUncovered: *allowUncovered, Redactor: rd.redactor,
		})
		rep.Results = append(rep.Results, runResult{RunID: p.RunID, Redacted: len(res.Redacted), Already: len(res.Already)})
		if err != nil {
			rep.Results[len(rep.Results)-1].Error = err.Error()
			passErr = fmt.Errorf("run %s: %w", p.RunID, err)
			break
		}
	}
	// The storage layer: a store that keeps the bytes it replaced (SQLite's log) overwrites them.
	if c, ok := rd.redactor.(agent.Compactor); ok {
		cr, err := c.Compact(ctx)
		switch {
		case err != nil:
			rep.Compaction = &compaction{Detail: err.Error()}
		default:
			rep.Compaction = &compaction{Complete: cr.Complete, Detail: cr.Detail}
		}
	} else {
		rep.Compaction = &compaction{ByOperator: true, Detail: "this store does not overwrite the bytes it replaced: on Postgres they stay as dead tuples until VACUUM (and VACUUM FULL to return the space), which is the operator's to run, and replicas, WAL archives and backups hold their own copies"}
	}
	if passErr != nil && errors.Is(passErr, audit.ErrRedactTrailUntrusted) {
		// Nothing was written or erased: the trail is not the one the earlier heads attest, so no head is
		// signed over it either.
		emit(stdout, rep, *asJSON, st, 1)
		return fail("%v", passErr)
	}
	if after, err := trail.Load(ctx, audit.RedactionTrailName); err == nil && len(after) == len(existing) {
		// Nothing was written (a refusal before the first record): there is nothing new to attest.
		if passErr != nil {
			emit(stdout, rep, *asJSON, st, 1)
			return fail("%v (nothing was erased or written)", passErr)
		}
		rep.Executed = true
		fmt.Fprintln(stderr, "bide journal redact: nothing to erase: every selected record is already redacted or none was selected; no trail record or head was written")
		if rep.Compaction != nil && !rep.Compaction.Complete && !rep.Compaction.ByOperator {
			// Running the command again to finish an earlier pass's compaction: still not a success while the engine holds the old bytes.
			emit(stdout, rep, *asJSON, st, 1)
			return fail("the storage was not compacted, so the old bytes of an earlier erasure may remain in the database's files: close the readers of the database and run the compaction again")
		}
		return emit(stdout, rep, *asJSON, st, 0)
	}
	// The trail's head is signed after a failed pass too: the begin and end records it wrote are
	// attested, so the next attempt finds a trail its earlier heads cover.
	head, err := audit.SignRedactionTrail(ctx, trail, signer, trailVerifier, *deployment, prior, time.Now())
	if err != nil {
		emit(stdout, rep, *asJSON, st, 1)
		if passErr != nil {
			return fail("%v; and signing the trail head failed: %v", passErr, err)
		}
		return fail("the erasure ran and is recorded, but signing the trail head failed: %v (nothing was signed; the records are in the trail and bide audit redactions reports the unattested tail)", err)
	}
	if err := appendLine(*headOut, head); err != nil {
		emit(stdout, rep, *asJSON, st, 1)
		return fail("the erasure ran and is recorded, but writing the head to %s failed: %v", *headOut, err)
	}
	rep.HeadOut = *headOut
	if passErr != nil {
		emit(stdout, rep, *asJSON, st, 1)
		return fail("%v (the attempt is recorded in the redaction trail and its signed head was appended to %s)", passErr, *headOut)
	}
	rep.Executed = true
	if rep.Compaction != nil && !rep.Compaction.Complete && !rep.Compaction.ByOperator {
		// Erased and recorded, but the engine still holds the old bytes: not a success to report.
		emit(stdout, rep, *asJSON, st, 1)
		return fail("the erasure is recorded, but the storage was not compacted, so the old bytes may remain in the database's files: close the readers of the database and run the compaction again (bide journal redact repeats it with the same plan)")
	}
	return emit(stdout, rep, *asJSON, st, 0)
}

type runResult struct {
	RunID    string `json:"run_id"`
	Redacted int    `json:"redacted"`
	Already  int    `json:"already"`
	Error    string `json:"error,omitempty"`
}

type compaction struct {
	Complete bool   `json:"complete"`
	Detail   string `json:"detail"`
	// ByOperator is set for a store that does not compact (Postgres): the operator's VACUUM does.
	ByOperator bool `json:"by_operator,omitempty"`
}

type report struct {
	Compaction       *compaction           `json:"compaction,omitempty"`
	DryRun           bool                  `json:"dry_run"`
	Executed         bool                  `json:"executed"`
	PlanDigest       string                `json:"plan_digest"`
	Plans            []agent.RedactionPlan `json:"plans"`
	Refusals         []string              `json:"refusals,omitempty"`
	Results          []runResult           `json:"results,omitempty"`
	HeadOut          string                `json:"head_out,omitempty"`
	CopiesNotReached []string              `json:"copies_not_reached"`
}

func emit(w io.Writer, rep report, asJSON bool, st term.Style, code int) int {
	if asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(w, string(b))
		return code
	}
	for _, p := range rep.Plans {
		state := "over"
		if !p.Terminal {
			state = "NOT over"
		}
		fmt.Fprintf(w, "run %s: %d record(s) in the journal, run is %s\n", p.RunID, p.JournalSize, state)
		if p.Refusal != "" {
			fmt.Fprintf(w, "  REFUSED: %s\n", p.Refusal)
		}
		for _, r := range p.Records {
			note := ""
			if r.AlreadyRedacted {
				note = "  (already redacted)"
			}
			fmt.Fprintf(w, "  replace  #%d  %-40s %-12s %6d bytes  leaf %s%s\n", r.Position, clean(r.Name), r.Kind, r.Length, r.LeafHash[:16], note)
		}
		if rest := p.JournalSize - len(p.Records) - len(p.Kept); rest > 0 {
			fmt.Fprintf(w, "  keep     %d other record(s) unchanged (protected records, bookkeeping, or not selected)\n", rest)
		}
		for _, k := range p.Kept {
			fmt.Fprintf(w, "  keep     #%d  %-40s %s\n", k.Position, clean(k.Name), k.Reason)
		}
	}
	fmt.Fprintf(w, "plan digest: %s\n", rep.PlanDigest)
	for _, r := range rep.Results {
		fmt.Fprintf(w, "run %s: replaced %d, already redacted %d\n", r.RunID, r.Redacted, r.Already)
	}
	if c := rep.Compaction; c != nil {
		switch {
		case c.ByOperator:
			fmt.Fprintf(w, "storage, for the operator: %s\n", c.Detail)
		case c.Complete:
			fmt.Fprintf(w, "storage: %s\n", c.Detail)
		default:
			fmt.Fprintf(w, "STORAGE NOT COMPACTED: %s\n", c.Detail)
		}
	}
	if rep.Executed && rep.Compaction != nil && !rep.Compaction.Complete && !rep.Compaction.ByOperator {
		fmt.Fprintf(w, "erased and recorded; the trail's signed head was appended to %s\n", rep.HeadOut)
	} else if rep.Executed {
		fmt.Fprintf(w, "%s: erased and recorded; the trail's signed head was appended to %s\n", st.Heading("DONE"), rep.HeadOut)
	} else if rep.DryRun {
		fmt.Fprintf(w, "dry run: nothing was changed. To erase: add -execute -plan-digest %s -reason ... -ticket ... -sign-key ... -head-out ...\n", rep.PlanDigest)
	}
	fmt.Fprintln(w, "not reached by this command (the operator must cover them):")
	for _, c := range rep.CopiesNotReached {
		fmt.Fprintf(w, "  - %s\n", c)
	}
	return code
}

// clean makes a record key safe to print on one line.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			return '?'
		}
		return r
	}, s)
}

// planDigest combines the plans' digests, in run order, so one value names everything a review saw.
func planDigest(plans []agent.RedactionPlan) string {
	if len(plans) == 1 {
		return plans[0].Digest
	}
	ps := slices.Clone(plans)
	slices.SortFunc(ps, func(a, b agent.RedactionPlan) int { return cmp.Compare(a.RunID, b.RunID) })
	h := sha256.New()
	for _, p := range ps {
		h.Write([]byte(p.RunID + "\x00" + p.Digest + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func executorName(s audit.Signer) string {
	name := "unknown user"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	return fmt.Sprintf("%s (log key %s)", name, audit.KeyID(string(s.Alg()), s.PublicKey()))
}

func version() string { return "bide" }

func appendLine(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	w.Write(append(b, '\n'))
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s is not the JSON expected: %w", path, err)
	}
	return nil
}

func readPubKey(s string) (audit.Verifier, error) {
	if v, err := audit.ParsePublicKey(s); err == nil {
		return v, nil
	}
	b, err := os.ReadFile(s)
	if err != nil {
		return nil, fmt.Errorf("-pubkey: not a public key (ed25519:<hex> or 64 hex digits) and not a readable file: %w", err)
	}
	v, err := audit.ParsePublicKey(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("-pubkey %s: %w", s, err)
	}
	return v, nil
}

// reader is what the command reads and writes through.
type reader struct {
	journal  *agent.Journal
	redactor agent.Redactor
	events   audit.EventStore
	trail    audit.EventStore
	closers  []func() error
}

func (r *reader) close() {
	for _, c := range r.closers {
		c()
	}
}

func openJournal(ctx context.Context, db, pgEnv, schema, redactEnv string) (*reader, error) {
	r := &reader{}
	if db != "" {
		s, err := sqlite.Open(db)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", db, err)
		}
		r.closers = append(r.closers, s.Close)
		j, err := agent.NewJournal(s)
		if err != nil {
			return nil, err
		}
		r.journal, r.redactor, r.events, r.trail = j, s, s.EventStore(), s.EventStore()
		return r, nil
	}
	opts := []postgres.Option{postgres.WithoutMigration()}
	if schema != "" {
		opts = append(opts, postgres.WithSchema(schema))
	}
	open := func(env string) (*postgres.Store, error) {
		dsn := getenv(env)
		if dsn == "" {
			return nil, fmt.Errorf("-postgres-env %s: the variable is empty or not set", env)
		}
		s, err := postgres.Open(ctx, dsn, opts...)
		if err != nil {
			return nil, errors.New(redactDSN(err.Error(), dsn))
		}
		r.closers = append(r.closers, s.Close)
		return s, nil
	}
	s, err := open(pgEnv)
	if err != nil {
		return nil, err
	}
	j, err := agent.NewJournal(s)
	if err != nil {
		return nil, err
	}
	r.journal, r.redactor, r.events, r.trail = j, s, s.EventStore(), s.EventStore()
	if redactEnv != "" {
		rs, err := open(redactEnv)
		if err != nil {
			return nil, err
		}
		r.redactor = rs
	}
	return r, nil
}

func openTrail(ctx context.Context, r *reader, trailDB, trailEnv, schema string) (audit.EventStore, func(), error) {
	switch {
	case trailDB != "":
		s, err := sqlite.Open(trailDB)
		if err != nil {
			return nil, nil, fmt.Errorf("open the trail database %s: %w", trailDB, err)
		}
		return s.EventStore(), func() { s.Close() }, nil
	case trailEnv != "":
		dsn := getenv(trailEnv)
		if dsn == "" {
			return nil, nil, fmt.Errorf("-trail-postgres-env %s: the variable is empty or not set", trailEnv)
		}
		opts := []postgres.Option{postgres.WithoutMigration()}
		if schema != "" {
			opts = append(opts, postgres.WithSchema(schema))
		}
		s, err := postgres.Open(ctx, dsn, opts...)
		if err != nil {
			return nil, nil, errors.New(redactDSN(err.Error(), dsn))
		}
		return s.EventStore(), func() { s.Close() }, nil
	}
	return r.trail, func() {}, nil
}

// redactDSN removes the DSN and its password from an error text.
func redactDSN(msg, dsn string) string {
	msg = strings.ReplaceAll(msg, dsn, "<dsn>")
	if i := strings.Index(dsn, "://"); i >= 0 {
		if at := strings.LastIndex(dsn, "@"); at > i {
			if colon := strings.Index(dsn[i+3:at], ":"); colon >= 0 {
				if pw := dsn[i+3+colon+1 : at]; pw != "" {
					msg = strings.ReplaceAll(msg, pw, "<password>")
				}
			}
		}
	}
	return msg
}

// subRuns lists the sub-agent runs of run, by the store's Lister.
func subRuns(ctx context.Context, j *agent.Journal, run string) ([]string, error) {
	l, ok := agent.Capability[agent.Lister](j.Store())
	if !ok {
		return nil, errors.New("-subruns: the store cannot list its runs")
	}
	var out []string
	for id, err := range l.Runs(ctx, agent.RunFilter{Prefix: run + ">"}) {
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", err)
		}
		if agent.IsSubRun(id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// readHeads reads the signed heads in path, one JSON head per line; a file that does not exist holds
// none.
func readHeads(path string) ([]audit.SignedTreeHead, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var out []audit.SignedTreeHead
	for i, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var h audit.SignedTreeHead
		if err := json.Unmarshal([]byte(line), &h); err != nil {
			return nil, fmt.Errorf("%s line %d is not a signed head: %w", path, i+1, err)
		}
		out = append(out, h)
	}
	return out, nil
}

// leased reports whether another holder has a live lease on runID, found by trying to take it and
// giving it back at once. A store with no leases, or a role that cannot take one, says no: the
// store's own redaction refuses a leased run in any case.
func leased(ctx context.Context, j *agent.Journal, runID string) bool {
	l, ok := agent.Capability[agent.Leaser](j.Store())
	if !ok {
		return false
	}
	got, err := l.AcquireLease(ctx, runID, "bide-journal-redact-check", time.Second)
	if err != nil {
		return false
	}
	if got {
		l.ReleaseLease(ctx, runID, "bide-journal-redact-check")
	}
	return !got
}

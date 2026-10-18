package journalcmd

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/internal/approvecmd"
	"github.com/bide-ai/bide/internal/term"
)

// hold is bide journal hold: place, lift and list legal holds. A hold keeps the runs of a matter from
// being erased: the store refuses to replace a record of a held run, in the same statement that would.
// Placing and lifting are recorded in the signed redaction trail (who, when, the matter, the runs, the
// reason, the ticket), and bide audit redactions reports the holds in force, a hold the store and the
// trail disagree about, and any erasure that crossed one.
func hold(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "-help" || args[0] == "--help" {
		fmt.Fprint(stderr, holdUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "place", "lift", "list":
		return holdVerb(args[0], args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "%sbide journal hold: unknown verb %q\n\n%s", term.For(stderr, term.Auto).ErrorPrefix(), args[0], holdUsage)
	return 2
}

const holdUsage = `usage: bide journal hold place (-db <file> | -postgres-env <var>) -id <hold> -matter <text> -run <id>... [-tree]
                               -reason <text> -ticket <id> -deployment <name> -sign-key <file> -head-out <file>
       bide journal hold lift  (-db <file> | -postgres-env <var>) -id <hold> -reason <text> -ticket <id>
                               -deployment <name> -sign-key <file> -head-out <file>
       bide journal hold list  (-db <file> | -postgres-env <var>)

  place   keep the named runs (and with -tree the sub-agent runs below them) from being erased; one hold
          can name several runs, so a matter is one hold. The store enforces it first, then the placement
          is recorded in the signed redaction trail.
  lift    record the lift in the trail first, then remove the hold; a hold is never lifted without evidence.
  list    the holds in force in the store.

A hold stops bide's own erasure. It does not stop a database administrator, a restore or a hand-written
DELETE, and it does not protect the anchor (an object-store legal hold on the anchor is a separate
control). On Postgres the connecting role needs SELECT, INSERT and DELETE on the holds table and nothing
else; the role that erases cannot read or change it.
`

func holdVerb(verb string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide journal hold "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", "the SQLite journal")
	pgEnv := fs.String("postgres-env", "", "the environment variable that holds the Postgres DSN of the role that places and lifts holds (never given on the command line)")
	schema := fs.String("schema", "", "with -postgres-env: the schema that holds the journal")
	trailDB := fs.String("trail-db", "", "the SQLite file that holds the redaction trail, when kept apart from the journal")
	trailEnv := fs.String("trail-postgres-env", "", "the environment variable that holds the DSN of the Postgres database that holds the redaction trail")
	id := fs.String("id", "", "the hold's ID")
	matter := fs.String("matter", "", "place: the matter the hold is for, in words (recorded)")
	var runs stringList
	fs.Var(&runs, "run", "place: a run the hold covers; repeat")
	tree := fs.Bool("tree", false, "place: also cover the sub-agent runs below each run")
	reason := fs.String("reason", "", "place and lift: why (recorded)")
	ticket := fs.String("ticket", "", "place and lift: the ticket or authority (recorded)")
	deployment := fs.String("deployment", "", "place and lift: the deployment name signed into every trail record")
	signKey := fs.String("sign-key", "", "place and lift: the log signing key file that signs the record and the trail's head (never printed)")
	headOut := fs.String("head-out", "", "place and lift: the file of the trail's signed heads: read for the earlier heads, and the new head is appended")
	acceptUnheaded := fs.Bool("accept-unheaded-trail", false, "place and lift: sign although the trail holds records and no earlier head attests them")
	color := term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "%sbide journal hold %s: %s\n", term.For(stderr, *color).ErrorPrefix(), verb, fmt.Sprintf(format, a...))
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "%sbide journal hold %s: %s\n", term.For(stderr, *color).ErrorPrefix(), verb, fmt.Sprintf(format, a...))
		return 1
	}
	switch {
	case fs.NArg() > 0:
		return usageErr("unexpected argument: every input is a flag, and a DSN is never an argument")
	case (*db == "") == (*pgEnv == ""):
		return usageErr("give one of -db and -postgres-env")
	case *trailDB != "" && *trailEnv != "":
		return usageErr("give one of -trail-db and -trail-postgres-env")
	case verb != "list" && strings.TrimSpace(*id) == "":
		return usageErr("-id is required")
	case verb == "place" && (strings.TrimSpace(*matter) == "" || len(runs) == 0):
		return usageErr("place needs -matter and at least one -run")
	case verb != "list" && (strings.TrimSpace(*reason) == "" || strings.TrimSpace(*ticket) == "" || strings.TrimSpace(*deployment) == "" || *signKey == "" || *headOut == ""):
		return usageErr("-reason, -ticket, -deployment, -sign-key and -head-out are required")
	}
	ctx, stop := signalContext()
	defer stop()
	rd, err := openJournal(ctx, *db, *pgEnv, *schema, "")
	if err != nil {
		return fail("%v", err)
	}
	defer rd.close()
	holder, ok := agent.Capability[agent.Holder](rd.journal.Store())
	if !ok {
		return fail("this store keeps no legal holds")
	}
	if verb == "list" {
		hs, err := holder.Holds(ctx)
		if err != nil {
			return fail("%v", err)
		}
		if len(hs) == 0 {
			fmt.Fprintln(stdout, "no legal hold is in force")
		}
		for _, h := range hs {
			tree := ""
			if h.Tree {
				tree = " (and their sub-agent runs)"
			}
			fmt.Fprintf(stdout, "hold %s: matter %q, placed %s, covers %s%s\n", h.ID, clean(h.Matter), time.UnixMilli(h.PlacedAtMs).UTC().Format(time.RFC3339), strings.Join(cleanAll(h.Runs), ", "), tree)
		}
		return 0
	}
	signer, err := approvecmd.LoadSigner(*signKey)
	if err != nil {
		return fail("%v", err)
	}
	trail := rd.trail
	if *trailDB != "" || *trailEnv != "" {
		ts, closeTrail, err := openTrail(ctx, rd, *trailDB, *trailEnv, *schema)
		if err != nil {
			return fail("%v", err)
		}
		defer closeTrail()
		trail = ts
	}
	verifier, err := audit.NewVerifier(signer.Alg(), signer.PublicKey())
	if err != nil {
		return fail("the signing key: %v", err)
	}
	prior, err := readHeads(*headOut)
	if err != nil {
		return fail("%v", err)
	}
	existing, err := trail.Load(ctx, audit.RedactionTrailName)
	if err != nil {
		return fail("read the redaction trail: %v", err)
	}
	if len(prior) == 0 && len(existing) > 0 && !*acceptUnheaded {
		return fail("refused: the redaction trail holds %d record(s) and %s holds no earlier signed head, so nothing attests them: give the earlier heads in that file, or -accept-unheaded-trail", len(existing), *headOut)
	}
	exec := executorName(signer)
	now := time.Now()
	switch verb {
	case "place":
		err = audit.PlaceLegalHold(ctx, trail, holder, signer, *deployment, prior, agent.Hold{ID: *id, Matter: *matter, Runs: runs, Tree: *tree}, *reason, *ticket, exec, now)
	case "lift":
		err = audit.LiftLegalHold(ctx, trail, holder, signer, *deployment, prior, *id, *reason, *ticket, exec, now)
	}
	// Whatever happened, a record the trail gained is attested by a new head.
	if after, lerr := trail.Load(ctx, audit.RedactionTrailName); lerr == nil && len(after) > len(existing) {
		head, serr := audit.SignRedactionTrail(ctx, trail, signer, verifier, *deployment, prior, time.Now())
		if serr == nil {
			serr = appendLine(*headOut, head)
		}
		if serr != nil {
			if err != nil {
				return fail("%v; and the trail's head could not be signed and written: %v", err, serr)
			}
			return fail("the hold is recorded, but the trail's head could not be signed and written to %s: %v", *headOut, serr)
		}
	}
	if err != nil {
		return fail("%v", err)
	}
	if verb == "place" {
		fmt.Fprintf(stdout, "hold %s placed and recorded; the trail's signed head was appended to %s\n", *id, *headOut)
	} else {
		fmt.Fprintf(stdout, "hold %s lifted and recorded; the trail's signed head was appended to %s\n", *id, *headOut)
	}
	return 0
}

func cleanAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = clean(s)
	}
	return out
}

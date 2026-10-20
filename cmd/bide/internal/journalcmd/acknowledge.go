package journalcmd

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/term"
)

// acknowledge is bide journal acknowledge: the operator's signed note that a leaf in the redaction
// trail that the tool did not write has been looked into. It writes one acknowledge record (the
// leaf's position and the hash of its bytes, the reason, the ticket, who) and a new signed head.
func acknowledge(args []string, stdout, stderr io.Writer, k Keys) int {
	fs := flag.NewFlagSet("bide journal acknowledge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", "the SQLite journal whose event store holds the redaction trail")
	pgEnv := fs.String("postgres-env", "", "the environment variable that holds the Postgres DSN of a role that can append to the event store (never given on the command line)")
	schema := fs.String("schema", "", "with -postgres-env: the schema that holds the journal")
	trailDB := fs.String("trail-db", "", "the SQLite file that holds the redaction trail, when kept apart from the journal")
	trailEnv := fs.String("trail-postgres-env", "", "the environment variable that holds the DSN of the Postgres database that holds the redaction trail")
	position := fs.Int("position", -1, "the position in the trail of the leaf to acknowledge (bide audit redactions names it in its finding)")
	reason := fs.String("reason", "", "what was found, in words (recorded)")
	ticket := fs.String("ticket", "", "the ticket or authority (recorded)")
	deployment := fs.String("deployment", "", "the deployment name signed into every trail record (required)")
	signKey := fs.String("sign-key", "", "the log signing key file that signs the acknowledgement and the trail's head (never printed)")
	headOut := fs.String("head-out", "", "the file of the trail's signed heads: it is read for the earlier heads and the new head is appended to it")
	acceptUnheaded := fs.Bool("accept-unheaded-trail", false, "sign although the trail holds records and no earlier head attests them")
	color := term.Flag(fs)
	if k.Flags != nil {
		k.Flags(fs)
	}
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: bide journal acknowledge (-db <file> | -postgres-env <var> | -trail-db <file> | -trail-postgres-env <var>)
                                -position <n> -reason <text> -ticket <id> -deployment <name> -sign-key <file> -head-out <file>

Acknowledges a leaf in the redaction trail that the redaction tool did not write: one that bide audit
redactions reports as BAD_SIGNATURE, REORDERED or MALFORMED. Investigate it first (who could append to
the event store?). The acknowledgement is a signed record naming the leaf's position and the hash of its
bytes, with the reason, the ticket and who; the audit then reports that leaf as acknowledged and stops
failing on it. It acknowledges nothing else: not a record the tool wrote, not an erasure made by hand, and
not another leaf later at that position. It cannot be undone: the trail is append-only.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "%sbide journal acknowledge: %s\n", term.For(stderr, *color).ErrorPrefix(), fmt.Sprintf(format, a...))
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "%sbide journal acknowledge: %s\n", term.For(stderr, *color).ErrorPrefix(), fmt.Sprintf(format, a...))
		return 1
	}
	switch {
	case fs.NArg() > 0:
		return usageErr("unexpected argument: every input is a flag, and a DSN is never an argument")
	case (*db != "") && (*pgEnv != "") || (*trailDB != "") && (*trailEnv != ""):
		return usageErr("give one of -db and -postgres-env, and one of -trail-db and -trail-postgres-env")
	case *db == "" && *pgEnv == "" && *trailDB == "" && *trailEnv == "":
		return usageErr("name the store that holds the trail: -db, -postgres-env, -trail-db or -trail-postgres-env")
	case *position < 0:
		return usageErr("-position is required")
	case strings.TrimSpace(*reason) == "" || strings.TrimSpace(*ticket) == "" || strings.TrimSpace(*deployment) == "" || *signKey == "" || *headOut == "":
		return usageErr("-reason, -ticket, -deployment, -sign-key and -head-out are required")
	}
	ctx, stop := signalContext()
	defer stop()
	signer, err := k.Load(*signKey)
	if err != nil {
		return fail("%v", err)
	}
	if cl, ok := signer.(io.Closer); ok {
		defer cl.Close()
	}
	var trail audit.EventStore
	if *db != "" || *pgEnv != "" {
		rd, err := openJournal(ctx, *db, *pgEnv, *schema, "")
		if err != nil {
			return fail("%v", err)
		}
		defer rd.close()
		trail = rd.trail
		if *trailDB != "" || *trailEnv != "" {
			ts, closeTrail, err := openTrail(ctx, rd, *trailDB, *trailEnv, *schema)
			if err != nil {
				return fail("%v", err)
			}
			defer closeTrail()
			trail = ts
		}
	} else {
		ts, closeTrail, err := openTrail(ctx, &reader{}, *trailDB, *trailEnv, *schema)
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
	prior, err := readHeads("-head-out", *headOut)
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
	if err := audit.CheckRedactionTrail(ctx, trail, signer, *deployment, prior); err != nil {
		return fail("%v", err)
	}
	if err := audit.AcknowledgeRecord(ctx, trail, signer, *deployment, *position, *reason, *ticket, executorName(signer), time.Now()); err != nil {
		return fail("%v", err)
	}
	head, err := audit.SignRedactionTrail(ctx, trail, signer, verifier, *deployment, prior, time.Now())
	if err != nil {
		return fail("the acknowledgement is recorded, but signing the trail head failed: %v", err)
	}
	if err := appendLine(*headOut, head); err != nil {
		return fail("the acknowledgement is recorded, but writing the head to %s failed: %v", *headOut, err)
	}
	fmt.Fprintf(stdout, "acknowledged the leaf at position %d of the redaction trail; the trail's signed head was appended to %s\n", *position, *headOut)
	return 0
}

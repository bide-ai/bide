package auditcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// stops verifies a deployment's stop trail (audit.VerifyStops): the kill switch's signed records, as the
// trail an AuditedEventStore signed and anchored holds them, against the policy the deployment pinned,
// and, when it reads a store, against the control run.
func (c *cli) stops(args []string) {
	fs := c.flagSet("stops")
	trailPath := fs.String("trail", "", "a stop trail export (audit.ExportStopTrail as JSON)")
	var heads stringList
	fs.Var(&heads, "sth", "a signed tree head over the trail, as JSON (audit.SignStopTrail, or an entry's sth from the anchor log of the store that anchors it); repeat for as many as you have: each must verify and match a prefix of the trail, the largest is the newest")
	pubkey := fs.String("pubkey", "", "the "+pubkeyHelp+"; it verifies every -sth")
	unanchored := fs.Bool("unanchored", false, "accept a trail with no signed head: it is read, and nothing attests that it is complete or unaltered")
	policyPath := fs.String("stop-policy", "", "the stop policy the deployment pinned, as a JSON document (agent.StopPolicyDoc): the approvers' public keys, the setters and lifters, the lift quorum and the deployment ID; every record must verify under it and the chain of policy transitions from its root")
	root := fs.String("stop-root", "", "the digest (agent.StopPolicy.Digest) of the deployment's root policy, when -stop-policy is a later one the chain reaches; default the pinned policy itself")
	db := fs.String("db", "", "read the trail and the control run from this SQLite database (opened read-only) instead of from a file")
	pgEnv := fs.String("postgres-env", "", "read them from the Postgres database whose DSN is in this environment variable (never given on the command line)")
	maxTail := fs.Duration("max-unattested", 0, "fail when records after the newest signed head have gone unattested longer than this (0: report the tail only)")
	asOf := fs.String("as-of", "", "judge the heads and the tail as of this RFC 3339 time (default now)")
	c.parse(fs, args)

	haveStore := *db != "" || *pgEnv != ""
	switch {
	case *db != "" && *pgEnv != "":
		c.usageLine(fs.Name() + ": give one of -db and -postgres-env, not both; run `" + fs.Name() + " -h` for usage")
	case *trailPath != "" && haveStore:
		c.usageLine(fs.Name() + ": give an exported trail (-trail) or a store (-db, -postgres-env), not both")
	case *trailPath == "" && !haveStore:
		c.usageLine(fs.Name() + ": missing -trail, or -db, or -postgres-env; run `" + fs.Name() + " -h` for usage")
	case haveStore && Stores == nil:
		c.usageLine(fs.Name() + ": this build reads an exported trail only (-trail); -db and -postgres-env are in the full bide command")
	case *policyPath == "":
		c.usageLine(fs.Name() + ": missing -stop-policy (the policy the deployment pinned; without it no record can be verified); run `" + fs.Name() + " -h` for usage")
	case !*unanchored && *pubkey == "":
		c.usageLine(fs.Name() + ": missing -pubkey (and an -sth for the trail); give -unanchored to read a trail nothing attests")
	case len(heads) > 0 && *pubkey == "":
		c.usageLine(fs.Name() + ": -sth needs -pubkey")
	case *maxTail < 0:
		c.usageLine(fs.Name() + ": -max-unattested must not be negative")
	}
	now := time.Now()
	if *asOf != "" {
		t, err := time.Parse(time.RFC3339, *asOf)
		if err != nil {
			c.usageLine(fmt.Sprintf("%s: -as-of: %v", fs.Name(), err))
		}
		now = t
	}

	in := audit.StopVerifyInput{Unanchored: *unanchored, Now: now, MaxClockSkew: maxClockSkew, MaxTailAge: *maxTail}
	ok := true
	var doc agent.StopPolicyDoc
	ok = c.note(c.readJSON(*policyPath, &doc)) && ok
	if ok {
		pol, err := audit.StopPolicyFromDoc(doc)
		if err != nil {
			ok = c.note(unusable(fmt.Errorf("-stop-policy %s: %w", *policyPath, err))) && ok
		}
		opts := []agent.StopOption{agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(pol), agent.WithStopVerifier(audit.StopVerifierFromAudit)}
		if *root != "" {
			opts = append(opts, agent.WithStopRoot(*root))
		}
		in.Options = opts
	}
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
		ok = c.note(c.readJSON(*trailPath, &in.Trail)) && ok
	}
	if haveStore {
		ok = c.note(c.readStopStore(&in, *db, *pgEnv)) && ok
	}
	if !ok {
		return
	}

	rep, err := audit.VerifyStops(in)
	if err != nil && !errors.Is(err, audit.ErrNotVerified) {
		c.note(unusable(err))
		return
	}
	c.stopReport(rep)
	if b, merr := json.Marshal(rep); merr == nil {
		c.artifact = b
	}
	if err != nil {
		c.fail("%d problem(s): the stop trail does not hold together, or does not match the control run", len(rep.Problems))
	}
	if !c.clean() {
		return
	}
	active := 0
	for _, s := range rep.Stops {
		if !s.Lifted {
			active++
		}
	}
	c.printf("PASS: %d record(s): every one verifies under the pinned policy and its chain; %d stop(s), %d in force\n", rep.Records, len(rep.Stops), active)
}

// readStopStore loads into in the stop trail and the control run of the store.
func (c *cli) readStopStore(in *audit.StopVerifyInput, db, pgEnv string) error {
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
	if in.Trail, err = audit.ExportStopTrail(ctx, s.Events, agent.StopTrailName); err != nil {
		return unusable(err)
	}
	j, err := agent.NewJournal(s.Journal)
	if err != nil {
		return unusable(err)
	}
	ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopBasic))
	if err != nil {
		return unusable(err)
	}
	if in.Control, err = ctl.StopRecords(ctx); err != nil {
		return unusable(fmt.Errorf("read the control run: %w", err))
	}
	in.HaveControl = true
	return nil
}

func (c *cli) stopReport(rep audit.StopReport) {
	switch {
	case rep.Anchored:
		line := fmt.Sprintf("INFO  stop trail: %d records; signed head over the first %d (signed %s)", rep.Records, rep.HeadSize, rep.HeadTime.Format(time.RFC3339))
		if rep.Heads > 1 {
			line += fmt.Sprintf(", the newest of %d that all verify", rep.Heads)
		}
		if rep.Tail > 0 {
			line += fmt.Sprintf("; the last %d are not attested by it (the oldest for %s)", rep.Tail, rep.TailAge.Round(time.Second))
		}
		c.println(line)
	default:
		c.printf("INFO  stop trail: %d records; UNANCHORED, nothing attests that it is complete or unaltered\n", rep.Records)
	}
	if rep.PolicyDigest != "" {
		c.printf("INFO  policy in force %.12s after %d transition(s)\n", rep.PolicyDigest, rep.PolicySeq)
	}
	stops := slices.Clone(rep.Stops)
	for _, s := range stops {
		state := "IN FORCE"
		if s.Lifted {
			state = "lifted"
		}
		line := fmt.Sprintf("INFO  stop %.12s: %s, set by %s at %s, %s", s.ID, cleanText(s.Scope), cleanText(s.By), s.At, state)
		if !s.Lifted && s.LiftsNeeded > 0 {
			line += fmt.Sprintf(" (%d of %d lifts)", s.LiftsHeld, s.LiftsNeeded)
		}
		if s.Reason != "" {
			line += fmt.Sprintf(": %s", cleanText(s.Reason))
		}
		c.println(line)
	}
	for _, r := range rep.StoreOnly {
		c.printf("FAIL  in the control run, not in the trail: %s by %s at %s\n", r.Kind, cleanText(r.By), r.At)
	}
	for _, r := range rep.TrailOnly {
		c.printf("FAIL  in the trail, not in the control run: %s by %s at %s\n", r.Kind, cleanText(r.By), r.At)
	}
	for _, p := range rep.Problems {
		c.printf("FAIL  %s\n", cleanText(p))
	}
}

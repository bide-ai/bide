package auditcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
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
	signHead := fs.Bool("sign-head", false, "after verifying the trail against the control run (-db or -postgres-env, required), sign a head over exactly the verified records with -log-key and append it, as one JSON line, to -head-out; the heads in that file are read back by -sth. Run it on a schedule: with no -max-unattested the audit fails on any record after the newest signed head")
	logKey := fs.String("log-key", "", "with -sign-head: the private key file that signs the head (as bide keygen writes it; on Unix not readable by group or others). It is the deployment's log key, not an approver's: -pubkey names its public half")
	headOut := fs.String("head-out", "", "with -sign-head: the file the signed head is appended to (created if absent); it is verified first, so a trail that shrank or was rewritten since an earlier head is refused, not re-signed")
	maxTail := fs.Duration("max-unattested", 0, "accept records after the newest signed head for this long: a tail older than that fails, and with 0 (default) any unattested tail fails, since whoever can write the trail's store could have deleted the newest records unseen")
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
	case (*logKey != "" || *headOut != "") && !*signHead:
		c.usageLine(fs.Name() + ": -log-key and -head-out go with -sign-head")
	case *signHead && (*logKey == "" || *headOut == ""):
		c.usageLine(fs.Name() + ": -sign-head needs -log-key and -head-out")
	case *signHead && !haveStore:
		c.usageLine(fs.Name() + ": -sign-head needs the store (-db or -postgres-env): a head is signed only over a trail that was compared with the control run")
	case *signHead && *pubkey == "":
		c.usageLine(fs.Name() + ": -sign-head needs -pubkey (the public half of -log-key), which verifies the heads already in -head-out and the new one")
	case *signHead && LoadLogSigner == nil:
		c.usageLine(fs.Name() + ": this build does not sign; -sign-head is in the full bide command")
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
		hs, err := c.readHeadFile(p)
		ok = c.note(err) && ok
		in.Heads = append(in.Heads, hs...)
	}
	if *signHead {
		// The head file's earlier heads are verified with the trail: a trail that shrank or was rewritten since
		// one is a problem, not something to sign over. The trail being signed is unanchored until this head.
		prev, err := c.readHeadFileIfExists(*headOut)
		ok = c.note(err) && ok
		in.Heads = append(in.Heads, prev...)
		in.Unanchored = true
		in.MaxTailAge = math.MaxInt64 // the records since the newest head are what this run is about to attest
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
	c.stopReport(rep, *maxTail)
	if *signHead && err == nil {
		c.signStopHead(in, rep, *logKey, *headOut)
	}
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

func (c *cli) stopReport(rep audit.StopReport, maxTail time.Duration) {
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
		line := fmt.Sprintf("INFO  stop %.12s: %s, set by %s at %s, %s", s.ID, cleanText(s.Scope), cleanText(s.By), cleanText(s.At), state)
		if !s.Lifted && s.LiftsNeeded > 0 {
			line += fmt.Sprintf(" (%d of %d lifts)", s.LiftsHeld, s.LiftsNeeded)
		}
		if s.Reason != "" {
			line += fmt.Sprintf(": %s", cleanText(s.Reason))
		}
		c.println(line)
	}
	for _, r := range rep.Unapplied {
		c.printf("WARN  UNAPPLIED %s by %s at %s: signed and on the trail, not in the control run (a write that did not finish); agent.StopControl.ApplyPending applies it\n", cleanText(r.Kind), cleanText(r.By), cleanText(r.At))
	}
	for _, r := range rep.Superseded {
		c.printf("WARN  SUPERSEDED %s by %s at %s: signed and on the trail, not in the control run, and a later record went on without it (an abandoned write); it cannot be applied\n", cleanText(r.Kind), cleanText(r.By), cleanText(r.At))
	}
	for _, r := range rep.StoreOnly {
		c.printf("FAIL  in the control run, not in the trail: %s by %s at %s\n", cleanText(r.Kind), cleanText(r.By), cleanText(r.At))
	}
	for _, r := range rep.TrailOnly {
		c.printf("FAIL  in the trail, not in the control run: %s by %s at %s\n", cleanText(r.Kind), cleanText(r.By), cleanText(r.At))
	}
	for _, p := range rep.Problems {
		c.printf("FAIL  %s\n", cleanText(p))
	}
}

// LoadLogSigner loads the private key that signs a stop-trail head (-log-key). The full bide command sets it
// (a key file, the file form bide keygen writes); the audit-only build leaves it nil, so -sign-head is refused
// there and the verifier's import graph stays the standard library and the core module.
var LoadLogSigner func(ref string) (audit.Signer, error)

// readHeadFile reads the signed heads in path: one JSON head, or several, one per line (the form -head-out
// appends), each read strictly.
func (c *cli) readHeadFile(path string) ([]audit.SignedTreeHead, error) {
	b, err := readInput(path)
	if err != nil {
		return nil, err
	}
	return c.parseHeads(path, b)
}

// readHeadFileIfExists is readHeadFile, and no heads for a file that does not exist yet.
func (c *cli) readHeadFileIfExists(path string) ([]audit.SignedTreeHead, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return c.readHeadFile(path)
}

func (c *cli) parseHeads(path string, b []byte) ([]audit.SignedTreeHead, error) {
	var out []audit.SignedTreeHead
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, unusable(fmt.Errorf("parse %s: %w", path, err))
		}
		var h audit.SignedTreeHead
		if err := audit.UnmarshalStrict(raw, &h); err != nil {
			return nil, unusable(fmt.Errorf("parse %s: %w", path, err))
		}
		if err := checkHeadTimes(reflect.ValueOf(&h), time.Now()); err != nil {
			return nil, &cliError{class: errNotVerified, err: fmt.Errorf("%s: %w", path, err)}
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil, unusable(fmt.Errorf("parse %s: no signed head in it", path))
	}
	return out, nil
}

// signStopHead signs a head over exactly the leaves in just verified and appends it to out. When the newest
// head already in out commits to every record, nothing is added: a scheduled run with nothing new is a no-op.
func (c *cli) signStopHead(in audit.StopVerifyInput, rep audit.StopReport, keyRef, out string) {
	if len(in.Trail.Leaves) == 0 {
		c.fail("the trail has no records: a head over nothing attests nothing")
	}
	if rep.Anchored && rep.Tail == 0 {
		c.printf("INFO  head of size %d already attests every record: nothing signed\n", rep.HeadSize)
		return
	}
	signer, err := LoadLogSigner(keyRef)
	if err != nil {
		c.note(internal(fmt.Errorf("-log-key: %w", err)))
		return
	}
	if cl, ok := signer.(io.Closer); ok {
		defer cl.Close()
	}
	if signer.Alg() != in.Verifier.Alg() || !bytes.Equal(signer.PublicKey(), in.Verifier.PublicKey()) {
		c.note(internal(errors.New("-log-key is not the key -pubkey names: a head signed with it would not verify; nothing signed")))
		return
	}
	h, err := audit.SignStopTrailExport(in.Trail, signer, time.Now())
	if err != nil {
		c.note(internal(fmt.Errorf("sign the head: %w", err)))
		return
	}
	if err := h.Verify(in.Verifier); err != nil {
		c.note(internal(fmt.Errorf("the head just signed does not verify under -pubkey: %w", err)))
		return
	}
	line, err := json.Marshal(h)
	if err != nil {
		c.note(internal(err))
		return
	}
	f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		c.note(internal(fmt.Errorf("-head-out %s: %w", out, err)))
		return
	}
	_, werr := f.Write(append(line, '\n'))
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		c.note(internal(fmt.Errorf("-head-out %s: %w", out, werr)))
		return
	}
	c.printf("HEAD  signed a head over the %d verified record(s) and appended it to %s\n", h.Size, out)
}

// Command bide-anchor verifies and exports the signed tree heads that an object-store anchor wrote to a
// bucket under Object Lock (-backend s3: the awss3 anchor, the default; -backend gcs: the Google Cloud
// Storage anchor).
//
//	bide-anchor verify [-backend s3] -dir ./downloaded -pubkey ed25519:<hex> [-run ID] [-kind journal]
//	bide-anchor export -pubkey K -run ID (-dir ./downloaded | -bucket B -owner ACCOUNT) [-o heads.jsonl]
//	bide-anchor check  -pubkey K -run ID -bucket B -owner ACCOUNT
//
// verify works offline on a directory of objects (for example "aws s3 sync s3://bucket/bide-anchor/
// ./downloaded/bide-anchor/"): it checks every object's name, encoding and signature. It cannot see
// the objects' lock state, which is S3 metadata and not in the file.
//
// export writes the verified heads of one run or trail, one per line in increasing size, the file
// that "bide audit verify-receipts -heads" and "verify-decision-coverage -heads" read.
//
// check reads the bucket with read-only credentials and reports how the heads are stored: delete
// markers, objects that are not locked, retention that has ended, governance-mode locks, rewritten
// keys. A report with findings exits 1.
//
// Credentials and the region come from the AWS SDK's default chain, never from flags. A server other
// than AWS (a test MinIO) is selected with AWS_ENDPOINT_URL_S3 and -path-style.
//
// Exit status: 0 verified, 1 an object or head did not verify or check found something, 2 usage or I/O (including an unreadable -dir),
// 3 the bucket's state is unknown (an online read was unavailable, denied or timed out: retry, and
// alert if it persists). An aborted audit is never 0, and it is not a finding.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/anchor/awss3"
	"github.com/bide-ai/bide/audit"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, defaultClient))
}

// clientFunc makes the S3 client; tests replace it.
type clientFunc func(ctx context.Context, pathStyle bool) (awss3.Client, error)

func defaultClient(ctx context.Context, pathStyle bool) (awss3.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = pathStyle }), nil
}

const usage = `usage:
  bide-anchor verify [-backend B] -dir DIR -pubkey KEY [-run ID] [-kind journal|events]
  bide-anchor export [-backend B] -pubkey KEY -run ID [-kind journal|events] (-dir DIR | -bucket B (-owner ACCOUNT | -project NUMBER) [-prefix P] [-path-style]) [-o FILE]
  bide-anchor check  [-backend B] -pubkey KEY -run ID [-kind journal|events] -bucket B (-owner ACCOUNT | -project NUMBER) [-prefix P] [-path-style]
KEY is the log operator's public key (ed25519:<hex>, or a file holding it).`

const (
	exitOK       = 0
	exitFindings = 1
	// exitUnknown: an online read was refused, failed or could not finish (unavailable, denied, timed out),
	// so the bucket's state is unknown. It is never 0 and never 1: retry, and alert if it persists.
	exitUnknown = 3
	exitUsage   = 2
)

func run(ctx context.Context, args []string, stdout, stderr io.Writer, mk clientFunc) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet("bide-anchor "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dir, run, bucket, ownerID, project, prefix, out string
		pubkey, backendName                             string
		kind                                            = audit.TreeJournal
		pathStyle, force                                bool
		kmsKey, lockMode                                string
		retention                                       time.Duration
		timeout                                         = 5 * time.Minute
	)
	// Each command takes only the flags that mean something to it, so its help lists only those.
	fs.StringVar(&backendName, "backend", "s3", "the object store the anchor wrote to: "+backendNames)
	fs.StringVar(&pubkey, "pubkey", "", "the log operator's public key (ed25519:<hex>, or a file holding it)")
	fs.StringVar(&kind, "kind", audit.TreeJournal, "the tree kind: journal, or events for a notification or event trail")
	switch cmd {
	case "verify":
		fs.StringVar(&dir, "dir", "", "a directory of downloaded anchor objects")
		fs.StringVar(&run, "run", "", "only this run or trail ID (default: every one)")
	case "export", "check":
		fs.StringVar(&run, "run", "", "the run or trail ID")
		if cmd == "export" {
			fs.StringVar(&dir, "dir", "", "read a directory of downloaded anchor objects (offline)")
			fs.StringVar(&out, "o", "", "write to this file instead of standard output")
			fs.BoolVar(&force, "force", false, "with -bucket: export even when the lock audit has findings")
		}
		fs.DurationVar(&timeout, "timeout", timeout, "with -bucket: give up after this long and exit 3 (state unknown); 0: no limit")
		fs.StringVar(&bucket, "bucket", "", "read this anchor bucket (online, with read-only credentials)")
		fs.StringVar(&ownerID, "owner", "", "-backend s3: the 12-digit AWS account ID that owns the bucket (with -bucket)")
		fs.StringVar(&project, "project", "", "-backend gcs: the number of the Google Cloud project that owns the bucket (with -bucket)")
		fs.StringVar(&prefix, "prefix", "", "the key prefix the anchor was configured with (default bide-anchor/)")
		fs.StringVar(&kmsKey, "kms-key", "", "the KMS key the heads must be encrypted with (gcs: projects/P/locations/L/keyRings/R/cryptoKeys/K; s3: an AWS KMS key ID (UUID), an mrk- key ID or a key ARN, nothing else, not an alias; leave it out on an S3-compatible server with named keys): a bucket default or an object with another key is a finding")
		fs.DurationVar(&retention, "retention", 0, "the retention the anchor was configured with: a bucket retention shorter than this (and, for s3, an object retained for less from its own write) is a finding; without it only the 24 h minimum is checked. A default retention in years counts as 365 days a year (7 years: 2555 days, 61320h), so pass at most that")
		fs.StringVar(&lockMode, "lock-mode", "", "-backend s3: the object lock mode the anchor was configured with, COMPLIANCE or GOVERNANCE: a bucket default that is weaker is a finding, and with GOVERNANCE a governance-locked object is not")
		fs.BoolVar(&pathStyle, "path-style", false, "use path-style addressing (a non-AWS server)")
	}
	switch cmd {
	case "verify", "export", "check":
	case "-h", "-help", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "bide-anchor: unknown command %q\n%s\n", cmd, usage)
		return exitUsage
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "bide-anchor: "+format+"\n", a...)
		return code
	}
	if fs.NArg() != 0 {
		return fail(exitUsage, "unexpected argument %q", fs.Arg(0))
	}
	if pubkey == "" {
		return fail(exitUsage, "-pubkey is required")
	}
	v, err := readKey(pubkey)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if kind != audit.TreeJournal && kind != audit.TreeEvents {
		return fail(exitUsage, "-kind %q is not journal or events", kind)
	}
	online := bucket != ""
	switch {
	case cmd == "verify" && (dir == "" || online):
		return fail(exitUsage, "verify takes -dir (and no -bucket): it works offline on downloaded objects; use check for a bucket")
	case cmd == "check" && (!online || dir != ""):
		return fail(exitUsage, "check takes -bucket and -owner or -project (and no -dir)")
	case cmd == "export" && online == (dir != ""):
		return fail(exitUsage, "export takes either -dir or -bucket, not both or neither")
	case cmd != "verify" && run == "":
		return fail(exitUsage, "-run is required")
	}
	if dir != "" {
		// -dir is a directory of downloaded objects: a path to one object file or a missing path is a usage
		// error, not a one-head export or a pass.
		if st, err := os.Stat(dir); err != nil {
			return fail(exitUsage, "-dir: %v", err)
		} else if !st.IsDir() {
			return fail(exitUsage, "-dir %s is not a directory: give the directory the objects were downloaded to", dir)
		}
	}

	if timeout < 0 {
		return fail(exitUsage, "-timeout must not be negative")
	}
	if lockMode != "" && lockMode != "COMPLIANCE" && lockMode != "GOVERNANCE" {
		return fail(exitUsage, "-lock-mode %q is not COMPLIANCE or GOVERNANCE", lockMode)
	}
	if flagSet(fs, "retention") && retention <= 0 {
		return fail(exitUsage, "-retention must be positive")
	}
	be, err := lookupBackend(backendName, mk)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	var reader headReader
	if online {
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		reader, err = be.open(ctx, openOptions{bucket: bucket, owner: ownerID, project: project, kmsKey: kmsKey, retention: retention, lockMode: lockMode, prefix: prefix, pathStyle: pathStyle, verifier: v,
			onSkip: func(f string) {
				fmt.Fprintf(stderr, "bide-anchor: skipped an object that is no anchored head: %s\n", f)
			}})
		if err != nil {
			// Opening may read the bucket (gcs checks its owner): a verdict is a finding (1), a mistake in
			// the call or its configuration is usage (2), and a read that did not finish is unknown (3).
			switch {
			case be.isFinding(err):
				return fail(exitFindings, "%v", err)
			case errors.Is(err, errUsage) || errors.Is(err, agent.ErrConfig):
				return fail(exitUsage, "%v", err)
			default:
				return failRead(be, fail, err)
			}
		}
	}

	switch cmd {
	case "check":
		rep, err := reader.Audit(ctx, kind, run, time.Now())
		if err != nil {
			return failRead(be, fail, err)
		}
		fmt.Fprintf(stdout, "%d verified head(s) for %s %q\n", len(rep.Heads), kind, run)
		for _, f := range rep.Findings {
			fmt.Fprintf(stdout, "FINDING: %s\n", f)
		}
		if len(rep.Heads) == 0 {
			fmt.Fprintf(stdout, "FAIL: no verified head is anchored for %s %q\n", kind, run)
			return exitFindings
		}
		if conflicts := equivocations(rep.Heads); len(conflicts) > 0 {
			for _, c := range conflicts {
				fmt.Fprintf(stdout, "FINDING: %s\n", c)
			}
			return exitFindings
		}
		if len(rep.Findings) > 0 {
			fmt.Fprintf(stdout, "FAIL: %d finding(s): do not rely on this anchor until they are explained\n", len(rep.Findings))
			return exitFindings
		}
		fmt.Fprintln(stdout, "PASS: every head verifies and is locked (against this key, at this time); this does not show the journal matches them: check the journal against the heads")
		return exitOK

	case "verify":
		heads, err := be.loadDir(dir, v)
		if err != nil {
			if !be.isFinding(err) {
				return fail(exitUsage, "cannot read %s: %v", dir, err) // an I/O failure, not a verdict about the objects
			}
			fmt.Fprintf(stdout, "FAIL: %v\n", err)
			return exitFindings
		}
		heads = filter(heads, kind, run, cmd == "verify" && flagSet(fs, "kind"))
		if len(heads) == 0 {
			fmt.Fprintln(stdout, "FAIL: no verified head matches")
			return exitFindings
		}
		for _, line := range summarize(heads) {
			fmt.Fprintln(stdout, line)
		}
		if conflicts := equivocations(heads); len(conflicts) > 0 {
			for _, c := range conflicts {
				fmt.Fprintf(stdout, "FAIL: %s\n", c)
			}
			return exitFindings
		}
		fmt.Fprintf(stdout, "PASS: %d head(s) verify under this key. This is an offline check of the files: it cannot show that S3 locked them (run check), nor that the directory is complete: a stale or partial sync verifies too, so compare the largest sizes above with the journal's.\n", len(heads))
		return exitOK

	default: // export
		var heads []audit.SignedTreeHead
		if online {
			// Export what the lock audit finds sound: a head under a weak lock, or next to a delete
			// marker, is evidence somebody should look at before it is relied on.
			rep, aerr := reader.Audit(ctx, kind, run, time.Now())
			if aerr != nil {
				return failRead(be, fail, aerr)
			}
			if len(rep.Findings) > 0 && !force {
				for _, f := range rep.Findings {
					fmt.Fprintf(stderr, "bide-anchor: FINDING: %s\n", f)
				}
				return fail(exitFindings, "%d finding(s) in how the heads are stored; not exporting (run check, or export -force to take the verified heads anyway)", len(rep.Findings))
			}
			heads, err = reader.Heads(ctx, kind, run)
		} else {
			heads, err = be.loadDir(dir, v)
			heads = filter(heads, kind, run, true)
			if err == nil && len(heads) == 0 {
				err = fmt.Errorf("%w for %s %q", be.errNotFound, kind, run)
			}
		}
		if errors.Is(err, awss3.ErrEquivocation) {
			return fail(exitFindings, "%v; not exporting", err)
		}
		if err != nil {
			if online {
				return failRead(be, fail, err)
			}
			if !be.isFinding(err) {
				return fail(exitUsage, "cannot read %s: %v", dir, err)
			}
			return fail(exitFindings, "%v", err)
		}
		if conflicts := equivocations(heads); len(conflicts) > 0 {
			return fail(exitFindings, "%s; not exporting", strings.Join(conflicts, "; "))
		}
		slices.SortStableFunc(heads, func(a, b audit.SignedTreeHead) int { return cmp.Compare(a.Size, b.Size) })
		var sb strings.Builder
		for _, h := range heads {
			b, err := json.Marshal(h)
			if err != nil {
				return fail(exitUsage, "%v", err)
			}
			sb.Write(b)
			sb.WriteByte('\n')
		}
		for _, line := range summarize(heads) {
			fmt.Fprintf(stderr, "bide-anchor: %s\n", line)
		}
		fmt.Fprintln(stderr, "bide-anchor: this is every head in the set you gave, not proof the set is complete: a stale or partial copy verifies too. Compare the largest size with the journal's current size (bide audit against these heads).")
		if out == "" {
			io.WriteString(stdout, sb.String())
			return exitOK
		}
		if err := os.WriteFile(out, []byte(sb.String()), 0o644); err != nil {
			return fail(exitUsage, "%v", err)
		}
		fmt.Fprintf(stderr, "bide-anchor: wrote %d verified head(s) to %s\n", len(heads), out)
		return exitOK
	}
}

func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// filter keeps the heads of run (all when run is empty) and, when byKind, of that kind.
func filter(heads []audit.SignedTreeHead, kind, run string, byKind bool) []audit.SignedTreeHead {
	var out []audit.SignedTreeHead
	for _, h := range heads {
		if run != "" && h.RunID != run || byKind && h.Kind != kind {
			continue
		}
		out = append(out, h)
	}
	return out
}

func summarize(heads []audit.SignedTreeHead) []string {
	type k struct{ kind, run string }
	n := map[k]int{}
	lo, hi := map[k]int{}, map[k]int{}
	var order []k
	for _, h := range heads {
		key := k{h.Kind, h.RunID}
		if _, ok := n[key]; !ok {
			order = append(order, key)
			lo[key], hi[key] = h.Size, h.Size
		}
		n[key]++
		lo[key], hi[key] = min(lo[key], h.Size), max(hi[key], h.Size)
	}
	slices.SortFunc(order, func(a, b k) int { return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.run, b.run)) })
	var out []string
	for _, key := range order {
		out = append(out, fmt.Sprintf("%s %q: %d head(s), sizes %d to %d", key.kind, key.run, n[key], lo[key], hi[key]))
	}
	return out
}

// equivocations reports two verified heads of one tree and size that commit to different roots: the
// log key signed two histories, which no honest operation produces.
func equivocations(heads []audit.SignedTreeHead) []string {
	type k struct {
		kind, run string
		size      int
	}
	first := map[k]audit.SignedTreeHead{}
	var out []string
	for _, h := range heads {
		key := k{h.Kind, h.RunID, h.Size}
		if p, ok := first[key]; ok {
			if !p.SameTree(h.TreeHead) {
				out = append(out, fmt.Sprintf("%s %q has two different signed heads of size %d (roots differ): the log key signed two histories", h.Kind, h.RunID, h.Size))
			}
			continue
		}
		first[key] = h
	}
	return out
}

// readKey reads the public key from its text form or a file holding it.
func readKey(s string) (audit.Verifier, error) {
	if v, err := audit.ParsePublicKey(s); err == nil {
		return v, nil
	}
	b, err := os.ReadFile(s)
	if err != nil {
		return nil, fmt.Errorf("-pubkey takes ed25519:<hex> or a file holding it: %q is neither", s)
	}
	v, err := audit.ParsePublicKey(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("public key in %s: %w", s, err)
	}
	return v, nil
}

// failRead reports an error from an online read. An error that is a finding (a head the log key did not
// sign, two histories, no head) exits 1. Any other (the store unavailable, access denied, a timeout)
// means the bucket's state is unknown, and exits 3, so that a script cannot take it for either a pass or
// a finding.
func failRead(be backend, fail func(int, string, ...any) int, err error) int {
	if be.isFinding(err) {
		return fail(exitFindings, "%v", err)
	}
	return fail(exitUnknown, "the state of the bucket is unknown (the read did not complete; retry, and alert if it persists): %v", err)
}

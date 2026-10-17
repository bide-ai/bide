// Command bide-anchor verifies and exports the signed tree heads that the awss3 anchor wrote to an S3
// bucket under Object Lock.
//
//	bide-anchor verify -dir ./downloaded -pubkey ed25519:<hex> [-run ID] [-kind journal]
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
// Exit status: 0 verified, 1 an object or head did not verify or check found something, 2 usage or I/O.
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
  bide-anchor verify -dir DIR -pubkey KEY [-run ID] [-kind journal|events]
  bide-anchor export -pubkey KEY -run ID [-kind journal|events] (-dir DIR | -bucket B -owner ACCOUNT [-prefix P] [-path-style]) [-o FILE]
  bide-anchor check  -pubkey KEY -run ID [-kind journal|events] -bucket B -owner ACCOUNT [-prefix P] [-path-style]
KEY is the log operator's public key (ed25519:<hex>, or a file holding it).`

const (
	exitOK       = 0
	exitFindings = 1
	exitUsage    = 2
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
		dir       = fs.String("dir", "", "a directory of downloaded anchor objects")
		pubkey    = fs.String("pubkey", "", "the log operator's public key (ed25519:<hex>, or a file holding it)")
		run       = fs.String("run", "", "the run or trail ID")
		kind      = fs.String("kind", audit.TreeJournal, "the tree kind: journal, or events for a notification or event trail")
		bucket    = fs.String("bucket", "", "the anchor bucket")
		ownerID   = fs.String("owner", "", "the 12-digit AWS account ID that owns the bucket")
		prefix    = fs.String("prefix", "", "the key prefix the anchor was configured with (default bide-anchor/)")
		pathStyle = fs.Bool("path-style", false, "use path-style addressing (a non-AWS server)")
		out       = fs.String("o", "", "export: write to this file instead of standard output")
	)
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
	if *pubkey == "" {
		return fail(exitUsage, "-pubkey is required")
	}
	v, err := readKey(*pubkey)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if *kind != audit.TreeJournal && *kind != audit.TreeEvents {
		return fail(exitUsage, "-kind %q is not journal or events", *kind)
	}
	online := *bucket != ""
	switch {
	case cmd == "verify" && (*dir == "" || online):
		return fail(exitUsage, "verify takes -dir (and no -bucket): it works offline on downloaded objects; use check for a bucket")
	case cmd == "check" && (!online || *dir != ""):
		return fail(exitUsage, "check takes -bucket and -owner (and no -dir)")
	case cmd == "export" && online == (*dir != ""):
		return fail(exitUsage, "export takes either -dir or -bucket, not both or neither")
	case cmd != "verify" && *run == "":
		return fail(exitUsage, "-run is required")
	}

	var reader *awss3.Reader
	if online {
		c, err := mk(ctx, *pathStyle)
		if err != nil {
			return fail(exitUsage, "AWS configuration: %v", err)
		}
		reader, err = awss3.NewReader(awss3.ReaderConfig{Client: c, Bucket: *bucket, ExpectedBucketOwner: *ownerID, Prefix: *prefix, Verifier: v})
		if err != nil {
			return fail(exitUsage, "%v", err)
		}
	}

	switch cmd {
	case "check":
		rep, err := reader.Audit(ctx, *kind, *run, time.Now())
		if err != nil {
			return fail(exitFindings, "%v", err)
		}
		fmt.Fprintf(stdout, "%d verified head(s) for %s %q\n", len(rep.Heads), *kind, *run)
		for _, f := range rep.Findings {
			fmt.Fprintf(stdout, "FINDING: %s\n", f)
		}
		if len(rep.Heads) == 0 {
			fmt.Fprintf(stdout, "FAIL: no verified head is anchored for %s %q\n", *kind, *run)
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
		heads, err := awss3.LoadDir(*dir, v)
		if err != nil {
			fmt.Fprintf(stdout, "FAIL: %v\n", err)
			return exitFindings
		}
		heads = filter(heads, *kind, *run, cmd == "verify" && flagSet(fs, "kind"))
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
		fmt.Fprintf(stdout, "PASS: %d head(s) verify under this key. This is an offline check of the files: it cannot show that S3 locked them; run check for that.\n", len(heads))
		return exitOK

	default: // export
		var heads []audit.SignedTreeHead
		if online {
			heads, err = reader.Heads(ctx, *kind, *run)
		} else {
			heads, err = awss3.LoadDir(*dir, v)
			heads = filter(heads, *kind, *run, true)
			if err == nil && len(heads) == 0 {
				err = fmt.Errorf("%w for %s %q", awss3.ErrNotFound, *kind, *run)
			}
		}
		if err != nil {
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
		if *out == "" {
			io.WriteString(stdout, sb.String())
			return exitOK
		}
		if err := os.WriteFile(*out, []byte(sb.String()), 0o644); err != nil {
			return fail(exitUsage, "%v", err)
		}
		fmt.Fprintf(stderr, "bide-anchor: wrote %d verified head(s) to %s\n", len(heads), *out)
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

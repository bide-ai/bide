package identitycmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/bide-ai/bide/internal/identitypreflight"
	"github.com/bide-ai/bide/internal/term"
)

// runCheck is bide identity check: the pre-flight of a certificate-bound approval setup. The checks are the
// identitypreflight package's, which call the gate's own code; this wires flags, output and exit statuses.
func runCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide identity check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bide identity check -policy <file> -params <file> (-expect <file> | -expect-digest <hex> | -no-expect) (-crl-dir <dir> | -fetch | -no-status) [-cert <chain.pem> [-chain <more.pem>] [-as <approver id>]] [-at <time>] [-json]")
		fs.PrintDefaults()
	}
	var o identitypreflight.Options
	fs.StringVar(&o.Policy, "policy", "", "the certificate policy file the gate loads (the same file bide identity fetch-crl takes)")
	fs.StringVar(&o.Params, "params", "", "the declaration of the CA timings (ca_period, max_status_age, exec_status_age, per-issuer values): a copy of what the gate declares")
	fs.StringVar(&o.Expect, "expect", "", "the gate's recorded policy: the \"policy\" member of its identity record (or the whole record), to compare the policy file and -params with, member by member")
	fs.StringVar(&o.ExpectDigest, "expect-digest", "", "the gate's policy digest (lowercase hex SHA-256: the pause note's identity_policy_digest, or the identity record's policy_digest): compared with the digest of the policy file and -params, which is an equality only when -params states every member the digest covers (w, min_w, routine_grace, clock_tolerance, coverage_tolerance, allow_holds, consecutive_numbers, retained, effective_from); otherwise the check fails and names the missing members")
	fs.BoolVar(&o.NoExpect, "no-expect", false, "do not compare with the gate's recorded policy or digest; the result is then partial (exit 4) and never ready")
	fs.StringVar(&o.CRLDir, "crl-dir", "", "offline: read the CRLs from this directory (a bide identity fetch-crl archive, which is not written to, or a flat directory of .crl, .der or .pem files); no network is used")
	fs.BoolVar(&o.Fetch, "fetch", false, "online: fetch the CRLs from the policy's crl sources (http and https only, with the size limits, timeouts and same-host redirect rule of fetch-crl; nothing is stored). Run it on the gate's host")
	fs.BoolVar(&o.NoStatus, "no-status", false, "do not check CRL status; the result is then partial (exit 4) and never ready")
	fs.StringVar(&o.Cert, "cert", "", "an approver's certificate chain, PEM, leaf first (as bide approve -cert takes it): is it accepted for the approver, as a decision made now, and would the call run")
	fs.StringVar(&o.Chain, "chain", "", "more certificates (PEM) to carry behind -cert, such as an intermediates bundle")
	fs.StringVar(&o.As, "as", "", "the approver id the certificate is for (default: the policy's only approver id)")
	at := fs.String("at", "", "judge at this time, RFC 3339 (default now); not with -fetch")
	asJSON := fs.Bool("json", false, "print the report as one JSON document")
	color := term.Flag(fs)
	if st := parse(fs, args); st >= 0 {
		return st
	}
	if fs.NArg() > 0 {
		return usageErr(fs, stderr, "unexpected argument %q", fs.Arg(0))
	}
	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			return usageErr(fs, stderr, "-at: %v", err)
		}
		o.At = t
	}
	o.Client = httpClient()
	rep, err := identitypreflight.Run(ctx, o)
	var ue *identitypreflight.UsageError
	var ie *identitypreflight.IOError
	switch {
	case errors.As(err, &ue):
		return usageErr(fs, stderr, "%s", ue.Msg)
	case errors.As(err, &ie):
		st := term.For(stderr, *color)
		fmt.Fprintf(stderr, "%sbide identity check: %v\n", st.ErrorPrefix(), ie)
		return identitypreflight.ExitIO
	case err != nil:
		return failure("check", stderr, "%v", err)
	}
	if *asJSON {
		if err := rep.JSON(stdout); err != nil {
			return failure("check", stderr, "writing the report: %v", err)
		}
	} else {
		rep.Text(stdout, term.For(stdout, *color))
	}
	return rep.ExitCode()
}

// Package identitycmd is bide identity: commands for the certificate-bound approvers' status data.
//
//	bide identity fetch-crl         fetch, verify and archive the CRLs of the issuers a policy pins
//	bide identity check             pre-flight a certificate-bound approval setup, and say why a certificate would be refused
//	bide identity reset-high-water  set aside a CRL that poisoned an issuer's rollback mark
//	bide identity ack-reset         acknowledge a reset record, which stops its start-up report
//
// fetch-crl is the crlrefresh.Refresher: what it fetches and refuses is what a gate's embedded refresher
// does. It reads the same policy file as the gate and a declaration of the CA timings (the values the
// gate declares in CertApprover and StatusParams), writes the dated archive, and prints one JSON line
// per event on standard output.
package identitycmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/crlrefresh"
	"github.com/bide-ai/bide/internal/policyfile"
	"github.com/bide-ai/bide/internal/safefile"
	"github.com/bide-ai/bide/internal/term"
)

// Verbs are the verbs of bide identity, in the order help shows them.
func Verbs() []string { return []string{"fetch-crl", "check", "reset-high-water", "ack-reset"} }

// Main runs bide identity with args (after "identity") and returns the exit status: 0 on success, 1 for a
// failure (for fetch-crl -once, an issuer without a fresh CRL; for check, a setup that will not work), 2 for a
// usage error, and for check also 3 (a file could not be read) and 4 (nothing failed but part was not
// checked).
func Main(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, args, stdout, stderr)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "-help" || args[0] == "--help" || args[0] == "help" {
		usage(stderr)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "fetch-crl":
		return runFetch(ctx, args[1:], stdout, stderr)
	case "check":
		return runCheck(ctx, args[1:], stdout, stderr)
	case "reset-high-water":
		return runReset(ctx, args[1:], stdout, stderr)
	case "ack-reset":
		return runAck(ctx, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "%sbide identity: unknown verb %q\n\n", term.For(stderr, term.Auto).ErrorPrefix(), args[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: bide identity fetch-crl -policy <file> -params <file> -cache <dir> [-once]
       bide identity check -policy <file> -params <file> (-expect <file> | -expect-digest <hex> | -no-expect) (-crl-dir <dir> | -fetch | -no-status) [-cert <chain.pem>] [-json]
       bide identity reset-high-water -policy <file> -params <file> -cache <dir> -issuer <hex> -reason <text> -operator <name> [-set-aside <hash>]... [-accept-lost-listings]
       bide identity ack-reset -policy <file> -params <file> -cache <dir> -issuer <hex> -name <RESET-...json>

Keep the CRLs of a certificate policy's issuing CAs current and archived:

  fetch-crl         fetches the CRLs from the URLs in the policy file's "crl" member (http and https
                    only), verifies each against its issuer's own certificate, refuses rollbacks and
                    equivocation, and writes the dated archive. With -once it makes one pass and exits
                    1 unless every issuer holds a fresh CRL (a cron job); otherwise it runs until
                    interrupted.
  check             the pre-flight before go-live, and when an approver's decision is refused: does
                    the policy load, do the pinned CAs and intermediates match, are the CRLs reachable
                    and fresh enough for the declared ages, do the policy file and -params agree with the
                    gate's recorded policy, and would the gate accept an approver's certificate (given with
                    -cert) and let its call run. It runs the gate's own code for each answer and prints
                    one line per check with the fix, and what it cannot check ahead of time. Exit 0 ready,
                    1 will not work as configured, 2 usage, 3 a file could not be read, 4 nothing failed but
                    part was left unchecked (-no-status, -no-expect).
  reset-high-water  the way out of a poisoned rollback mark (a CA that issued one CRL with a mistaken
                    huge number, or restarted its numbering): sets aside the CRL at the mark, with a
                    durable record, so a restart cannot bring the mark back.
  ack-reset         acknowledges a reset record; until then every start reports it.

The CA timings come from the -params file, which must carry the values the gate declares. Standard
output is one JSON line per event. A gate process embeds the same refresher (crlrefresh.New);
this command is for the archive an audit kit reads and for monitoring. See
docs/guides/certificate-approvers.md.
`)
}

func parse(fs *flag.FlagSet, args []string) int {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	return -1
}

func usageErr(fs *flag.FlagSet, stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "%s%s: %s\n", term.For(stderr, term.Auto).ErrorPrefix(), fs.Name(), fmt.Sprintf(format, a...))
	fs.Usage()
	return 2
}

func failure(verb string, stderr io.Writer, format string, a ...any) int {
	st := term.For(stderr, term.Auto)
	fmt.Fprintf(stderr, "%s%s\n", st.ErrorPrefix(), st.Hint(fmt.Sprintf("bide identity %s: %s", verb, fmt.Sprintf(format, a...)), "; "))
	return 1
}

// common is the flags every verb takes.
type common struct {
	policy, params, cache string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.policy, "policy", "", "the certificate policy file (anchors, intermediates, and the crl and intermediate_certs members)")
	fs.StringVar(&c.params, "params", "", "the declaration of the CA timings (ca_period, max_status_age, exec_status_age, per-issuer values)")
	fs.StringVar(&c.cache, "cache", "", "the archive directory (created 0700; one that others can write to is refused)")
}

func (c *common) missing() string {
	switch {
	case c.policy == "":
		return "-policy is required"
	case c.params == "":
		return "-params is required"
	case c.cache == "":
		return "-cache is required"
	}
	return ""
}

// eventLine is the JSON form of an crlrefresh.Event.
type eventLine struct {
	Time     time.Time               `json:"time"`
	Event    string                  `json:"event"`
	Issuer   string                  `json:"issuer,omitempty"`
	Feed     string                  `json:"feed,omitempty"`
	URL      string                  `json:"url,omitempty"`
	Detail   string                  `json:"detail,omitempty"`
	Number   string                  `json:"number,omitempty"`
	Held     string                  `json:"held,omitempty"`
	Hash     string                  `json:"hash,omitempty"`
	Error    string                  `json:"error,omitempty"`
	Outcomes []crlrefresh.URLOutcome `json:"outcomes,omitempty"`
}

type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) write(v any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, _ := json.Marshal(v)
	_, _ = l.w.Write(append(b, '\n'))
}

func (l *lineWriter) event(e crlrefresh.Event) {
	ln := eventLine{Time: e.At, Event: string(e.Kind), Feed: e.Feed, URL: e.URL, Detail: e.Detail, Outcomes: e.Outcomes}
	if e.Issuer != (audit.SPKIHash{}) {
		ln.Issuer = hex.EncodeToString(e.Issuer[:])
	}
	if e.Number != nil {
		ln.Number = e.Number.String()
	}
	if e.Held != nil {
		ln.Held = e.Held.String()
	}
	if e.Hash != ([32]byte{}) {
		ln.Hash = hex.EncodeToString(e.Hash[:])
	}
	if e.Err != nil {
		ln.Error = e.Err.Error()
	}
	l.write(ln)
}

func (l *lineWriter) alarm(a crlrefresh.Alarm) {
	l.write(map[string]any{"time": a.At, "event": "alarm", "issuer": hex.EncodeToString(a.Issuer[:]), "active": a.Active,
		"critical": a.Critical, "age": a.Age.String(), "threshold": a.Threshold.String(), "limit": a.Limit.String(), "detail": a.Detail})
}

func (l *lineWriter) health(h crlrefresh.IssuerHealth) {
	m := map[string]any{"event": "health", "issuer": hex.EncodeToString(h.Issuer[:]), "urls": h.URLs, "delta_urls": h.DeltaURLs,
		"last_attempt": h.LastAttempt, "last_success": h.LastSuccess, "newest_this_update": h.NewestThisUpdate,
		"newest_next_update": h.NewestNextUpdate, "age": h.Age.String(), "next_expected": h.NextExpected, "next_attempt": h.NextAttempt,
		"consecutive_failures": h.ConsecutiveFailures, "last_error": h.LastError, "archive_backlog": h.ArchiveBacklog,
		"paired": h.Paired, "alarm": h.Alarm, "critical": h.Critical, "limit": h.Limit.String(), "alarm_age": h.AlarmAge.String(),
		"unacked_resets": h.UnackedResets}
	if h.NewestNumber != nil {
		m["newest_number"] = h.NewestNumber.String()
	}
	l.write(m)
}

// httpClient is the command's HTTP client: the library's defaults, plus the proxy from the environment
// (library code never reads it).
func httpClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	tr.DisableCompression = true
	tr.DialContext = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 5 * time.Second
	tr.ResponseHeaderTimeout = 10 * time.Second
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

// build loads the policy, the sources and the declaration and returns the refresher.
func build(c *common, lw *lineWriter) (*crlrefresh.Refresher, error) {
	pol, err := policyfile.Read(c.policy)
	if err != nil {
		return nil, err
	}
	src, certs, err := policyfile.ReadSources(c.policy)
	if err != nil {
		return nil, err
	}
	pb, err := safefile.Read(c.params, 1<<20, nil)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", c.params, err)
	}
	decl, err := audit.ParseStatusDecl(c.params, pb)
	if err != nil {
		return nil, err
	}
	snap := audit.NewStatusSnapshot(audit.WithIssuers(issuerCerts(pol, certs)...))
	ap, err := decl.Approver(pol, snap, time.Now)
	if err != nil {
		return nil, err
	}
	return crlrefresh.New(ap, src, crlrefresh.Options{
		Issuers: certs, DeltaPeriodFor: decl.DeltaPeriodFor, StandIn: &crlrefresh.StandIn{Missing: decl.MissingForDigest()}, Client: httpClient(), Archive: crlrefresh.DirArchive(c.cache),
		Hooks: crlrefresh.Hooks{OnEvent: lw.event, OnAlarm: lw.alarm},
	})
}

func issuerCerts(pol *audit.CertIdentity, inter []*x509.Certificate) []*x509.Certificate {
	return append(pol.TrustAnchors(), inter...)
}

func runFetch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide identity fetch-crl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bide identity fetch-crl -policy <file> -params <file> -cache <dir> [-once] [-health-every <duration>]")
		fs.PrintDefaults()
	}
	var c common
	c.register(fs)
	once := fs.Bool("once", false, "make one pass over every issuer and exit 1 unless each holds a fresh CRL (for cron); without it, run until interrupted")
	healthEvery := fs.Duration("health-every", 5*time.Minute, "how often a running fetch-crl prints a health line per issuer (0 prints none; -once prints one at the end)")
	if st := parse(fs, args); st >= 0 {
		return st
	}
	if m := c.missing(); m != "" {
		return usageErr(fs, stderr, "%s", m)
	}
	if fs.NArg() > 0 {
		return usageErr(fs, stderr, "unexpected argument %q", fs.Arg(0))
	}
	lw := &lineWriter{w: stdout}
	r, err := build(&c, lw)
	if err != nil {
		return failure("fetch-crl", stderr, "%v", err)
	}
	if *once {
		bad := 0
		for _, res := range r.RefreshOnce(ctx) {
			if res.Err != nil {
				bad++
				fmt.Fprintf(stderr, "%s%v\n", term.For(stderr, term.Auto).ErrorPrefix(), res.Err)
			}
		}
		for _, h := range r.Health() {
			lw.health(h)
		}
		if bad > 0 {
			return failure("fetch-crl", stderr, "%d issuer(s) without a fresh CRL", bad)
		}
		return 0
	}
	if *healthEvery > 0 {
		go func() {
			t := time.NewTicker(*healthEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					for _, h := range r.Health() {
						lw.health(h)
					}
				}
			}
		}()
	}
	if err := r.Run(ctx); err != nil {
		return failure("fetch-crl", stderr, "%v", err)
	}
	return 0
}

func issuerFlag(fs *flag.FlagSet) *string {
	return fs.String("issuer", "", "the issuing CA, as the lowercase hex SHA-256 of its public key (SubjectPublicKeyInfo)")
}

func parseIssuer(s string) (audit.SPKIHash, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(s))
	var h audit.SPKIHash
	if err != nil || len(raw) != len(h) {
		return h, fmt.Errorf("-issuer %q is not a SHA-256 in hex", s)
	}
	copy(h[:], raw)
	return h, nil
}

type hashList []string

func (h *hashList) String() string     { return strings.Join(*h, ",") }
func (h *hashList) Set(s string) error { *h = append(*h, s); return nil }

func runReset(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide identity reset-high-water", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bide identity reset-high-water -policy <file> -params <file> -cache <dir> -issuer <hex> -reason <text> -operator <name> [-set-aside <sha256 hex>]... [-accept-lost-listings]")
		fs.PrintDefaults()
	}
	var c common
	c.register(fs)
	issuer := issuerFlag(fs)
	reason := fs.String("reason", "", "why the mark is reset (recorded in the archive)")
	operator := fs.String("operator", "", "who resets it (recorded in the archive)")
	accept := fs.Bool("accept-lost-listings", false, "allow the reset although a CRL set aside lists a revocation that no remaining CRL lists (the serials are recorded)")
	var set hashList
	fs.Var(&set, "set-aside", "the SHA-256 (hex) of an archived CRL to set aside; repeatable. Without it, the archived CRL at the current mark")
	if st := parse(fs, args); st >= 0 {
		return st
	}
	if m := c.missing(); m != "" {
		return usageErr(fs, stderr, "%s", m)
	}
	ih, err := parseIssuer(*issuer)
	if err != nil {
		return usageErr(fs, stderr, "%v", err)
	}
	var hashes [][32]byte
	for _, s := range set {
		raw, err := hex.DecodeString(s)
		if err != nil || len(raw) != 32 {
			return usageErr(fs, stderr, "-set-aside %q is not a SHA-256 in hex", s)
		}
		hashes = append(hashes, [32]byte(raw))
	}
	lw := &lineWriter{w: stdout}
	r, err := build(&c, lw)
	if err != nil {
		return failure("reset-high-water", stderr, "%v", err)
	}
	if err := r.Seed(ctx); err != nil {
		return failure("reset-high-water", stderr, "reading the archive: %v", err)
	}
	if err := r.ResetHighWater(ctx, ih, crlrefresh.ResetRequest{Reason: *reason, Operator: *operator, SetAside: hashes, AcceptLostListings: *accept}); err != nil {
		return failure("reset-high-water", stderr, "%v", err)
	}
	return 0
}

func runAck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide identity ack-reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bide identity ack-reset -policy <file> -params <file> -cache <dir> -issuer <hex> -name <RESET-...json>")
		fs.PrintDefaults()
	}
	var c common
	c.register(fs)
	issuer := issuerFlag(fs)
	name := fs.String("name", "", "the reset record's file name, as the start-up report gives it")
	if st := parse(fs, args); st >= 0 {
		return st
	}
	if m := c.missing(); m != "" {
		return usageErr(fs, stderr, "%s", m)
	}
	ih, err := parseIssuer(*issuer)
	if err != nil {
		return usageErr(fs, stderr, "%v", err)
	}
	if *name == "" {
		return usageErr(fs, stderr, "-name is required")
	}
	lw := &lineWriter{w: stdout}
	r, err := build(&c, lw)
	if err != nil {
		return failure("ack-reset", stderr, "%v", err)
	}
	if err := r.AckReset(ctx, ih, *name); err != nil {
		return failure("ack-reset", stderr, "%v", err)
	}
	return 0
}

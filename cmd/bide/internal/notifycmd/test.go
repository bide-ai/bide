package notifycmd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/safefile"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/notify"
)

// kinds are the notification kinds a sample can be, in the order help lists them.
var kinds = []agent.NotificationKind{
	agent.NotificationApprovalPending,
	agent.NotificationApprovalExpired,
	agent.NotificationOutcomeUnknown,
	agent.NotificationRunAborted,
	agent.NotificationRecoveryFailing,
	agent.NotificationOverdue,
	agent.NotificationResolved,
	agent.NotificationEscalated,
	agent.NotificationStopSet,
	agent.NotificationStopLifted,
}

func kindNames() string {
	var s []string
	for _, k := range kinds {
		s = append(s, string(k))
	}
	return strings.Join(s, ", ")
}

// sampleNotification is a notification of kind for a run that does not exist. Its run ID and key
// hold a random nonce, so each test is a new event to a receiver that drops redeliveries (bide
// notify relay does), and the run ID says "notify-test" wherever a person reads it.
func sampleNotification(kind agent.NotificationKind, now time.Time, run string) (agent.Notification, error) {
	if run == "" {
		var nonce [4]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return agent.Notification{}, err
		}
		run = "notify-test-" + hex.EncodeToString(nonce[:])
	}
	n := agent.Notification{
		Kind: kind, RunID: run, EventAt: now.Add(-5 * time.Second), Observed: now, Deadline: now.Add(5 * time.Minute), Attempt: 1,
		Principal: &agent.Principal{OnBehalfOf: "notify-test", AuthorityRef: "bide notify test"},
	}
	switch kind {
	case agent.NotificationApprovalPending:
		n.Key = agent.ApprovalKey(run, "call_test", 0)
		n.Approval = &agent.NotifyApproval{ToolUseID: "call_test", ToolName: "refund", Need: 2, Of: 3, Approvers: []string{"alice", "bob", "carol"}, Approved: 1}
	case agent.NotificationApprovalExpired:
		// The expiry of the approval a test of kind approval_pending sends for the same run.
		n.Key = agent.ExpiredKey(run, "call_test")
		n.Approval = &agent.NotifyApproval{ToolUseID: "call_test", ToolName: "refund"}
		n.Deadline = time.Time{}
	case agent.NotificationOutcomeUnknown:
		at := n.EventAt
		n.Key = agent.HaltedKey(run, agent.OpTool, "call_test", at.UnixMilli())
		n.Halt = &agent.NotifyHalt{Op: agent.OpRef{Kind: agent.OpTool, ID: "call_test", ToolName: "refund"}, Cause: agent.HaltCrashed, AttemptedAt: at}
	case agent.NotificationRunAborted:
		n.Key = agent.AbortedKey(run)
		n.Reason = "notify test"
	case agent.NotificationRecoveryFailing:
		n.Key = agent.RecoveryKey(run, 1)
		n.Reason = "resume_error"
	case agent.NotificationOverdue:
		n.Key = agent.OverdueKey(agent.ApprovalKey(run, "call_test", 0))
		n.Reason = "deadline"
	case agent.NotificationResolved:
		// The all-clear of the approval a test of kind approval_pending sends for the same run.
		orig := agent.ApprovalKey(run, "call_test", 0)
		n.Key = agent.ResolvedKey(orig)
		n.Reason = "decided"
		n.Clears = &agent.NotifyClears{Key: orig, ID: agent.NotifyID(orig), Kind: agent.NotificationApprovalPending}
		n.Deadline = time.Time{}
	case agent.NotificationEscalated:
		// The first step of the chain of the approval a test of kind approval_pending sends for the same run.
		orig := agent.ApprovalKey(run, "call_test", 0)
		n.Key = agent.EscalationKey(orig, 1)
		n.Approval = &agent.NotifyApproval{ToolUseID: "call_test", ToolName: "refund", Need: 2, Of: 3, Approvers: []string{"alice", "bob", "carol"}, Approved: 1}
		n.Escalates = &agent.NotifyEscalates{Key: orig, ID: agent.NotifyID(orig), Kind: agent.NotificationApprovalPending, Level: 1}
		n.Reason, n.Receiver = "undecided", "escalation-test"
	case agent.NotificationStopSet, agent.NotificationStopLifted:
		// A stop that does not exist: its ID is the digest of the sample's run, so each test is a new event.
		sum := sha256.Sum256([]byte(run))
		id := hex.EncodeToString(sum[:])
		n.Stop = &agent.NotifyStop{ID: id, Scope: agent.StopScope{Kind: agent.StopTool, Name: "refund"}, Reason: "bide notify test", Compensation: agent.CompensationAllow, By: "notify-test", Host: "localhost", Mode: agent.StopBasic}
		n.Deadline = time.Time{}
		n.Key = agent.StopSetKey(id)
		if kind == agent.NotificationStopLifted {
			n.Key = agent.StopLiftedKey(id)
			n.Stop.LiftedBy = []string{"notify-test"}
		} else {
			n.Deadline = now.Add(5 * time.Minute)
		}
	default:
		return agent.Notification{}, fmt.Errorf("unknown kind %q (one of %s)", kind, kindNames())
	}
	n.ID = agent.NotifyID(n.Key)
	return n, nil
}

// testOpts are what runTest reads from the environment, replaced by tests.
var (
	getenv   = os.Getenv
	readFile = func(path string) ([]byte, error) { return safefile.Read(path, safefile.MaxDocument, nil) }
	timeNow  = time.Now
)

func runTest(ctx context.Context, args []string, stdout, stderr io.Writer, _ func(net.Addr)) int {
	fs := flag.NewFlagSet("bide notify test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `usage: bide notify test -url <receiver> [-kind <kind>] [-secret-env <VAR> | -secret-file <file>]
                        [-ca <file>] [-pin <hash>] [-client-cert <file> -client-key <file>] [-timeout <d>]

Sends one signed sample notification to the receiver at -url, the way the notifier sends a real
one (notify.NewWebhook: the same payload, signature, TLS 1.2+ and mutual TLS, CA and pin options),
and reports the TLS handshake, the HTTP status, whether the receiver acknowledged (answered 2xx)
and the timing. The sample is for a run that does not exist (its ID starts notify-test-); a relay
that forwards to Slack or PagerDuty forwards it too. The secret is never printed.

Kinds: %s. A resolved is the all-clear of the approval_pending with the same -run: send that first, then
the resolved, to see a relay open and then close a PagerDuty incident.

Exit status: 0 the receiver acknowledged; 1 it did not; 2 usage error.

`, kindNames())
		fs.PrintDefaults()
	}
	var sec secretSource
	sec.register(fs)
	rawURL := fs.String("url", "", "the receiver's URL, https (http only for localhost)")
	kind := fs.String("kind", string(agent.NotificationApprovalPending), "the kind of the sample notification: "+kindNames())
	ca := fs.String("ca", "", "verify the receiver against the CA certificates in this PEM file instead of the system roots")
	pin := fs.String("pin", "", "require the receiver's key to match this SPKI SHA-256 (64 hex characters or base64); two, comma separated, during a rotation")
	cert := fs.String("client-cert", "", "present this client certificate (PEM) for mutual TLS; needs -client-key")
	key := fs.String("client-key", "", "the client certificate's private key (PEM)")
	timeout := fs.Duration("timeout", notify.DefaultTimeout, "the request timeout")
	runID := fs.String("run", "", "the sample's run ID (default: notify-test-<random>, a new event each time). Send an approval_pending and then a resolved for the same -run to test a relay's all-clear")
	viewURL := fs.String("view-url", "", "put a view link for the run viewer's base URL in the payload, as WithViewURL does")
	viewDB := fs.String("view-db", "", "the journal file the payload's view command opens, as WithViewDB does")
	color := term.Flag(fs)
	if st := parse(fs, args); st >= 0 {
		return st
	}
	if fs.NArg() > 0 {
		return usageErr(fs, stderr, "unexpected argument %q", fs.Arg(0))
	}
	if *rawURL == "" {
		return usageErr(fs, stderr, "-url is required")
	}
	if (*cert == "") != (*key == "") {
		return usageErr(fs, stderr, "-client-cert and -client-key go together")
	}
	if !validKind(*kind) {
		return usageErr(fs, stderr, "unknown -kind %q (one of %s)", *kind, kindNames())
	}
	out := term.For(stdout, *color)

	secrets, err := sec.load(getenv, readFile)
	if err != nil {
		return failure("test", stderr, "%v", err)
	}
	opts := []notify.Option{notify.WithSecrets(secrets.current, secrets.previous), notify.WithTimeout(*timeout)}
	if *ca != "" {
		opts = append(opts, notify.WithServerCA(*ca))
	}
	if *pin != "" {
		opts = append(opts, notify.WithServerPin(strings.Split(*pin, ",")...))
	}
	if *cert != "" {
		opts = append(opts, notify.WithClientCert(*cert, *key))
	}
	if *viewURL != "" {
		opts = append(opts, notify.WithViewURL(*viewURL))
	}
	if *viewDB != "" {
		opts = append(opts, notify.WithViewDB(*viewDB))
	}
	hook, err := notify.NewWebhook(*rawURL, opts...)
	if err != nil {
		return failure("test", stderr, "%s", configMessage(err))
	}
	n, err := sampleNotification(agent.NotificationKind(*kind), timeNow(), *runID)
	if err != nil {
		return failure("test", stderr, "%v", err)
	}

	res, derr := hook.Probe(ctx, n)
	report(stdout, out, *rawURL, n, res, derr)
	if derr != nil {
		return 1
	}
	return 0
}

func validKind(k string) bool {
	for _, v := range kinds {
		if string(v) == k {
			return true
		}
	}
	return false
}

// configMessage is a NewWebhook error without its package and sentinel prefix.
func configMessage(err error) string {
	s := err.Error()
	s = strings.TrimPrefix(s, "notify: ")
	s = strings.TrimPrefix(s, agent.ErrConfig.Error()+": ")
	return s
}

// origin is the scheme and host of a URL: the part that is safe to print, since a webhook URL's path
// and query can be a credential.
func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "the receiver"
	}
	return u.Scheme + "://" + u.Host
}

func ms(d time.Duration) string {
	if d < time.Millisecond {
		return "<1 ms"
	}
	return fmt.Sprintf("%d ms", d.Milliseconds())
}

// report prints how the delivery went: one line each for the TLS handshake and the HTTP exchange,
// the signature, and a verdict, with a hint when it failed.
func report(w io.Writer, st term.Style, rawURL string, n agent.Notification, res notify.ProbeResult, derr error) {
	fmt.Fprintf(w, "%s %s to %s (event %s...)\n", st.Heading("bide notify test:"), st.Bold(string(n.Kind)), origin(rawURL), n.ID[:12])
	switch {
	case !res.TLS:
		fmt.Fprintf(w, "  %-9s %s\n", "tls", st.Dim("not used (http to localhost)"))
	case res.Handshake:
		extra := ""
		if a := res.Auth; a != nil {
			var parts []string
			if a.CustomCA {
				parts = append(parts, "private CA")
			}
			if a.ServerPinned {
				parts = append(parts, "key pinned")
			}
			if a.MTLS {
				parts = append(parts, "client certificate presented")
			}
			if len(parts) > 0 {
				extra = "; " + strings.Join(parts, ", ")
			}
		}
		fmt.Fprintf(w, "  %-9s %sok: TLS %s in %s%s\n", "tls", st.Pass(), res.TLSVersion, ms(res.HandshakeTime), extra)
	case res.HandshakeErr != nil:
		fmt.Fprintf(w, "  %-9s %sfailed: %s\n", "tls", st.Cross(), st.Fail(reason(res.HandshakeErr)))
	default:
		fmt.Fprintf(w, "  %-9s %s\n", "tls", st.Dim("not reached: no connection was made"))
	}
	switch {
	case res.Status != 0:
		verdict := "refused"
		if res.Status/100 == 2 {
			verdict = "acknowledged"
		}
		fmt.Fprintf(w, "  %-9s %sHTTP %d, %s (%d request%s, %s in all)\n", "delivery", mark(st, res.Status/100 == 2), res.Status, verdict, res.Requests, plural(res.Requests), ms(res.Elapsed))
	case res.HandshakeErr != nil:
		fmt.Fprintf(w, "  %-9s %s\n", "delivery", st.Dim("not sent: the handshake failed"))
	default:
		fmt.Fprintf(w, "  %-9s %sno response: %s\n", "delivery", st.Cross(), st.Fail(reason(derr)))
	}
	if a := res.Auth; a != nil {
		fmt.Fprintf(w, "  %-9s %s signed with the %s secret\n", "signature", a.Signature, a.SecretSlot)
	}
	if derr == nil {
		fmt.Fprintf(w, "%s%s\n", st.Pass(), st.BoldOK("the receiver acknowledged the test notification"))
		return
	}
	fmt.Fprintf(w, "%s%s\n", st.Cross(), st.BoldFail("the receiver did not acknowledge the test notification"))
	if h := hint(res, derr); h != "" {
		fmt.Fprintf(w, "  %s %s\n", st.Bold("next:"), h)
	}
}

func mark(st term.Style, ok bool) string {
	if ok {
		return st.Pass()
	}
	return st.Cross()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// reason is err's text without the package prefix and the URL that wraps a transport error.
func reason(err error) string {
	if err == nil {
		return "unknown"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	s := err.Error()
	s = strings.TrimPrefix(s, "notify: ")
	return s
}

// hint says what to try next for a failure, by what failed.
func hint(res notify.ProbeResult, err error) string {
	var perm *notify.PermanentError
	msg := ""
	if res.HandshakeErr != nil {
		msg = res.HandshakeErr.Error()
	} else if err != nil {
		msg = err.Error()
	}
	switch {
	case strings.Contains(msg, "unknown authority"):
		return "the receiver's certificate is not signed by a CA this machine trusts; pass -ca <file> with the CA that signed it (bide notify listen prints one)"
	case strings.Contains(msg, "not valid for") || strings.Contains(msg, "doesn't contain any IP SANs"):
		return "the certificate does not cover the host in -url; use a name or address the certificate lists"
	case strings.Contains(msg, "expired") || strings.Contains(msg, "not yet valid"):
		return "the receiver's certificate is expired or not yet valid; check its dates and this machine's clock"
	case strings.Contains(msg, "matches no configured pin"):
		return "the receiver's key is not the one pinned; compare -pin with the receiver's SPKI hash (it changes when the key does)"
	case strings.Contains(msg, "bad certificate") || strings.Contains(msg, "certificate required"):
		return "the receiver wants a client certificate this machine did not present or it did not accept; pass -client-cert and -client-key"
	case isTimeout(err) || isTimeout(res.HandshakeErr):
		return "no answer in time: is the address reachable, and is the receiver slow? raise -timeout if it needs longer"
	case errors.As(err, &perm) && (perm.Status == 401 || perm.Status == 403):
		return "the receiver refused the signature: its secret differs from this one, or it is rotating (use -previous-secret-env); the secret is never shown, compare how each side reads it"
	case errors.As(err, &perm) && perm.Status == 404:
		return "the receiver has nothing at that path; check the path in -url"
	case errors.As(err, &perm) && perm.Status == 405:
		return "the receiver does not accept POST at that path"
	case errors.As(err, &perm):
		return fmt.Sprintf("the receiver refused the delivery with HTTP %d; its logs say why", perm.Status)
	case res.Status >= 500 || res.Status == 429:
		return fmt.Sprintf("the receiver failed or is busy (HTTP %d); bide retries this, so look at the receiver's logs", res.Status)
	case isRefused(err) || isRefused(res.HandshakeErr):
		return "nothing is listening at that address and port; start the receiver (bide notify listen) or check -url"
	case res.Handshake && res.Status == 0:
		return "the connection broke after the handshake; a receiver that requires mutual TLS says so only now (TLS 1.3), so try -client-cert and -client-key, and look at the receiver's log"
	case isNoSuchHost(err):
		return "the host name does not resolve; check -url"
	case strings.Contains(msg, "http: server gave HTTP response to HTTPS client"):
		return "the receiver speaks plain http on that port; use an https URL, or put TLS in front of it"
	}
	return ""
}

// isRefused reports a refused connection by its error number, not by how the operating system
// words it ("connection refused" on Unix, "actively refused it" on Windows). Windows reports
// WSAECONNREFUSED (10061), which is not syscall.ECONNREFUSED there, so both are checked; 10061 is
// no error number on Unix, where nothing matches it.
func isRefused(err error) bool {
	return err != nil && (errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.Errno(10061)))
}

// isNoSuchHost reports a name that does not resolve, by the resolver's own error.
func isNoSuchHost(err error) bool {
	var dns *net.DNSError
	return errors.As(err, &dns) && dns.IsNotFound
}

// isTimeout reports a deadline or timeout from the context, the os package or the net package.
func isTimeout(err error) bool {
	var ne net.Error
	return err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()))
}

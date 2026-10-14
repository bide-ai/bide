package notifycmd

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/notify/relay"
)

// Defaults of bide notify relay's outbound credentials.
const (
	defaultSlackEnv     = "SLACK_WEBHOOK_URL"
	defaultPagerDutyEnv = "PAGERDUTY_ROUTING_KEY"
)

func runRelay(ctx context.Context, args []string, stdout, stderr io.Writer, ready func(net.Addr)) int {
	fs := flag.NewFlagSet("bide notify relay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: bide notify relay [-addr <host:port>] [-cert <file> -key <file>] [-client-ca <file>]
                         [-secret-env <VAR> | -secret-file <file>]
                         [-slack-url-env <VAR> | -slack-url-file <file>]
                         [-pagerduty-key-env <VAR> | -pagerduty-key-file <file>] [-view-url <url>]

Receives bide's signed webhooks and forwards each verified notification to Slack and PagerDuty.
Point bide at it (notify.NewWebhook with the same secret); it needs no bide library or journal.

For each request it checks the signature, the timestamp and the signed delivery ID (notify.Verify)
and answers 401 to anything else. It then forwards to every configured destination and answers 204
only when all of them took the notification; if one failed it answers 502, and bide sends the event
again. Because bide's delivery is at least once, the relay remembers the delivery IDs it forwarded
(-dedupe of them; -state keeps them across a restart) and acknowledges a repeat without forwarding it again; after a partial failure
only the destination that failed is tried again.

  Slack       an incoming webhook URL: a Block Kit message with the run, kind, summary, deadline,
              who may approve, and an "Open in viewer" button when there is a link (the payload's
              view.url, else -view-url plus the run ID). The URL is a credential: it is read from
              the environment or a file, and never printed or logged.
  PagerDuty   Events API v2: a trigger whose dedup_key is the event's ID (the same on every
              delivery, so PagerDuty collapses repeats) and a resolve for an all-clear notification.
              The routing key is read like the Slack URL.

A destination is on when its variable is set or its file is named; at least one is needed. The
relay never forwards error text or tool arguments, even if bide sends them. bide requires an https
URL for a receiver (http only for localhost), so give -cert and -key, or run the relay behind a
proxy that terminates TLS on the same host. Forwarding has a budget (-forward-timeout) below bide's
10 s request timeout. Stop it with Ctrl-C. Exit status: 0 after a clean stop; 1 failure; 2 usage.

`)
		fs.PrintDefaults()
	}
	var sec secretSource
	sec.register(fs)
	addr := fs.String("addr", "127.0.0.1:8080", "the address to listen on")
	certFile := fs.String("cert", "", "serve TLS with this certificate (PEM); needs -key. Without it the relay speaks plain http")
	keyFile := fs.String("key", "", "the private key (PEM) of -cert")
	clientCA := fs.String("client-ca", "", "require a client certificate signed by a CA in this PEM file (mutual TLS); needs -cert")
	slackEnv := fs.String("slack-url-env", defaultSlackEnv, "the environment variable that holds the Slack incoming webhook URL")
	slackFile := fs.String("slack-url-file", "", "read the Slack webhook URL from this file; used instead of -slack-url-env")
	pdEnv := fs.String("pagerduty-key-env", defaultPagerDutyEnv, "the environment variable that holds the PagerDuty Events API v2 routing key")
	pdFile := fs.String("pagerduty-key-file", "", "read the PagerDuty routing key from this file; used instead of -pagerduty-key-env")
	pdURL := fs.String("pagerduty-url", relay.DefaultPagerDutyURL, "the PagerDuty Events API endpoint (the EU region has its own)")
	viewURL := fs.String("view-url", "", "the run viewer's base URL, for the \"Open in viewer\" button when a notification has no view.url")
	dedupe := fs.Int("dedupe", relay.DefaultDedupeSize, "how many delivery IDs to remember")
	statePath := fs.String("state", "", "keep the delivery IDs forwarded and the events cleared in this file, so they survive a restart (without it a restart forgets them); one relay per file, enforced by a lock")
	fwd := fs.Duration("forward-timeout", relay.DefaultForwardTimeout, "the budget for forwarding one delivery; keep it below bide's request timeout (10 s)")
	color := term.Flag(fs)
	if st := parse(fs, args); st >= 0 {
		return st
	}
	if fs.NArg() > 0 {
		return usageErr(fs, stderr, "unexpected argument %q", fs.Arg(0))
	}
	if (*certFile == "") != (*keyFile == "") {
		return usageErr(fs, stderr, "-cert and -key go together")
	}
	if *clientCA != "" && *certFile == "" {
		return usageErr(fs, stderr, "-client-ca needs -cert and -key: mutual TLS is a TLS option")
	}
	if *dedupe < 1 {
		return usageErr(fs, stderr, "-dedupe needs a positive number")
	}
	out := term.For(stdout, *color)

	secrets, err := sec.load(getenv, readFile)
	if err != nil {
		return failure("relay", stderr, "%v", err)
	}
	var sinks []relay.Sink
	slackURL, err := readOne(getenv, readFile, *slackEnv, *slackFile)
	if err != nil {
		return failure("relay", stderr, "%v", err)
	}
	if slackURL != "" {
		s, err := relay.NewSlack(relay.SlackConfig{WebhookURL: slackURL, ViewURL: *viewURL})
		if err != nil {
			return failure("relay", stderr, "%v", err) // names the sink, never the URL
		}
		sinks = append(sinks, s)
	}
	pdKey, err := readOne(getenv, readFile, *pdEnv, *pdFile)
	if err != nil {
		return failure("relay", stderr, "%v", err)
	}
	if pdKey != "" {
		p, err := relay.NewPagerDuty(relay.PagerDutyConfig{RoutingKey: pdKey, URL: *pdURL})
		if err != nil {
			return failure("relay", stderr, "%v", err)
		}
		sinks = append(sinks, p)
	}
	if len(sinks) == 0 {
		return failure("relay", stderr, "nothing to forward to; set %s (a Slack incoming webhook URL) or %s (a PagerDuty routing key), or name a file with -slack-url-file or -pagerduty-key-file", *slackEnv, *pdEnv)
	}
	logf := func(f string, a ...any) {
		fmt.Fprintf(stderr, "%s relay: %s\n", timeNow().Format("15:04:05"), fmt.Sprintf(strings.TrimPrefix(f, "relay: "), a...))
	}
	h, err := relay.New(relay.Config{Secrets: secrets.list(), Sinks: sinks, DedupeSize: *dedupe, ForwardTimeout: *fwd, StatePath: *statePath, Logf: logf})
	if err != nil {
		return failure("relay", stderr, "%v", err)
	}

	defer h.Close() // releases the -state lock

	var cfg *tls.Config
	scheme := "http"
	if *certFile != "" {
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			return failure("relay", stderr, "loading the certificate: %v", err)
		}
		if cfg, err = serverTLS(cert, *clientCA); err != nil {
			return failure("relay", stderr, "%v", err)
		}
		scheme = "https"
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return failure("relay", stderr, "%v", err)
	}
	defer ln.Close()

	var names []string
	for _, s := range sinks {
		names = append(names, s.Name())
	}
	fmt.Fprintf(stdout, "%s %s://%s/\n", out.Heading("bide notify relay:"), scheme, ln.Addr())
	fmt.Fprintf(stdout, "  forwarding to %s; verifying with %d secret%s (never printed); remembering %d delivery IDs\n", strings.Join(names, " and "), len(secrets.list()), plural(len(secrets.list())), *dedupe)
	if scheme == "http" {
		fmt.Fprintf(stdout, "  %s\n", out.Dim("plain http: bide requires https for a receiver that is not on localhost, so terminate TLS in front of this or give -cert and -key"))
	}
	fmt.Fprintf(stdout, "  test it:  bide notify test -url %s://%s/\n\nWaiting for notifications (Ctrl-C to stop)...\n", scheme, dialAddr(ln.Addr()))
	if ready != nil {
		ready(ln.Addr())
	}
	if err := serve(ctx, ln, h, cfg, func(f string, a ...any) { fmt.Fprintf(stderr, f+"\n", a...) }); err != nil {
		return failure("relay", stderr, "%v", err)
	}
	return 0
}

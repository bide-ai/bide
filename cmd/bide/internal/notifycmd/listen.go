package notifycmd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/notify"
	"github.com/bide-ai/bide/notify/relay"
)

func runListen(ctx context.Context, args []string, stdout, stderr io.Writer, ready func(net.Addr)) int {
	fs := flag.NewFlagSet("bide notify listen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: bide notify listen [-addr <host:port>] [-secret-env <VAR> | -secret-file <file>]
                          [-cert <file> -key <file>] [-client-ca <file>] [-ca-out <file>] [-json]

Runs a local HTTPS receiver that checks each request with notify.Verify (the signature, the
timestamp and the signed delivery ID) and prints the verified event, or why it was refused. Use it
with bide notify test, or point a real notifier at it, to see a whole setup work on one machine.

Without -cert and -key it makes a self-signed certificate for localhost, 127.0.0.1, ::1 (and the
host of -addr), valid for 24 hours, and writes it to a file (-ca-out, or a new file in the temp
directory) for the sender's -ca. It prints that file and the certificate's pin (the SPKI SHA-256
for -pin), and the command that tests it. The secret is never printed.

With -client-ca the receiver requires a client certificate signed by that CA (mutual TLS) and
prints the certificate's pin for each request.

It answers 204 to a verified request and 401 to any other; it forwards nothing and keeps nothing.
Stop it with Ctrl-C. Exit status: 0 after a clean stop; 1 failure; 2 usage error.

`)
		fs.PrintDefaults()
	}
	var sec secretSource
	sec.register(fs)
	addr := fs.String("addr", "127.0.0.1:8443", "the address to listen on")
	certFile := fs.String("cert", "", "serve this certificate (PEM) instead of a generated one; needs -key")
	keyFile := fs.String("key", "", "the private key (PEM) of -cert")
	clientCA := fs.String("client-ca", "", "require a client certificate signed by a CA in this PEM file (mutual TLS)")
	caOut := fs.String("ca-out", "", "write the generated certificate here (default: a new file in the temp directory)")
	asJSON := fs.Bool("json", false, "print each verified payload as JSON, one line each, instead of a summary")
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
	out := term.For(stdout, *color)
	secrets, err := sec.load(getenv, readFile)
	if err != nil {
		return failure("listen", stderr, "%v", err)
	}

	var cert tls.Certificate
	var intro []string
	if *certFile != "" {
		if cert, err = tls.LoadX509KeyPair(*certFile, *keyFile); err != nil {
			return failure("listen", stderr, "loading the certificate: %v", err)
		}
		intro = append(intro, "certificate: "+*certFile)
	} else {
		host, _, _ := net.SplitHostPort(*addr)
		ss, err := newSelfSigned([]string{host}, timeNow())
		if err != nil {
			return failure("listen", stderr, "making a certificate: %v", err)
		}
		cert = ss.cert
		path := *caOut
		if path == "" {
			path, err = writeTemp("bide-notify-ca-*.pem", ss.pem)
		} else {
			err = os.WriteFile(path, ss.pem, 0o600)
		}
		if err != nil {
			return failure("listen", stderr, "writing the certificate: %v", err)
		}
		intro = append(intro,
			"self-signed certificate for "+strings.Join(ss.names, ", ")+", valid 24 hours",
			"CA file (-ca): "+path,
			"pin (-pin):    "+ss.pin)
		defer func() { fmt.Fprintf(stdout, "%s\n", out.Dim("the certificate file "+path+" can be deleted")) }()
	}
	cfg, err := serverTLS(cert, *clientCA)
	if err != nil {
		return failure("listen", stderr, "%v", err)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return failure("listen", stderr, "%v", err)
	}
	defer ln.Close()

	l := &listener{out: out, w: stdout, secrets: secrets.list(), asJSON: *asJSON}
	fmt.Fprintf(stdout, "%s https://%s/\n", out.Heading("bide notify listen:"), ln.Addr())
	for _, s := range intro {
		fmt.Fprintf(stdout, "  %s\n", s)
	}
	fmt.Fprintf(stdout, "  verifying with %d secret%s (never printed)", len(l.secrets), plural(len(l.secrets)))
	if *clientCA != "" {
		fmt.Fprint(stdout, "; a client certificate is required")
	}
	fmt.Fprintf(stdout, "\n\nTry it in another terminal:\n  bide notify test -url https://%s/ %s\n\nWaiting for notifications (Ctrl-C to stop)...\n", dialAddr(ln.Addr()), caHint(*certFile, intro))
	if ready != nil {
		ready(ln.Addr())
	}
	if err := serve(ctx, ln, l, cfg, func(f string, a ...any) { fmt.Fprintf(stderr, f+"\n", a...) }); err != nil {
		return failure("listen", stderr, "%v", err)
	}
	return 0
}

// dialAddr is the address a sender uses for a listener: a wildcard address becomes localhost.
func dialAddr(a net.Addr) string {
	host, port, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

// caHint is the -ca flag for the test command: the generated file, or a reminder when the
// certificate is the user's own.
func caHint(certFile string, intro []string) string {
	if certFile != "" {
		return "-ca <the CA that signed " + certFile + ">"
	}
	for _, s := range intro {
		if p, ok := strings.CutPrefix(s, "CA file (-ca): "); ok {
			return "-ca " + p
		}
	}
	return ""
}

// listener is the http.Handler of bide notify listen.
type listener struct {
	out     term.Style
	secrets []string
	asJSON  bool
	mu      sync.Mutex // serializes output lines
	w       io.Writer
}

func (l *listener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	client := ""
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		client = "; client certificate " + notify.SPKIHash(r.TLS.PeerCertificates[0])[:16] + "..."
	}
	if err := notify.Verify(r, l.secrets, 0); err != nil {
		l.line("rejected", l.out.Fail, "%s%s", relay.RejectionClass(err), client)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var p notify.Payload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		l.line("rejected", l.out.Fail, "verified, but the body is not JSON%s", client)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if l.asJSON {
		b, _ := json.Marshal(p)
		l.mu.Lock()
		fmt.Fprintf(l.w, "%s\n", b)
		l.mu.Unlock()
	} else {
		l.line("verified", l.out.OK, "%s run=%s id=%s... attempt=%d%s\n%s%s", p.Kind, p.RunID, short(p.ID), p.Attempt, client, "           ", l.out.Dim(p.Summary))
	}
	w.WriteHeader(http.StatusNoContent)
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func (l *listener) line(label string, paint func(string) string, format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "%s %s %s\n", timeNow().Format("15:04:05"), paint(fmt.Sprintf("%-8s", label)), fmt.Sprintf(format, a...))
}

// Package notifycmd is bide notify: the commands that make the webhook notifier easy to set up.
//
//	bide notify test    send one signed sample notification to a receiver and say how it went
//	bide notify listen  run a local HTTPS receiver that verifies and prints each notification
//	bide notify relay   verify bide's webhooks and forward them to Slack and PagerDuty
//
// All three use package notify (the sender, Verify) and notify/relay, so what they do is what the
// notifier and a receiver do. A signing secret is read from an environment variable or a file and
// is never taken as a flag value, printed, or logged.
package notifycmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"

	"github.com/bide-ai/bide/internal/safefile"
	"github.com/bide-ai/bide/internal/term"
)

// Verbs are the verbs of bide notify, in the order help shows them.
func Verbs() []string { return []string{"test", "listen", "relay"} }

// Environment variables the verbs read by default.
const (
	defaultSecretEnv   = "BIDE_WEBHOOK_SECRET"
	defaultPreviousEnv = "BIDE_WEBHOOK_SECRET_PREVIOUS"
)

// verbFunc runs one verb. ready, if not nil, is told the address a server verb listens on once it
// does (tests use it); a stop of ctx ends a server verb with status 0.
type verbFunc func(ctx context.Context, args []string, stdout, stderr io.Writer, ready func(net.Addr)) int

// Main runs bide notify with args (after "notify") and returns the exit status: 0 on success (for
// test, the receiver acknowledged; for listen and relay, a clean stop), 1 for a failure, 2 for a
// usage error.
func Main(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return run(ctx, args, stdout, stderr, nil)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, ready func(net.Addr)) int {
	if len(args) == 0 || isHelp(args[0]) || args[0] == "help" {
		usage(stderr)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	verbs := map[string]verbFunc{"test": runTest, "listen": runListen, "relay": runRelay}
	v, ok := verbs[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "%sbide notify: unknown verb %q\n\n", term.For(stderr, term.Auto).ErrorPrefix(), args[0])
		usage(stderr)
		return 2
	}
	return v(ctx, args[1:], stdout, stderr, ready)
}

func isHelp(a string) bool { return a == "-h" || a == "-help" || a == "--help" }

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: bide notify test   -url <receiver> [-kind <kind>] [secret and TLS flags]
       bide notify listen [-addr <host:port>] [secret flags]
       bide notify relay  [-addr <host:port>] [secret flags] [-slack-url-env ...] [-pagerduty-key-env ...]

Set up and test the webhook notifier (agent.WithNotifier with notify.NewWebhook):

  test    sends one signed sample notification to your receiver, with the same payload, signature
          and TLS options the notifier uses, and reports the TLS handshake, the HTTP status, whether
          the receiver acknowledged and how long each took
  listen  starts a local HTTPS receiver with a self-signed certificate that verifies each request
          and prints the event, so test and listen check a whole setup on one machine
  relay   a receiver that verifies bide's webhooks and forwards them to Slack (Block Kit) and
          PagerDuty (Events API v2), dropping redeliveries by delivery ID

The signing secret is read from an environment variable (default BIDE_WEBHOOK_SECRET) or a file
(-secret-file), never from a flag value, and is never printed or logged. Run bide notify <verb> -h
for a verb's flags. See docs/guides/notifications.md.
`)
}

// parse parses fs's flags and returns a status to exit with, or -1 to go on. -h exits 0.
func parse(fs *flag.FlagSet, args []string) int {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	return -1
}

// usageErr prints a usage error for verb and returns 2.
func usageErr(fs *flag.FlagSet, stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "%s%s: %s\n", term.For(stderr, term.Auto).ErrorPrefix(), fs.Name(), fmt.Sprintf(format, a...))
	fs.Usage()
	return 2
}

// failure prints an error line for verb and returns 1.
func failure(verb string, stderr io.Writer, format string, a ...any) int {
	st := term.For(stderr, term.Auto)
	fmt.Fprintf(stderr, "%s%s\n", st.ErrorPrefix(), st.Hint(fmt.Sprintf("bide notify %s: %s", verb, fmt.Sprintf(format, a...)), "; "))
	return 1
}

// secretSource says where a verb reads its signing secrets from. The flags name an environment
// variable or a file, never the secret itself, so it cannot end up in a shell history or a process
// list.
type secretSource struct {
	env, file, prevEnv, prevFile string
}

func (s *secretSource) register(fs *flag.FlagSet) {
	fs.StringVar(&s.env, "secret-env", defaultSecretEnv, "the environment variable that holds the signing secret")
	fs.StringVar(&s.file, "secret-file", "", "read the signing secret from this file, or - for standard input, or a <(...) process substitution (leading and trailing white space is dropped); used instead of -secret-env")
	fs.StringVar(&s.prevEnv, "previous-secret-env", defaultPreviousEnv, "the environment variable that holds the previous secret, during a rotation (optional)")
	fs.StringVar(&s.prevFile, "previous-secret-file", "", "read the previous secret from this file; used instead of -previous-secret-env")
}

// secrets holds what load found.
type secrets struct{ current, previous string }

// list is the secrets to verify with: the current, then the previous if there is one.
func (s secrets) list() []string {
	if s.previous == "" {
		return []string{s.current}
	}
	return []string{s.current, s.previous}
}

// load reads the secrets. An error names the variable or file, never a value.
func (s *secretSource) load(getenv func(string) string, readFile func(string) ([]byte, error)) (secrets, error) {
	cur, err := readOne(getenv, readFile, s.env, s.file, "-secret-file")
	if err != nil {
		return secrets{}, err
	}
	if cur == "" {
		src := "the environment variable " + s.env
		if s.file != "" {
			src = "the file " + s.file
		}
		return secrets{}, fmt.Errorf("no signing secret: %s is empty or unset; set it (for a new one: export %s=$(openssl rand -hex 32)) or pass -secret-file", src, defaultSecretEnv)
	}
	prev, err := readOne(getenv, readFile, s.prevEnv, s.prevFile, "-previous-secret-file")
	if err != nil {
		return secrets{}, err
	}
	return secrets{current: cur, previous: prev}, nil
}

// readOne reads a secret from the file, which flag names ("-" there is standard input), or else from
// the environment variable envName.
func readOne(getenv func(string) string, readFile func(string) ([]byte, error), envName, file, flag string) (string, error) {
	if file != "" {
		var b []byte
		var err error
		if file == "-" {
			b, err = safefile.ReadOrStdin(flag, file, safefile.MaxKey)
		} else {
			b, err = readFile(file)
		}
		if err != nil {
			err = safefile.Explain(flag, err, true)
			var pe *os.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			return "", fmt.Errorf("reading the secret file %s: %v", file, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if envName == "" {
		return "", nil
	}
	return strings.TrimSpace(getenv(envName)), nil
}

// Package approvecmd is bide approve and bide keygen: the command line an approver uses to decide
// on a gated call. bide approve reads the call from the journal, shows it, asks for confirmation,
// signs the decision with the approver's Ed25519 key and records it (agent.SubmitDecision for an
// m-of-n gate, agent.Approve for a single-approver gate). The decision is the library's: this
// package adds no way to record anything the library would not, and the gate re-verifies every
// signature itself when the run resumes.
package approvecmd

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/approveline"
	"github.com/bide-ai/bide/internal/policyfile"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/internal/safefile"
	"github.com/bide-ai/bide/internal/strictjson"
	"github.com/bide-ai/bide/internal/term"
)

// Stores opens the journals bide approve reads and writes. It is how the command plugs in store
// backends without this package importing one. readOnly is set for -list, which never writes.
type Stores struct {
	SQLite   func(path string, readOnly bool) (agent.Store, io.Closer, error)
	Postgres func(dsn string, readOnly bool) (agent.Store, io.Closer, error)
}

// now is the clock that stamps a decision's signed time; a test replaces it.
var now = time.Now

// defaultPostgresEnv is the environment variable the listed commands name for a Postgres journal
// opened with a literal -postgres DSN: the command line never carries the DSN.
const defaultPostgresEnv = "BIDE_POSTGRES_DSN"

const usageText = `bide approve: record a signed approval, or a rejection, of a call that waits for a decision

  bide approve -db <file> -run <id> -call <tool-use id> -as <approver> -key <key-file> [flags]
  bide approve -postgres-env <VAR> -run <id> -call <tool-use id> -as <approver> -key <key-file>
  bide approve -db <file> -list
  bide approve -to <url> -run <id> -call <tool-use id> -as <approver> -key <key-file> -approver-keys <file>

It reads the call from the journal, never from a flag or a notification, and shows what it is
about to sign: the run, the tool, the call's arguments (indented JSON, long ones cut; -full shows
all), the gate (k of n, who has decided, who is pending) and the SHA-256 of the arguments the
signature binds. It then asks you to type your approver id (or y) to confirm; -yes skips the
question, for scripts. The decision is signed with your private key (made by bide keygen: Ed25519, ECDSA P-256, ML-DSA-65 or hybrid),
checked against the call before it is recorded, and recorded with the time you decided.

It refuses, and records nothing, when the call has no waiting gate, the run is not waiting on it,
the gate already passed or was denied, you are not one of the gate's approvers, you already decided
the other way (it says what, and when), or the key file can be read by others (on Unix, as ssh refuses one). It
checks again after you confirm, in case the gate was decided or the run ended while you read. If
your counted decision is the one you are making again (approve after approve, reject after reject),
it signs and writes nothing, prints "already recorded: your approve at <time> counts; nothing to do"
and exits 0, so a retry after a lost response is safe, under every scheme. It
never refuses, and -list never settles a gate, on records it has not verified: without
-approver-keys the standing is labelled unverified. A call whose approval-pending note was lost (the
note is the journal's only record of the gate's policy) is refused with a precise message: the
approvers and the count shown must come from the journal, never from a flag.

If the arguments are longer than the view (40 lines, 200 characters a line), bide approve will not
sign: the signature binds all of them, and what it hid could be what matters. Rerun with -full. If
you and another terminal decide as one approver at once, both decisions are recorded, the first
valid one counts, and the other command exits 1 ("another decision of yours counts").

An m-of-n decision needs -approver-keys, a JSON object of approver id to public key (the file bide
view -approver-keys reads; bide keygen -as <id> prints an entry): the journal holds approver ids,
not keys, so this file is the only way to check that your key is the one the application enrols. It
refuses a key that is not the one enrolled for you before recording anything, and prints a verified
tally. -unverified decides without it: the decision is recorded even if the key is not the enrolled
one (then it never counts), a warning says so, and the tally printed counts the decisions recorded,
labelled unverified. A single-approver gate has no signature, so it takes neither.

A single-approver gate (agent.Approve) has no approver identity and no signature: bide approve
records the decision as agent.Approve does, takes neither -as nor -key, and says so.

-reject records a rejection: the same checks, the same signature over a denial. One command with a
flag keeps the checks in one place; a rejection is the same act with the opposite byte, and the
confirmation prompt says REJECT in capitals.

An m-of-n decision also leaves a receipt (-receipt, default next to -key; mode 0600, never
overwritten): the signed decision with the call's arguments, the journal record's name and position,
and when it was recorded; no key material. The decision is signed into the audit log at the app's
next write to the run (bide approve cannot tell whether the application audits the journal, so it
says this either way). -verify-receipt <file> -approver-keys <keys> checks a receipt on its own,
reading no journal; bide audit verify-receipts checks receipts against the journal and its signed
heads, and flags a decision deleted after signing.

-to <url> decides through an application's approval intake (package approvals) instead of writing a
journal, for an approver with no database access: the application records the signed decision through
its own journal, so its audit log signs it as it is written. The call is fetched from the endpoint and
shown, and what is signed is what is shown: the command computes the SHA-256 of the arguments it
displays and refuses an endpoint whose own digest differs, shows the digest it computed, and signs
that. The endpoint verifies the signature against the call it holds, so arguments shown that are not
the journal's produce a decision that records nothing. The same refusals apply (the call is not
waiting, you are not an approver, you already decided), and the gate is read again after you confirm.
The URL must be https (http only to a loopback address), carry no credentials, and a redirect is
not followed. -tls-ca names the CA that signs the endpoint's certificate (instead of the system
roots), -client-cert and -client-key present a client certificate for mutual TLS, and -token-env
names an environment variable that holds a bearer token to send. -to serves m-of-n gates (a
single-approver gate has no signature), and -list does not go with it. The decision's receipt is
the one the endpoint returns, checked against what you signed and against your key before it is
written. If the request fails with no answer, the decision may or may not have been recorded: run
the command again, and a decision of yours that counts is refused as already decided.

-cert <chain.pem> signs a certificate-bound decision: the approver is identified by a certificate from the
bank's CA, not by an enrolled key. The decision is signed over the bide.approval.v4 bytes (which bind the
SHA-256 of your leaf certificate) with the key of the leaf, and travels with the chain, which the gate
checks when it counts the decision. The file holds CERTIFICATE blocks only, the leaf first (at most 8
certificates); a private key block in it is refused. Before anything is signed the command checks that the
key (-key, a key file, or an HSM or KMS key for bide-hsm) is the key of the leaf, by comparing the keys, that
the certificate is valid now and has at least -cert-min-left (60 seconds) left, and, with -identity-policy
<policy file> (the certificate policy: trust anchors, pinned intermediates, the approver-to-identity map),
that the policy accepts the chain for -as; -unverified signs without the policy check and says so. No
failure falls back to a bare-key decision. A certificate-bound approver of the gate (the journal's pause
note says who) must use -cert, and a bare-key approver of such a gate must not: the other would be recorded
and never counted. -approver-keys does not apply to a certificate-bound decision. The command cannot tell
you the gate counted it: the gate needs a signed tree head over the decision and current revocation status;
see docs/guides/certificate-approvers.md.

After the decision is recorded, re-run the run's root run ID (agent.Run with the same ID) or let
RecoverLoop pick it up: the gate counts the decisions and the call runs or is denied. bide approve
never runs the call.

-list prints the calls in the journal that wait for a decision, each with the bide approve command
that decides it and, for a gate with a deadline (ApprovalPolicy.Timeout), the time left; a gate
past its deadline is marked and gets no command, since it refuses decisions. -json prints them as JSON. With -approver-keys it counts each gate as the gate does
(verified); without it the counts are labelled unverified and no gate is called settled. Run and
call ids that hold control, bidirectional or invisible characters get no command (take their ids from -json and give that call to -item). A Postgres journal is named by -postgres-env, the name
of the environment variable that holds the DSN, so the password stays out of the argument list,
the shell history and the commands -list prints; -postgres takes the DSN itself, and the listed
commands then name $` + defaultPostgresEnv + `.

A key in an HSM, on a smart card or token, or in a cloud KMS is used by bide-hsm approve, a separate
program (the PKCS#11 library needs cgo, which this one is built without): the same command, with
-key naming the key by reference, a PKCS#11 URI or a KMS key ARN, in place of a key file.
go install github.com/bide-ai/bide/cmd/bide/bide-hsm@latest builds it.

Exit status: 0 on success (for -list, even when nothing waits), 1 if the command refused or
failed, 2 for a usage error.

Flags:
`

// refusal is an error the command reports as a refusal: nothing was recorded, and the message says
// why.
type refusal struct{ msg string }

func (e *refusal) Error() string { return e.msg }

func refuse(format string, args ...any) error { return &refusal{fmt.Sprintf(format, args...)} }

// flags are bide approve's inputs, as parsed.
type flags struct {
	db, postgres, postgresEnv string
	run, call, as, key        string
	item                      string // -item: one waiting call of -list -json (a file, or - for standard input), in place of -run and -call
	approverKeys              string
	reject, yes, full         bool
	unverified                bool
	receipt, verifyReceipt    string
	identityPolicy, logKey    string
	list, asJSON              bool
	to                        string // an approval intake's base URL (package approvals): decide through it
	tlsCA                     string
	clientCert, clientKey     string
	tokenEnv                  string
	cert                      string        // -cert: the approver's certificate chain (PEM, leaf first): sign a certificate-bound decision
	certMinLeft               time.Duration // -cert-min-left: the least validity the leaf must have left
}

// Main is the bide approve command: it parses args and decides on, or lists, calls that wait for
// a decision. Confirmation is read from in. It returns the exit status: 0 on success, 1 on a
// refusal or a failure, 2 for a usage error. The approver's key is a key file (FileKeys).
func Main(args []string, in io.Reader, stdout, stderr io.Writer, stores Stores) int {
	return MainWith(args, in, stdout, stderr, stores, FileKeys())
}

// MainWith is Main with the approver's key taken from src instead of a key file: the display, the
// confirmation, the signature, the record and the receipt are the same code, whatever holds the
// key. bide-hsm uses it to sign with a key in an HSM or a KMS.
func MainWith(args []string, in io.Reader, stdout, stderr io.Writer, stores Stores, src KeySource) int {
	var f flags
	fs := flag.NewFlagSet(src.Prog, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), src.Usage)
		fs.PrintDefaults()
	}
	fs.StringVar(&f.db, "db", "", "SQLite journal file")
	fs.StringVar(&f.postgres, "postgres", "", "Postgres journal, a pgx DSN such as postgres://user:pass@host:5432/db; prefer -postgres-env, which keeps the password out of the argument list")
	fs.StringVar(&f.postgresEnv, "postgres-env", "", "name of the environment variable that holds the Postgres DSN")
	fs.StringVar(&f.run, "run", "", "the run whose journal holds the call: the pause's run ID")
	fs.StringVar(&f.call, "call", "", "the call's tool-use ID (for a plan flow node, its key, such as node:review)")
	fs.StringVar(&f.item, "item", "", "take -run and -call exactly from one call of bide approve -list -json output instead: a file, or - for standard input (which then also needs -yes, since the confirmation would read the same input). The input is one call's object, or an array of exactly one. For ids that cannot be pasted on a command line (a line break or an invisible or bidirectional character in them)")
	fs.StringVar(&f.as, "as", "", "your approver id, exactly as the gate's policy lists it (not for a single-approver gate)")
	fs.StringVar(&f.key, "key", "", src.KeyHelp)
	fs.StringVar(&f.approverKeys, "approver-keys", "", `approver public keys, a JSON object of approver id to key text, e.g. {"ops":"ed25519:ab12..."}, the file bide view -approver-keys reads (`+src.EnrolHelp+`): refuse a -key that is not the one enrolled for -as, and verify the tally. Required for an m-of-n gate unless -unverified`)
	fs.BoolVar(&f.unverified, "unverified", false, "decide an m-of-n gate without -approver-keys: the key is not checked against the one enrolled for you, and the tally is not verified")
	fs.StringVar(&f.receipt, "receipt", "", src.ReceiptHelp)
	fs.StringVar(&f.verifyReceipt, "verify-receipt", "", "verify a decision receipt on its own, against -approver-keys, instead of deciding anything: its signature over the exact call it states")
	fs.StringVar(&f.identityPolicy, "identity-policy", "", "the certificate policy file (trust anchors, pinned intermediates, the approver-to-identity map; see docs/guides/certificate-approvers.md). With -cert: your chain must be accepted for -as by it at this moment, before anything is signed. With -verify-receipt, for a certificate-bound receipt: the chain and identity are checked against it at the time the receipt's anchoring head was signed; revocation status is not checked")
	fs.StringVar(&f.cert, "cert", "", "sign a certificate-bound decision (bide.approval.v4) with your certificate chain: a PEM file, leaf first, the leaf being the certificate of -key. The key must be the leaf's key (checked before anything is signed), and with -identity-policy the chain must be accepted for -as at this moment. Never falls back to a bare-key decision")
	fs.DurationVar(&f.certMinLeft, "cert-min-left", defaultCertMinLeft, "with -cert: the least validity the leaf must have left, so the decision is provable before the certificate expires")
	fs.StringVar(&f.logKey, "log-key", "", "with -verify-receipt, for a certificate-bound receipt: the audit log's public key (ed25519:<hex>, or a file holding it), which vouches for the receipt's anchoring proof")
	fs.BoolVar(&f.reject, "reject", false, "record a rejection instead of an approval")
	fs.BoolVar(&f.yes, "yes", false, "do not ask for confirmation (for scripts); what is signed is still shown")
	fs.BoolVar(&f.full, "full", false, "show all of the call's arguments, not the first lines")
	fs.StringVar(&f.to, "to", "", "decide through an application's approval intake (package approvals) at this base URL (https, or http to loopback) instead of writing a journal: the call is fetched from the endpoint, shown, signed and posted")
	fs.StringVar(&f.tlsCA, "tls-ca", "", "with -to: the PEM CA certificate(s) that sign the endpoint's certificate, instead of the system roots")
	fs.StringVar(&f.clientCert, "client-cert", "", "with -to: a PEM client certificate for mutual TLS (with -client-key)")
	fs.StringVar(&f.clientKey, "client-key", "", "with -to: the PEM private key of -client-cert")
	fs.StringVar(&f.tokenEnv, "token-env", "", "with -to: name of the environment variable that holds a bearer token to send (the token never goes on the command line)")
	fs.BoolVar(&f.list, "list", false, "list the calls that wait for a decision, each with its "+src.Prog+" command; nothing is recorded")
	fs.BoolVar(&f.asJSON, "json", false, "with -list: print the waiting calls as JSON")
	if src.Flags != nil {
		src.Flags(fs)
	}
	color := term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	st, es := term.For(stdout, *color), term.For(stderr, *color)
	usage := func(msg string) int {
		fmt.Fprintln(stderr, es.ErrorPrefix()+src.Prog+":", msg)
		fs.Usage()
		return 2
	}
	if fs.NArg() > 0 {
		if src.NoEchoArgs {
			return usage("unexpected argument (the command takes flags only; the key goes after -key)")
		}
		return usage(fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	}
	if msg := f.checkStdin(); msg != "" {
		return usage(msg)
	}
	if f.item != "" {
		if msg := f.checkItem(); msg != "" {
			return usage(msg)
		}
		if err := f.readItem(in); err != nil {
			fmt.Fprintln(stderr, es.ErrorPrefix()+src.Prog+":", err)
			return 1
		}
	}
	if msg := f.check(); msg != "" {
		return usage(msg)
	}
	// No signal handling: Ctrl-C at the prompt ends the command, and a decision is one journal write,
	// so an interrupt leaves it recorded or not recorded.
	ctx := context.Background()
	c := &cmd{f: f, in: bufio.NewReader(in), out: stdout, st: st, stores: stores, src: src}
	var err error
	switch {
	case f.verifyReceipt != "":
		err = c.verifyReceiptFile()
	case f.to != "":
		err = c.approveRemote(ctx)
	case f.list:
		err = c.list(ctx)
	default:
		err = c.approve(ctx)
	}
	if err != nil {
		fmt.Fprintln(stderr, es.ErrorPrefix()+src.Prog+":", es.Hint(err.Error(), "; "))
		return 1
	}
	return 0
}

// maxItem bounds the input of -item: one call of -list -json is far smaller.
const maxItem = 1 << 20

// checkItem reports a usage error in how -item is combined with other flags, or "". It runs before
// -item is read, so that an ambiguous command line reads nothing.
func (f flags) checkItem() string {
	switch {
	case f.run != "" || f.call != "":
		return "-item gives -run and -call (exactly as -list -json printed them): do not also pass -run or -call"
	case f.list || f.asJSON:
		return "-item does not go with -list or -json"
	case f.verifyReceipt != "":
		return "-item does not go with -verify-receipt, which reads no journal"
	case f.item == "-" && !f.yes:
		return "-item - reads standard input, which the confirmation would read too: give -yes (what is signed is still shown), or put the item in a file"
	}
	return ""
}

// checkStdin reports a usage error in how flags that take "-" (standard input) are combined, or "".
// Only one can read it, and the confirmation would read it too, so -yes is needed.
func (f flags) checkStdin() string {
	n := 0
	for _, v := range []string{f.item, f.approverKeys, f.logKey} {
		if v == "-" {
			n++
		}
	}
	switch {
	case n > 1:
		return "only one of -item, -approver-keys and -log-key can be - (standard input)"
	case n == 1 && !f.yes:
		return "- reads standard input, which the confirmation would read too: give -yes (what is signed is still shown), or put the file on disk"
	}
	return ""
}

// readItem sets run and call from the one call of -list -json output that -item names: a file, or
// standard input for "-". The input is the call's object or an array of exactly one object, decoded
// strictly (no repeated name, no invalid UTF-8) so what the text shows is what is used; the ids are
// taken as the JSON decodes them, so a line break or an invisible character in one is kept.
func (f *flags) readItem(stdin io.Reader) error {
	var data []byte
	var err error
	if f.item == "-" {
		data, err = io.ReadAll(io.LimitReader(stdin, maxItem+1))
	} else {
		data, err = safefile.Read(f.item, maxItem, nil)
		var refused *safefile.RefusedError
		if errors.As(err, &refused) && refused.Kind == safefile.TooLarge {
			return fmt.Errorf("-item %s: more than %d bytes (the limit for -item): it is one call of -list -json, not the whole list", f.item, maxItem)
		}
		if errors.As(err, &refused) {
			err = safefile.Explain("-item", err, true)
		} else if err != nil {
			err = unwrapPath(err)
		}
	}
	if err != nil {
		return fmt.Errorf("-item %s: %w", f.item, err)
	}
	if len(data) > maxItem {
		return fmt.Errorf("-item %s: more than %d bytes: it is one call of -list -json, not the whole list", f.item, maxItem)
	}
	type call struct {
		Run  string `json:"run"`
		Call string `json:"call"`
	}
	opts := &strictjson.Options{AllowUnknown: true}
	var one call
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '[' {
		var many []call
		if err := strictjson.Unmarshal(data, &many, opts); err != nil {
			return fmt.Errorf("-item %s: %w", f.item, err)
		}
		if len(many) != 1 {
			return fmt.Errorf("-item %s: %d calls; name exactly one (for example with jq '.[0]')", f.item, len(many))
		}
		one = many[0]
	} else if err := strictjson.Unmarshal(data, &one, opts); err != nil {
		return fmt.Errorf("-item %s: %w", f.item, err)
	}
	if one.Run == "" || one.Call == "" {
		return fmt.Errorf("-item %s: the call has no \"run\" or no \"call\" (it is one call of bide approve -list -json)", f.item)
	}
	f.run, f.call = one.Run, one.Call
	return nil
}

// check reports a usage error in the flags, or "".
func (f flags) check() string {
	if f.postgresEnv != "" && !approveline.ValidEnvName(f.postgresEnv) {
		return approveline.NotAnEnvName
	}
	if f.verifyReceipt != "" {
		if f.approverKeys == "" && f.identityPolicy == "" {
			return "-verify-receipt needs -approver-keys, the file of enrolled approver public keys (or -identity-policy and -log-key for a certificate-bound receipt)"
		}
		if (f.identityPolicy == "") != (f.logKey == "") {
			return "-identity-policy and -log-key go together: a certificate-bound receipt is checked against the policy at the time the log's anchoring head was signed"
		}
		for name, set := range map[string]bool{"-db": f.db != "", "-postgres": f.postgres != "", "-postgres-env": f.postgresEnv != "", "-run": f.run != "", "-call": f.call != "", "-item": f.item != "", "-as": f.as != "", "-key": f.key != "", "-reject": f.reject, "-yes": f.yes, "-full": f.full, "-list": f.list, "-json": f.asJSON, "-unverified": f.unverified, "-receipt": f.receipt != "", "-to": f.to != "", "-cert": f.cert != ""} {
			if set {
				return name + " does not go with -verify-receipt, which reads no journal"
			}
		}
		return ""
	}
	if f.logKey != "" {
		return "-log-key goes with -verify-receipt only"
	}
	if f.identityPolicy != "" && f.cert == "" {
		return "-identity-policy goes with -cert (to check your chain before signing) or with -verify-receipt"
	}
	if f.cert != "" {
		switch {
		case f.list:
			return "-cert does not go with -list"
		case f.as == "" || f.key == "":
			return "-cert signs a certificate-bound decision: give -as (your approver id) and -key (the key of the certificate's leaf)"
		case f.identityPolicy == "" && !f.unverified:
			return "-cert needs -identity-policy, the certificate policy file the chain is checked against before signing, or -unverified to sign without that check"
		case f.unverified && f.identityPolicy != "":
			return "-unverified and -identity-policy are opposites: give the policy to check the chain, or -unverified to skip the check"
		case f.certMinLeft < 0:
			return "-cert-min-left is negative"
		}
	}
	if f.receipt != "" && f.list {
		return "-receipt does not go with -list"
	}
	if f.to != "" {
		for name, set := range map[string]bool{"-db": f.db != "", "-postgres": f.postgres != "", "-postgres-env": f.postgresEnv != "", "-list": f.list, "-json": f.asJSON} {
			if set {
				return name + " does not go with -to, which decides through an endpoint and opens no journal"
			}
		}
		switch {
		case f.run == "":
			return "no -run given"
		case f.call == "":
			return "no -call given"
		case f.as == "" || f.key == "":
			return "-to takes a signed decision: give -as and -key (the intake serves m-of-n gates only)"
		case f.unverified && f.approverKeys != "":
			return "-unverified and -approver-keys are opposites: give the keys to check the decision, or -unverified to skip the check"
		case f.cert != "" && f.approverKeys != "":
			return "-approver-keys enrols bare keys: with -cert your key is checked against your certificate and -identity-policy instead"
		case f.approverKeys == "" && !f.unverified && f.cert == "":
			return "an m-of-n decision needs -approver-keys, or -unverified to decide without checking your key against the enrolled one"
		case (f.clientCert == "") != (f.clientKey == ""):
			return "-client-cert and -client-key go together"
		}
		return ""
	}
	for name, set := range map[string]bool{"-tls-ca": f.tlsCA != "", "-client-cert": f.clientCert != "", "-client-key": f.clientKey != "", "-token-env": f.tokenEnv != ""} {
		if set {
			return name + " goes with -to"
		}
	}
	n := 0
	for _, s := range []string{f.db, f.postgres, f.postgresEnv} {
		if s != "" {
			n++
		}
	}
	if n != 1 {
		return "give exactly one of -db, -postgres and -postgres-env"
	}
	if f.list {
		for name, set := range map[string]bool{"-run": f.run != "", "-call": f.call != "", "-as": f.as != "", "-key": f.key != "", "-reject": f.reject, "-yes": f.yes, "-full": f.full, "-unverified": f.unverified, "-receipt": f.receipt != ""} {
			if set {
				return name + " does not go with -list"
			}
		}
		return ""
	}
	if f.asJSON {
		return "-json goes with -list"
	}
	switch {
	case f.run == "":
		return "no -run given"
	case f.call == "":
		return "no -call given (-list shows the calls that wait)"
	}
	if f.unverified && f.approverKeys != "" {
		return "-unverified and -approver-keys are opposites: give the keys to check the decision, or -unverified to skip the check"
	}
	if (f.as == "") != (f.key == "") {
		return "-as and -key go together"
	}
	return ""
}

// cmd is one bide approve invocation.
type cmd struct {
	f      flags
	in     *bufio.Reader
	out    io.Writer
	st     term.Style
	stores Stores
	src    KeySource
	cert   *certDecision // the certificate-bound decision being made (-cert), nil for a bare-key one
}

// open opens the journal the flags name. A SQLite file must already exist: a mistyped path must not
// create an empty journal and report that nothing waits in it.
func (c *cmd) open(readOnly bool) (*agent.Journal, io.Closer, approveline.Journal, error) {
	var (
		s     agent.Store
		close io.Closer
		err   error
		tgt   approveline.Journal
	)
	switch {
	case c.f.db != "":
		if c.stores.SQLite == nil {
			return nil, nil, tgt, errors.New("this build cannot open a SQLite journal")
		}
		if _, serr := os.Stat(c.f.db); serr != nil {
			return nil, nil, tgt, fmt.Errorf("journal %s: %w", c.f.db, unwrapPath(serr))
		}
		tgt = approveline.Journal{SQLite: c.f.db}
		s, close, err = c.stores.SQLite(c.f.db, readOnly)
	default:
		dsn, env := c.f.postgres, cmp.Or(c.f.postgresEnv, defaultPostgresEnv)
		if c.f.postgresEnv != "" {
			if dsn = os.Getenv(c.f.postgresEnv); dsn == "" {
				return nil, nil, tgt, fmt.Errorf("-postgres-env %s: the variable is empty or not set", c.f.postgresEnv)
			}
		}
		if c.stores.Postgres == nil {
			return nil, nil, tgt, errors.New("this build cannot open a Postgres journal")
		}
		tgt = approveline.Journal{PostgresEnv: env}
		s, close, err = c.stores.Postgres(dsn, readOnly)
		if err != nil {
			err = errors.New("cannot open the Postgres journal (the error is not shown, as it can hold the DSN)")
		}
	}
	if err != nil {
		return nil, nil, tgt, fmt.Errorf("open journal: %w", err)
	}
	j, err := agent.NewJournal(s)
	if err != nil {
		close.Close()
		return nil, nil, tgt, err
	}
	return j, close, tgt, nil
}

// gateOf is the waiting gate on call in run's records, and the call it gates, from the same
// records: what is shown and what is signed are read once, here.
type gateOf struct {
	recs []agent.Record
	view *runview.View
	gate runview.Gate
	call agent.ToolUse
}

// load reads run's journal and finds the gate on call. It refuses a run with no records and a
// call with no gate.
func load(ctx context.Context, j *agent.Journal, run, call string) (*gateOf, error) {
	recs, err := j.History(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("read run %s: %w", safe(run), err)
	}
	if len(recs) == 0 {
		return nil, refuse("run %s is not in this journal", safe(run))
	}
	v := runview.Build(ctx, run, recs, nil)
	_, tu, found := agent.FindToolCall(recs, call)
	i := slices.IndexFunc(v.Gates, func(g runview.Gate) bool { return g.ToolUseID == call })
	if found && i < 0 {
		for _, r := range recs {
			if (r.Kind == agent.StepToolResult || r.Kind == agent.StepSagaFail) && r.ToolUseID == call {
				return nil, refuse("call %s of run %s already has a result and no gate record: it is not gated, or it already ran", safe(call), safe(run))
			}
		}
		// A call with no result and no gate record is either not gated or gated with its
		// approval-pending note lost (the note is informational, written once when the run first
		// pauses; a lost write is never made up). Without the note the policy is not in the journal,
		// and this command will not take one from a flag: the approvers and the count it shows must
		// come from the journal, or an operator's typo would decide who may sign.
		return nil, refuse("call %s of run %s has no result and no approval-pending record: it is not gated, or its note was lost and the journal does not hold the gate's policy, so this command cannot check the approver or the count and will not take them from a flag; decide from the application with agent.SubmitDecision (or agent.Approve for a single-approver gate)", safe(call), safe(run))
	}
	if !found || i < 0 {
		var ids []string
		for _, g := range v.Gates {
			ids = append(ids, safe(g.ToolUseID))
		}
		if len(ids) == 0 {
			return nil, refuse("run %s has no gated call %s: it has no approval gate at all", safe(run), safe(call))
		}
		return nil, refuse("run %s has no approval gate on call %s; its gated calls: %s", safe(run), safe(call), strings.Join(ids, ", "))
	}
	return &gateOf{recs: recs, view: v, gate: v.Gates[i], call: tu}, nil
}

// policy is the gate's policy as the journal records it, separation of duties included: the
// policy every count here runs under, so a requester's decision is never counted (and the run's
// run:principal, which names the requesters, is in the records counted).
func (g *gateOf) policy() agent.ApprovalPolicy { return gatePolicy(g.gate) }

// gatePolicy is the m-of-n policy the gate view g records.
func gatePolicy(g runview.Gate) agent.ApprovalPolicy {
	return agent.ApprovalPolicy{Need: g.Need, Approvers: g.Approvers, ExcludePrincipal: g.ExcludePrincipal}
}

// requester reports whether as is a requester of the run the gate g excludes (separation of
// duties): equal to one of its requesters, ignoring ASCII case, as the gate compares them.
func requester(g runview.Gate, as string) bool {
	return g.ExcludePrincipal && (slices.Contains(g.Excluded, as) || slices.ContainsFunc(g.Requesters, func(r string) bool { return agent.SameApprover(as, r) }))
}

// subject is what the approver signs a decision on, from the records read.
func (g *gateOf) subject(run string) agent.ApprovalSubject {
	return agent.ApprovalSubject{RunID: run, ToolUseID: g.call.ID, ToolName: g.call.Name, Args: g.call.Args}
}

// verdict is what a recorded decision of an approver says, for a message.
func verdict(approved bool) string {
	if approved {
		return "approved"
	}
	return "denied"
}

// recorded is what preflight returns when the approver's counted decision is the one they are
// making again (an approve after an approve, a reject after a reject): nothing to sign or record.
type recorded struct {
	approved            bool
	decidedAt, recordAt string
}

func (e *recorded) Error() string {
	return fmt.Sprintf("already recorded: your %s at %s counts; nothing to do", verdictVerb(e.approved), safe(cmp.Or(e.decidedAt, "an unknown time")))
}

// verdictVerb is the decision as the approver names it.
func verdictVerb(approved bool) string {
	if approved {
		return "approve"
	}
	return "reject"
}

// preflight refuses a decision that cannot count or cannot be made: the run ended, the gate is
// decided, the call is not waiting, the approver is not one of the gate's, or already decided. It
// reads the journal only. verifierFor, when non-nil, tells a valid earlier decision of the approver
// from one that does not verify.
func (g *gateOf) preflight(run, as string, reject bool, verifierFor agent.ApproverVerifierFor) error {
	gt := g.gate
	// An approver whose counted decision is the one they are making again has nothing to do, even
	// if the gate has since passed or the run has ended: a retry after a lost response succeeds.
	if verifierFor != nil && as != "" && slices.Contains(gt.Approvers, as) {
		_, checks := agent.TallyApprovals(g.recs, g.subject(run), g.policy(), verifierFor)
		for _, ck := range checks {
			if ck.Approver != as || !ck.Counted || ck.Approved == reject {
				continue
			}
			d := runview.Decision{}
			for _, x := range gt.Decisions {
				if x.Approver == as && x.Approved == ck.Approved {
					d = x
					break
				}
			}
			return &recorded{approved: ck.Approved, decidedAt: d.DecidedAt, recordAt: d.RecordedAt}
		}
	}
	switch g.view.State {
	case string(agent.RunCompleted), string(agent.RunAborted), string(agent.RunCancelled), string(agent.RunEnded):
		return refuse("run %s has ended (%s); it does not wait on call %s", safe(run), g.view.StateText, safe(gt.ToolUseID))
	}
	if dl, at, ok := agent.RecordedDeadline(g.recs, gt.ToolUseID); ok && !now().Before(at) {
		return refuse("the gate on call %s expired at %s (its deadline): it takes no more decisions, and the call will not run", safe(gt.ToolUseID), safe(dl.ExpiresAt))
	}
	if gt.Tallied {
		out := "passed"
		if len(gt.ApprovedBy) < gt.Need {
			out = "was denied"
		}
		return refuse("the gate on call %s already decided: it %s (%s)", safe(gt.ToolUseID), out, safe(gt.Status))
	}
	if i := slices.IndexFunc(gt.Decisions, func(d runview.Decision) bool { return !d.Signed }); gt.Single && i >= 0 {
		d := gt.Decisions[i]
		return refuse("the single-approver gate on call %s already has its decision: %s (recorded %s)", safe(gt.ToolUseID), verdict(d.Approved), cmp.Or(d.RecordedAt, "at an unknown time"))
	}
	if !gt.Waiting {
		return refuse("run %s is not waiting on call %s: the call has a result, or the run never paused at its gate (no approval-pending record)", safe(run), safe(gt.ToolUseID))
	}
	if gt.Single {
		return nil
	}
	if len(gt.Approvers) == 0 {
		return refuse("the journal does not record the policy of the gate on call %s, so approvers cannot be checked", safe(gt.ToolUseID))
	}
	if !slices.Contains(gt.Approvers, as) {
		return refuse("%q is not an approver of the gate on call %s; its approvers are: %s", safe(as), safe(gt.ToolUseID), safe(strings.Join(gt.Approvers, ", ")))
	}
	if requester(gt, as) {
		return refuse("%s is a requester of run %s (the gate excludes the requester: separation of duties); your decision would not count", safe(as), safe(run))
	}
	// Refusing on the standing of the gate or on an earlier decision of the approver needs the
	// decisions verified: a record that does not verify holds nobody's place, and one that nothing
	// has checked must never lock an approver out or settle a gate (a junk record is one
	// SubmitDecision call away for anyone who can write the journal). Without the approvers' keys
	// bide approve refuses only on what is certain; SubmitDecision's own check, under the
	// approver's key, still refuses a second valid decision of the same key.
	if verifierFor == nil {
		return nil
	}
	t, checks := agent.TallyApprovals(g.recs, g.subject(run), g.policy(), verifierFor)
	switch {
	case t.Passed():
		return refuse("the gate on call %s already has its %d approval(s) (verified); nothing more is needed: re-run run %s", safe(gt.ToolUseID), gt.Need, safe(run))
	case t.Unreachable():
		return refuse("the gate on call %s can no longer pass (%d denied of %d approvers, %d needed): re-run run %s and the call is denied", safe(gt.ToolUseID), t.Denied, len(gt.Approvers), gt.Need, safe(run))
	}
	for _, ck := range checks {
		if ck.Approver != as || !ck.Counted {
			continue
		}
		d := runview.Decision{}
		for _, x := range gt.Decisions {
			if x.Approver == as && x.Approved == ck.Approved {
				d = x
				break
			}
		}
		return refuse("%s already decided on call %s: %s at %s (recorded %s)", safe(as), safe(gt.ToolUseID), verdict(ck.Approved), safe(cmp.Or(d.DecidedAt, "an unknown time")), safe(cmp.Or(d.RecordedAt, "an unknown time")))
	}
	return nil
}

// approverKeys reads -approver-keys: id -> verifier. It fails closed: an entry that is not a key
// is an error, not a skipped approver.
func approverKeys(path string) (map[string]audit.Verifier, error) {
	b, err := safefile.ReadOrStdin("-approver-keys", path, 1<<20)
	if err != nil {
		var refused *safefile.RefusedError
		if errors.As(err, &refused) && refused.Kind == safefile.TooLarge {
			return nil, fmt.Errorf("-approver-keys %s is larger than 1 MiB (the limit for -approver-keys)", path)
		}
		return nil, fmt.Errorf("-approver-keys: %w", safefile.Explain("-approver-keys", err, true))
	}
	var raw map[string]string
	if err := audit.UnmarshalStrict(b, &raw); err != nil {
		return nil, fmt.Errorf("-approver-keys %s: %w", path, err)
	}
	keys := make(map[string]audit.Verifier, len(raw))
	for id, text := range raw {
		v, err := audit.ParsePublicKey(text)
		if err != nil {
			return nil, fmt.Errorf("-approver-keys %s: the key of %q: %w", path, safe(id), err)
		}
		keys[id] = v
	}
	return keys, nil
}

// verifiers resolves approver ids to the keys read.
func verifiers(keys map[string]audit.Verifier) agent.ApproverVerifierFor {
	return func(id string) (agent.ApproverVerifier, bool) {
		v, ok := keys[id]
		return v, ok
	}
}

// approve is the whole of bide approve for one call.
func (c *cmd) approve(ctx context.Context) error {
	f := c.f
	j, closer, _, err := c.open(false)
	if err != nil {
		return err
	}
	defer closer.Close()
	g, err := load(ctx, j, f.run, f.call)
	if err != nil {
		return err
	}
	var receiptErr error
	single := g.gate.Single
	if single && (f.as != "" || f.key != "") {
		return refuse("the gate on call %s is a single-approver gate (agent.Approve): its decision carries no approver id and no signature, so -as and -key do not apply; drop them", safe(f.call))
	}
	if !single && (f.as == "" || f.key == "") {
		return refuse("the gate on call %s is a %s gate: give -as (your approver id) and -key (%s)", safe(f.call), gateShape(g.gate), c.src.KeyNoun)
	}
	if single && f.unverified {
		return refuse("the gate on call %s is a single-approver gate: its decision is unsigned, so there is no key to verify and -unverified does not apply; drop it", safe(f.call))
	}
	if single && f.cert != "" {
		return refuse("the gate on call %s is a single-approver gate: its decision is unsigned and carries no certificate, so -cert does not apply", safe(f.call))
	}
	if !single && f.approverKeys == "" && !f.unverified && f.cert == "" {
		return refuse("an m-of-n decision needs -approver-keys: the file of enrolled approver public keys, a JSON object of approver id to key text (the file bide view -approver-keys reads; %s %s prints this approver's entry). Without it nothing checks that your key is the one the application enrols, and a decision signed with another key is recorded and never counts. To decide anyway, pass -unverified", c.src.EnrolCmd, cmp.Or(safe(f.as), "<id>"))
	}
	var keys map[string]audit.Verifier
	var verifierFor agent.ApproverVerifierFor
	if f.approverKeys != "" {
		if keys, err = approverKeys(f.approverKeys); err != nil {
			return err
		}
		verifierFor = verifiers(keys)
	}
	if err := g.preflight(f.run, f.as, f.reject, verifierFor); err != nil {
		return c.alreadyRecorded(err)
	}
	var signer audit.Signer
	if !single {
		if f.cert != "" {
			// The certificate is read and checked before the key is opened: a bad file costs no HSM login.
			if err := g.refuseCertForBare(f.as); err != nil {
				return err
			}
			if c.cert, err = c.loadCert(); err != nil {
				return err
			}
		}
		if signer, err = c.src.Load(f.key); err != nil {
			// The source's error may hold text from a vendor library: show it escaped.
			return errors.New(safe(err.Error()))
		}
		if cl, ok := signer.(io.Closer); ok {
			defer cl.Close()
		}
		if f.cert == "" {
			if err := checkEnrolled(keys, f.as, signer); err != nil {
				return err
			}
			if err := g.refuseBoundWithoutCert(f.as); err != nil {
				return err
			}
		} else if err := c.cert.chain.checkKeyIsLeaf(signer); err != nil {
			return err
		}
	}

	receiptPath := ""
	if !single {
		if receiptPath, err = c.receiptTarget(); err != nil {
			return err
		}
	}
	s := g.subject(f.run)
	shown := argsDigest(s)
	if clipped := c.show(g, s, signer); clipped {
		return refuse("the call's arguments are longer than the view, and the signature binds all of them: this command will not sign what you have not seen. Rerun with -full to see them all (the digest above is the one that would be signed)")
	}
	if !c.confirm(single) {
		return refuse("not confirmed; nothing was recorded")
	}
	// The gate may have been decided, or the run ended, while the approver sat at the prompt: read
	// the journal again, check again, and sign only what was shown.
	if g, err = load(ctx, j, f.run, f.call); err != nil {
		return fmt.Errorf("the journal changed while you were deciding; nothing was recorded: %w", err)
	}
	if err := g.preflight(f.run, f.as, f.reject, verifierFor); err != nil {
		if rec, ok := errors.AsType[*recorded](err); ok {
			return c.alreadyRecorded(rec)
		}
		return fmt.Errorf("the journal changed while you were deciding; nothing was recorded: %w", err)
	}
	if s = g.subject(f.run); argsDigest(s) != shown {
		return errors.New("the call's arguments changed while you were deciding; nothing was signed")
	}

	if single {
		if err := agent.Approve(ctx, j, f.run, f.call, !f.reject); err != nil {
			return fmt.Errorf("record the decision: %w", err)
		}
		fmt.Fprintf(c.out, "\n%s%s the call, unsigned (a single-approver gate records no signature)\n", c.st.Pass(), c.verb(!f.reject))
	} else {
		at := now()
		var d agent.Decision
		var msg []byte
		if c.cert != nil {
			// The chain is checked again at the time that will be signed, and the key against the leaf once
			// more: the approver sat at a prompt, and no failure falls back to a bare-key decision.
			if err := c.cert.chain.checkKeyIsLeaf(signer); err != nil {
				return err
			}
			if err := c.cert.check(f.as, at, f.certMinLeft); err != nil {
				return err
			}
			if d, msg, err = c.cert.sign(signer, s, f.run, f.call, f.as, !f.reject, at); err != nil {
				return err
			}
			if err := c.cert.verifySigned(d, msg); err != nil {
				return err
			}
		} else {
			msg = agent.ApprovalDecisionBytes(s, f.as, !f.reject, at)
			sig, err := signer.Sign(msg)
			if err != nil {
				return fmt.Errorf("sign the decision (nothing was recorded): %w", err)
			}
			d = agent.Decision{RunID: f.run, ToolUseID: f.call, ApproverID: f.as, Approved: !f.reject, DecidedAt: at, Alg: signer.Alg(), Signature: sig}
		}
		if !digestAt(msg, s, argsDigest(s)) {
			return errors.New("internal error: the decision bytes do not carry the shown arguments digest; nothing was recorded")
		}
		// The check verifies the signature against the call as the journal holds it, with the
		// public key derived from the private key, so a decision that does not verify is never
		// recorded; the gate re-verifies every record itself against the application's keys.
		own, err := audit.NewVerifier(signer.Alg(), signer.PublicKey())
		if err != nil {
			return fmt.Errorf("your key is not a usable verification key (nothing was recorded): %w", err)
		}
		ownFor := func(id string) (agent.ApproverVerifier, bool) { return own, id == f.as }
		check := ownFor
		if c.cert != nil {
			// The same write-time check the approvals intake makes, with what this command can judge: the
			// chain against the policy, the signature under the leaf's key.
			li := localIntake{cd: c.cert, as: f.as}
			check = func(id string) (agent.ApproverVerifier, bool) { return li, id == f.as }
		}
		if err := agent.SubmitDecision(ctx, j, d, agent.WithDecisionCheck(check)); err != nil {
			return fmt.Errorf("the decision was not recorded: %w", err)
		}
		fmt.Fprintf(c.out, "\n%s%s call %s as %s, signed and recorded (decided %s)\n", c.st.Pass(), c.verb(!f.reject), safe(f.call), c.st.Bold(f.as), agent.FormatTimestamp(at))
		mine, rerr := c.writeReceipt(ctx, j, s, d, receiptPath)
		receiptErr = rerr
		if err := c.after(ctx, j, verifierFor, ownFor, mine); err != nil {
			return err
		}
		return receiptErr
	}
	if err := c.after(ctx, j, verifierFor, nil, ""); err != nil {
		return err
	}
	return receiptErr
}

// alreadyRecorded turns preflight's *recorded into a success: it says the decision counts, signs
// and writes nothing, and points at the receipt written when it was signed (by default next to the
// key). Any other error is returned as it is.
func (c *cmd) alreadyRecorded(err error) error {
	rec, ok := errors.AsType[*recorded](err)
	if !ok {
		return err
	}
	fmt.Fprintf(c.out, "%s%s\n", c.st.Pass(), rec.Error())
	if rec.recordAt != "" {
		fmt.Fprintf(c.out, "recorded %s; nothing was signed or written\n", safe(rec.recordAt))
	}
	if c.f.key != "" {
		p := filepath.Join(c.src.receiptDir(c.f.key), defaultReceiptName(c.f.run, c.f.call, c.f.as, rec.decidedAt))
		if _, serr := os.Stat(p); serr == nil {
			fmt.Fprintf(c.out, "receipt %s\n", c.st.Path(p))
		} else {
			fmt.Fprintln(c.out, "its receipt was written when you signed it (by default "+c.src.ReceiptWhere+", or where -receipt named it)")
		}
	}
	return nil
}

// verb is the past tense of the decision, colored.
func (c *cmd) verb(approved bool) string {
	if approved {
		return c.st.OK("Approved")
	}
	return c.st.Fail("Rejected")
}

func gateShape(g runview.Gate) string {
	if g.Single {
		return "single-approver"
	}
	return fmt.Sprintf("%d-of-%d", g.Need, len(g.Approvers))
}

// checkEnrolled refuses a private key whose public key is not the one -approver-keys enrols for as.
func checkEnrolled(keys map[string]audit.Verifier, as string, sg audit.Signer) error {
	if keys == nil {
		return nil
	}
	v, ok := keys[as]
	if !ok {
		return refuse("-approver-keys has no key for %q", safe(as))
	}
	if sg.Alg() != v.Alg() || !bytes.Equal(sg.PublicKey(), v.PublicKey()) {
		return refuse("your -key (%s %s) is not the key enrolled for %q in -approver-keys (%s %s); a decision signed with it would be recorded and never count", sg.Alg(), keyName(sg), safe(as), v.Alg(), audit.KeyID(string(v.Alg()), v.PublicKey()))
	}
	return nil
}

// show prints what is about to be signed, all of it read from the journal.
func (c *cmd) show(g *gateOf, s agent.ApprovalSubject, sg audit.Signer) (clipped bool) {
	st, gt := c.st, g.gate
	action := st.Bold("Approve this call?")
	if c.f.reject {
		action = st.BoldFail("REJECT this call?")
	}
	fmt.Fprintf(c.out, "%s %s\n\n", action, st.Dim("(read from the journal)"))
	row := func(k, v string) { fmt.Fprintf(c.out, "  %-10s %s\n", k, v) }
	row("run", safe(s.RunID))
	row("call", safe(s.ToolUseID))
	row("tool", st.Bold(safe(s.ToolName)))
	if gt.Single {
		row("gate", "one approver, one decision; the decision is recorded without a signature")
	} else {
		row("gate", fmt.Sprintf("%d of %d must approve: %s", gt.Need, len(gt.Approvers), safe(strings.Join(gt.Approvers, ", "))))
		for _, d := range gt.Decisions {
			if d.Approver == "" || d.DecidedAt == "" {
				continue
			}
			label := ""
			if c.f.approverKeys == "" {
				label = " (recorded, unverified)"
			}
			if d.Note != "" {
				label += " (" + d.Note + ")"
			}
			row("decided", fmt.Sprintf("%s %s at %s%s", safe(d.Approver), verdict(d.Approved), safe(d.DecidedAt), label))
		}
		row("pending", safe(strings.Join(gt.Pending, ", ")))
		row("you", fmt.Sprintf("%s, with %s key %s", st.Bold(c.f.as), sg.Alg(), keyName(sg)))
		if w, ok := sg.(interface{ Where() string }); ok && w.Where() != "" {
			row("held in", safe(w.Where()))
		}
		if n, ok := sg.(interface{ Notice() string }); ok && n.Notice() != "" {
			row("", st.Warn(safe(n.Notice())))
		}
		switch {
		case c.cert != nil:
			c.cert.show(row, st, c.f.as, now())
		case c.f.approverKeys == "":
			row("", st.BoldFail("UNVERIFIED")+" "+st.Warn("this key is not checked against the key enrolled for you (-unverified, no -approver-keys): if the application enrols another, the decision is recorded and never counts"))
		}
	}
	if gt.AskedAt != "" {
		row("asked", safe(gt.AskedAt))
	}
	text := prettyArgs(g.call.Args)
	hidden := 0
	if !c.f.full {
		text, hidden = clip(text)
	}
	fmt.Fprintf(c.out, "\n  arguments\n")
	for _, l := range strings.Split(text, "\n") {
		fmt.Fprintf(c.out, "    %s\n", l)
	}
	if hidden > 0 {
		fmt.Fprintf(c.out, "    %s\n", st.Warn(fmt.Sprintf("[%d more bytes not shown; the signature binds all of them; -full shows everything]", hidden)))
	}
	if gt.Single {
		fmt.Fprintln(c.out)
		return hidden > 0
	}
	fmt.Fprintf(c.out, "\n  %s %s\n", "args SHA-256", st.Bold(argsDigest(s)))
	bound := "your signature binds this digest with the run, call, tool and your id, your decision and its time, and nothing else"
	if c.cert != nil {
		bound = "your signature binds this digest with the run, call, tool and your id, your decision and its time, and the hash of your certificate, and nothing else"
	}
	fmt.Fprintf(c.out, "  %s\n\n", st.Dim(bound))
	return hidden > 0
}

// confirm asks the approver to confirm, unless -yes. For an m-of-n gate the approver types their
// id (or y); a single-approver gate has no id, so y. Anything else, and end of input, declines.
func (c *cmd) confirm(single bool) bool {
	if c.f.yes {
		return true
	}
	what := "approve"
	if c.f.reject {
		what = "REJECT"
	}
	if single {
		fmt.Fprintf(c.out, "Type y to %s: ", what)
	} else {
		fmt.Fprintf(c.out, "Type %s (or y) to %s: ", c.f.as, what)
	}
	line, _ := c.in.ReadString('\n')
	line = strings.TrimSpace(line)
	return line == "y" || line == "Y" || line == "yes" || (!single && line == c.f.as)
}

// after reads the call's journal again and prints the gate's standing and what to do next. With the
// approvers' keys the tally is verified here (agent.TallyApprovals, the rule the gate runs);
// without them it counts the decisions recorded.
func (c *cmd) after(ctx context.Context, j *agent.Journal, verifierFor, own agent.ApproverVerifierFor, mine string) error {
	f, st := c.f, c.st
	g, err := load(ctx, j, f.run, f.call)
	if err != nil {
		return fmt.Errorf("the decision was recorded; reading the gate again failed: %w", err)
	}
	gt := g.gate
	passed, denied := false, false
	unverified := false
	var counts []agent.DecisionCheck
	switch {
	case gt.Single:
		if i := slices.IndexFunc(gt.Decisions, func(d runview.Decision) bool { return !d.Signed }); i >= 0 {
			passed, denied = gt.Decisions[i].Approved, !gt.Decisions[i].Approved
		}
		fmt.Fprintf(c.out, "%s\n", gt.Status)
	case verifierFor != nil:
		var t agent.ApprovalTally
		t, counts = agent.TallyApprovals(g.recs, g.subject(f.run), g.policy(), verifierFor)
		passed, denied = t.Passed(), t.Unreachable()
		fmt.Fprintf(c.out, "verified tally: %d of %d needed approved%s%s\n", t.Approved, t.Need, names(" by", t.ApprovedBy), names("; denied by", t.DeniedBy))
		if len(t.Pending) > 0 && !passed && !denied {
			fmt.Fprintf(c.out, "still pending: %s\n", safe(strings.Join(t.Pending, ", ")))
		}
	default:
		unverified = true
		passed, denied = len(gt.ApprovedBy) >= gt.Need, len(gt.Approvers)-len(gt.Excluded)-len(gt.DeniedBy) < gt.Need
		fmt.Fprintf(c.out, "%s recorded decisions: %d of %d needed approved%s%s\n", st.BoldFail("UNVERIFIED"), len(gt.ApprovedBy), gt.Need, names(" by", gt.ApprovedBy), names("; denied by", gt.DeniedBy))
		if len(gt.Pending) > 0 && !passed && !denied {
			fmt.Fprintf(c.out, "still pending: %s\n", safe(strings.Join(gt.Pending, ", ")))
		}
		fmt.Fprintln(c.out, st.Dim("(counted from the journal, unverified; the gate checks every signature when the run resumes. Give -approver-keys to verify them here.)"))
	}
	var countErr error
	if mine != "" {
		// Does this decision count? With the approvers' keys, by the gate's rule. Without them, by the
		// same rule over the approver's own key alone: a record that key did not sign holds nobody's
		// place, so a junk record before this one does not displace it.
		ownCounts := counts
		if verifierFor == nil {
			_, ownCounts = agent.TallyApprovals(g.recs, g.subject(f.run), g.policy(), own)
		}
		if c.cert == nil {
			countErr = ownDecisionCounts(mine, ownCounts)
		}
	}
	switch {
	case countErr != nil:
		fmt.Fprintf(c.out, "\n%s %s\n", st.Cross(), countErr)
	case unverified && (passed || denied):
		fmt.Fprintf(c.out, "\n%s the recorded decisions would settle the gate if their signatures verify. Next: re-run run %s (or let RecoverLoop resume it); the gate checks every signature and then runs or denies the call.\n", st.Bang(), st.Bold(safe(f.run)))
	case passed:
		fmt.Fprintf(c.out, "\n%s the gate has its approvals. Next: re-run run %s (agent.Run with the same run ID, or let RecoverLoop resume it; for a sub-agent's run, its root run ID); the call then runs.\n", st.Pass(), st.Bold(safe(f.run)))
	case denied:
		fmt.Fprintf(c.out, "\n%s the gate can no longer pass. Next: re-run run %s; the call is denied and the model is told so.\n", st.Cross(), st.Bold(safe(f.run)))
	default:
		fmt.Fprintf(c.out, "\nNext: the other pending approvers decide. When the gate has its decisions, re-run run %s (or let RecoverLoop resume it).\n", st.Bold(safe(f.run)))
	}
	return countErr
}

// ownDecisionCounts checks that the decision just recorded (the journal record named mine) is the
// approver's decision the gate counts. Two decisions of one approver race (an approve and a reject
// from two terminals): both are recorded, the first valid one in journal order counts, and the other
// command must not report success.
func ownDecisionCounts(mine string, checks []agent.DecisionCheck) error {
	for _, ck := range checks {
		if ck.Step == mine && ck.Counted {
			return nil
		}
	}
	return fmt.Errorf("another decision of yours counts: this one (record %s) was recorded, but the gate counts only an approver's first valid decision, and it already counted another; nothing you do now changes it", safe(mine))
}

func names(label string, ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return label + " " + safe(strings.Join(ids, ", "))
}

// waiting is one call that waits for a decision, as -list reports it.
type waiting struct {
	Run        string   `json:"run"`
	Call       string   `json:"call"`
	Tool       string   `json:"tool"`
	Single     bool     `json:"single,omitempty"`
	Need       int      `json:"need,omitempty"`
	Approvers  []string `json:"approvers,omitempty"`
	ApprovedBy []string `json:"approved_by,omitempty"`
	DeniedBy   []string `json:"denied_by,omitempty"`
	Pending    []string `json:"pending,omitempty"`
	// Requesters and Excluded: the gate excludes the run's requesters (separation of duties), and
	// the approvers who fill no seat for it. No command is offered for them.
	Requesters []string `json:"requesters,omitempty"`
	Excluded   []string `json:"excluded,omitempty"`
	AskedAt    string   `json:"asked_at,omitempty"`
	// ExpiresAt is the gate's recorded deadline, Timeout its length, and Due where it stands on this
	// command's clock. Overdue: the deadline has passed and no drive has recorded the expiry; the gate
	// refuses decisions, so no command is offered.
	ExpiresAt string `json:"expires_at,omitempty"`
	Timeout   string `json:"timeout,omitempty"`
	Due       string `json:"due,omitempty"`
	Overdue   bool   `json:"overdue,omitempty"`
	// Verified says the standing above is the gate's own count (agent.TallyApprovals with the
	// approvers' keys, -approver-keys). Without it the standing counts the decisions as recorded,
	// signatures unchecked, and is never taken to settle the gate.
	Verified bool `json:"verified"`
	// Settled says the recorded decisions already settle the gate ("approved": it has its approvals;
	// "denied": it can no longer pass): the run only needs re-running, and no command is offered. Set
	// only when Verified.
	Settled  string   `json:"settled,omitempty"`
	Commands []string `json:"commands"`
	// Identity is the gate's approver identity, for a gate with a certificate-bound approver
	// (runview.GateIdentity): what it is paused for and the identity, tier, basis and status time of
	// each counted decision. Such a gate is never Verified here: the keys -approver-keys holds cannot
	// check a certificate-bound decision, the gate does.
	Identity *runview.GateIdentity `json:"identity,omitempty"`
	// Held: the gate counted and the call is held before execution by the identity status re-check.
	Held bool `json:"held,omitempty"`
}

// list prints the calls that wait for a decision in the journal.
func (c *cmd) list(ctx context.Context) error {
	j, closer, tgt, err := c.open(true)
	if err != nil {
		return err
	}
	defer closer.Close()
	var verifierFor agent.ApproverVerifierFor
	if c.f.approverKeys != "" {
		keys, err := approverKeys(c.f.approverKeys)
		if err != nil {
			return err
		}
		verifierFor = verifiers(keys)
	}
	l, ok := agent.Capability[agent.Lister](j.Store())
	if !ok {
		return errors.New("this journal's store cannot list runs; use -run with -call, or bide view")
	}
	var runs []string
	for id, err := range l.Runs(ctx, agent.RunFilter{ExcludeHolding: []string{"run:complete", "run:aborted", "run:cancelled"}}) {
		if err != nil {
			return fmt.Errorf("list runs: %w", err)
		}
		runs = append(runs, id)
	}
	slices.Sort(runs)
	var out []waiting
	for _, run := range runs {
		recs, err := j.History(ctx, run)
		if err != nil {
			return fmt.Errorf("read run %s: %w", safe(run), err)
		}
		v := runview.Build(ctx, run, recs, nil)
		runview.MarkDeadlines(v.Gates, now())
		for _, g := range v.Gates {
			if !(g.Waiting || g.Held) || v.State == string(agent.RunCompleted) || v.State == string(agent.RunAborted) || v.State == string(agent.RunCancelled) || v.State == string(agent.RunEnded) {
				continue
			}
			w := waiting{Run: run, Call: g.ToolUseID, Tool: g.ToolName, Single: g.Single, Need: g.Need, Approvers: g.Approvers, ApprovedBy: g.ApprovedBy, DeniedBy: g.DeniedBy, Pending: g.Pending, AskedAt: g.AskedAt,
				ExpiresAt: g.ExpiresAt, Timeout: g.Timeout, Due: g.Due, Overdue: g.Overdue, Identity: g.Identity, Held: g.Held}
			bound := g.Identity != nil && g.Identity.Bound
			if g.ExcludePrincipal {
				w.Requesters, w.Excluded = g.Requesters, g.Excluded
			}
			if _, tu, found := agent.FindToolCall(recs, g.ToolUseID); found && verifierFor != nil && !g.Single && g.Need > 0 && !bound {
				// The gate's own count, with the approvers' keys: records that do not verify count for
				// nothing, and a pending approver is one with no valid decision.
				t, _ := agent.TallyApprovals(recs, agent.ApprovalSubject{RunID: run, ToolUseID: g.ToolUseID, ToolName: tu.Name, Args: tu.Args}, gatePolicy(g), verifierFor)
				w.Verified, w.ApprovedBy, w.DeniedBy, w.Pending = true, t.ApprovedBy, t.DeniedBy, t.Pending
				switch {
				case t.Passed():
					w.Settled = "approved"
				case t.Unreachable():
					w.Settled = "denied"
				}
			}
			line := func(approver string) {
				cmdline, err := approveline.Command(tgt, run, g.ToolUseID, approver, "", "", g.Single)
				if bound && slices.Contains(g.Identity.Approvers, approver) {
					// A bare-key decision does not count for a certificate-bound approver.
					cmdline, err = approveline.CertCommand(tgt, run, g.ToolUseID, approver)
				}
				if err == nil {
					cmdline = c.src.rewriteLine(cmdline)
				} else {
					cmdline = "(no command: this run or call id holds characters that cannot be shown or pasted safely; save that call from bide approve -list -json to a file and decide with -item FILE)"
				}
				w.Commands = append(w.Commands, cmdline)
			}
			switch {
			case g.Overdue: // the gate refuses decisions
			case g.Held: // counted: no decision is wanted
			case g.Single:
				line("")
			case w.Settled != "":
			default:
				for _, id := range w.Pending { // a requester is never pending: the gate excludes them
					line(id)
				}
			}
			if w.Commands == nil {
				w.Commands = []string{}
			}
			out = append(out, w)
		}
	}
	if c.f.asJSON {
		if out == nil {
			out = []waiting{}
		}
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	st := c.st
	if len(out) == 0 {
		fmt.Fprintln(c.out, "no call waits for a decision in this journal")
		return nil
	}
	fmt.Fprintf(c.out, "%s\n", st.Heading(fmt.Sprintf("%d call(s) wait for a decision:", len(out))))
	anyCert := false
	for _, w := range out {
		fmt.Fprintf(c.out, "\n  run %s  call %s  tool %s\n", st.Bold(safe(w.Run)), st.Bold(safe(w.Call)), st.Bold(safe(w.Tool)))
		if w.Single {
			fmt.Fprintf(c.out, "    one approver, one decision (unsigned)\n")
		}
		if w.Due != "" {
			text := w.Due
			if w.Timeout != "" {
				text += "; times out after " + w.Timeout
			}
			if w.Overdue {
				text = st.Fail(text)
			}
			fmt.Fprintf(c.out, "    %s\n", text)
		}
		if !w.Single {
			label := ""
			switch {
			case w.Identity != nil && w.Identity.Bound:
				label = " (identity-bound: certificate-bound decisions count only when the gate checks them; this command does not)"
			case !w.Verified:
				label = " (decisions as recorded, unverified: give -approver-keys)"
			}
			fmt.Fprintf(c.out, "    %d of %d: %s%s%s\n", w.Need, len(w.Approvers), safe(strings.Join(w.Approvers, ", ")), names("; approved by", w.ApprovedBy)+names("; denied by", w.DeniedBy), label)
			if len(w.Requesters) > 0 {
				fmt.Fprintf(c.out, "    excluding the requester (separation of duties): requested by %s%s\n", safe(strings.Join(w.Requesters, ", ")), names("; cannot approve:", w.Excluded))
			}
			switch w.Settled {
			case "approved":
				fmt.Fprintf(c.out, "    %s\n", st.OK("approvals verified: re-run the run and the call runs (agent.Run with the same run ID, or RecoverLoop)"))
			case "denied":
				fmt.Fprintf(c.out, "    %s\n", st.Fail("can no longer pass: re-run the run and the call is denied"))
			default:
				fmt.Fprintf(c.out, "    pending: %s\n", safe(strings.Join(w.Pending, ", ")))
			}
		}
		c.listIdentity(w)
		for _, cmdline := range w.Commands {
			fmt.Fprintf(c.out, "    %s\n", st.Path(safe(cmdline)))
			anyCert = anyCert || strings.Contains(cmdline, " -cert ")
		}
	}
	if c.f.postgres != "" {
		fmt.Fprintf(c.out, "\n%s\n", st.Dim("Set $"+defaultPostgresEnv+" to the DSN, or use -postgres-env with your own variable: the commands above never carry the DSN."))
	}
	fmt.Fprintf(c.out, "\n%s\n", st.Dim(c.src.ListHint))
	if anyCert {
		fmt.Fprintf(c.out, "%s\n", st.Dim("A command with -cert is for a certificate-bound approver (a bare-key decision does not count for them): replace '"+approveline.CertFilePlaceholder+"' with the PEM file of your certificate chain, leaf first, and '"+approveline.PolicyFilePlaceholder+"' with the identity policy file (docs/guides/certificate-approvers.md)."))
	}
	return nil
}

// listIdentity prints a gate's approver identity under its -list entry. Only a final tier is printed in
// the OK style; a provisional tier is warned, anything else is a failure, and the words say the
// tier either way. Every value comes from the journal and is printed through safe.
func (c *cmd) listIdentity(w waiting) {
	gi, st := w.Identity, c.st
	if gi == nil || !gi.Bound {
		return
	}
	style := st.Warn
	switch gi.Class {
	case "audited":
		style = st.OK // only an audit result is green
	case "final":
		style = func(t string) string { return t } // final as recorded by the gate: not green
	case "bad":
		style = st.Fail
	}
	fmt.Fprintf(c.out, "    approver identity: %s\n", style(safe(gi.Line)))
	if gi.PhaseText != "" {
		fmt.Fprintf(c.out, "      %s\n", style(safe(gi.PhaseText)))
	}
	if gi.Detail != "" && gi.Hold == nil {
		fmt.Fprintf(c.out, "      %s\n", safe(gi.Detail))
	}
	for _, p := range gi.Problems {
		fmt.Fprintf(c.out, "      %s\n", st.Fail(safe(p)))
	}
	if h := gi.Hold; h != nil {
		fmt.Fprintf(c.out, "      held %s UTC: %s%s\n", safe(h.CheckedAt), safe(h.Reason), cond(h.Detail != "", ": "+safe(h.Detail), ""))
	}
	for _, ic := range gi.Cards {
		tier := safe(ic.TierText)
		switch ic.Class {
		case "audited":
			tier = st.OK(tier)
		case "final":
		case "prov":
			tier = st.Warn(tier)
		default:
			tier = st.Fail(tier)
		}
		name := ""
		if ic.DisplayName != "" {
			name = " (" + safe(ic.DisplayName) + ")"
		}
		fmt.Fprintf(c.out, "      %s: %s%s  %s\n", safe(ic.Approver), safe(ic.Identifier), name, tier)
		if ic.Disputed {
			fmt.Fprintf(c.out, "        %s\n", st.Fail("DISPUTED: newer status shows a revocation (keyCompromise) that voids this count back to the certificate's notBefore"))
		}
		if ic.Basis != "" {
			fmt.Fprintf(c.out, "        basis: %s\n", safe(ic.Basis))
		}
		if ic.StatusTime != "" {
			fmt.Fprintf(c.out, "        status time (S) %s UTC%s\n", safe(ic.StatusTime), cond(ic.StatusAge != "", " ("+safe(ic.StatusAge)+")", ""))
		}
		if ic.AnchoredAt != "" {
			fmt.Fprintf(c.out, "        provable at %s UTC (%s)\n", safe(ic.AnchoredAt), safe(ic.TimeSource))
		}
	}
	if len(gi.Cards) > 0 {
		fmt.Fprintf(c.out, "      %s\n", st.Dim("as recorded at counting by the gate, not re-verified here; a revocation first reported after S is not covered"))
	}
	if gi.PolicyDigest != "" {
		fmt.Fprintf(c.out, "      policy %s %s\n", safe(gi.PolicyDigest), st.Dim("(as the journal records it; not recomputed here)"))
	}
}

func cond(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

// auditLine says when a recorded decision is signed into the audit log. It is the same whatever the
// journal is: bide approve cannot tell from the journal whether the application audits it (an
// audit.AuditedStore is configured in the application, and publishes its signed tree heads to its
// own anchor), and the decision is covered by the first head signed after it is recorded.
const auditLine = "the decision is signed into the audit log at the app's next write to this run"

// receiptTarget returns -receipt, or "" for the default name (which holds the decision time, known
// only when it is signed), and checks before anything is signed or recorded that the receipt can be
// written: a named file must not exist (a receipt is never overwritten) and its directory must.
func (c *cmd) receiptTarget() (string, error) {
	p := c.f.receipt
	dir := filepath.Dir(p)
	if p == "" {
		dir = c.src.receiptDir(c.f.key)
	}
	if p != "" {
		if _, err := os.Lstat(p); err == nil {
			return "", refuse("the receipt file %s exists, and a receipt is never overwritten: remove it, or name another with -receipt", p)
		}
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", refuse("the directory for the receipt, %s, does not exist: name another file with -receipt", dir)
	}
	return p, nil
}

// maxNamePart bounds each of the run, call and approver parts of a default receipt name, so that the
// whole name (three parts, the 24-byte decision time, three separators and ".receipt.json") stays
// under the 255 bytes a file name may be on the common file systems.
const maxNamePart = 56

// defaultReceiptName is <run>-<call>-<approver>-<decided_at>.receipt.json with every character outside
// letters, digits, dot, dash and underscore written as an underscore, so a run or call id (any
// string) can place the file only in the key's directory. The decision time keeps one approver's
// decisions on one call (a first that never counted, then a second) in files of their own; a name
// that is taken anyway is refused when written, never overwritten.
func defaultReceiptName(run, call, approver, decidedAt string) string {
	clean := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
		if b.Len() > maxNamePart {
			return b.String()[:maxNamePart]
		}
		return b.String()
	}
	return strings.TrimLeft(clean(run)+"-"+clean(call)+"-"+clean(approver)+"-"+clean(decidedAt), ".") + ".receipt.json"
}

// writeReceipt writes the receipt of the decision d just recorded, found in the journal by what the
// approver signed, to path (mode 0600, exclusive), and prints where it went and when the decision
// is signed into the audit log. The decision is recorded either way; a receipt that cannot be
// written is reported as an error, with that said.
func (c *cmd) writeReceipt(ctx context.Context, j *agent.Journal, s agent.ApprovalSubject, d agent.Decision, path string) (record string, err error) {
	recs, err := j.History(ctx, c.f.run)
	if err != nil {
		return "", fmt.Errorf("the decision was recorded; reading the journal for its receipt failed: %w", err)
	}
	decidedAt := agent.FormatTimestamp(d.DecidedAt)
	if path == "" {
		path = filepath.Join(c.src.receiptDir(c.f.key), defaultReceiptName(c.f.run, c.f.call, c.f.as, decidedAt))
	}
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		if !agent.IsApprovalDecision(r, d.ToolUseID) || r.Approver() != d.ApproverID || r.DecidedAt() != decidedAt || !bytes.Equal(r.Signature(), d.Signature) {
			continue
		}
		rc, err := audit.NewDecisionReceipt(s, d, r, i)
		if err != nil {
			return r.Name, fmt.Errorf("the decision was recorded; its receipt could not be made: %w", err)
		}
		b, err := json.MarshalIndent(rc, "", "  ")
		if err != nil {
			return r.Name, fmt.Errorf("the decision was recorded; its receipt could not be made: %w", err)
		}
		if err := writeExclusive(path, append(b, '\n'), 0o600); err != nil {
			return r.Name, fmt.Errorf("the decision was recorded, but its receipt was not written: %w", err)
		}
		fmt.Fprintf(c.out, "%sreceipt %s %s\n", c.st.Pass(), c.st.Path(path), c.st.Dim("(mode 0600; verify it with "+c.src.Prog+" -verify-receipt, or bide audit verify-receipts)"))
		fmt.Fprintf(c.out, "audit: %s\n", auditLine)
		return r.Name, nil
	}
	return "", errors.New("the decision was recorded, but its record is not in the journal read back, so no receipt was written")
}

// verifyReceiptFile is bide approve -verify-receipt: the receipt's signature over the exact call it
// states, by the key -approver-keys enrols for its approver. It reads no journal.
func (c *cmd) verifyReceiptFile() error {
	b, err := safefile.Read(c.f.verifyReceipt, safefile.MaxDocument, nil)
	if err != nil {
		return fmt.Errorf("receipt %s: %w", c.f.verifyReceipt, unwrapPath(safefile.Explain("-verify-receipt", err, false)))
	}
	var r audit.DecisionReceipt
	if err := audit.UnmarshalStrict(b, &r); err != nil {
		return fmt.Errorf("receipt %s: %w", c.f.verifyReceipt, err)
	}
	if len(r.Cert) > 0 {
		return c.verifyCertReceipt(r)
	}
	if c.f.approverKeys == "" {
		return refuse("the receipt is of a bare-key decision: -verify-receipt needs -approver-keys for it")
	}
	keys, err := approverKeys(c.f.approverKeys)
	if err != nil {
		return err
	}
	v, ok := keys[r.Approver]
	if !ok {
		return refuse("-approver-keys has no key for the receipt's approver %q", safe(r.Approver))
	}
	if err := r.Verify(v); err != nil {
		return refuse("the receipt does not verify: %s", safe(err.Error()))
	}
	st := c.st
	fmt.Fprintf(c.out, "%ssignature verifies for %s %s call %s (tool %s) of run %s at %s\n", st.Pass(), st.Bold(safe(r.Approver)), approvedWord(r.Approved), safe(r.ToolUseID), safe(r.ToolName), safe(r.RunID), safe(r.DecidedAt))
	fmt.Fprintf(c.out, "  args SHA-256 %s\n  journal record %s at position %d, recorded %s\n", r.ArgsSHA, safe(r.Record), r.Index, safe(r.RecordedAt))
	fmt.Fprintf(c.out, "%s\n", st.Dim("this checks the receipt alone; bide audit verify-receipts checks that the signed heads of the journal commit to the decision"))
	return nil
}

// verifyCertReceipt checks a certificate-bound receipt on its own: its anchoring proof under the log's
// key fixes the time A the decision became provable, and the policy accepts the chain, the identity and
// the signature at A. Revocation status is not checked, and the output says so.
func (c *cmd) verifyCertReceipt(r audit.DecisionReceipt) error {
	if c.f.identityPolicy == "" {
		return refuse("the receipt is certificate-bound (it carries the approver's certificate chain): pass -identity-policy and -log-key to check its chain and identity")
	}
	ci, err := policyfile.Read(c.f.identityPolicy)
	if err != nil {
		return err
	}
	logKey, err := readLogKey(c.f.logKey)
	if err != nil {
		return err
	}
	if r.Anchor == nil {
		return refuse("the receipt has no anchoring proof, so the time the decision became provable is not known: bide audit verify-receipts -update adds one once a signed head covers it")
	}
	id, err := r.VerifyIdentity(ci, logKey)
	if err != nil {
		return refuse("the receipt does not verify: %s", safe(err.Error()))
	}
	st := c.st
	name := ""
	if id.DisplayName != "" {
		name = fmt.Sprintf(" (certificate name %q, not part of the identity)", safe(id.DisplayName))
	}
	fmt.Fprintf(c.out, "IDENTITY %s%s%s accepted for %s, who %s call %s (tool %s) of run %s at %s\n", "", st.Bold(safe(id.Identifier)), name, st.Bold(safe(r.Approver)), approvedWord(r.Approved), safe(r.ToolUseID), safe(r.ToolName), safe(r.RunID), safe(r.DecidedAt))
	fmt.Fprintf(c.out, "  accepted at %s (%s); certificate valid %s to %s\n  leaf %s, trust anchor key %s\n", agent.FormatTimestamp(id.AnchoredAt), id.TimeSource, agent.FormatTimestamp(id.NotBefore), agent.FormatTimestamp(id.NotAfter), id.LeafHash, id.AnchorKey)
	fmt.Fprintf(c.out, "  args SHA-256 %s\n  journal record %s at position %d, recorded %s\n", r.ArgsSHA, safe(r.Record), r.Index, safe(r.RecordedAt))
	fmt.Fprintln(c.out, "revocation status was NOT checked, so this is the identity check, not the gate's verdict (final or provisional); the receipt alone does not show the journal kept the decision: bide audit verify-receipts checks that")
	return nil
}

// readLogKey reads the log's public key given as key text or as a file holding it.
func readLogKey(s string) (audit.Verifier, error) {
	if v, err := audit.ParsePublicKey(s); err == nil {
		return v, nil
	}
	b, err := safefile.ReadOrStdin("-log-key", s, safefile.MaxKey)
	if err != nil {
		return nil, refuse("-log-key is neither a public key (ed25519:<hex>) nor a readable file: %s", safe(unwrapPath(safefile.Explain("-log-key", err, true)).Error()))
	}
	v, err := audit.ParsePublicKey(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, refuse("-log-key %s: %s", safe(s), safe(err.Error()))
	}
	return v, nil
}

func approvedWord(approved bool) string {
	if approved {
		return "approved"
	}
	return "denied"
}

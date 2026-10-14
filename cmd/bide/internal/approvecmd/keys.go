package approvecmd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/term"
)

// A private key file holds the Ed25519 seed as 64 lowercase hex characters and a newline, the
// form bide demo writes its log key in. The seed is the whole private key (ed25519.NewKeyFromSeed
// derives the rest). The public key is written beside it, as <file>.pub, in the text policies and
// verifiers read (audit.FormatPublicKey: "ed25519:<hex>").
const (
	seedHexLen = 2 * ed25519.SeedSize
	// maxKeyFile bounds what is read from a key file: a seed, its newline, and slack for an editor's
	// trailing white space. A file larger than this is not a key file.
	maxKeyFile = 1024
)

// errKeyFormat is the only thing said about a key file whose content is not a seed. It never
// includes the content, or any part of it: a hex decoding error would name the first bad
// character, which is one character of a secret in a terminal log.
var errKeyFormat = errors.New("not a bide approver key: want 64 hex characters, the Ed25519 seed bide keygen writes")

// LoadKey reads the approver private key at path. It refuses a key file that is not a regular file
// and, on Unix, one that group or others can read or write, as ssh does: the file's mode is read
// from the open file, so what is checked is what is read. It never returns or prints key
// material in an error.
func LoadKey(path string) (ed25519.PrivateKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, unwrapPath(err))
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, unwrapPath(err))
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("key file %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("key file %s is accessible by others (mode %04o); a private key must not be: run chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, unwrapPath(err))
	}
	if len(b) > maxKeyFile {
		return nil, fmt.Errorf("key file %s: %w", path, errKeyFormat)
	}
	text := strings.TrimSpace(string(b))
	if len(text) != seedHexLen {
		return nil, fmt.Errorf("key file %s: %w", path, errKeyFormat)
	}
	seed, err := hex.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, errKeyFormat) // not err: it names a character of the key
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// unwrapPath returns the underlying error of a *PathError, so a message names the path once.
func unwrapPath(err error) error {
	if pe, ok := errors.AsType[*os.PathError](err); ok {
		return pe.Err
	}
	return err
}

// publicKeyText is the text form of priv's public key (audit.FormatPublicKey).
func publicKeyText(priv ed25519.PrivateKey) string {
	return audit.FormatPublicKey(audit.AlgEd25519, priv.Public().(ed25519.PublicKey))
}

// keygenUsage heads bide keygen's -h output.
const keygenUsage = `usage: bide keygen -out <key-file> [-as <approver id>] [-color auto|always|never]

Creates an approver key pair for bide approve. The private key is written to <key-file> (64 hex
characters, the Ed25519 seed) with mode 0600, and is never printed; the public key is written to
<key-file>.pub and printed, in the form approval policies and verifiers read:

  ed25519:<hex>

Enroll that public key as the approver's verifier in the application that runs the gated agent
(audit.ParsePublicKey reads it), and in the JSON object bide approve -approver-keys and bide view
-approver-keys read: {"ops": "ed25519:<hex>"}. With -as, bide keygen prints that JSON entry for
the id.

It refuses to overwrite a file. The private key file is not encrypted; keep it on the approver's
own machine, readable only by the approver. Anyone who can read it can sign as the approver.

Flags:
`

// KeygenMain is bide keygen: it creates an approver key pair. It returns the exit status: 0 on
// success, 1 on a failure, 2 for a usage error.
func KeygenMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), keygenUsage)
		fs.PrintDefaults()
	}
	out := fs.String("out", "", "file to write the private key to (mode 0600); the public key goes to <file>.pub. It must not exist")
	as := fs.String("as", "", "the approver id the key is for: prints the approver-keys entry for it")
	color := term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	es := term.For(stderr, *color)
	usage := func(msg string) int {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide keygen:", msg)
		fs.Usage()
		return 2
	}
	if fs.NArg() > 0 {
		return usage(fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	}
	if *out == "" {
		return usage("no -out given")
	}
	if *as != "" {
		if err := (agent.ApprovalPolicy{Need: 1, Approvers: []string{*as}}).Validate(); err != nil {
			return usage("-as: " + err.Error())
		}
	}
	st := term.For(stdout, *color)
	priv, err := writeKeyPair(*out)
	if err != nil {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide keygen:", err)
		return 1
	}
	pub := publicKeyText(priv)
	fmt.Fprintf(stdout, "%swrote private key %s %s\n", st.Pass(), st.Path(*out), st.Dim("(mode 0600; keep it secret, it is not encrypted)"))
	fmt.Fprintf(stdout, "%swrote public key  %s\n", st.Pass(), st.Path(*out+".pub"))
	fmt.Fprintf(stdout, "\npublic key  %s\n", st.Bold(pub))
	if *as != "" {
		fmt.Fprintf(stdout, "approver-keys entry  %s\n", st.Bold(fmt.Sprintf("{%q: %q}", *as, pub)))
	}
	return 0
}

// writeKeyPair generates a key pair and writes the private key to path (mode 0600, exclusive:
// an existing file is never overwritten) and the public key to path+".pub" (mode 0644). The
// private key is written first and the check that neither file exists runs before either is
// created, so a refusal leaves nothing behind.
func writeKeyPair(path string) (ed25519.PrivateKey, error) {
	for _, p := range []string{path, path + ".pub"} {
		if _, err := os.Lstat(p); err == nil {
			return nil, fmt.Errorf("%s exists; bide keygen does not overwrite a file (remove it, or choose another -out)", p)
		}
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := writeExclusive(path, []byte(hex.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return nil, err
	}
	if err := writeExclusive(path+".pub", []byte(publicKeyText(priv)+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("%w (the private key %s was written; its public key is derived from it, bide approve prints it)", err, path)
	}
	return priv, nil
}

// writeExclusive creates path with mode perm and writes data, failing if path exists. perm is
// subject to the process's umask, which can only remove bits.
func writeExclusive(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, unwrapPath(err))
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

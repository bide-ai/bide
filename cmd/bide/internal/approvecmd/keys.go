package approvecmd

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/internal/csr"
	"github.com/bide-ai/bide/internal/approveline"
	"github.com/bide-ai/bide/internal/term"
)

// A private key file holds the seed or seeds of one key as lowercase hex, one line, so the whole
// private key is a few bytes of text. The scheme is the line's prefix; no prefix is Ed25519, the
// form bide demo writes its log key in:
//
//	<64 hex>                                     ed25519 (the Ed25519 seed)
//	ecdsa-p256:<64 hex>                          ECDSA P-256 (the private scalar, 32 bytes big-endian)
//	ml-dsa-65:<64 hex>                           ML-DSA-65 (the FIPS 204 seed)
//	ed25519+ml-dsa-65:<64 hex>:<64 hex>          hybrid (the Ed25519 seed, then the ML-DSA-65 seed)
//
// The command holds the key only as an audit.Signer (LoadSigner returns one), so everything it does
// with a key (sign a decision, derive the public key, name the scheme) goes through that interface
// and a signer held in an HSM or a KMS can take its place without changing the flow. The public key
// is written beside the key file as <file>.pub in the text policies and verifiers read
// (audit.FormatPublicKey: "<scheme>:<hex>").
const (
	seedHexLen = 2 * ed25519.SeedSize
	// maxKeyFile bounds what is read from a key file: a hybrid key line and slack for an editor's
	// trailing white space. A file larger than this is not a key file.
	maxKeyFile = 1024
)

// algs are the schemes bide keygen makes and bide approve signs with, in the order help lists them.
var algs = []audit.Alg{audit.AlgEd25519, audit.AlgECDSAP256, audit.AlgMLDSA65, audit.AlgHybrid}

// errKeyFormat is the only thing said about a key file whose content is not a key. It never
// includes the content, or any part of it: a hex decoding error would name the first bad
// character, which is one character of a secret in a terminal log.
var errKeyFormat = errors.New("not a bide approver key: want the hex seed bide keygen writes (ed25519), or ecdsa-p256:<hex>, ml-dsa-65:<hex>, or ed25519+ml-dsa-65:<hex>:<hex>")

// KeySource says where the approver's private key comes from, and carries the words that name it,
// so one implementation of the display, the confirmation, the signature, the record and the receipt
// serves a key file (FileKeys) and a key held in an HSM or a KMS (bide-hsm).
type KeySource struct {
	// Prog is the command's name in its messages and usage: "bide approve".
	Prog string
	// Usage heads the command's -h output; the flags follow it.
	Usage string
	// KeyHelp is the help text of the -key flag.
	KeyHelp string
	// KeyNoun says what -key takes, for "give -as and -key (<KeyNoun>)": "your private key file".
	KeyNoun string
	// EnrolCmd is the command that prints an approver-keys entry for a key, named in the refusal that
	// asks for -approver-keys: "bide keygen -as".
	EnrolCmd string
	// ReceiptWhere says where a default receipt goes, for the message of a repeated decision.
	ReceiptWhere string
	// NoEchoArgs keeps a stray positional argument out of the usage error: a key reference given
	// without -key can hold a secret.
	NoEchoArgs bool
	// EnrolHelp completes "(...)" after -approver-keys in the help: how to get an entry for a key.
	EnrolHelp string
	// ReceiptHelp is the help text of -receipt.
	ReceiptHelp string
	// ListHint is the line -list prints under the commands it suggests, about the placeholders.
	ListHint string
	// Placeholder is what -list writes for -key in a suggested command; empty keeps
	// approveline.KeyFilePlaceholder.
	Placeholder string
	// Flags registers the source's own flags before the command parses its arguments. Optional.
	Flags func(fs *flag.FlagSet)
	// Load returns the signer for the value of -key. It runs after the arguments are parsed, so a
	// closure can read the flags Flags registered. A signer that is an io.Closer is closed when the
	// command ends, and one with a Notice() string method has the notice shown with the key. It must never put key material, a PIN or a credential in an error.
	Load func(ref string) (audit.Signer, error)
	// ReceiptDir returns the directory a default receipt goes in for the value of -key. Nil means
	// the directory of the key file named.
	ReceiptDir func(ref string) string
}

// FileKeys is the key source of bide approve: -key names a key file bide keygen wrote.
func FileKeys() KeySource {
	return KeySource{
		Prog:         "bide approve",
		Usage:        usageText,
		KeyHelp:      "your private key file, as bide keygen writes it (Ed25519, ECDSA P-256, ML-DSA-65 or hybrid) (not for a single-approver gate); on Unix it must not be readable by group or others",
		KeyNoun:      "your private key file",
		EnrolHelp:    "bide keygen -as prints an entry",
		EnrolCmd:     "bide keygen -as",
		ReceiptWhere: "next to your key",
		ReceiptHelp:  "where to write the decision receipt (default: next to -key, named <run>-<call>-<approver>.receipt.json); mode 0600, never overwritten",
		ListHint:     "Replace '" + approveline.KeyFilePlaceholder + "' with the path of your private key (bide keygen makes one) and '" + approveline.KeysFilePlaceholder + "' with the file of enrolled approver public keys. bide approve shows what it signs and asks first.",
		Placeholder:  approveline.KeyFilePlaceholder,
		Load:         LoadSigner,
	}
}

// receiptDir is the directory of a default receipt for the -key value ref.
func (k KeySource) receiptDir(ref string) string {
	if k.ReceiptDir != nil {
		return k.ReceiptDir(ref)
	}
	return filepath.Dir(ref)
}

// rewriteLine adapts a command line approveline built ("bide approve ... '<key-file>' ...") to this
// source: its command name and its placeholder for -key.
func (k KeySource) rewriteLine(line string) string {
	if k.Prog != "bide approve" {
		line = k.Prog + strings.TrimPrefix(line, "bide approve")
	}
	if k.Placeholder != "" && k.Placeholder != approveline.KeyFilePlaceholder {
		// Only the -key argument: a run or call id may itself read '<key-file>'.
		line = strings.Replace(line, " -key '"+approveline.KeyFilePlaceholder+"'", " -key '"+k.Placeholder+"'", 1)
	}
	return line
}

// LoadSigner reads the approver private key at path as an audit.Signer. It refuses a key file that
// is not a regular file and, on Unix, one that another user owns or that group or others can read or
// write, as ssh does (OpenPrivate): what is checked is the open file, so it is what is read. It never returns or
// prints key material in an error.
func LoadSigner(path string) (audit.Signer, error) {
	f, err := OpenPrivate(path, "key file")
	if err != nil {
		if _, ok := errors.AsType[*PrivateFileError](err); ok {
			return nil, err
		}
		return nil, fmt.Errorf("key file %s: %w", path, unwrapPath(err))
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, unwrapPath(err))
	}
	if len(b) > maxKeyFile {
		return nil, fmt.Errorf("key file %s: %w", path, errKeyFormat)
	}
	sg, err := parseSigner(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	return sg, nil
}

// errNoMLDSA is what a build without crypto/mldsa (the FIPS 140-3 module v1.0.0) says.
var errNoMLDSA = errors.New("ml-dsa-65 is not available in this build (the FIPS 140-3 module v1.0.0 lacks it)")

// parseSigner reads a key line (see the format above). Its errors say only that the line is not a
// key.
func parseSigner(line string) (audit.Signer, error) {
	alg, rest := audit.AlgEd25519, line
	if i := strings.IndexByte(line, ':'); i >= 0 {
		alg, rest = audit.Alg(line[:i]), line[i+1:]
	}
	seeds := strings.Split(rest, ":")
	want := 1
	if alg == audit.AlgHybrid {
		want = 2
	}
	if len(seeds) != want {
		return nil, errKeyFormat
	}
	raw := make([][]byte, want)
	for i, h := range seeds {
		if len(h) != seedHexLen {
			return nil, errKeyFormat
		}
		b, err := hex.DecodeString(h)
		if err != nil {
			return nil, errKeyFormat // not err: it names a character of the key
		}
		raw[i] = b
	}
	switch alg {
	case audit.AlgEd25519:
		return audit.Ed25519Signer{Priv: ed25519.NewKeyFromSeed(raw[0])}, nil
	case audit.AlgECDSAP256:
		k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw[0])
		if err != nil {
			return nil, errKeyFormat // a scalar that is zero or not below the group order
		}
		return audit.ECDSAP256Signer{Priv: k}, nil
	case audit.AlgMLDSA65:
		k, err := mldsa.NewPrivateKey(mldsa.MLDSA65(), raw[0])
		if err != nil {
			return nil, errNoMLDSA
		}
		return audit.MLDSASigner{Priv: k}, nil
	case audit.AlgHybrid:
		k, err := mldsa.NewPrivateKey(mldsa.MLDSA65(), raw[1])
		if err != nil {
			return nil, errNoMLDSA
		}
		return audit.HybridSigner{Ed: audit.Ed25519Signer{Priv: ed25519.NewKeyFromSeed(raw[0])}, ML: audit.MLDSASigner{Priv: k}}, nil
	}
	return nil, errKeyFormat
}

// newSigner makes a fresh key of scheme alg and returns it with the key line that reloads it.
func newSigner(alg audit.Alg) (audit.Signer, string, error) {
	ed := func() (ed25519.PrivateKey, error) {
		_, k, err := ed25519.GenerateKey(rand.Reader)
		return k, err
	}
	ml := func() (*mldsa.PrivateKey, error) { return mldsa.GenerateKey(mldsa.MLDSA65()) }
	switch alg {
	case audit.AlgEd25519:
		k, err := ed()
		if err != nil {
			return nil, "", err
		}
		return audit.Ed25519Signer{Priv: k}, hex.EncodeToString(k.Seed()), nil
	case audit.AlgECDSAP256:
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, "", err
		}
		d, err := k.Bytes()
		if err != nil {
			return nil, "", err
		}
		return audit.ECDSAP256Signer{Priv: k}, "ecdsa-p256:" + hex.EncodeToString(d), nil
	case audit.AlgMLDSA65:
		k, err := ml()
		if err != nil {
			return nil, "", err
		}
		return audit.MLDSASigner{Priv: k}, "ml-dsa-65:" + hex.EncodeToString(k.Bytes()), nil
	case audit.AlgHybrid:
		e, err := ed()
		if err != nil {
			return nil, "", err
		}
		k, err := ml()
		if err != nil {
			return nil, "", err
		}
		return audit.HybridSigner{Ed: audit.Ed25519Signer{Priv: e}, ML: audit.MLDSASigner{Priv: k}}, "ed25519+ml-dsa-65:" + hex.EncodeToString(e.Seed()) + ":" + hex.EncodeToString(k.Bytes()), nil
	}
	return nil, "", fmt.Errorf("unknown scheme %q: want one of %v", alg, algs)
}

// unwrapPath returns the underlying error of a *PathError, so a message names the path once.
func unwrapPath(err error) error {
	if pe, ok := errors.AsType[*os.PathError](err); ok {
		return pe.Err
	}
	return err
}

// publicKeyText is the text form of the signer's public key (audit.FormatPublicKey).
func publicKeyText(sg audit.Signer) string {
	return audit.FormatPublicKey(sg.Alg(), sg.PublicKey())
}

// keyName names a signer's public key briefly: its scheme and the SHA-256 identity the gate
// compares (audit.KeyID). An ML-DSA public key is nearly 4000 hex characters; this is what a person
// can read and compare.
func keyName(sg audit.Signer) string {
	return audit.KeyID(string(sg.Alg()), sg.PublicKey())
}

// keygenUsage heads bide keygen's -h output.
const keygenUsage = `usage: bide keygen -out <key-file> [-as <approver id>] [-alg ed25519|ecdsa-p256|ml-dsa-65|ed25519+ml-dsa-65] [-csr <request.pem>] [-color auto|always|never]

Creates an approver key pair for bide approve. The private key is written to <key-file> (the key's
seed as hex: 64 characters for ed25519, the default; "ecdsa-p256:" and the 32-byte private
scalar for ECDSA P-256, the scheme most HSMs and cloud KMS sign; "ml-dsa-65:" and the seed for ML-DSA-65;
"ed25519+ml-dsa-65:" and both seeds for a hybrid, for buyers who require post-quantum or hybrid
signatures) with mode 0600, and is never printed; the public key is written to <key-file>.pub and
printed, in the form approval policies and verifiers read:

  <scheme>:<hex>        e.g. ed25519:<hex>

An ML-DSA-65 or hybrid public key is long (nearly 4000 hex characters). ML-DSA needs a Go
build with crypto/mldsa, which the FIPS 140-3 module v1.0.0 lacks.

Enroll that public key as the approver's verifier in the application that runs the gated agent
(audit.ParsePublicKey reads it), and in the JSON object bide approve -approver-keys and bide view
-approver-keys read: {"ops": "<scheme>:<hex>"}. With -as, bide keygen prints that JSON entry for
the id.

With -csr, bide keygen also writes a PKCS#10 certificate signing request for the new key (Ed25519
or ECDSA P-256 only) to the file, as PEM. It proves possession of the key and names nothing: the
subject is empty and there are no extensions, so the CA or identity provider sets the name (SAN)
and the key usage. Send it to the CA that issues the approver's certificate; the issued chain is
what bide approve -cert reads. For a key in an HSM or a KMS, bide-hsm csr makes the request.

It refuses to overwrite a file (the key, its .pub or the request). The private key file is not encrypted; keep it on the approver's
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
	alg := fs.String("alg", string(audit.AlgEd25519), "signature scheme: ed25519, ecdsa-p256, ml-dsa-65 or ed25519+ml-dsa-65 (hybrid)")
	csr := fs.String("csr", "", "also write a certificate signing request for the new key to this PEM file (ed25519 or ecdsa-p256; no names in it, the CA sets them). It must not exist")
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
	if !slices.Contains(algs, audit.Alg(*alg)) {
		return usage(fmt.Sprintf("-alg %q: want one of %v", *alg, algs))
	}
	if *csr != "" && audit.Alg(*alg) != audit.AlgEd25519 && audit.Alg(*alg) != audit.AlgECDSAP256 {
		return usage(fmt.Sprintf("-csr: a certificate-bound approver signs with ed25519 or ecdsa-p256, not %s", *alg))
	}
	if *csr != "" && (*csr == *out || *csr == *out+".pub") {
		return usage("-csr names the same file as the key")
	}
	priv, err := writeKeyPairCSR(*out, *csr, audit.Alg(*alg))
	if err != nil {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide keygen:", err)
		return 1
	}
	pub := publicKeyText(priv)
	fmt.Fprintf(stdout, "%swrote private key %s %s\n", st.Pass(), st.Path(*out), st.Dim("(mode 0600; keep it secret, it is not encrypted)"))
	fmt.Fprintf(stdout, "%swrote public key  %s\n", st.Pass(), st.Path(*out+".pub"))
	if *csr != "" {
		fmt.Fprintf(stdout, "%swrote certificate signing request  %s\n", st.Pass(), st.Path(*csr))
	}
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
func writeKeyPair(path string, alg audit.Alg) (audit.Signer, error) {
	return writeKeyPairCSR(path, "", alg)
}

// writeKeyPairCSR is writeKeyPair that also writes a certificate signing request for the new key to
// csrPath (if not empty; see csr.go). The request is built before any file is created, so a refusal
// (an existing file, or a scheme that has no request) leaves nothing behind.
func writeKeyPairCSR(path, csrPath string, alg audit.Alg) (audit.Signer, error) {
	paths := []string{path, path + ".pub"}
	if csrPath != "" {
		paths = append(paths, csrPath)
	}
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			return nil, fmt.Errorf("%s exists; bide keygen does not overwrite a file (remove it, or choose another -out)", p)
		}
	}
	priv, line, err := newSigner(alg)
	if err != nil {
		return nil, err
	}
	var req []byte
	if csrPath != "" {
		if req, err = csr.Build(priv, csr.Names{}); err != nil {
			return nil, err
		}
	}
	if err := writeExclusive(path, []byte(line+"\n"), 0o600); err != nil {
		return nil, err
	}
	if err := writeExclusive(path+".pub", []byte(publicKeyText(priv)+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("%w (the private key %s was written; its public key is derived from it, bide approve prints it)", err, path)
	}
	if req != nil {
		if err := writeExclusive(csrPath, req, 0o644); err != nil {
			return nil, fmt.Errorf("%w (the private key %s was written; make a request for it with your CA's tooling)", err, path)
		}
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

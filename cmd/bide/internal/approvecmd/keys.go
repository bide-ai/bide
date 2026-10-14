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
	"runtime"
	"slices"
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
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

// LoadSigner reads the approver private key at path as an audit.Signer. It refuses a key file that
// is not a regular file and, on Unix, one that group or others can read or write, as ssh does: the
// file's mode is read from the open file, so what is checked is what is read. It never returns or
// prints key material in an error.
func LoadSigner(path string) (audit.Signer, error) {
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
const keygenUsage = `usage: bide keygen -out <key-file> [-as <approver id>] [-alg ed25519|ecdsa-p256|ml-dsa-65|ed25519+ml-dsa-65] [-color auto|always|never]

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
	alg := fs.String("alg", string(audit.AlgEd25519), "signature scheme: ed25519, ecdsa-p256, ml-dsa-65 or ed25519+ml-dsa-65 (hybrid)")
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
	priv, err := writeKeyPair(*out, audit.Alg(*alg))
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
func writeKeyPair(path string, alg audit.Alg) (audit.Signer, error) {
	for _, p := range []string{path, path + ".pub"} {
		if _, err := os.Lstat(p); err == nil {
			return nil, fmt.Errorf("%s exists; bide keygen does not overwrite a file (remove it, or choose another -out)", p)
		}
	}
	priv, line, err := newSigner(alg)
	if err != nil {
		return nil, err
	}
	if err := writeExclusive(path, []byte(line+"\n"), 0o600); err != nil {
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

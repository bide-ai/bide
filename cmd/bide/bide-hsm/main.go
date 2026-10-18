// The bide-hsm command is bide approve for approvers whose signing keys live in an HSM, on a
// smart card or token, or in a cloud KMS. It is the same command as bide approve (the display, the
// confirmation, the signature, the record and the receipt are one implementation, in
// github.com/bide-ai/bide/cmd/bide/internal/approvecmd), with -key naming a key by reference
// instead of a key file: an RFC 7512 PKCS#11 URI, or an AWS KMS key ARN. bide-hsm pubkey prints the
// approver-keys entry that enrols the key.
//
// It is its own module, and the one binary of bide that needs cgo (the PKCS#11 library is loaded
// at run time), so the bide command stays pure Go and reproducible. Build it from source with
// CGO_ENABLED=1 go install github.com/bide-ai/bide/cmd/bide/bide-hsm@<version>. Built without cgo
// it still signs with a KMS key and reports that a PKCS#11 key needs a cgo build.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/hsmkeys"
	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/keyref"
	"github.com/bide-ai/bide/cmd/bide/internal/approvecmd"
	"github.com/bide-ai/bide/cmd/bide/internal/stopcmd"
	"github.com/bide-ai/bide/internal/auditcli"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/signer/awskms"
	"github.com/bide-ai/bide/signer/azurekv"
	"github.com/bide-ai/bide/signer/gcpkms"
)

// version is the release version, set by a build with -ldflags "-X main.version=v1.2.3".
var version = ""

// deps are what a test replaces: the KMS client and the journal stores.
type deps struct {
	kms    func(ctx context.Context, region string) (awskms.API, error)
	gcp    func(ctx context.Context) (gcpkms.API, error)
	azure  azurekv.API
	stores approvecmd.Stores
	// stopStores are the journals bide-hsm stop opens, with the event store of the stop trail.
	stopStores stopcmd.Stores
	// auditStores are the read-only stores bide-hsm audit stops reads; nil opens SQLite and Postgres.
	auditStores *auditcli.StoreOpeners
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, deps{stores: approvecmd.JournalStores(), stopStores: stopcmd.JournalStores()}))
}

// keyRefHelp describes the key references, for the help of every command that takes one.
const keyRefHelp = `A key reference is one of:

  pkcs11:token=<label>;object=<label>?module-path=<absolute path>&pin-source=file:/path
      An RFC 7512 PKCS#11 URI, for a key in an HSM or on a smart card or token. token (or
      slot-id) selects the token; object (the key's label), id (its CKA_ID as percent-encoded
      bytes) or both select the key; module-path is the vendor's PKCS#11 library, loaded and run
      by this program, so name only a library you trust. The PIN is read from
      pin-source=file:/absolute/path (on Unix the file must not be readable by others) or
      pin-source=env:NAME, or asked for on the controlling terminal with echo off by
      pin-source=prompt (never read from a pipe; refused when there is no terminal), or typed on
      the token's own PIN pad by pin-source=protected (the token must report a protected
      authentication path, else it is refused; needs -alg, as each login asks on the pad), and with
      no pin-source from $` + keyref.DefaultPINEnv + `. The PIN is never part of the
      reference: pin-value is refused, and so is every attribute not listed here (serial,
      manufacturer, module-name, ...), not ignored, since an ignored attribute would let a
      reference select a key more loosely than it reads. Percent-encode ; ? & = % and spaces in a
      value, and quote the whole reference in a shell, which treats ; and & as operators.
  awskms:arn:aws:kms:<region>:<account>:key/<id>
      An AWS KMS key by its full ARN (alias/<name> in place of key/<id> names an alias, which is
      resolved once). Credentials come from the AWS SDK's default chain (environment, shared
      config, instance or task role); the ARN's region is used.
  gcpkms:projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>/cryptoKeyVersions/<n>
      A Google Cloud KMS key version by its full resource name with the numeric version (no alias,
      no "latest": signing uses exactly that version). Credentials are Google's application default
      credentials. Only an HSM-backed version is used unless -allow-software-key (Cloud KMS has no
      HSM-backed Ed25519 key, so an Ed25519 key needs it, and approve then says so beside the key);
      -verify-attestation requires the key's Cloud HSM attestation to prove it was generated in the
      HSM and cannot be extracted.
  azurekv:https://<vault>.vault.azure.net/keys/<name>/<32-hex-version>
      An Azure Key Vault or Managed HSM key by its full identifier with the version pinned (a base
      identifier without a version would follow rotation and is refused). The key is ECDSA P-256 (Key
      Vault has no Ed25519), HSM-protected unless -allow-software-key, and not exportable.
      Credentials are the Azure SDK's default credential chain. The principal needs the
      "Key Vault Crypto User" role (Managed HSM Crypto User) on the key.

The key is Ed25519 or ECDSA P-256, the schemes HSMs and KMS sign; -alg names the scheme, and
without it the key's type is detected. A PKCS#11 key must be marked non-extractable and sensitive
by the token, or it is refused, since a key that can be read out of the token defeats keeping it
there; -allow-extractable lifts that, and bide-hsm approve then says so beside the key. The token
or KMS signs; bide-hsm never holds the private key.
`

const usage = `bide-hsm: bide approve for approvers whose keys are in an HSM or a cloud KMS

  bide-hsm approve -db <file> -run <id> -call <tool-use id> -as <approver> -key <key ref> -approver-keys <keys> [flags]
  bide-hsm approve -db <file> -list
  bide-hsm stop    -db <file> -all|-tool <name>|-flow <name>|-agent <name> -reason <text> -stop-policy <file> -as <approver> -key <key ref>
  bide-hsm audit   stops -db <file> -stop-policy <file> -pubkey <key> -sign-head -log-key <key ref> -head-out <file>
  bide-hsm pubkey  -key <key ref> [-as <approver>]
  bide-hsm csr     -key <key ref> [-out <file>] [-policy <policy file> -as <approver>]
  bide-hsm version

bide-hsm approve is bide approve, with -key naming a key by reference instead of a key file. It
reads the call from the journal, shows what it will sign, asks you to confirm, has the HSM or KMS
sign the decision, checks it against the call, records it, and writes a receipt.
bide-hsm pubkey prints the approver-keys entry that enrols a key. bide-hsm csr writes a certificate
signing request for a key, signed by the key, for a certificate-bound approver. bide-hsm stop is bide stop
for the kill switch's audited mode, with -key naming the HSM or KMS key that signs the stop, the lift or the
change of mode. bide-hsm audit stops is bide audit stops, whose -sign-head signs the stop trail's head with
-log-key naming the log key in an HSM or KMS. Each command takes -h.

` + keyRefHelp

const approveUsage = `bide-hsm approve: record a signed approval, or a rejection, of a call that waits for a decision,
with a key in an HSM or a KMS

  bide-hsm approve -db <file> -run <id> -call <tool-use id> -as <approver> -key <key ref> -approver-keys <keys> [flags]
  bide-hsm approve -postgres-env <VAR> -run <id> -call <tool-use id> -as <approver> -key <key ref> -approver-keys <keys>
  bide-hsm approve -db <file> -list

This is bide approve (see bide approve -h, and the bide command reference) with one difference:
-key is a reference to a key that stays in an HSM or a KMS, not a key file. Everything else is the
same code: what it shows, the confirmation, the checks that refuse, -approver-keys, -unverified,
-reject, -yes, -full, -list, -json, -verify-receipt, the exit status (0 on success, 1 if it
refused or failed, 2 for a usage error). Two differences follow from the key: a default receipt is
written in the working directory (there is no key file to put it beside), and the HSM or KMS is
opened (a PKCS#11 login; for KMS, one billed self-test signature) after the checks on the gate
pass and before you are asked to confirm, so that the key shown is the key that signs. The
decision itself is signed only after you confirm. A refusal on the gate (you are not an approver,
you already decided the other way) and a repeat of a decision that already counts happen before
the key is opened.

A certificate-bound approver uses -cert <chain.pem> (and -identity-policy <policy file>), exactly as with
bide approve: the key stays in the HSM or KMS, the certificate is issued for its public key (bide-hsm pubkey
prints it; the CA's request is signed by the key), and the command refuses a key that is not the key of the
certificate's leaf before it asks the HSM or KMS to sign anything. The chain is a PEM file; a certificate
object on the token is not read. The file, the policy and the validity are checked before the key is opened.

` + keyRefHelp + `
Flags:
`

func run(args []string, in io.Reader, stdout, stderr io.Writer, d deps) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "approve":
		return approvecmd.MainWith(args[1:], in, stdout, stderr, d.stores, keySource(d))
	case "stop":
		return stopcmd.MainWith(args[1:], in, stdout, stderr, d.stopStores, stopKeySource(d))
	case "audit":
		return auditCmd(args[1:], stdout, stderr, d)
	case "pubkey":
		return pubkey(args[1:], stdout, stderr, d)
	case "csr":
		return csrCmd(args[1:], stdout, stderr, d)
	case "version":
		return versionCmd(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "%sbide-hsm: unknown command %q\n", term.For(stderr, term.Auto).ErrorPrefix(), args[0])
	fmt.Fprintln(stderr, "commands: approve, stop, audit, pubkey, csr, version (bide-hsm -h)")
	return 2
}

// keyFlags are the flags that say how a key reference is opened, shared by approve and pubkey.
type keyFlags struct {
	alg              *string
	allowExtractable *bool
	allowSoftware    *bool
	verifyAttest     *bool
}

func addKeyFlags(fs *flag.FlagSet) keyFlags {
	return keyFlags{
		alg:              fs.String("alg", "", "the key's scheme: ed25519 or ecdsa-p256 (default: detect it)"),
		allowExtractable: fs.Bool("allow-extractable", false, "use a PKCS#11 key the token does not mark non-extractable and sensitive; by default such a key is refused"),
		allowSoftware:    fs.Bool("allow-software-key", false, "use a software-protected key: a Google Cloud KMS key version at protection level SOFTWARE, or an Azure Key Vault key that is not HSM-protected; by default only an HSM-backed key is used, and Cloud KMS has no HSM-backed Ed25519 key"),
		verifyAttest:     fs.Bool("verify-attestation", false, "for a Google Cloud KMS key version in an HSM, require its Cloud HSM attestation to prove the key was generated there and cannot be extracted"),
	}
}

// open parses ref and opens its signer.
func (k keyFlags) open(ref string, d deps) (audit.Signer, error) {
	r, err := keyref.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("key reference: %w", err)
	}
	if *k.allowExtractable && r.Kind != keyref.KindPKCS11 {
		return nil, errors.New("-allow-extractable applies to a PKCS#11 key only: a KMS key cannot be exported")
	}
	if *k.allowSoftware && r.Kind != keyref.KindGCPKMS && r.Kind != keyref.KindAzureKV {
		return nil, errors.New("-allow-software-key applies to a gcpkms: or azurekv: key only")
	}
	if *k.verifyAttest && r.Kind != keyref.KindGCPKMS {
		return nil, errors.New("-verify-attestation applies to a gcpkms: key only")
	}
	if *k.allowSoftware && *k.verifyAttest {
		return nil, errors.New("-allow-software-key and -verify-attestation do not go together: a software key has no HSM attestation")
	}
	return hsmkeys.Open(context.Background(), r, hsmkeys.Options{AllowExtractable: *k.allowExtractable, Alg: audit.Alg(*k.alg), KMSClient: d.kms,
		AllowSoftwareKey: *k.allowSoftware, VerifyAttestation: *k.verifyAttest, GCPClient: d.gcp, AzureClient: d.azure})
}

// keySource is the key source of bide-hsm approve.
func keySource(d deps) approvecmd.KeySource {
	var kf keyFlags
	return approvecmd.KeySource{
		Prog:         "bide-hsm approve",
		Usage:        approveUsage,
		KeyHelp:      "reference to your key in an HSM or KMS: a pkcs11: URI (RFC 7512; quote it), awskms:<key ARN>, gcpkms:<key version> or azurekv:<key identifier>; not for a single-approver gate",
		KeyNoun:      "your key reference, a pkcs11: URI, awskms:<key ARN>, gcpkms:<key version> or azurekv:<key identifier>",
		EnrolHelp:    "bide-hsm pubkey -as prints an entry",
		EnrolCmd:     "bide-hsm pubkey -as",
		ReceiptWhere: "in the working directory",
		NoEchoArgs:   true,
		ReceiptHelp:  "where to write the decision receipt (default: in the working directory, named <run>-<call>-<approver>-<time>.receipt.json); mode 0600, never overwritten",
		ListHint:     "Replace '<key-ref>' with the reference to your key (a pkcs11: URI or awskms:<key ARN>; quote it) and '<keys-file>' with the file of enrolled approver public keys. bide-hsm pubkey prints the entry that enrols a key. bide-hsm approve shows what it signs and asks first.",
		Placeholder:  "<key-ref>",
		Flags:        func(fs *flag.FlagSet) { kf = addKeyFlags(fs) },
		Load:         func(ref string) (audit.Signer, error) { return kf.open(ref, d) },
		ReceiptDir:   func(string) string { return "." },
	}
}

// stopKeySource is the key source of bide-hsm stop: the same keys as bide-hsm approve, with the words of the
// stop command.
func stopKeySource(d deps) approvecmd.KeySource {
	src := keySource(d)
	src.Prog = "bide-hsm stop"
	src.KeyHelp = "audited mode: reference to your key in an HSM or KMS: a pkcs11: URI (RFC 7512; quote it), awskms:<key ARN>, gcpkms:<key version> or azurekv:<key identifier>"
	return src
}

const pubkeyUsage = `usage: bide-hsm pubkey -key <key ref> [-as <approver id>] [-alg ed25519|ecdsa-p256] [-allow-extractable]

Prints the public key of a key in an HSM or a KMS, in the form approval policies and verifiers read
(<scheme>:<hex>), and with -as the entry for the JSON object bide approve -approver-keys and bide
view -approver-keys read: {"ops": "<scheme>:<hex>"}. Enroll that key in the application that runs
the gated agent, and in the file approvers pass as -approver-keys.

It opens the key as bide-hsm approve does, so it needs the PIN (a PKCS#11 key) or the credentials
(a KMS key), checks that the key can sign, and refuses what approve refuses. For a KMS key that
check is one signature, billed as one KMS request. The private key is never read out.

` + keyRefHelp + `
Flags:
`

func pubkey(args []string, stdout, stderr io.Writer, d deps) int {
	fs := flag.NewFlagSet("bide-hsm pubkey", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), pubkeyUsage)
		fs.PrintDefaults()
	}
	key := fs.String("key", "", "reference to the key: a pkcs11: URI (quote it), awskms:<key ARN>, gcpkms:<key version> or azurekv:<key identifier>")
	as := fs.String("as", "", "the approver id the key is for: prints the approver-keys entry for it")
	kf := addKeyFlags(fs)
	color := term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	es := term.For(stderr, *color)
	bad := func(msg string) int {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide-hsm pubkey:", msg)
		fs.Usage()
		return 2
	}
	switch {
	case fs.NArg() > 0:
		// Not echoed: a key reference given without -key can hold a secret.
		return bad("unexpected argument (the command takes flags only; the key goes after -key)")
	case *key == "":
		return bad("no -key given")
	}
	if *as != "" {
		if err := (agent.ApprovalPolicy{Need: 1, Approvers: []string{*as}}).Validate(); err != nil {
			return bad("-as: " + err.Error())
		}
	}
	sg, err := kf.open(*key, d)
	if err != nil {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide-hsm pubkey:", err)
		return 1
	}
	if cl, ok := sg.(io.Closer); ok {
		defer cl.Close()
	}
	st := term.For(stdout, *color)
	pub := audit.FormatPublicKey(sg.Alg(), sg.PublicKey())
	fmt.Fprintf(stdout, "public key  %s\n", st.Bold(pub))
	if w, ok := sg.(interface{ Where() string }); ok {
		fmt.Fprintf(stdout, "held in     %s\n", w.Where())
	}
	if n, ok := sg.(interface{ Notice() string }); ok && n.Notice() != "" {
		fmt.Fprintf(stdout, "notice      %s\n", st.Warn(n.Notice()))
	}
	if *as != "" {
		fmt.Fprintf(stdout, "approver-keys entry  %s\n", st.Bold(fmt.Sprintf("{%q: %q}", *as, pub)))
	}
	return 0
}

func versionCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		fmt.Fprintln(stdout, "usage: bide-hsm version")
		return 0
	}
	if len(args) > 0 {
		fmt.Fprintf(stderr, "%sbide-hsm version: takes no arguments\n", term.For(stderr, term.Auto).ErrorPrefix())
		return 2
	}
	v, cgo := version, "unknown"
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v == "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
		for _, s := range bi.Settings {
			if s.Key == "CGO_ENABLED" {
				cgo = s.Value
			}
		}
	}
	if v == "" {
		v = "(built from source)"
	}
	fmt.Fprintf(stdout, "bide-hsm %s\n", v)
	switch cgo {
	case "1":
		fmt.Fprintln(stdout, "PKCS#11: supported (built with cgo)")
	case "0":
		fmt.Fprintln(stdout, "PKCS#11: not supported (built without cgo); AWS KMS keys work")
	default:
		fmt.Fprintln(stdout, "PKCS#11: unknown (no build information)")
	}
	return 0
}

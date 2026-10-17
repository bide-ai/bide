package main

import (
	"encoding/asn1"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/internal/csr"
	"github.com/bide-ai/bide/internal/policyfile"
	"github.com/bide-ai/bide/internal/term"
)

const csrUsage = `usage: bide-hsm csr -key <key ref> [-out <file>] [-force] [-policy <policy file> -as <approver id>] [-san-email <address> | -san-uri <uri>] [-eku <usage>[,<usage>...]] [key flags]

Writes a PKCS#10 certificate signing request (PEM) for a key in an HSM or a KMS, signed by that key
through the same key sources as bide-hsm approve, so no vendor tooling is needed. It is the request a
CA or identity provider turns into the certificate bide-hsm approve -cert uses. The key signs the
request once and is never read out; for a KMS key that is one Sign call (billed) beside the one
bide-hsm pubkey makes. The request holds the key's public key; before anything is written, it checks
the request's signature and that its public key is the key source's, so a faulty token or a key
replaced behind the reference writes nothing.

The key is Ed25519 (signature algorithm Ed25519, RFC 8410) or ECDSA P-256 (ecdsa-with-SHA256), the
schemes bide-hsm keys have; there is no RSA request. Ed25519 is available where the key source has it
(PKCS#11, AWS KMS, and Google Cloud KMS with -allow-software-key; not Azure Key Vault).

The request is written to -out, or to standard output without -out or with -out -. A file is never
overwritten: -out names a file that does not exist, unless -force replaces a regular file (a
directory or a symbolic link is refused). Everything else goes to the other stream (to standard
error when the request is on standard output), so the output is only the request.

By default the request names nothing (an empty subject, no extensions): the CA sets the name, the
usage and the policy. A CA may ignore or replace what a request asks for, so the certificate it issues
is what counts, and bide-hsm approve -cert checks that before anything is signed. To ask for names:

  -san-email <address>   one rfc822Name subject alternative name (a bare address)
  -san-uri <uri>         one URI subject alternative name (absolute, with a host, no fragment)
  -eku <usage>,...       extended key usages: clientAuth, serverAuth, emailProtection, codeSigning,
                         timeStamping, ocspSigning, or a dotted OID

-policy <file> with -as <approver id> reads the certificate policy file bide-hsm approve takes as
-identity-policy and asks for what the policy's gate looks for: as the one SAN, the approver's
identifier in the policy (an email address or a person URI, in the policy's canonical form), and the
policy's extended key usage. Then -san-email, -san-uri and -eku are refused: the policy decides. It
cannot ask for the policy OID (a CA asserts it, after the bank's sign-on and checks), and says on
standard error what the CA must issue for the gate to accept the certificate: the one SAN, the
usage, the policy OID and the longest lifetime. It reads the policy only; it checks no certificate.

Check a request before sending it: openssl req -in <file> -noout -verify -text.

Exit status is 0 on success, 1 if the key could not be used, the request failed its checks or the
output was refused, 2 for a usage error.

` + keyRefHelp + `
Flags:
`

// ekuNames are the extended key usages -eku names without a dotted OID.
var ekuNames = map[string]asn1.ObjectIdentifier{
	"serverauth":      {1, 3, 6, 1, 5, 5, 7, 3, 1},
	"clientauth":      {1, 3, 6, 1, 5, 5, 7, 3, 2},
	"codesigning":     {1, 3, 6, 1, 5, 5, 7, 3, 3},
	"emailprotection": {1, 3, 6, 1, 5, 5, 7, 3, 4},
	"timestamping":    {1, 3, 6, 1, 5, 5, 7, 3, 8},
	"ocspsigning":     {1, 3, 6, 1, 5, 5, 7, 3, 9},
}

// parseEKUs reads the -eku list.
func parseEKUs(s string) ([]asn1.ObjectIdentifier, error) {
	var out []asn1.ObjectIdentifier
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if o, ok := ekuNames[strings.ToLower(f)]; ok {
			out = append(out, o)
			continue
		}
		var o asn1.ObjectIdentifier
		for _, arc := range strings.Split(f, ".") {
			n, err := strconv.Atoi(arc)
			if err != nil || n < 0 || arc != strconv.Itoa(n) {
				return nil, fmt.Errorf("%q is neither a usage name nor a dotted OID", f)
			}
			o = append(o, n)
		}
		if len(o) < 3 {
			return nil, fmt.Errorf("%q is not a usage name or a dotted OID", f)
		}
		out = append(out, o)
	}
	return out, nil
}

func csrCmd(args []string, stdout, stderr io.Writer, d deps) int {
	fs := flag.NewFlagSet("bide-hsm csr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), csrUsage)
		fs.PrintDefaults()
	}
	key := fs.String("key", "", "reference to the key: a pkcs11: URI (quote it), awskms:<key ARN>, gcpkms:<key version> or azurekv:<key identifier>")
	out := fs.String("out", "", "where to write the request (default, or -: standard output); a file that exists is not overwritten without -force")
	force := fs.Bool("force", false, "replace -out if it is a regular file that exists")
	policy := fs.String("policy", "", "the certificate policy file (as bide-hsm approve -identity-policy): ask for the identifier and usage it expects for -as; needs -as")
	as := fs.String("as", "", "the approver id in the -policy file whose identifier the request asks for")
	sanEmail := fs.String("san-email", "", "ask for this email address as the one subject alternative name")
	sanURI := fs.String("san-uri", "", "ask for this URI as the one subject alternative name")
	eku := fs.String("eku", "", "ask for these extended key usages, comma separated: clientAuth, serverAuth, emailProtection, codeSigning, timeStamping, ocspSigning or a dotted OID")
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
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide-hsm csr:", msg)
		fs.Usage()
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide-hsm csr:", err)
		return 1
	}
	var names csr.Names
	var ekus []asn1.ObjectIdentifier
	switch {
	case fs.NArg() > 0:
		// Not echoed: a key reference given without -key can hold a secret.
		return bad("unexpected argument (the command takes flags only; the key goes after -key)")
	case *key == "":
		return bad("no -key given")
	case *policy != "" && *as == "":
		return bad("-policy needs -as, the approver id whose identifier the request asks for")
	case *as != "" && *policy == "":
		return bad("-as goes with -policy, the file that holds the approver's identifier")
	case *policy != "" && (*sanEmail != "" || *sanURI != "" || *eku != ""):
		return bad("-policy decides the names: do not give -san-email, -san-uri or -eku with it")
	case *sanEmail != "" && *sanURI != "":
		return bad("a request asks for one subject alternative name: -san-email or -san-uri, not both")
	}
	if *eku != "" {
		var err error
		if ekus, err = parseEKUs(*eku); err != nil {
			return bad("-eku: " + err.Error())
		}
	}
	names = csr.Names{Email: *sanEmail, URI: *sanURI, EKU: ekus}
	if *sanEmail != "" || *sanURI != "" || ekus != nil {
		// Checked before a key is opened, so a bad name costs no PIN and no billed signature.
		if err := csr.Check(names); err != nil {
			return bad(err.Error())
		}
	}

	var req *requirements
	if *policy != "" {
		c, err := policyfile.Read(*policy)
		if err != nil {
			return fail(err)
		}
		id, ok := c.Identity(*as)
		if !ok {
			return fail(fmt.Errorf("approver %q is not in the policy file (it names %s)", *as, strings.Join(c.ApproverIDs(), ", ")))
		}
		switch c.IDKind() {
		case audit.IDEmail:
			names.Email = id
		default:
			names.URI = id
		}
		names.EKU = []asn1.ObjectIdentifier{c.EKU()}
		req = &requirements{kind: c.IDKind(), id: id, eku: c.EKU(), emailEKU: c.AllowsEmailProtectionEKU(), policy: c.PolicyOID().String(), life: c.MaxLeafLifetime().String()}
		if err := csr.Check(names); err != nil {
			return fail(fmt.Errorf("the policy's identifier for %q cannot be put in a request: %w", *as, err))
		}
	}

	toStdout := *out == "" || *out == "-"
	if !toStdout {
		if err := checkOutput(*out, *force); err != nil {
			return fail(err)
		}
	}
	sg, err := kf.open(*key, d)
	if err != nil {
		return fail(err)
	}
	if cl, ok := sg.(io.Closer); ok {
		defer cl.Close()
	}
	pem, err := csr.Build(sg, names)
	if err != nil {
		return fail(err)
	}

	info, st := stdout, term.For(stdout, *color)
	if toStdout {
		info, st = stderr, es
	}
	if toStdout {
		if _, err := stdout.Write(pem); err != nil {
			return fail(fmt.Errorf("write the request: %w", err))
		}
	} else {
		if err := writeOutput(*out, pem, *force); err != nil {
			return fail(err)
		}
		fmt.Fprintf(info, "%swrote certificate signing request  %s\n", st.Pass(), st.Path(*out))
	}
	fmt.Fprintf(info, "public key  %s\n", st.Bold(audit.FormatPublicKey(sg.Alg(), sg.PublicKey())))
	if w, ok := sg.(interface{ Where() string }); ok {
		fmt.Fprintf(info, "held in     %s\n", w.Where())
	}
	if n, ok := sg.(interface{ Notice() string }); ok && n.Notice() != "" {
		fmt.Fprintf(info, "notice      %s\n", st.Warn(n.Notice()))
	}
	asked := "nothing: an empty subject and no extensions, so the CA sets the name and the usage"
	switch {
	case names.Email != "":
		asked = "the email " + names.Email
	case names.URI != "":
		asked = "the URI " + names.URI
	}
	if len(names.EKU) > 0 {
		if names.Email == "" && names.URI == "" {
			asked = "no name"
		}
		asked += ", usage " + ekuText(names.EKU)
	}
	fmt.Fprintf(info, "asks for    %s\n", asked)
	if req != nil {
		fmt.Fprintf(stderr, "the CA must issue a certificate for this key whose one subject alternative name is the %s %q, with extended key usage %s",
			req.kind, req.id, req.eku)
		if req.emailEKU {
			fmt.Fprint(stderr, " (or emailProtection, 1.3.6.1.5.5.7.3.4, which this policy also accepts)")
		}
		fmt.Fprintf(stderr, ", the certificate policy %s, and a lifetime (notAfter minus notBefore) of at most %s. The request cannot ask for the policy; the CA asserts it. bide-hsm approve -cert checks the issued certificate against the policy before it signs.\n", req.policy, req.life)
	}
	return 0
}

// requirements are what the policy's gate looks for in the certificate.
type requirements struct {
	kind     audit.IDKind
	id       string
	eku      asn1.ObjectIdentifier
	emailEKU bool
	policy   string
	life     string
}

func ekuText(eku []asn1.ObjectIdentifier) string {
	var s []string
	for _, o := range eku {
		s = append(s, o.String())
	}
	return strings.Join(s, ",")
}

// checkOutput refuses an -out the request must not be written to, before the key is opened.
func checkOutput(path string, force bool) error {
	fi, err := os.Lstat(path)
	switch {
	case err == nil && !force:
		return fmt.Errorf("%s exists; bide-hsm csr does not overwrite a file (remove it, choose another -out, or give -force)", path)
	case err == nil && !fi.Mode().IsRegular():
		return fmt.Errorf("%s exists and is not a regular file; -force replaces only a regular file", path)
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("%s: %w", path, unwrapPathError(err))
	}
	return nil
}

func unwrapPathError(err error) error {
	if pe, ok := errors.AsType[*os.PathError](err); ok {
		return pe.Err
	}
	return err
}

// writeOutput writes the request: a new file exclusively, or with force a temporary file renamed over
// the existing regular file, so that a failure leaves the old file or no file, never half a request.
func writeOutput(path string, data []byte, force bool) error {
	if err := checkOutput(path, force); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		tmp, err := os.CreateTemp(filepath.Dir(path), ".csr-*")
		if err != nil {
			return fmt.Errorf("write the request: %w", unwrapPathError(err))
		}
		defer os.Remove(tmp.Name())
		_, werr := tmp.Write(data)
		if cerr := tmp.Close(); werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = os.Chmod(tmp.Name(), 0o644)
		}
		if werr == nil {
			werr = os.Rename(tmp.Name(), path)
		}
		if werr != nil {
			return fmt.Errorf("write the request: %w", unwrapPathError(werr))
		}
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, unwrapPathError(err))
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("write %s: %w", path, unwrapPathError(err))
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("write %s: %w", path, unwrapPathError(err))
	}
	return nil
}

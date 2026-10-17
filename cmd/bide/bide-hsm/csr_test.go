package main

import (
	"bytes"
	"crypto/x509"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/fakekms"
	"github.com/bide-ai/bide/internal/testca"
	"github.com/bide-ai/bide/signer/pkcs11/pkcs11test"
)

// request parses the one CERTIFICATE REQUEST block of b.
func request(t *testing.T, b []byte) *x509.CertificateRequest {
	t.Helper()
	blk, rest := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("not one CERTIFICATE REQUEST block: %q", b)
	}
	req, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.CheckSignature(); err != nil {
		t.Fatalf("proof of possession: %v", err)
	}
	return req
}

// sameKey fails unless the request is for the key whose `bide-hsm pubkey` text is pub.
func sameKey(t *testing.T, req *x509.CertificateRequest, pub string) {
	t.Helper()
	want, err := x509.MarshalPKIXPublicKey(publicKeyOf(t, pub))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(req.RawSubjectPublicKeyInfo, want) {
		t.Fatalf("the request is not for the key %s", pub)
	}
}

// opensslVerifies has openssl verify the request and read the same public key. It skips when openssl is
// absent unless BIDE_REQUIRE_OPENSSL is set, when that fails: CI sets it, so the check cannot silently
// not run there. The Go checks always run.
func opensslVerifies(t *testing.T, path string, req *x509.CertificateRequest) {
	t.Helper()
	ossl, err := exec.LookPath("openssl")
	if err != nil {
		if os.Getenv("BIDE_REQUIRE_OPENSSL") != "" {
			t.Fatalf("BIDE_REQUIRE_OPENSSL is set and openssl is not on PATH: %v", err)
		}
		t.Log("openssl is not on PATH: the request was checked by crypto/x509 only")
		return
	}
	if out, err := exec.Command(ossl, "req", "-in", path, "-noout", "-verify").CombinedOutput(); err != nil {
		t.Fatalf("openssl req -verify: %v\n%s", err, out)
	}
	pub, err := exec.Command(ossl, "req", "-in", path, "-pubkey", "-noout").Output()
	if err != nil {
		t.Fatal(err)
	}
	if blk, _ := pem.Decode(pub); blk == nil || !bytes.Equal(blk.Bytes, req.RawSubjectPublicKeyInfo) {
		t.Fatal("openssl reads another public key than the request's")
	}
}

// A request is made for a key in a token and a key in a KMS, for each scheme: it verifies (openssl too),
// holds the key `pubkey` prints, names nothing by default, and the file is the only output of -out.
func TestCSRFromEachSource(t *testing.T) {
	tok := pkcs11test.Require(t)
	for _, tc := range []struct {
		name, label, arn string
		add              func(*fakekms.KMS, string)
	}{
		{"ed25519", edLabel, kmsEd, (*fakekms.KMS).AddEd25519},
		{"ecdsa-p256", p256Label, kmsP256, (*fakekms.KMS).AddP256},
	} {
		kms := fakekms.New()
		tc.add(kms, tc.arn)
		pin := "file:" + pinFile(t, tok)
		for kind, ref := range map[string]string{"token": uri(tok, tc.label, pin), "kms": "awskms:" + tc.arn} {
			t.Run(tc.name+"/"+kind, func(t *testing.T) {
				pub := enrolEntry(t, hsm(t, kms, "", "pubkey", "-as", "ops", "-key", ref), "ops")
				out := filepath.Join(t.TempDir(), "ops.csr.pem")
				before := kms.Signs
				r := hsm(t, kms, "", "csr", "-key", ref, "-out", out)
				if r.code != 0 {
					t.Fatalf("%d\n%s", r.code, r.all())
				}
				if kind == "kms" && kms.Signs-before != 2 {
					t.Fatalf("%d KMS signatures for the request (and the open-time self-test), want 2 in all", kms.Signs-before)
				}
				if strings.Contains(r.out, "CERTIFICATE REQUEST") || !strings.Contains(r.out+r.errOut, out) || !strings.Contains(r.all(), pub) {
					t.Fatalf("output should name the file and the key, not hold the request:\n%s", r.all())
				}
				b, err := os.ReadFile(out)
				if err != nil {
					t.Fatal(err)
				}
				req := request(t, b)
				sameKey(t, req, pub)
				if len(req.RawSubject) > 2 || len(req.Extensions) != 0 {
					t.Fatalf("the request names something: %x %v", req.RawSubject, req.Extensions)
				}
				opensslVerifies(t, out, req)

				// -out - and no -out: the request is all of stdout.
				for _, args := range [][]string{{"csr", "-key", ref}, {"csr", "-key", ref, "-out", "-"}} {
					r := hsm(t, kms, "", args...)
					if r.code != 0 {
						t.Fatalf("%v: %d\n%s", args, r.code, r.all())
					}
					sameKey(t, request(t, []byte(r.out)), pub)
				}
			})
		}
	}
}

// The file is never overwritten without -force, and the key is not opened (no PIN, no billed
// signature) when the output is refused; -force replaces a regular file with the new request.
func TestCSROverwrite(t *testing.T) {
	kms := fakekms.New()
	kms.AddP256(kmsP256)
	dir := t.TempDir()
	out := filepath.Join(dir, "r.pem")
	if err := os.WriteFile(out, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := hsm(t, kms, "", "csr", "-key", "awskms:"+kmsP256, "-out", out)
	if r.code != 1 || !strings.Contains(r.errOut, "exists") || !strings.Contains(r.errOut, "-force") {
		t.Fatalf("%d\n%s", r.code, r.all())
	}
	if b, _ := os.ReadFile(out); string(b) != "keep me" {
		t.Fatalf("the file was overwritten: %q", b)
	}
	if kms.Signs != 0 {
		t.Fatalf("the key was used (%d signatures) although the output was refused", kms.Signs)
	}
	if r := hsm(t, kms, "", "csr", "-key", "awskms:"+kmsP256, "-out", out, "-force"); r.code != 0 {
		t.Fatalf("-force: %d\n%s", r.code, r.all())
	}
	request(t, mustRead(t, out))
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("-force left %d files in the directory, want only the request", len(ents))
	}
	// -force does not follow a directory or replace a non-regular file.
	if r := hsm(t, kms, "", "csr", "-key", "awskms:"+kmsP256, "-out", dir, "-force"); r.code != 1 {
		t.Fatalf("-out a directory: %d\n%s", r.code, r.all())
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(out, link); err != nil {
			t.Fatal(err)
		}
		before := mustRead(t, out)
		if r := hsm(t, kms, "", "csr", "-key", "awskms:"+kmsP256, "-out", link, "-force"); r.code != 1 || !bytes.Equal(before, mustRead(t, out)) {
			t.Fatalf("-out a symlink: %d\n%s", r.code, r.all())
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// policyFile writes a certificate policy file for the kind (person-uri or email) with ops' identity,
// next to a root the file names.
func policyFile(t *testing.T, kind string, allowEmailEKU bool) string {
	t.Helper()
	root := testca.NewRoot(t, "root", testca.Opts{})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "root.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	ident := testca.Person(1)
	if kind == "email" {
		ident = "Ops@Bank.Example"
	}
	pj, _ := json.Marshal(map[string]any{
		"anchors": []string{"root.pem"}, "identity": map[string]string{"ops": ident}, "id_kind": kind,
		"eku": testca.EKU.String(), "allow_email_protection_eku": allowEmailEKU, "policy_oid": testca.PolicyOID.String(),
		"canonical": map[string]string{"host": "idp.bank.example"}, "max_leaf_lifetime": "8h",
	})
	p := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(p, pj, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// san returns the request's one SAN as text.
func san(t *testing.T, req *x509.CertificateRequest) string {
	t.Helper()
	switch {
	case len(req.EmailAddresses) == 1 && len(req.URIs) == 0:
		return req.EmailAddresses[0]
	case len(req.URIs) == 1 && len(req.EmailAddresses) == 0:
		return req.URIs[0].String()
	}
	t.Fatalf("SAN is email %v uri %v, want exactly one", req.EmailAddresses, req.URIs)
	return ""
}

func ekuOf(t *testing.T, req *x509.CertificateRequest) []string {
	t.Helper()
	var got []string
	for _, e := range req.Extensions {
		if e.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 37}) {
			var oids []asn1.ObjectIdentifier
			if _, err := asn1.Unmarshal(e.Value, &oids); err != nil {
				t.Fatal(err)
			}
			for _, o := range oids {
				got = append(got, o.String())
			}
		}
	}
	return got
}

// With -policy and -as the request asks for what the gate looks for: the policy's identifier for the
// approver (in the policy's canonical form) as the one SAN, and the policy's EKU. The request does not
// ask for the policy OID, which a CA asserts; stderr says what the CA must issue, and stdout stays
// the request.
func TestCSRFromPolicy(t *testing.T) {
	kms := fakekms.New()
	kms.AddP256(kmsP256)
	ref := "awskms:" + kmsP256
	for kind, want := range map[string]string{"person-uri": testca.Person(1), "email": "ops@bank.example"} {
		t.Run(kind, func(t *testing.T) {
			p := policyFile(t, kind, false)
			r := hsm(t, kms, "", "csr", "-key", ref, "-policy", p, "-as", "ops")
			if r.code != 0 {
				t.Fatalf("%d\n%s", r.code, r.all())
			}
			req := request(t, []byte(r.out))
			if got := san(t, req); got != want {
				t.Fatalf("SAN = %q, want %q", got, want)
			}
			if got := ekuOf(t, req); len(got) != 1 || got[0] != testca.EKU.String() {
				t.Fatalf("EKU = %v, want %v", got, testca.EKU)
			}
			for _, w := range []string{"the CA must issue", want, testca.EKU.String(), testca.PolicyOID.String(), "8h"} {
				if !strings.Contains(r.errOut, w) {
					t.Errorf("stderr does not say %q:\n%s", w, r.errOut)
				}
			}
			if strings.Contains(r.out, "the CA must issue") {
				t.Fatal("the CA's requirements are on stdout")
			}
		})
	}
	// A policy that also accepts emailProtection says so; the request still asks for the policy's EKU.
	r := hsm(t, kms, "", "csr", "-key", ref, "-policy", policyFile(t, "email", true), "-as", "ops")
	if r.code != 0 || !strings.Contains(r.errOut, "emailProtection") {
		t.Fatalf("%d\n%s", r.code, r.all())
	}
}

func TestCSRFlagRefusals(t *testing.T) {
	kms := fakekms.New()
	kms.AddP256(kmsP256)
	ref := "awskms:" + kmsP256
	p := policyFile(t, "person-uri", false)
	for name, tc := range map[string]struct {
		args []string
		code int
		want string
	}{
		"no key":                   {[]string{"csr"}, 2, "no -key"},
		"stray argument":           {[]string{"csr", "-key", ref, "pkcs11:token=T;object=k?pin-value=hunter2secret"}, 2, "unexpected argument"},
		"both sans":                {[]string{"csr", "-key", ref, "-san-email", "a@b.example", "-san-uri", "https://b.example/a"}, 2, "not both"},
		"bad email":                {[]string{"csr", "-key", ref, "-san-email", "Ann <a@b.example>"}, 2, "email"},
		"bad uri":                  {[]string{"csr", "-key", ref, "-san-uri", "people/1"}, 2, "URI"},
		"bad eku":                  {[]string{"csr", "-key", ref, "-eku", "nonsense"}, 2, "-eku"},
		"policy without as":        {[]string{"csr", "-key", ref, "-policy", p}, 2, "-as"},
		"as without policy":        {[]string{"csr", "-key", ref, "-as", "ops"}, 2, "-policy"},
		"policy with san":          {[]string{"csr", "-key", ref, "-policy", p, "-as", "ops", "-san-email", "a@b.example"}, 2, "-policy"},
		"policy with eku":          {[]string{"csr", "-key", ref, "-policy", p, "-as", "ops", "-eku", "clientAuth"}, 2, "-policy"},
		"approver not in policy":   {[]string{"csr", "-key", ref, "-policy", p, "-as", "nobody"}, 1, "nobody"},
		"missing policy file":      {[]string{"csr", "-key", ref, "-policy", filepath.Join(t.TempDir(), "none.json"), "-as", "ops"}, 1, "policy"},
		"allow-extractable on kms": {[]string{"csr", "-key", ref, "-allow-extractable"}, 1, "PKCS#11"},
		"alg mismatch":             {[]string{"csr", "-key", ref, "-alg", "ed25519"}, 1, ""},
		"rsa":                      {[]string{"csr", "-key", ref, "-alg", "rsa"}, 1, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := hsm(t, kms, "", tc.args...)
			if r.code != tc.code || !strings.Contains(r.errOut, tc.want) || strings.Contains(r.all(), "hunter2secret") || r.out != "" {
				t.Fatalf("exit %d (want %d), want %q in stderr, stdout must be empty:\n%s", r.code, tc.code, tc.want, r.all())
			}
		})
	}
	if kms.Signs != 0 {
		t.Fatalf("%d KMS signatures for requests that were refused", kms.Signs)
	}
}

// The names asked for with -san-email, -san-uri and -eku (a name or a dotted OID) are in the request.
func TestCSRNamesFromFlags(t *testing.T) {
	kms := fakekms.New()
	kms.AddEd25519(kmsEd)
	r := hsm(t, kms, "", "csr", "-key", "awskms:"+kmsEd, "-san-email", "ops@bank.example", "-eku", "clientAuth,emailProtection,1.2.3.4.5")
	if r.code != 0 {
		t.Fatalf("%d\n%s", r.code, r.all())
	}
	req := request(t, []byte(r.out))
	if san(t, req) != "ops@bank.example" {
		t.Fatalf("SAN %q", san(t, req))
	}
	if got := strings.Join(ekuOf(t, req), " "); got != "1.3.6.1.5.5.7.3.2 1.3.6.1.5.5.7.3.4 1.2.3.4.5" {
		t.Fatalf("EKU %s", got)
	}
	r = hsm(t, kms, "", "csr", "-key", "awskms:"+kmsEd, "-san-uri", "https://idp.bank.example/people/7")
	if r.code != 0 || san(t, request(t, []byte(r.out))) != "https://idp.bank.example/people/7" {
		t.Fatalf("%d\n%s", r.code, r.all())
	}
}

// A key source that fails yields no request and no file, and nothing in the message holds the PIN: a
// wrong PIN, a key that is not there, a KMS key in another region.
func TestCSRKeySourceFailures(t *testing.T) {
	tok := pkcs11test.Require(t)
	const wrongPIN = "97531357"
	bad := filepath.Join(t.TempDir(), "bad.pin")
	if err := os.WriteFile(bad, []byte(wrongPIN+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kms := fakekms.New()
	kms.AddP256(kmsP256)
	for name, ref := range map[string]string{
		"wrong pin":     uri(tok, p256Label, "file:"+bad),
		"no such key":   uri(tok, "no-such-label", "file:"+pinFile(t, tok)),
		"no such token": "pkcs11:token=nope;object=x?module-path=" + tok.Module + "&pin-source=file:" + pinFile(t, tok),
		"other region":  "awskms:" + strings.Replace(kmsP256, "us-east-1", "eu-west-1", 1),
		"no such kms":   "awskms:arn:aws:kms:us-east-1:111122223333:key/0000abcd-12ab-34cd-56ef-1234567890ab",
	} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "r.pem")
			r := hsm(t, kms, "", "csr", "-key", ref, "-out", out)
			if r.code != 1 || r.out != "" {
				t.Fatalf("exit %d, stdout %q:\n%s", r.code, r.out, r.errOut)
			}
			if _, err := os.Stat(out); err == nil {
				t.Fatal("a failed run left a file")
			}
			for _, secret := range []string{wrongPIN, tok.PIN} {
				if strings.Contains(r.all(), secret) {
					t.Fatalf("the output holds a PIN:\n%s", r.all())
				}
			}
		})
	}
}

// A token key the token does not mark non-extractable is refused as for approve and pubkey, and
// -allow-extractable lifts that.
func TestCSRExtractableKey(t *testing.T) {
	tok := pkcs11test.Require(t)
	ref := uri(tok, p256Extra, "file:"+pinFile(t, tok))
	if r := hsm(t, nil, "", "csr", "-key", ref); r.code != 1 || r.out != "" || !strings.Contains(r.errOut, "xtractable") {
		t.Fatalf("%d\n%s", r.code, r.all())
	}
	r := hsm(t, nil, "", "csr", "-key", ref, "-allow-extractable")
	if r.code != 0 {
		t.Fatalf("%d\n%s", r.code, r.all())
	}
	request(t, []byte(r.out))
	if !strings.Contains(r.errOut, "xtractable") {
		t.Fatalf("the notice is not shown:\n%s", r.errOut)
	}
}

// The usage and the command list name csr.
func TestCSRHelp(t *testing.T) {
	for _, h := range [][]string{{"csr", "-h"}, {"help"}} {
		if got := help(t, h...); !strings.Contains(got, "bide-hsm csr") {
			t.Errorf("%v does not mention bide-hsm csr", h)
		}
	}
	if r := hsm(t, nil, "", "nonsense"); !strings.Contains(r.errOut, "csr") {
		t.Errorf("the command list omits csr: %s", r.errOut)
	}
}

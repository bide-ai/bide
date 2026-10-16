package approvecmd

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKeygenCSR: bide keygen -csr writes a PKCS#10 request that verifies (proof of possession), holds
// the new key's public key and no names, for each scheme a certificate-bound approver signs with; it
// refuses other schemes and never overwrites a file.
func TestKeygenCSR(t *testing.T) {
	for _, alg := range []string{"ed25519", "ecdsa-p256"} {
		t.Run(alg, func(t *testing.T) {
			dir := t.TempDir()
			key, csrPath := filepath.Join(dir, "k"), filepath.Join(dir, "k.csr.pem")
			var out, errb bytes.Buffer
			if code := KeygenMain([]string{"-out", key, "-alg", alg, "-csr", csrPath}, &out, &errb); code != 0 {
				t.Fatalf("exit %d: %s", code, errb.String())
			}
			contains(t, "output", out.String(), "certificate signing request")
			b, err := os.ReadFile(csrPath)
			if err != nil {
				t.Fatal(err)
			}
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
			if !signerKey(t, key).Public().(interface{ Equal(crypto.PublicKey) bool }).Equal(req.PublicKey) {
				t.Fatal("the request's public key is not the key file's")
			}
			if len(req.RawSubject) > 2 || req.DNSNames != nil || req.EmailAddresses != nil || req.IPAddresses != nil || req.URIs != nil || len(req.Extensions) != 0 || len(req.Attributes) != 0 { //nolint:staticcheck
				t.Fatalf("the request names something: subject %x, %+v", req.RawSubject, req.Extensions)
			}
			if bytes.Contains(b, []byte(strings.TrimSpace(mustRead(t, key)))) {
				t.Fatal("the request holds the private key")
			}
			// No overwrite: the request file, then a fresh key with the request path taken.
			before := b
			if code := KeygenMain([]string{"-out", filepath.Join(dir, "k2"), "-alg", alg, "-csr", csrPath}, io.Discard, &errb); code != 1 || !strings.Contains(errb.String(), "does not overwrite") {
				t.Fatalf("overwrite: exit %d %s", code, errb.String())
			}
			if after, _ := os.ReadFile(csrPath); !bytes.Equal(before, after) {
				t.Fatal("the request was overwritten")
			}
			if _, err := os.Stat(filepath.Join(dir, "k2")); err == nil {
				t.Fatal("a refused keygen left a private key behind")
			}
		})
	}
}

// TestKeygenCSRRefusals: ML-DSA and hybrid keys have no request, and -csr cannot name the key's files;
// both are usage errors that write nothing.
func TestKeygenCSRRefusals(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	for _, args := range [][]string{
		{"-out", key, "-alg", "ml-dsa-65", "-csr", filepath.Join(dir, "r")},
		{"-out", key, "-alg", "ed25519+ml-dsa-65", "-csr", filepath.Join(dir, "r")},
		{"-out", key, "-csr", key},
		{"-out", key, "-csr", key + ".pub"},
	} {
		if code := KeygenMain(args, io.Discard, io.Discard); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("a refusal left %d files", len(ents))
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

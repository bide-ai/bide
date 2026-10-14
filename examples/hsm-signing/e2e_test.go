package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/bide-ai/bide/signer/pkcs11/pkcs11test"
)

func TestMain(m *testing.M) {
	os.Exit(pkcs11test.Run(m, func(tok *pkcs11test.Token) error { return tok.GenerateEd25519("bide-audit", []byte{1}) }))
}

func TestRunWithSoftHSM(t *testing.T) {
	tok := pkcs11test.Require(t)
	env := map[string]string{"BIDE_PKCS11_MODULE": tok.Module, "BIDE_PKCS11_TOKEN": tok.Label, "BIDE_HSM_PIN": tok.PIN}
	t.Setenv("BIDE_HSM_PIN", tok.PIN)
	var out bytes.Buffer
	if err := run(context.Background(), func(k string) string { return env[k] }, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"log public key: ed25519:", "signature valid, root is the journal's root", "verified with the public key alone"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), tok.PIN) {
		t.Error("the output contains the PIN")
	}
}

func TestRunWithoutConfig(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), func(string) string { return "" }, &out); err == nil || !strings.Contains(err.Error(), "BIDE_PKCS11_MODULE") {
		t.Fatalf("err = %v", err)
	}
}

func TestPINFile(t *testing.T) {
	tok := pkcs11test.Require(t)
	f := t.TempDir() + "/pin"
	if err := os.WriteFile(f, []byte(tok.PIN+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"BIDE_PKCS11_MODULE": tok.Module, "BIDE_PKCS11_TOKEN": tok.Label, "BIDE_HSM_PIN_FILE": f}
	var out bytes.Buffer
	if err := run(context.Background(), func(k string) string { return env[k] }, &out); err != nil {
		t.Fatal(err)
	}
}

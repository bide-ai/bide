package hsmkeys

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/fakekms"
	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/keyref"
	"github.com/bide-ai/bide/signer/pkcs11"
	"github.com/bide-ai/bide/signer/pkcs11/pkcs11test"
)

func TestMain(m *testing.M) {
	os.Exit(pkcs11test.Run(m, func(tok *pkcs11test.Token) error {
		for _, f := range []func() error{
			func() error { return tok.GenerateEd25519("ed", []byte{1}) },
			func() error { return tok.GenerateP256Key("p256", []byte{2}) },
			// The same label under both schemes, as a token may hold: a reference by label alone
			// does not say which is meant.
			func() error { return tok.GenerateEd25519("dup", []byte{3}) },
			func() error { return tok.GenerateP256Key("dup", []byte{4}) },
			// ... and when the other one is extractable, which RequireNonExtractable refuses.
			func() error { return tok.GenerateEd25519("dupx", []byte{5}) },
			func() error { return tok.GenerateP256Extractable("dupx", []byte{6}) },
			func() error { return tok.GenerateEd25519Extractable("ext", []byte{7}) },
		} {
			if err := f(); err != nil {
				return err
			}
		}
		return nil
	}))
}

func ref(t *testing.T, tok *pkcs11test.Token, label string) keyref.Ref {
	t.Helper()
	r, err := keyref.Parse("pkcs11:token=" + url.PathEscape(tok.Label) + ";object=" + label + "?module-path=" + url.PathEscape(tok.Module))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// counted is a PIN source that counts how often the token is asked to log in.
func counted(pin string, n *int) pkcs11.PINSource {
	return func() (string, error) { *n++; return pin, nil }
}

// The scheme is detected by trying Ed25519 and then P-256, and only a key that is not an Ed25519 key
// moves on: every other failure is the answer, and is not retried, because each attempt is a login
// and the token counts failed ones.
func TestSchemeDetectionLogsInOnlyAsOftenAsItMust(t *testing.T) {
	tok := pkcs11test.Require(t)
	for _, tc := range []struct {
		name, label, pin string
		alg              audit.Alg
		logins           int
		ok               bool
	}{
		{"ed25519 key, detected (and checked for a P-256 key of the same selection)", "ed", "", "", 2, true},
		{"ed25519 key, named", "ed", "", audit.AlgEd25519, 1, true},
		{"p256 key, detected", "p256", "", "", 2, true},
		{"p256 key, named", "p256", "", audit.AlgECDSAP256, 1, true},
		{"wrong PIN, ed25519 key", "ed", "1", "", 1, false},
		{"wrong PIN, p256 key", "p256", "1", "", 1, false},
		{"wrong PIN, named scheme", "ed", "1", audit.AlgEd25519, 1, false},
		{"no such key", "nobody", "", "", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pin, n := tc.pin, 0
			if pin == "" {
				pin = tok.PIN
			}
			s, err := Open(context.Background(), ref(t, tok, tc.label), Options{Alg: tc.alg, PIN: counted(pin, &n)})
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok = %v", err, tc.ok)
			}
			if s != nil {
				s.(interface{ Close() error }).Close()
			}
			if n != tc.logins {
				t.Fatalf("the token was asked to log in %d times, want %d", n, tc.logins)
			}
		})
	}
}

func TestNeitherScheme(t *testing.T) {
	// A KMS key of a spec bide does not use is neither; the error says both were tried.
	k := fakekms.New()
	const arn = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	k.AddP256(arn)
	r, err := keyref.Parse("awskms:" + arn)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), r, Options{KMSClient: k.Client})
	if err != nil || s.Alg() != audit.AlgECDSAP256 {
		t.Fatalf("detected %v %v", s, err)
	}
	if _, err := Open(context.Background(), r, Options{KMSClient: k.Client, Alg: audit.AlgEd25519}); err == nil {
		t.Fatal("a P-256 key opened as Ed25519")
	}
	if _, err := Open(context.Background(), keyref.Ref{}, Options{}); err == nil {
		t.Fatal("an empty reference opened")
	}
}

// A selection that matches an Ed25519 key and a P-256 key is not resolved by picking one: the
// approver cannot see which was meant. -alg, or an id, selects.
func TestSameLabelUnderTwoSchemesIsAmbiguous(t *testing.T) {
	tok := pkcs11test.Require(t)
	for _, label := range []string{"dup", "dupx"} {
		n := 0
		_, err := Open(context.Background(), ref(t, tok, label), Options{PIN: counted(tok.PIN, &n)})
		if !errors.Is(err, pkcs11.ErrAmbiguous) || !strings.Contains(err.Error(), "-alg") {
			t.Fatalf("%s: err = %v, want ErrAmbiguous naming -alg", label, err)
		}
	}
	for _, alg := range []audit.Alg{audit.AlgEd25519, audit.AlgECDSAP256} {
		s, err := Open(context.Background(), ref(t, tok, "dup"), Options{Alg: alg, PIN: func() (string, error) { return tok.PIN, nil }})
		if err != nil || s.Alg() != alg {
			t.Fatalf("-alg %s: %v %v", alg, s, err)
		}
		s.(io.Closer).Close()
	}
	// By id the selection is unique, and detection needs no -alg.
	r, err := keyref.Parse("pkcs11:token=" + url.PathEscape(tok.Label) + ";object=dup;id=%04?module-path=" + url.PathEscape(tok.Module))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), r, Options{PIN: func() (string, error) { return tok.PIN, nil }})
	if err != nil || s.Alg() != audit.AlgECDSAP256 {
		t.Fatalf("by id: %v %v", s, err)
	}
	s.(io.Closer).Close()
}

// The signer says where its key is held, without the source of the PIN, and says when the
// non-extractable check was lifted.
func TestWhereAndNotice(t *testing.T) {
	tok := pkcs11test.Require(t)
	pin := func() (string, error) { return tok.PIN, nil }
	s, err := Open(context.Background(), ref(t, tok, "ed"), Options{PIN: pin})
	if err != nil {
		t.Fatal(err)
	}
	defer s.(io.Closer).Close()
	d := s.(interface {
		Where() string
		Notice() string
	})
	if w := d.Where(); !strings.Contains(w, "token="+url.PathEscape(tok.Label)) || !strings.Contains(w, "object=ed") || strings.Contains(w, "pin") {
		t.Fatalf("Where = %q", w)
	}
	if d.Notice() != "" {
		t.Fatalf("Notice with the check on: %q", d.Notice())
	}
	x, err := Open(context.Background(), ref(t, tok, "ext"), Options{PIN: pin, AllowExtractable: true})
	if err != nil {
		t.Fatal(err)
	}
	defer x.(io.Closer).Close()
	if n := x.(interface{ Notice() string }).Notice(); !strings.Contains(n, "-allow-extractable") {
		t.Fatalf("Notice = %q", n)
	}
	if _, err := Open(context.Background(), ref(t, tok, "ext"), Options{PIN: pin}); !errors.Is(err, pkcs11.ErrExtractable) {
		t.Fatalf("extractable key by default: %v", err)
	}
}

// A build that cannot load a PKCS#11 module says so before it asks for a PIN.
func TestNoCgoIsReportedBeforeThePIN(t *testing.T) {
	defer func(old func() bool) { cgoEnabled = old }(cgoEnabled)
	cgoEnabled = func() bool { return false }
	r, err := keyref.Parse("pkcs11:token=t;object=o?module-path=/m.so")
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	_, err = Open(context.Background(), r, Options{PIN: func() (string, error) { asked = true; return "", nil }})
	if !errors.Is(err, pkcs11.ErrNoCgo) || asked {
		t.Fatalf("err = %v, PIN asked = %v", err, asked)
	}
}

func TestPINFileMustBeRegular(t *testing.T) {
	dir := t.TempDir()
	for name, mk := range map[string]func() string{
		"directory": func() string { return dir },
		"fifo":      func() string { return makeFIFO(t, dir) },
	} {
		path := mk()
		if path == "" {
			continue // no FIFOs on this system
		}
		done := make(chan error, 1)
		go func() { _, err := pinFromFile(path)(); done <- err }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Errorf("%s: err = %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: reading the PIN file blocked", name)
		}
	}
}

func promptRef(t *testing.T, tok *pkcs11test.Token, label string) keyref.Ref {
	t.Helper()
	r, err := keyref.Parse("pkcs11:token=" + url.PathEscape(tok.Label) + ";object=" + label + "?module-path=" + url.PathEscape(tok.Module) + "&pin-source=prompt")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// pin-source=prompt asks once per Open however many times detection opens the token, shows the
// token it asks for, and does not keep the PIN afterwards.
func TestPromptAsksOnce(t *testing.T) {
	tok := pkcs11test.Require(t)
	defer func(old func(string) (string, error)) { readPIN = old }(readPIN)
	var prompts []string
	readPIN = func(p string) (string, error) { prompts = append(prompts, p); return tok.PIN, nil }
	for _, label := range []string{"ed", "p256", "dup"} { // detection: 2 opens; 2 opens; ambiguity
		prompts = nil
		s, err := Open(context.Background(), promptRef(t, tok, label), Options{})
		if label == "dup" {
			if !errors.Is(err, pkcs11.ErrAmbiguous) {
				t.Fatalf("dup: %v", err)
			}
		} else if err != nil {
			t.Fatalf("%s: %v", label, err)
		} else {
			s.(io.Closer).Close()
		}
		if len(prompts) != 1 || !strings.Contains(prompts[0], tok.Label) || strings.Contains(prompts[0], tok.PIN) {
			t.Fatalf("%s: prompts = %q, want one naming the token", label, prompts)
		}
	}
	// A wrong PIN is reported as one, and the prompt is not repeated.
	prompts = nil
	readPIN = func(p string) (string, error) { prompts = append(prompts, p); return "97531357", nil }
	if _, err := Open(context.Background(), promptRef(t, tok, "p256"), Options{}); err == nil || strings.Contains(err.Error(), "97531357") || len(prompts) != 1 {
		t.Fatalf("wrong PIN: %v, %d prompts", err, len(prompts))
	}
	// A failed prompt (no terminal) is the error, and the token is not touched.
	readPIN = func(string) (string, error) { return "", ErrNoTerminal }
	if _, err := Open(context.Background(), promptRef(t, tok, "ed"), Options{}); !errors.Is(err, ErrNoTerminal) {
		t.Fatalf("no terminal: %v", err)
	}
}

// A file that opens but is not a terminal is not asked for a PIN.
func TestPromptRefusesAFileThatIsNotATerminal(t *testing.T) {
	p := t.TempDir() + "/notatty"
	if err := os.WriteFile(p, []byte("12345678\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer func(old func() (string, string)) { ttyNames = old }(ttyNames)
	ttyNames = func() (string, string) { return p, p }
	if pin, err := readPINFromTerminal("PIN: "); !errors.Is(err, ErrNoTerminal) || pin != "" {
		t.Fatalf("pin %q, err %v", pin, err)
	}
}

func TestPINMemoForgets(t *testing.T) {
	defer func(old func(string) (string, error)) { readPIN = old }(readPIN)
	n := 0
	readPIN = func(string) (string, error) { n++; return "pinpin", nil }
	m := &pinMemo{}
	src := m.source("t")
	for range 3 {
		if p, err := src(); err != nil || p != "pinpin" {
			t.Fatal(p, err)
		}
	}
	if n != 1 {
		t.Fatalf("asked %d times", n)
	}
	m.forget()
	if m.pin != "" || m.set {
		t.Fatal("the PIN is still held")
	}
	src()
	if n != 2 {
		t.Fatalf("after forget: asked %d times, want 2", n)
	}
}

// An alias is shown with the key it resolved to, and a KMS that answers with another account's key
// is refused before anything is signed.
func TestKMSAliasAndWrongAnswer(t *testing.T) {
	const (
		key   = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
		alias = "arn:aws:kms:us-east-1:111122223333:alias/approver"
		other = "arn:aws:kms:us-east-1:999999999999:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	)
	k := fakekms.New()
	k.AddEd25519(key)
	k.AddAlias(alias, key)
	r, err := keyref.Parse("awskms:" + alias)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), r, Options{KMSClient: k.Client})
	if err != nil {
		t.Fatal(err)
	}
	if w := s.(interface{ Where() string }).Where(); !strings.Contains(w, alias) || !strings.Contains(w, "resolved to "+key) {
		t.Fatalf("Where = %q", w)
	}
	r, _ = keyref.Parse("awskms:" + key)
	s, err = Open(context.Background(), r, Options{KMSClient: k.Client})
	if err != nil || strings.Contains(s.(interface{ Where() string }).Where(), "resolved") {
		t.Fatalf("key ARN: %v", err)
	}
	k.Answer = other
	signs := k.Signs
	if _, err := Open(context.Background(), r, Options{KMSClient: k.Client}); err == nil || k.Signs != signs {
		t.Fatalf("a wrong answer was accepted: %v (signs %d -> %d)", err, signs, k.Signs)
	}
}

// pin-source=protected needs the scheme named, and against a token with no PIN pad (SoftHSM2) it is
// refused, not turned into a login with no PIN.
func TestProtectedPINSource(t *testing.T) {
	tok := pkcs11test.Require(t)
	r, err := keyref.Parse("pkcs11:token=" + url.PathEscape(tok.Label) + ";object=ed?module-path=" + url.PathEscape(tok.Module) + "&pin-source=protected")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), r, Options{}); err == nil || !strings.Contains(err.Error(), "-alg") {
		t.Fatalf("without -alg: %v", err)
	}
	if _, err := Open(context.Background(), r, Options{Alg: audit.AlgEd25519}); !errors.Is(err, pkcs11.ErrNoProtectedAuthPath) {
		t.Fatalf("on a token without a pad: %v", err)
	}
}

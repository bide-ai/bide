package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/fakekms"
	"github.com/bide-ai/bide/cmd/bide/internal/approvecmd"
	"github.com/bide-ai/bide/cmd/bide/internal/stopcmd"
	"github.com/bide-ai/bide/internal/auditcli"
	"github.com/bide-ai/bide/internal/testca"
	"github.com/bide-ai/bide/signer/pkcs11/pkcs11test"
	"github.com/bide-ai/bide/store/sqlite"
)

// The token holds, for each scheme, a key an approver should keep (non-extractable and sensitive)
// and one a token should not hold (extractable and not sensitive).
const (
	edLabel, p256Label   = "ops-ed25519", "fin-p256"
	edExtract, p256Extra = "ext-ed25519", "ext-p256"
)

func TestMain(m *testing.M) {
	os.Exit(pkcs11test.Run(m, func(tok *pkcs11test.Token) error {
		for _, f := range []func() error{
			func() error { return tok.GenerateEd25519(edLabel, []byte{1}) },
			func() error { return tok.GenerateP256Key(p256Label, []byte{2}) },
			func() error { return tok.GenerateEd25519Extractable(edExtract, []byte{3}) },
			func() error { return tok.GenerateP256Extractable(p256Extra, []byte{4}) },
			func() error { return tok.GenerateEd25519AlwaysAuth("always-auth", []byte{9}) },
		} {
			if err := f(); err != nil {
				return err
			}
		}
		return nil
	}))
}

const (
	kmsEd   = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	kmsP256 = "arn:aws:kms:us-east-1:111122223333:key/9999abcd-12ab-34cd-56ef-1234567890ab"
)

// pinFile writes the token's PIN to a mode 0600 file and returns its path.
func pinFile(t *testing.T, tok *pkcs11test.Token) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pin")
	if err := os.WriteFile(p, []byte(tok.PIN+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// uri is a pkcs11: URI for the test token's key with the label.
func uri(tok *pkcs11test.Token, label, pin string, extra ...string) string {
	u := "pkcs11:token=" + url.PathEscape(tok.Label) + ";object=" + url.PathEscape(label) + "?module-path=" + url.PathEscape(tok.Module)
	if pin != "" {
		u += "&pin-source=" + pin
	}
	for _, e := range extra {
		u += "&" + e
	}
	return u
}

type result struct {
	code        int
	out, errOut string
}

func (r result) all() string { return r.out + r.errOut }

// hsm runs bide-hsm in the process with the fake KMS.
func hsm(t *testing.T, kms *fakekms.KMS, stdin string, args ...string) result {
	t.Helper()
	d := deps{stores: approvecmd.JournalStores()}
	if kms != nil {
		d.kms = kms.Client
	}
	return hsmWith(t, d, stdin, args...)
}

// hsmWith runs bide-hsm in the process with the given dependencies (the journal stores are filled in).
func hsmWith(t *testing.T, d deps, stdin string, args ...string) result {
	t.Helper()
	d.stores = approvecmd.JournalStores()
	d.stopStores = stopcmd.JournalStores()
	var out, errb bytes.Buffer
	if len(args) > 1 {
		args = append(args[:1:1], append([]string{"-color", "never"}, args[1:]...)...)
	}
	code := run(args, strings.NewReader(stdin), &out, &errb, d)
	return result{code, out.String(), errb.String()}
}

// fixture is a SQLite journal with one run paused at a 2-of-3 gate (ops, finance, risk).
type fixture struct {
	t       *testing.T
	db      string
	keysF   string
	charges atomic.Int32
	pubs    map[string]audit.Verifier
	policy  *agent.ApprovalPolicy
	args    string
}

const (
	runID    = "refund-1049"
	callID   = "c2"
	callArgs = `{"order":1049,"amount":120}`
)

func (f *fixture) drive() (*agent.Result, error) {
	s, err := sqlite.Open(f.db)
	if err != nil {
		f.t.Fatal(err)
	}
	defer s.Close()
	j := agenttest.MustJournal(s)
	refund := agent.MustFunc("refund", "refund an order", func(context.Context, json.RawMessage) (string, error) {
		f.charges.Add(1)
		return "refunded", nil
	}, agent.WithApproval(f.policy))
	m := agenttest.NewScriptedModel(agenttest.ToolTurn(callID, "refund", f.args), agenttest.TextTurn("done"))
	verify := func(id string) (agent.ApproverVerifier, bool) { v, ok := f.pubs[id]; return v, ok }
	return agenttest.MustNew(m, j, agent.WithTools(refund), agent.WithApproverVerifiers(verify)).Run(context.Background(), runID, agent.UserText("refund"))
}

// newFixture enrols the approvers' public keys, as bide-hsm pubkey prints them (the risk approver
// keeps a key nobody here uses), and pauses the run at the gate.
func newFixture(t *testing.T, entries map[string]string) *fixture {
	t.Helper()
	return newFixtureArgs(t, entries, callArgs)
}

func newFixtureArgs(t *testing.T, entries map[string]string, args string) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, args: args, db: filepath.Join(dir, "j.db"), keysF: filepath.Join(dir, "keys.json"), pubs: map[string]audit.Verifier{},
		policy: &agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}}
	for id, text := range entries {
		v, err := audit.ParsePublicKey(text)
		if err != nil {
			t.Fatal(err)
		}
		f.pubs[id] = v
	}
	b, _ := json.Marshal(entries)
	if err := os.WriteFile(f.keysF, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.drive(); err == nil {
		t.Fatal("the first drive did not pause")
	} else if _, ok := errors.AsType[*agent.ApprovalPending](err); !ok {
		t.Fatal(err)
	}
	return f
}

// enrol runs bide-hsm pubkey for ref and returns the key text it printed.
func enrol(t *testing.T, kms *fakekms.KMS, id, ref string, extra ...string) string {
	t.Helper()
	r := hsm(t, kms, "", append([]string{"pubkey", "-as", id, "-key", ref}, extra...)...)
	if r.code != 0 {
		t.Fatalf("pubkey %s: %d\n%s", id, r.code, r.all())
	}
	return enrolEntry(t, r, id)
}

// enrolEntry returns the key text of the approver-keys entry a pubkey run printed.
func enrolEntry(t *testing.T, r result, id string) string {
	t.Helper()
	var entry map[string]string
	for _, l := range strings.Split(r.out, "\n") {
		if rest, ok := strings.CutPrefix(l, "approver-keys entry  "); ok {
			if err := json.Unmarshal([]byte(rest), &entry); err != nil {
				t.Fatal(err)
			}
		}
	}
	if entry[id] == "" {
		t.Fatalf("no approver-keys entry in\n%s", r.out)
	}
	if !strings.Contains(r.out, "public key  "+entry[id]) {
		t.Fatalf("public key line missing in\n%s", r.out)
	}
	return entry[id]
}

func randomKeyText(t *testing.T) string {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	return audit.FormatPublicKey(audit.AlgEd25519, pub)
}

// TestTwoOfThreeAcrossTokenAndKMS: a 2-of-3 gate decided by one approver with a key on a PKCS#11
// token and one with a key in KMS, for each scheme. The gate re-verifies both signatures when the
// run resumes, and bide audit verify-receipts checks the receipts against the journal and its
// signed heads.
func TestTwoOfThreeAcrossTokenAndKMS(t *testing.T) {
	tok := pkcs11test.Require(t)
	for _, tc := range []struct {
		name, tokenLabel string
		kmsARN           string
		addKMS           func(*fakekms.KMS, string)
		scheme           string
	}{
		{"ed25519", edLabel, kmsEd, (*fakekms.KMS).AddEd25519, "ed25519"},
		{"ecdsa-p256", p256Label, kmsP256, (*fakekms.KMS).AddP256, "ecdsa-p256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kms := fakekms.New()
			tc.addKMS(kms, tc.kmsARN)
			pin := "file:" + pinFile(t, tok)
			opsRef := uri(tok, tc.tokenLabel, pin)
			finRef := "awskms:" + tc.kmsARN

			opsKey, finKey := enrol(t, kms, "ops", opsRef), enrol(t, kms, "finance", finRef)
			if !strings.HasPrefix(opsKey, tc.scheme+":") || !strings.HasPrefix(finKey, tc.scheme+":") {
				t.Fatalf("keys: %s %s", opsKey, finKey)
			}
			f := newFixture(t, map[string]string{"ops": opsKey, "finance": finKey, "risk": randomKeyText(t)})
			rdir := t.TempDir()
			decide := func(id, ref string, extra ...string) result {
				return hsm(t, kms, "", append([]string{"approve", "-db", f.db, "-run", runID, "-call", callID, "-as", id, "-key", ref, "-approver-keys", f.keysF, "-yes", "-receipt", filepath.Join(rdir, id+".receipt.json")}, extra...)...)
			}

			// -list offers a bide-hsm command with a key-reference placeholder.
			l := hsm(t, nil, "", "approve", "-db", f.db, "-list")
			if l.code != 0 || !strings.Contains(l.out, "bide-hsm approve -db") || !strings.Contains(l.out, "-key '<key-ref>'") || strings.Contains(l.out, "bide approve") {
				t.Fatalf("-list: %d\n%s", l.code, l.all())
			}

			// A decision signed with the wrong key is refused before anything is recorded.
			if r := decide("finance", opsRef); r.code != 1 || !strings.Contains(r.errOut, "not the key enrolled") {
				t.Fatalf("wrong key: %d\n%s", r.code, r.all())
			}

			r := decide("ops", opsRef)
			if r.code != 0 || !strings.Contains(r.out, "signed and recorded") || !strings.Contains(r.out, "with "+tc.scheme+" key") || !strings.Contains(r.out, "1 of 2") {
				t.Fatalf("ops: %d\n%s", r.code, r.all())
			}
			if _, err := f.drive(); err == nil {
				t.Fatal("one of two approvals released the gate")
			}
			if r := decide("finance", finRef); r.code != 0 || !strings.Contains(r.out, "verified tally: 2 of 2 needed approved by ops, finance") {
				t.Fatalf("finance: %d\n%s", r.code, r.all())
			}
			if kms.Signs < 2 {
				t.Fatalf("the KMS signed %d times: the open self-test and the decision are expected", kms.Signs)
			}
			// Retrying a decision that counts signs nothing more.
			signs := kms.Signs
			if r := decide("finance", finRef); r.code != 0 || !strings.Contains(r.out, "already recorded") {
				t.Fatalf("retry: %d\n%s", r.code, r.all())
			}
			if kms.Signs != signs { // not even the self-test at open: the key is not touched
				t.Fatalf("a retry used the key: %d signs, was %d", kms.Signs, signs)
			}

			// The gate verifies both signatures when the run resumes.
			if res, err := f.drive(); err != nil || res == nil || f.charges.Load() != 1 {
				t.Fatalf("resume: %v %v charges %d", res, err, f.charges.Load())
			}

			// Each receipt verifies on its own, and against the journal and signed heads.
			for _, id := range []string{"ops", "finance"} {
				if r := hsm(t, nil, "", "approve", "-verify-receipt", filepath.Join(rdir, id+".receipt.json"), "-approver-keys", f.keysF); r.code != 0 || !strings.Contains(r.out, "signature verifies for "+id) {
					t.Fatalf("-verify-receipt %s: %d\n%s", id, r.code, r.all())
				}
			}
			s, err := sqlite.Open(f.db)
			if err != nil {
				t.Fatal(err)
			}
			j := agenttest.MustJournal(s)
			exp, err := audit.ExportJournal(context.Background(), j, runID)
			if err != nil {
				t.Fatal(err)
			}
			th, err := audit.NewTreeHead(context.Background(), j, runID, time.Now().Add(time.Minute).UnixNano())
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			_, logPriv, _ := ed25519.GenerateKey(rand.Reader)
			logSigner := audit.Ed25519Signer{Priv: logPriv}
			sth, err := audit.SignTreeHead(th, logSigner)
			if err != nil {
				t.Fatal(err)
			}
			write := func(name string, v any) string {
				p := filepath.Join(rdir, name)
				b, _ := json.Marshal(v)
				if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			}
			journal, heads := write("journal.json", exp), write("heads.jsonl", sth)
			var out, errb bytes.Buffer
			code := auditcli.Main("test", []string{"-color", "never", "verify-receipts", "-receipt", rdir, "-journal", journal, "-heads", heads,
				"-pubkey", audit.FormatPublicKey(logSigner.Alg(), logSigner.PublicKey()), "-approver-keys", f.keysF}, &out, &errb)
			if code != 0 || strings.Count(out.String(), "PASS") < 2 || !strings.Contains(out.String(), "ops approved") || !strings.Contains(out.String(), "finance approved") {
				t.Fatalf("verify-receipts: %d\n%s\n%s", code, out.String(), errb.String())
			}
		})
	}
}

// A receipt with no -receipt goes in the working directory, there being no key file to put it by.
func TestDefaultReceiptInWorkingDirectory(t *testing.T) {
	kms := fakekms.New()
	kms.AddEd25519(kmsEd)
	ref := "awskms:" + kmsEd
	key := enrol(t, kms, "ops", ref)
	f := newFixture(t, map[string]string{"ops": key, "finance": randomKeyText(t), "risk": randomKeyText(t)})
	dir := t.TempDir()
	t.Chdir(dir)
	r := hsm(t, kms, "", "approve", "-db", f.db, "-run", runID, "-call", callID, "-as", "ops", "-key", ref, "-approver-keys", f.keysF, "-yes")
	if r.code != 0 {
		t.Fatalf("%d\n%s", r.code, r.all())
	}
	m, _ := filepath.Glob(filepath.Join(dir, "*.receipt.json"))
	if len(m) != 1 || !strings.HasPrefix(filepath.Base(m[0]), runID+"-"+callID+"-ops-") {
		t.Fatalf("receipts in the working directory: %v\n%s", m, r.out)
	}
}

// The confirmation is asked, and declining records nothing; the key's scheme and fingerprint are
// shown beside the call, and the key reference is never an argument of the signed bytes.
func TestConfirmationAndDisplay(t *testing.T) {
	kms := fakekms.New()
	kms.AddP256(kmsP256)
	ref := "awskms:" + kmsP256
	key := enrol(t, kms, "ops", ref)
	f := newFixture(t, map[string]string{"ops": key, "finance": randomKeyText(t), "risk": randomKeyText(t)})
	args := []string{"approve", "-db", f.db, "-run", runID, "-call", callID, "-as", "ops", "-key", ref, "-approver-keys", f.keysF, "-receipt", filepath.Join(t.TempDir(), "r.json")}
	r := hsm(t, kms, "no\n", args...)
	if r.code != 1 || !strings.Contains(r.errOut, "not confirmed") || !strings.Contains(r.out, "Type ops (or y) to approve") || !strings.Contains(r.out, "ecdsa-p256 key") || !strings.Contains(r.out, "refund") {
		t.Fatalf("decline: %d\n%s", r.code, r.all())
	}
	if r := hsm(t, kms, "ops\n", args...); r.code != 0 || !strings.Contains(r.out, "signed and recorded") {
		t.Fatalf("confirm: %d\n%s", r.code, r.all())
	}
}

// RequireNonExtractable is on by default: a key the token lets out is refused, by pubkey and by
// approve, unless -allow-extractable, which approve then shows.
func TestExtractableKeyRefusedByDefault(t *testing.T) {
	tok := pkcs11test.Require(t)
	pin := "file:" + pinFile(t, tok)
	for _, label := range []string{edExtract, p256Extra} {
		ref := uri(tok, label, pin)
		r := hsm(t, nil, "", "pubkey", "-key", ref)
		if r.code != 1 || !strings.Contains(r.errOut, "non-extractable") {
			t.Fatalf("%s pubkey: %d\n%s", label, r.code, r.all())
		}
		if strings.Contains(r.all(), tok.PIN) {
			t.Fatal("the PIN is in the output")
		}
		if r := hsm(t, nil, "", "pubkey", "-key", ref, "-allow-extractable"); r.code != 0 {
			t.Fatalf("%s -allow-extractable: %d\n%s", label, r.code, r.all())
		}
		// approve refuses it too, before showing anything.
		f := newFixture(t, map[string]string{"ops": enrol(t, nil, "ops", ref, "-allow-extractable"), "finance": randomKeyText(t), "risk": randomKeyText(t)})
		base := []string{"approve", "-db", f.db, "-run", runID, "-call", callID, "-as", "ops", "-key", ref, "-approver-keys", f.keysF, "-yes", "-receipt", filepath.Join(t.TempDir(), "r.json")}
		if r := hsm(t, nil, "", base...); r.code != 1 || !strings.Contains(r.errOut, "non-extractable") || strings.Contains(r.out, "Approve this call") {
			t.Fatalf("%s approve: %d\n%s", label, r.code, r.all())
		}
		r = hsm(t, nil, "", append(base, "-allow-extractable")...)
		if r.code != 0 || !strings.Contains(r.out, "-allow-extractable: the token was not required") {
			t.Fatalf("%s approve -allow-extractable: %d\n%s", label, r.code, r.all())
		}
	}
}

func TestAlgFlagAndDetection(t *testing.T) {
	tok := pkcs11test.Require(t)
	pin := "file:" + pinFile(t, tok)
	// Detected without -alg; named with it; refused when it names the other scheme.
	for _, tc := range []struct{ label, alg, ok string }{{edLabel, "", "ed25519:"}, {p256Label, "", "ecdsa-p256:"}, {p256Label, "ecdsa-p256", "ecdsa-p256:"}, {edLabel, "ed25519", "ed25519:"}} {
		args := []string{"pubkey", "-key", uri(tok, tc.label, pin)}
		if tc.alg != "" {
			args = append(args, "-alg", tc.alg)
		}
		if r := hsm(t, nil, "", args...); r.code != 0 || !strings.Contains(r.out, "public key  "+tc.ok) {
			t.Fatalf("%v: %d\n%s", tc, r.code, r.all())
		}
	}
	if r := hsm(t, nil, "", "pubkey", "-key", uri(tok, p256Label, pin), "-alg", "ed25519"); r.code != 1 || !strings.Contains(r.errOut, "Ed25519") {
		t.Fatalf("P-256 key as ed25519: %d\n%s", r.code, r.all())
	}
	if r := hsm(t, nil, "", "pubkey", "-key", uri(tok, edLabel, pin), "-alg", "ecdsa-p256"); r.code != 1 || !strings.Contains(r.errOut, "P-256") {
		t.Fatalf("ed25519 key as P-256: %d\n%s", r.code, r.all())
	}
	if r := hsm(t, nil, "", "pubkey", "-key", uri(tok, edLabel, pin), "-alg", "rsa"); r.code != 1 {
		t.Fatalf("-alg rsa: %d\n%s", r.code, r.all())
	}
	// The key by id instead of label.
	r := hsm(t, nil, "", "pubkey", "-key", "pkcs11:token="+url.PathEscape(tok.Label)+";id=%02?module-path="+url.PathEscape(tok.Module)+"&pin-source="+pin)
	if r.code != 0 || !strings.Contains(r.out, "ecdsa-p256:") {
		t.Fatalf("by id: %d\n%s", r.code, r.all())
	}
	// A KMS key is detected too, and a wrong scheme named is refused.
	kms := fakekms.New()
	kms.AddP256(kmsP256)
	if r := hsm(t, kms, "", "pubkey", "-key", "awskms:"+kmsP256); r.code != 0 || !strings.Contains(r.out, "ecdsa-p256:") {
		t.Fatalf("kms detect: %d\n%s", r.code, r.all())
	}
	if r := hsm(t, kms, "", "pubkey", "-key", "awskms:"+kmsP256, "-alg", "ed25519"); r.code != 1 {
		t.Fatalf("kms wrong alg: %d\n%s", r.code, r.all())
	}
	// The ARN's region selects the client: a key ARN in a region the fake does not serve is not found.
	if r := hsm(t, kms, "", "pubkey", "-key", "awskms:"+strings.Replace(kmsP256, "us-east-1", "eu-west-1", 1)); r.code != 1 {
		t.Fatalf("kms other region: %d\n%s", r.code, r.all())
	}
}

func TestUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no command", nil, 2, "bide-hsm pubkey  -key <key ref>"},
		{"unknown command", []string{"keygen"}, 2, "unknown command"},
		{"pubkey without -key", []string{"pubkey"}, 2, "no -key"},
		{"pubkey with extra arg", []string{"pubkey", "-key", "awskms:x", "y"}, 2, "unexpected argument"},
		{"pubkey bad -as", []string{"pubkey", "-key", "awskms:x", "-as", ""}, 0, ""},
		{"approve key without -as", []string{"approve", "-db", "x.db", "-run", "r", "-call", "c", "-key", "awskms:x"}, 2, "-as and -key go together"},
		{"bad reference", []string{"pubkey", "-key", "file.key"}, 1, "key reference"},
		{"pin-value", []string{"pubkey", "-key", "pkcs11:token=t;object=o?module-path=/m.so&pin-value=hunter2"}, 1, "pin-value is refused"},
		{"allow-extractable on kms", []string{"pubkey", "-key", "awskms:arn:aws:kms:us-east-1:111122223333:key/abc", "-allow-extractable"}, 1, "PKCS#11 key only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := hsm(t, nil, "", tc.args...)
			if tc.want == "" {
				return
			}
			if r.code != tc.code || !strings.Contains(r.errOut, tc.want) {
				t.Fatalf("%d\n%s", r.code, r.all())
			}
			if strings.Contains(r.all(), "hunter2") {
				t.Fatal("the value of pin-value is in the output")
			}
		})
	}
	if r := hsm(t, nil, "", "version"); r.code != 0 || !strings.Contains(r.out, "bide-hsm") {
		t.Fatalf("version: %d %s", r.code, r.all())
	}
	if r := hsm(t, nil, "", "help"); r.code != 0 || !strings.Contains(r.out, "pkcs11:token=") {
		t.Fatalf("help: %d %s", r.code, r.all())
	}
}

func TestPINFileMustBePrivate(t *testing.T) {
	tok := pkcs11test.Require(t)
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	p := pinFile(t, tok)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	r := hsm(t, nil, "", "pubkey", "-key", uri(tok, edLabel, "file:"+p))
	if r.code != 1 || !strings.Contains(r.errOut, "accessible by others") || strings.Contains(r.all(), tok.PIN) {
		t.Fatalf("%d\n%s", r.code, r.all())
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := hsm(t, nil, "", "pubkey", "-key", uri(tok, edLabel, "file:"+p)); r.code != 0 {
		t.Fatalf("0600: %d\n%s", r.code, r.all())
	}
	if r := hsm(t, nil, "", "pubkey", "-key", uri(tok, edLabel, "file:"+filepath.Join(t.TempDir(), "missing"))); r.code != 1 || !strings.Contains(r.errOut, "PIN file") {
		t.Fatalf("missing: %d\n%s", r.code, r.all())
	}
}

func TestPINFromEnvironment(t *testing.T) {
	tok := pkcs11test.Require(t)
	t.Setenv("MY_APPROVER_PIN", tok.PIN)
	if r := hsm(t, nil, "", "pubkey", "-key", uri(tok, edLabel, "env:MY_APPROVER_PIN")); r.code != 0 {
		t.Fatalf("named variable: %d\n%s", r.code, r.all())
	}
	// With no pin-source the variable is BIDE_HSM_PIN; unset, the error names it and no PIN.
	t.Setenv("BIDE_HSM_PIN", "")
	if r := hsm(t, nil, "", "pubkey", "-key", uri(tok, edLabel, "")); r.code != 1 || !strings.Contains(r.errOut, "BIDE_HSM_PIN") {
		t.Fatalf("unset default: %d\n%s", r.code, r.all())
	}
	t.Setenv("BIDE_HSM_PIN", tok.PIN)
	if r := hsm(t, nil, "", "pubkey", "-key", uri(tok, edLabel, "")); r.code != 0 {
		t.Fatalf("default variable: %d\n%s", r.code, r.all())
	}
}

// buildBinary builds the real bide-hsm (with cgo) into dir, or skips.
func buildBinary(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "bide-hsm")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", exe, ".")
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build bide-hsm with cgo: %v\n%s", err, out)
	}
	return exe
}

// TestBinaryNeverPrintsThePIN runs the real binary through every path that touches the PIN (the
// right one, a wrong one, none, one in the URI, a PIN file that is unreadable) and checks that the
// PIN, in any form it was given, appears in nothing the binary prints, nor in the receipt or the
// journal it writes.
func TestBinaryNeverPrintsThePIN(t *testing.T) {
	tok := pkcs11test.Require(t)
	exe := buildBinary(t)
	const wrongPIN = "97531357"
	dir := t.TempDir()
	good, bad := filepath.Join(dir, "good.pin"), filepath.Join(dir, "bad.pin")
	os.WriteFile(good, []byte(tok.PIN+"\n"), 0o600)
	os.WriteFile(bad, []byte(wrongPIN+"\n"), 0o600)
	secrets := []string{tok.PIN, wrongPIN}
	exec1 := func(env []string, stdin string, args ...string) (int, string) {
		if len(args) > 1 {
			args = append(args[:1:1], append([]string{"-color", "never"}, args[1:]...)...)
		}
		c := exec.Command(exe, args...)
		c.Env = append(os.Environ(), env...)
		c.Stdin = strings.NewReader(stdin)
		out, err := c.CombinedOutput()
		code := 0
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, string(out)
	}
	noSecret := func(what, out string) {
		t.Helper()
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("%s: the output holds a PIN:\n%s", what, out)
			}
		}
	}

	key := uri(tok, edLabel, "")
	type try struct {
		what string
		env  []string
		ref  string
		code int
	}
	for _, tc := range []try{
		{"right PIN in a file", nil, key + "&pin-source=file:" + good, 0},
		{"wrong PIN in a file", nil, key + "&pin-source=file:" + bad, 1},
		{"right PIN in the environment", []string{"BIDE_HSM_PIN=" + tok.PIN}, key, 0},
		{"wrong PIN in the environment", []string{"BIDE_HSM_PIN=" + wrongPIN}, key, 1},
		{"no PIN", []string{"BIDE_HSM_PIN="}, key, 1},
		{"right PIN in the URI", nil, key + "&pin-value=" + tok.PIN, 1},
		{"wrong PIN in the URI", nil, key + "&pin-value=" + wrongPIN, 1},
		{"PIN as another attribute", nil, key + "&pin=" + wrongPIN, 1},
		{"PIN in the object", nil, strings.Replace(key, "object="+edLabel, "object="+wrongPIN, 1) + "&pin-source=file:" + bad, 1},
	} {
		code, out := exec1(tc.env, "", "pubkey", "-key", tc.ref)
		noSecret(tc.what, out)
		if code != tc.code {
			t.Errorf("%s: exit %d, want %d\n%s", tc.what, code, tc.code, out)
		}
	}

	// A whole decision through the binary: the PIN is in none of the output, the receipt or the journal.
	_, entry := exec1([]string{"BIDE_HSM_PIN=" + tok.PIN}, "", "pubkey", "-as", "ops", "-key", key)
	var keyText string
	for _, l := range strings.Split(entry, "\n") {
		if rest, ok := strings.CutPrefix(l, "public key  "); ok {
			keyText = rest
		}
	}
	f := newFixture(t, map[string]string{"ops": keyText, "finance": randomKeyText(t), "risk": randomKeyText(t)})
	receipt := filepath.Join(dir, "r.json")
	code, out := exec1([]string{"BIDE_HSM_PIN=" + tok.PIN}, "", "approve", "-db", f.db, "-run", runID, "-call", callID, "-as", "ops", "-key", key, "-approver-keys", f.keysF, "-yes", "-receipt", receipt)
	noSecret("approve", out)
	if code != 0 || !strings.Contains(out, "signed and recorded") {
		t.Fatalf("approve: %d\n%s", code, out)
	}
	for _, p := range []string{receipt, f.db} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		noSecret(p, string(b))
	}
	if code, out := exec1(nil, "", "approve", "-db", f.db, "-list"); code != 0 {
		t.Fatalf("-list: %d\n%s", code, out)
	} else {
		noSecret("-list", out)
	}
	if code, out := exec1(nil, "", "version"); code != 0 || !strings.Contains(out, "PKCS#11: supported") {
		t.Fatalf("version: %d\n%s", code, out)
	}
}

// A failure other than "this key is not of the scheme tried" is the answer: a wrong PIN on a P-256
// key is reported as a wrong PIN, once, and not retried or turned into "neither Ed25519 nor P-256".
func TestFailuresAreNotRetriedAsAnotherScheme(t *testing.T) {
	tok := pkcs11test.Require(t)
	bad := filepath.Join(t.TempDir(), "bad.pin")
	os.WriteFile(bad, []byte("97531357\n"), 0o600)
	r := hsm(t, nil, "", "pubkey", "-key", uri(tok, p256Label, "file:"+bad))
	if r.code != 1 || strings.Contains(r.errOut, "neither") || strings.Contains(r.all(), "97531357") || strings.Contains(r.all(), tok.PIN) {
		t.Fatalf("wrong PIN: %d\n%s", r.code, r.all())
	}
	// A wrong PIN is tried once: the token counts failed logins and locks the PIN after a few, and a
	// retry under the other scheme would spend two. Two wrong PINs on an Ed25519 key leave the
	// right one working.
	for range 2 {
		if r := hsm(t, nil, "", "pubkey", "-key", uri(tok, edLabel, "file:"+bad)); r.code != 1 {
			t.Fatalf("wrong PIN on an Ed25519 key: %d\n%s", r.code, r.all())
		}
	}
	// The right PIN resets the token's failed-login count, so later tests are not locked out.
	if r := hsm(t, nil, "", "pubkey", "-key", uri(tok, p256Label, "file:"+pinFile(t, tok))); r.code != 0 {
		t.Fatalf("right PIN: %d\n%s", r.code, r.all())
	}
	pin := "file:" + pinFile(t, tok)
	for name, ref := range map[string]string{
		"no such key":           uri(tok, "nobody", pin),
		"no such token":         strings.Replace(uri(tok, edLabel, pin), "token="+url.PathEscape(tok.Label), "token=nosuch", 1),
		"no such module":        strings.Replace(uri(tok, edLabel, pin), "module-path="+url.PathEscape(tok.Module), "module-path=%2Fnonexistent%2Flib.so", 1),
		"id and label disagree": "pkcs11:token=" + url.PathEscape(tok.Label) + ";object=" + edLabel + ";id=%02?module-path=" + url.PathEscape(tok.Module) + "&pin-source=" + pin,
	} {
		if r := hsm(t, nil, "", "pubkey", "-key", ref); r.code != 1 || r.errOut == "" || strings.Contains(r.all(), tok.PIN) {
			t.Errorf("%s: %d\n%s", name, r.code, r.all())
		}
	}
}

func TestPINFileShapes(t *testing.T) {
	tok := pkcs11test.Require(t)
	dir := t.TempDir()
	empty, long := filepath.Join(dir, "empty"), filepath.Join(dir, "long")
	os.WriteFile(empty, []byte("\n"), 0o600)
	os.WriteFile(long, bytes.Repeat([]byte("1"), 5000), 0o600)
	for name, tc := range map[string]struct{ path, want string }{
		"empty": {empty, "is empty"}, "long": {long, "too long"}, "directory": {dir, "not a regular file"},
	} {
		r := hsm(t, nil, "", "pubkey", "-key", uri(tok, edLabel, "file:"+tc.path))
		if r.code != 1 || !strings.Contains(r.errOut, tc.want) {
			t.Errorf("%s: %d\n%s", name, r.code, r.all())
		}
	}
	// The PIN may end in a line break (or CR LF), which is dropped.
	crlf := filepath.Join(dir, "crlf")
	os.WriteFile(crlf, []byte(tok.PIN+"\r\n"), 0o600)
	if r := hsm(t, nil, "", "pubkey", "-key", uri(tok, edLabel, "file:"+crlf)); r.code != 0 {
		t.Fatalf("crlf: %d\n%s", r.code, r.all())
	}
}

// AWS KMS signs at most 4096 bytes under Ed25519, and an approver's decision signs the SHA-256 of
// the call's arguments, not the arguments: a call with arguments far past that limit is decided.
func TestLargeArgumentsUnderKMSEd25519(t *testing.T) {
	kms := fakekms.New()
	kms.AddEd25519(kmsEd)
	ref := "awskms:" + kmsEd
	key := enrol(t, kms, "ops", ref)
	big := `{"note":"` + strings.Repeat("x", 20000) + `"}`
	f := newFixtureArgs(t, map[string]string{"ops": key, "finance": randomKeyText(t), "risk": randomKeyText(t)}, big)
	r := hsm(t, kms, "", "approve", "-db", f.db, "-run", runID, "-call", callID, "-as", "ops", "-key", ref, "-approver-keys", f.keysF, "-yes", "-full", "-receipt", filepath.Join(t.TempDir(), "r.json"))
	if r.code != 0 || !strings.Contains(r.out, "signed and recorded") {
		t.Fatalf("%d\n%.600s", r.code, r.all())
	}
}

// A key reference given as a stray argument, without -key, is not echoed: it can hold a PIN.
func TestStrayReferenceIsNotEchoed(t *testing.T) {
	ref := "pkcs11:token=T;object=k?module-path=/x.so&pin-value=hunter2secret"
	for _, args := range [][]string{
		{"pubkey", ref},
		{"pubkey", "-key", "awskms:x", ref},
		{"approve", "-db", "x.db", "-run", "r", "-call", "c", ref},
	} {
		r := hsm(t, nil, "", args...)
		if r.code != 2 || !strings.Contains(r.errOut, "unexpected argument") || strings.Contains(r.all(), "hunter2secret") {
			t.Errorf("%v: %d\n%s", args, r.code, r.all())
		}
	}
}

// The approver sees where the key is held, not only its fingerprint; pubkey says so too, and
// both say when the non-extractable check was lifted.
func TestKeyLocationAndNoticeAreShown(t *testing.T) {
	tok := pkcs11test.Require(t)
	pin := "file:" + pinFile(t, tok)
	ref := uri(tok, edExtract, pin)
	r := hsm(t, nil, "", "pubkey", "-key", ref, "-allow-extractable")
	if r.code != 0 || !strings.Contains(r.out, "held in     pkcs11:token="+url.PathEscape(tok.Label)+";object="+edExtract) || !strings.Contains(r.out, "notice      -allow-extractable") || strings.Contains(r.out, pinFile(t, tok)) {
		t.Fatalf("pubkey: %d\n%s", r.code, r.all())
	}
	if r := hsm(t, nil, "", "pubkey", "-key", uri(tok, edLabel, pin)); r.code != 0 || !strings.Contains(r.out, "held in") || strings.Contains(r.out, "notice") {
		t.Fatalf("pubkey with the check on: %d\n%s", r.code, r.all())
	}
	f := newFixture(t, map[string]string{"ops": enrol(t, nil, "ops", uri(tok, edLabel, pin)), "finance": randomKeyText(t), "risk": randomKeyText(t)})
	r = hsm(t, nil, "no\n", "approve", "-db", f.db, "-run", runID, "-call", callID, "-as", "ops", "-key", uri(tok, edLabel, pin), "-approver-keys", f.keysF)
	if !strings.Contains(r.out, "held in    pkcs11:token="+url.PathEscape(tok.Label)+";object="+edLabel) || strings.Contains(r.all(), tok.PIN) {
		t.Fatalf("approve:\n%s", r.all())
	}
}

// The words bide-hsm approve prints name bide-hsm's own commands and places, not bide's.
func TestMessagesNameBideHSM(t *testing.T) {
	kms := fakekms.New()
	kms.AddEd25519(kmsEd)
	ref := "awskms:" + kmsEd
	key := enrol(t, kms, "ops", ref)
	f := newFixture(t, map[string]string{"ops": key, "finance": randomKeyText(t), "risk": randomKeyText(t)})
	t.Chdir(t.TempDir())
	r := hsm(t, kms, "", "approve", "-db", f.db, "-run", runID, "-call", callID, "-as", "ops", "-key", ref, "-yes")
	if r.code != 1 || !strings.Contains(r.errOut, "bide-hsm pubkey -as ops prints this approver's entry") || strings.Contains(r.errOut, "bide keygen") {
		t.Fatalf("no -approver-keys: %d\n%s", r.code, r.all())
	}
	args := []string{"approve", "-db", f.db, "-run", runID, "-call", callID, "-as", "ops", "-key", ref, "-approver-keys", f.keysF, "-yes"}
	if r := hsm(t, kms, "", args...); r.code != 0 {
		t.Fatalf("decide: %d\n%s", r.code, r.all())
	}
	if r := hsm(t, kms, "", args...); r.code != 0 || !strings.Contains(r.out, "receipt "+runID+"-"+callID+"-ops-") {
		t.Fatalf("retry: %d\n%s", r.code, r.all())
	}
	// With the receipt gone, the message says where it went by default.
	m, _ := filepath.Glob("*.receipt.json")
	for _, p := range m {
		os.Remove(p)
	}
	if r := hsm(t, kms, "", args...); r.code != 0 || !strings.Contains(r.out, "(by default in the working directory") || strings.Contains(r.out, "next to your key") {
		t.Fatalf("retry without the receipt: %d\n%s", r.code, r.all())
	}
}

// A build without cgo says so before it asks for a PIN, and says so in its version.
func TestBuildWithoutCgo(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "bide-hsm")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", exe, ".")
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("a build without cgo must work: %v\n%s", err, out)
	}
	c := exec.Command(exe, "pubkey", "-key", "pkcs11:token=t;object=o?module-path=/m.so")
	c.Env = append(os.Environ(), "BIDE_HSM_PIN=")
	out, err := c.CombinedOutput()
	if ee, ok := errors.AsType[*exec.ExitError](err); !ok || ee.ExitCode() != 1 || !strings.Contains(string(out), "no cgo") || strings.Contains(string(out), "BIDE_HSM_PIN") {
		t.Fatalf("pkcs11 reference: %v\n%s", err, out)
	}
	if out, err := exec.Command(exe, "version").CombinedOutput(); err != nil || !strings.Contains(string(out), "PKCS#11: not supported (built without cgo)") {
		t.Fatalf("version: %v\n%s", err, out)
	}
}

// The real AWS client path, against a local endpoint (AWS_ENDPOINT_URL_KMS): the ARN's region, not
// the environment's, scopes the request signature, and the credentials come from the SDK chain.
func TestRealKMSClientUsesTheARNsRegion(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	const arn = "arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	var scopes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scopes = append(scopes, r.Header.Get("Authorization"))
		var in struct{ Message []byte }
		json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		switch r.Header.Get("X-Amz-Target") {
		case "TrentService.GetPublicKey":
			json.NewEncoder(w).Encode(map[string]any{"KeyId": arn, "KeySpec": "ECC_NIST_EDWARDS25519", "KeyUsage": "SIGN_VERIFY",
				"SigningAlgorithms": []string{"ED25519_SHA_512"}, "PublicKey": base64.StdEncoding.EncodeToString(der)})
		case "TrentService.Sign":
			json.NewEncoder(w).Encode(map[string]any{"KeyId": arn, "SigningAlgorithm": "ED25519_SHA_512", "Signature": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, in.Message))})
		default:
			http.Error(w, `{"__type":"UnknownOperationException"}`, 400)
		}
	}))
	defer srv.Close()
	for k, v := range map[string]string{"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY": "secretexample", "AWS_SESSION_TOKEN": "",
		"AWS_ENDPOINT_URL_KMS": srv.URL, "AWS_EC2_METADATA_DISABLED": "true", "AWS_REGION": "us-east-1", "AWS_DEFAULT_REGION": "us-east-1",
		"AWS_CONFIG_FILE": filepath.Join(t.TempDir(), "none"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(t.TempDir(), "none"), "AWS_PROFILE": ""} {
		t.Setenv(k, v)
	}
	r := hsm(t, nil, "", "pubkey", "-key", "awskms:"+arn)
	want := audit.FormatPublicKey(audit.AlgEd25519, pub)
	if r.code != 0 || !strings.Contains(r.out, "public key  "+want) || !strings.Contains(r.out, "held in     awskms:"+arn) {
		t.Fatalf("%d\n%s", r.code, r.all())
	}
	if len(scopes) < 2 {
		t.Fatalf("%d requests reached the endpoint", len(scopes))
	}
	for _, a := range scopes {
		if !strings.Contains(a, "/eu-west-1/kms/aws4_request") || strings.Contains(a, "secretexample") {
			t.Fatalf("request scoped as %q, want the ARN's region eu-west-1", a)
		}
	}
	if strings.Contains(r.all(), "secretexample") {
		t.Fatal("the secret key is in the output")
	}
}

// An approver key that demands authentication for each signature (CKA_ALWAYS_AUTHENTICATE) decides:
// the PIN source is used again for the signature itself.
func TestApproveWithAKeyThatAuthenticatesEachSignature(t *testing.T) {
	tok := pkcs11test.Require(t)
	ref := uri(tok, "always-auth", "file:"+pinFile(t, tok))
	key := enrol(t, nil, "ops", ref)
	f := newFixture(t, map[string]string{"ops": key, "finance": randomKeyText(t), "risk": randomKeyText(t)})
	r := hsm(t, nil, "", "approve", "-db", f.db, "-run", runID, "-call", callID, "-as", "ops", "-key", ref, "-approver-keys", f.keysF, "-yes", "-receipt", filepath.Join(t.TempDir(), "r.json"))
	if r.code != 0 || !strings.Contains(r.out, "signed and recorded") || !strings.Contains(r.out, "verified tally: 1 of 2") {
		t.Fatalf("%d\n%s", r.code, r.all())
	}
}

// ---- certificate-bound decisions with a key in a token or a KMS ----

// publicKeyOf decodes the key text bide-hsm pubkey prints into the public key a certificate carries.
func publicKeyOf(t *testing.T, text string) crypto.PublicKey {
	t.Helper()
	scheme, hexKey, _ := strings.Cut(text, ":")
	b, err := hex.DecodeString(hexKey)
	if err != nil {
		t.Fatal(err)
	}
	switch scheme {
	case "ed25519":
		return ed25519.PublicKey(b)
	case "ecdsa-p256":
		k, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), b)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	t.Fatalf("scheme %q", scheme)
	return nil
}

// issued is a CA that has issued a certificate to a key the test does not hold, with the chain and policy
// files bide-hsm approve reads.
type issued struct {
	chain, policy string
	leaf          *testca.Leaf
}

func issue(t *testing.T, pub crypto.PublicKey, as string) issued {
	t.Helper()
	root := testca.NewRoot(t, "root", testca.Opts{})
	inter := root.NewIntermediate(t, "inter", testca.Opts{})
	leaf := inter.NewLeaf(t, testca.Opts{Pub: pub, Mutate: func(c *x509.Certificate) {
		c.NotBefore, c.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	}})
	dir := t.TempDir()
	var cb bytes.Buffer
	for _, der := range leaf.Chain() {
		cb.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	chain := filepath.Join(dir, "chain.pem")
	if err := os.WriteFile(chain, cb.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	spki := audit.SPKIHashOf(inter.Cert)
	pj, _ := json.Marshal(map[string]any{
		"anchors": []string{"root.pem"}, "intermediates": []string{hex.EncodeToString(spki[:])},
		"identity": map[string]string{as: testca.Person(1)}, "id_kind": "person-uri",
		"eku": testca.EKU.String(), "policy_oid": testca.PolicyOID.String(), "canonical": map[string]string{"host": "idp.bank.example"},
	})
	policy := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policy, pj, 0o600); err != nil {
		t.Fatal(err)
	}
	return issued{chain: chain, policy: policy, leaf: leaf}
}

// A certificate-bound decision with a key that stays in a token or a KMS: the key is checked against the
// certificate's before it signs, the HSM or KMS signs the v4 bytes, and the chain is recorded with the
// decision. A key that is not the leaf's signs nothing.
func TestApproveWithACertificate(t *testing.T) {
	tok := pkcs11test.Require(t)
	for _, tc := range []struct {
		name, tokenLabel string
		kmsARN           string
		addKMS           func(*fakekms.KMS, string)
		scheme           string
	}{
		{"ed25519", edLabel, kmsEd, (*fakekms.KMS).AddEd25519, "ed25519"},
		{"ecdsa-p256", p256Label, kmsP256, (*fakekms.KMS).AddP256, "ecdsa-p256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kms := fakekms.New()
			tc.addKMS(kms, tc.kmsARN)
			pin := "file:" + pinFile(t, tok)
			for kind, ref := range map[string]string{"token": uri(tok, tc.tokenLabel, pin), "kms": "awskms:" + tc.kmsARN} {
				t.Run(kind, func(t *testing.T) {
					key := enrol(t, kms, "ops", ref)
					other := "awskms:" + tc.kmsARN
					if kind == "kms" {
						other = uri(tok, tc.tokenLabel, pin)
					}
					c := issue(t, publicKeyOf(t, key), "ops")
					f := newFixture(t, map[string]string{"ops": key, "finance": randomKeyText(t), "risk": randomKeyText(t)})
					args := func(keyRef string, extra ...string) []string {
						return append([]string{"approve", "-db", f.db, "-run", runID, "-call", callID, "-as", "ops", "-key", keyRef, "-cert", c.chain, "-identity-policy", c.policy, "-yes",
							"-receipt", filepath.Join(t.TempDir(), "r.json")}, extra...)
					}
					// The other key (the other kind of HSM holding the other key) is not the leaf's: refused.
					if r := hsm(t, kms, "", args(other)...); r.code != 1 || !strings.Contains(r.errOut, "is not the key of the certificate's leaf") {
						t.Fatalf("wrong key: %d\n%s", r.code, r.all())
					}
					if n := len(decisionRecords(t, f)); n != 0 {
						t.Fatalf("%d decisions recorded by a wrong key", n)
					}
					r := hsm(t, kms, "", args(ref)...)
					if r.code != 0 || !strings.Contains(r.out, "signed and recorded") || !strings.Contains(r.out, "accepted for ops by the policy now") {
						t.Fatalf("%d\n%s", r.code, r.all())
					}
					recs := decisionRecords(t, f)
					if len(recs) != 1 || len(recs[0].ApproverCert()) != len(c.leaf.Chain()) || !bytes.Equal(recs[0].ApproverCert()[0], c.leaf.DER) {
						t.Fatalf("%d records", len(recs))
					}
					// The token or KMS signed the v4 bytes: the signature verifies under the leaf's key.
					rec := recs[0]
					at, err := agent.ParseTimestamp(rec.DecidedAt())
					if err != nil {
						t.Fatal(err)
					}
					v, err := audit.NewVerifier(audit.Alg(tc.scheme), mustKeyBytes(t, key))
					if err != nil {
						t.Fatal(err)
					}
					s := f.subject(t)
					if !v.Verify(agent.ApprovalDecisionBytesCert(s, "ops", true, at, agent.CertLeafHash(c.leaf.DER)), rec.Signature()) {
						t.Fatal("the signature is not over the v4 bytes")
					}
					if v.Verify(agent.ApprovalDecisionBytes(s, "ops", true, at), rec.Signature()) {
						t.Fatal("the signature also verifies over the bare bytes")
					}
				})
			}
		})
	}
}

func mustKeyBytes(t *testing.T, text string) []byte {
	t.Helper()
	_, h, _ := strings.Cut(text, ":")
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decisionRecords(t *testing.T, f *fixture) []agent.Record {
	t.Helper()
	s, err := sqlite.Open(f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recs, err := agenttest.MustJournal(s).History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []agent.Record
	for _, r := range recs {
		if agent.IsApprovalDecision(r, callID) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fixture) subject(t *testing.T) agent.ApprovalSubject {
	t.Helper()
	s, err := sqlite.Open(f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recs, err := agenttest.MustJournal(s).History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	_, call, ok := agent.FindToolCall(recs, callID)
	if !ok {
		t.Fatal("no call")
	}
	return agent.ApprovalSubject{RunID: runID, ToolUseID: callID, ToolName: call.Name, Args: call.Args}
}

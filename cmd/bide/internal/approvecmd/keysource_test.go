package approvecmd

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/audit"
)

// refSigner is a signer a key source loads by reference: it wraps a key file's signer, counts
// Close, and carries a notice.
type refSigner struct {
	audit.Signer
	closed *int
	notice string
}

func (r refSigner) Close() error   { *r.closed++; return nil }
func (r refSigner) Notice() string { return r.notice }
func (r refSigner) String() string { return "refSigner" }

// refSource is a KeySource whose -key is a name that maps to a key file, with its own flag, a
// receipt directory, and its own words.
func refSource(f *fixture, closed *int, loaded *[]string) KeySource {
	var tag *string
	return KeySource{
		Prog:        "tool approve",
		Usage:       "usage: tool approve [flags]\n\nFlags:\n",
		KeyHelp:     "a key name",
		KeyNoun:     "a key name",
		ListHint:    "Replace '<key-name>' with a key name.",
		Placeholder: "<key-name>",
		Flags:       func(fs *flag.FlagSet) { tag = fs.String("tag", "", "a flag of the source") },
		Load: func(ref string) (audit.Signer, error) {
			*loaded = append(*loaded, ref+"/"+*tag)
			if ref == "broken" {
				return nil, errors.New("the HSM is not there")
			}
			sg, err := LoadSigner(f.keys[ref])
			if err != nil {
				return nil, err
			}
			return refSigner{sg, closed, "THIS KEY IS SPECIAL"}, nil
		},
		ReceiptDir: func(string) string { return filepath.Join(f.dir, "receipts") },
	}
}

func (f *fixture) runWith(src KeySource, stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = MainWith(append([]string{"-db", f.db, "-color", "never"}, args...), strings.NewReader(stdin), &out, &errb, stores, src)
	return code, out.String(), errb.String()
}

// TestKeySource: the flow is one implementation whatever holds the key. A custom source's words are
// used in messages, its flags are parsed, its signer is closed, its notice is shown beside the key,
// its receipt directory is used, and what it refuses is reported under its name.
func TestKeySource(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	if err := os.Mkdir(filepath.Join(f.dir, "receipts"), 0o700); err != nil {
		t.Fatal(err)
	}
	closed, loaded := 0, []string(nil)
	src := refSource(f, &closed, &loaded)
	by := func(id, name string, extra ...string) []string {
		return append([]string{"-run", f.run, "-call", f.call, "-as", id, "-key", name, "-approver-keys", f.keysJSON, "-tag", "t1"}, extra...)
	}

	// -list names the source's program and placeholder, and its hint.
	code, out, errs := f.runWith(src, "", "-list")
	if code != 0 {
		t.Fatalf("-list: %d %s", code, errs)
	}
	contains(t, "-list", out, "tool approve -db "+word(f.db)+" -run refund-1049 -call c2 -as ops -key '<key-name>' -approver-keys '<keys-file>'", "Replace '<key-name>' with a key name.")
	if strings.Contains(out, "bide approve") || strings.Contains(out, "<key-file>") {
		t.Fatalf("-list names the file source:\n%s", out)
	}

	// A refusal and a usage error carry the source's program name; its help is its Usage.
	if code, _, errs = f.runWith(src, "", "-run", f.run, "-call", f.call, "-as", "ops", "-key", "ops", "-approver-keys", f.keysJSON, "-tag", "t", "-bogus"); code != 2 || !strings.Contains(errs, "flag provided but not defined") {
		t.Fatalf("bogus flag: %d %s", code, errs)
	}
	if code, _, errs = f.runWith(src, "", "-run", f.run, "-call", f.call, "-as", "ops"); code != 2 || !strings.Contains(errs, "tool approve: -as and -key go together") {
		t.Fatalf("-as without -key: %d %s", code, errs)
	}
	if code, out, errs = f.runWith(src, "", "-h"); code != 0 || !strings.Contains(errs+out, "usage: tool approve [flags]") || !strings.Contains(errs+out, "-tag") {
		t.Fatalf("-h: %d %s", code, errs+out)
	}
	// A gate that needs a key says what -key takes.
	if code, _, errs = f.runWith(src, "", "-run", f.run, "-call", f.call, "-approver-keys", f.keysJSON); code != 1 || !strings.Contains(errs, "give -as (your approver id) and -key (a key name)") {
		t.Fatalf("no -as: %d %s", code, errs)
	}

	// A load failure is the source's error under the source's name, and nothing is recorded.
	code, _, errs = f.runWith(src, "", by("ops", "broken", "-yes")...)
	if code != 1 || !strings.Contains(errs, "tool approve: the HSM is not there") || len(f.decisions()) != 0 {
		t.Fatalf("load failure: %d %s (%d decisions)", code, errs, len(f.decisions()))
	}

	// A decision: the flag reached Load, the notice is shown, the signer was closed, the receipt is
	// in the source's directory, and the receipt line names the source's program.
	code, out, errs = f.runWith(src, "", by("ops", "ops", "-yes")...)
	if code != 0 {
		t.Fatalf("decision: %d\n%s\n%s", code, out, errs)
	}
	if got := loaded[len(loaded)-1]; got != "ops/t1" {
		t.Fatalf("Load saw %q", got)
	}
	if closed != 1 {
		t.Fatalf("the signer was closed %d times, want 1", closed)
	}
	contains(t, "decision", out, "held in    the test HSM, slot 7", "THIS KEY IS SPECIAL", "signed and recorded", "verify it with tool approve -verify-receipt")
	m, _ := filepath.Glob(filepath.Join(f.dir, "receipts", "refund-1049-c2-ops-*.receipt.json"))
	if len(m) != 1 {
		t.Fatalf("receipts in the source's directory: %v\n%s", m, out)
	}
	// The default receipt did not go beside any file named by -key.
	if m, _ := filepath.Glob("*.receipt.json"); len(m) != 0 {
		t.Fatalf("a receipt in the working directory: %v", m)
	}

	// A retry is the source's too: it points at the source's receipt directory and loads no key.
	n := len(loaded)
	code, out, _ = f.runWith(src, "", by("ops", "ops", "-yes")...)
	if code != 0 || !strings.Contains(out, "already recorded") || len(loaded) != n || !strings.Contains(out, filepath.Join(f.dir, "receipts")) {
		t.Fatalf("retry: %d\n%s (loads %d, was %d)", code, out, len(loaded), n)
	}

	// An explicit -receipt wins over the source's directory.
	own := filepath.Join(t.TempDir(), "mine.json")
	code, out, errs = f.runWith(src, "", by("finance", "finance", "-yes", "-receipt", own)...)
	if _, err := os.Stat(own); code != 0 || err != nil {
		t.Fatalf("-receipt: %d %v\n%s\n%s", code, err, out, errs)
	}
	if closed != 2 {
		t.Fatalf("closed %d, want 2", closed)
	}
}

// The file source is what bide approve is: same words, same signer.
func TestFileKeysSource(t *testing.T) {
	k := FileKeys()
	if k.Prog != "bide approve" || k.Load == nil || k.Usage != usageText || k.Placeholder != "<key-file>" {
		t.Fatalf("%+v", k)
	}
	if got := k.receiptDir(filepath.Join("a", "b.key")); got != "a" {
		t.Fatalf("receiptDir = %q", got)
	}
	if got := k.rewriteLine("bide approve -db x -key '<key-file>'"); got != "bide approve -db x -key '<key-file>'" {
		t.Fatalf("rewriteLine changed a file source's line: %q", got)
	}
	r := KeySource{Prog: "x approve", Placeholder: "<ref>"}
	if got := r.rewriteLine("bide approve -db x -key '<key-file>' -as a"); got != "x approve -db x -key '<ref>' -as a" {
		t.Fatalf("rewriteLine = %q", got)
	}
}

// A source's error is shown escaped (it may hold text from a vendor library), a stray argument is
// not echoed when the source asks for that, and the refusals name the source's own enrolment command.
func TestKeySourceErrorsAndArguments(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	closed, loaded := 0, []string(nil)
	src := refSource(f, &closed, &loaded)
	src.EnrolCmd, src.ReceiptWhere, src.NoEchoArgs = "tool enrol -as", "in the tool's directory", true
	hostile := src
	hostile.Load = func(string) (audit.Signer, error) {
		return nil, errors.New("vendor says: \x1b[31mred\x1b]0;title\x07 and \u202eevil")
	}
	code, _, errs := f.runWith(hostile, "", "-run", f.run, "-call", f.call, "-as", "ops", "-key", "k", "-approver-keys", f.keysJSON, "-tag", "t", "-yes")
	if code != 1 || strings.ContainsAny(errs, "\x1b\x07\u202e") || !strings.Contains(errs, "vendor says") {
		t.Fatalf("hostile Load error: %d %q", code, errs)
	}
	if code, _, errs = f.runWith(src, "", "-run", f.run, "-call", f.call, "-as", "ops", "-key", "ops", "-tag", "t", "-yes"); code != 1 || !strings.Contains(errs, "tool enrol -as ops prints this approver's entry") {
		t.Fatalf("no -approver-keys: %d %s", code, errs)
	}
	// A repeat names where the receipt went by default, in the source's words.
	if err := os.Mkdir(filepath.Join(f.dir, "receipts"), 0o700); err != nil {
		t.Fatal(err)
	}
	by := []string{"-run", f.run, "-call", f.call, "-as", "ops", "-key", "ops", "-approver-keys", f.keysJSON, "-tag", "t", "-yes"}
	if code, out, errs := f.runWith(src, "", by...); code != 0 {
		t.Fatalf("decide: %d %s\n%s", code, errs, out)
	}
	rs, _ := filepath.Glob(filepath.Join(f.dir, "receipts", "*.receipt.json"))
	for _, r := range rs {
		os.Remove(r)
	}
	if code, out, _ := f.runWith(src, "", by...); code != 0 || !strings.Contains(out, "(by default in the tool's directory, or where -receipt named it)") {
		t.Fatalf("repeat: %d\n%s", code, out)
	}
	if code, _, errs = f.runWith(src, "", "-tag", "t", "stray-secret"); code != 2 || strings.Contains(errs, "stray-secret") || !strings.Contains(errs, "unexpected argument") {
		t.Fatalf("stray argument: %d %s", code, errs)
	}
	plain := src
	plain.NoEchoArgs = false
	if code, _, errs = f.runWith(plain, "", "-tag", "t", "stray-visible"); code != 2 || !strings.Contains(errs, `unexpected argument "stray-visible"`) {
		t.Fatalf("stray argument, echoed: %d %s", code, errs)
	}
}

// rewriteLine changes the -key placeholder only: a run or call id may itself read '<key-file>'.
func TestRewriteLineLeavesIdsAlone(t *testing.T) {
	k := KeySource{Prog: "x approve", Placeholder: "<ref>"}
	got := k.rewriteLine("bide approve -db d -run '<key-file>' -call '<key-file>' -as a -key '<key-file>' -approver-keys '<keys-file>'")
	want := "x approve -db d -run '<key-file>' -call '<key-file>' -as a -key '<ref>' -approver-keys '<keys-file>'"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// The default receipt name fits a file name whatever the ids' lengths.
func TestDefaultReceiptNameFitsAFileName(t *testing.T) {
	long := strings.Repeat("r", 5000)
	name := defaultReceiptName(long, long, long, "2026-10-13T12:14:03.123Z")
	if len(name) > 255 || !strings.HasSuffix(name, ".receipt.json") {
		t.Fatalf("%d bytes: %s", len(name), name)
	}
	// ... and a file of that name can be written, not lost to a name the file system refuses.
	p := filepath.Join(t.TempDir(), name)
	if err := writeExclusive(p, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %d-byte name: %v", len(name), err)
	}
}

// Where says where the test signer's key is held.
func (r refSigner) Where() string { return "the test HSM, slot 7" }

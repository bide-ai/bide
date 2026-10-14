package approvecmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/store/sqlite"
)

var approvers = []string{"ops", "finance", "risk"}

// fixture is a SQLite journal with one run paused at a 2-of-3 gate, and a key file per approver.
type fixture struct {
	t        *testing.T
	db       string
	dir      string
	keys     map[string]string // approver id -> key file
	pubs     map[string]audit.Verifier
	keysJSON string // an -approver-keys file for the three approvers
	charges  *atomic.Int32
	args     string
	run      string
	call     string
	identity string // the requester the run is driven for, when not empty
}

const refundArgs = `{"order":1049,"amount":120,"note":"refund <ok>"}`

func newFixture(t *testing.T, policy *agent.ApprovalPolicy, args string) *fixture {
	t.Helper()
	return newFixtureAlg(t, policy, args, audit.AlgEd25519)
}

// newFixtureAlg is newFixture with every approver's key in scheme alg.
func newFixtureAlg(t *testing.T, policy *agent.ApprovalPolicy, args string, alg audit.Alg) *fixture {
	t.Helper()
	return newFixtureID(t, policy, args, alg, "")
}

func newFixtureID(t *testing.T, policy *agent.ApprovalPolicy, args string, alg audit.Alg, identity string) *fixture {
	t.Helper()
	f := &fixture{identity: identity, t: t, dir: t.TempDir(), keys: map[string]string{}, pubs: map[string]audit.Verifier{}, charges: new(atomic.Int32), args: args, run: "refund-1049", call: "c2"}
	f.db = filepath.Join(f.dir, "j.db")
	enrol := map[string]string{}
	for _, id := range approvers {
		path := filepath.Join(f.dir, id+".key")
		priv, err := writeKeyPair(path, alg)
		if err != nil {
			t.Fatal(err)
		}
		f.keys[id] = path
		v, err := audit.ParsePublicKey(publicKeyText(priv))
		if err != nil {
			t.Fatal(err)
		}
		f.pubs[id] = v
		enrol[id] = publicKeyText(priv)
	}
	b, _ := json.Marshal(enrol)
	f.keysJSON = filepath.Join(f.dir, "keys.json")
	if err := os.WriteFile(f.keysJSON, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.drive(policy, true); err == nil {
		t.Fatal("the first drive did not pause")
	} else if _, ok := errors.AsType[*agent.ApprovalPending](err); !ok {
		t.Fatalf("the first drive: %v", err)
	}
	return f
}

func (f *fixture) store() (*agent.Journal, func()) {
	s, err := sqlite.Open(f.db)
	if err != nil {
		f.t.Fatal(err)
	}
	return agenttest.MustJournal(s), func() { s.Close() }
}

// drive runs the agent on the fixture's run, as an application does after approvers decide.
func (f *fixture) drive(policy *agent.ApprovalPolicy, first bool) (*agent.Result, error) {
	j, closeJ := f.store()
	defer closeJ()
	verify := func(id string) (agent.ApproverVerifier, bool) {
		v, ok := f.pubs[id]
		return v, ok
	}
	refund := agent.MustFunc("refund", "refund an order", func(context.Context, json.RawMessage) (string, error) {
		f.charges.Add(1)
		return "refunded", nil
	}, agent.WithApproval(policy))
	m := agenttest.NewScriptedModel(agenttest.ToolTurn(f.call, "refund", f.args), agenttest.TextTurn("done"))
	var opts []agent.RunOption
	if f.identity != "" {
		opts = append(opts, agent.WithIdentity(agent.Identity{Actor: "a", OnBehalfOf: f.identity}))
	}
	return agenttest.MustNew(m, j, agent.WithTools(refund), agent.WithApproverVerifiers(verify)).Run(context.Background(), f.run, agent.UserText("refund"), opts...)
}

var twoOfThree = &agent.ApprovalPolicy{Need: 2, Approvers: approvers}

// run runs bide approve with args against the fixture's journal; stdin is the confirmation.
func (f *fixture) run_(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = Main(append([]string{"-db", f.db, "-color", "never"}, args...), strings.NewReader(stdin), &out, &errb, stores)
	return code, out.String(), errb.String()
}

// as is the flags of a decision by approver id, checked against the enrolled keys (-approver-keys).
func (f *fixture) as(id string, extra ...string) []string {
	return append([]string{"-run", f.run, "-call", f.call, "-as", id, "-key", f.keys[id], "-approver-keys", f.keysJSON}, extra...)
}

// asUnverified is as without the enrolled keys, which needs the explicit -unverified.
func (f *fixture) asUnverified(id string, extra ...string) []string {
	return append([]string{"-run", f.run, "-call", f.call, "-as", id, "-key", f.keys[id], "-unverified"}, extra...)
}

// decisions returns the approval decision records on the call in the journal.
func (f *fixture) decisions() []agent.Record {
	j, closeJ := f.store()
	defer closeJ()
	recs, err := j.History(context.Background(), f.run)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []agent.Record
	for _, r := range recs {
		if r.Kind == agent.StepApproval && r.ToolUseID == f.call {
			out = append(out, r)
		}
	}
	return out
}

var stores = Stores{
	SQLite: func(path string, readOnly bool) (agent.Store, io.Closer, error) {
		var s *sqlite.Store
		var err error
		if readOnly {
			s, err = sqlite.OpenReadOnly(path)
		} else {
			s, err = sqlite.Open(path)
		}
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	},
}

func contains(t *testing.T, what, text string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(text, s) {
			t.Errorf("%s lacks %q:\n%s", what, s, text)
		}
	}
}

// TestEndToEnd: a call waits at a 2-of-3 gate; two of three approvers decide with bide approve; the
// gate passes; the run resumes and the call runs exactly once.
func TestEndToEnd(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)

	code, out, errs := f.run_("", "-list")
	if code != 0 {
		t.Fatalf("-list = %d: %s", code, errs)
	}
	contains(t, "-list", out, "run refund-1049", "call c2", "tool refund", "2 of 3", "pending: ops, finance, risk",
		"bide approve -db "+word(f.db)+" -run refund-1049 -call c2 -as ops -key '<key-file>' -approver-keys '<keys-file>'")

	// ops approves, confirming by typing the approver id.
	code, out, errs = f.run_("ops\n", f.as("ops", "-approver-keys", f.keysJSON)...)
	if code != 0 {
		t.Fatalf("ops: %d\n%s\n%s", code, out, errs)
	}
	contains(t, "ops output", out, "Approve this call?", `"amount": 120`, "refund <ok>", "args SHA-256", "verified tally: 1 of 2 needed approved by ops", "still pending: finance, risk")
	if got := len(f.decisions()); got != 1 {
		t.Fatalf("%d decisions after ops, want 1", got)
	}
	if f.charges.Load() != 0 {
		t.Fatal("the call ran before the gate passed")
	}
	if _, err := f.drive(twoOfThree, false); err == nil {
		t.Fatal("the run resumed with one of two approvals")
	}

	// finance approves with -yes.
	code, out, errs = f.run_("", f.as("finance", "-yes", "-approver-keys", f.keysJSON)...)
	if code != 0 {
		t.Fatalf("finance: %d\n%s\n%s", code, out, errs)
	}
	contains(t, "finance output", out, "verified tally: 2 of 2 needed approved by ops, finance", "the gate has its approvals", "re-run run refund-1049")

	// No more decisions are needed: risk is refused, and the refusal says why.
	code, _, errs = f.run_("", f.as("risk", "-yes", "-approver-keys", f.keysJSON)...)
	if code != 1 {
		t.Fatalf("risk after quorum = %d, want 1", code)
	}
	contains(t, "risk refusal", errs, "already has its 2 approval(s) (verified)")

	// The run resumes and the call runs once.
	res, err := f.drive(twoOfThree, false)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := f.charges.Load(); got != 1 {
		t.Fatalf("the call ran %d times, want 1", got)
	}
	_ = res

	// The list no longer shows it, and a decision on it is refused: the run ended.
	if _, out, _ := f.run_("", "-list"); !strings.Contains(out, "no call waits") {
		t.Fatalf("-list after the run: %s", out)
	}
	if code, _, errs := f.run_("", f.as("risk", "-yes")...); code != 1 || !strings.Contains(errs, "has ended") {
		t.Fatalf("approve on an ended run = %d: %s", code, errs)
	}
}

// TestSignedBytesComeFromTheJournal: what is signed is the journal's call, whatever the flags say.
// There is no flag for the arguments, the tool or the digest; the decision recorded verifies
// (audit's counting rule, with the enrolled keys) over the journal's own call, and the digest shown
// is the SHA-256 of that call's canonical arguments.
func TestSignedBytesComeFromTheJournal(t *testing.T) {
	f := newFixture(t, twoOfThree, `{"order":1049,"amount":120,"zeta":1.50,"alpha":"a"}`)
	for _, flag := range []string{"-args", "-tool", "-digest", "-hash", "-amount"} {
		if code, _, errs := f.run_("", append(f.as("ops", "-yes"), flag, "x")...); code != 2 || !strings.Contains(errs, "flag provided but not defined") {
			t.Fatalf("%s: code %d: %s", flag, code, errs)
		}
	}
	if n := len(f.decisions()); n != 0 {
		t.Fatalf("a usage error recorded %d decisions", n)
	}
	code, out, errs := f.run_("", f.as("ops", "-yes", "-approver-keys", f.keysJSON)...)
	if code != 0 {
		t.Fatalf("%d: %s\n%s", code, out, errs)
	}
	// The canonical arguments: keys sorted, numbers kept as written.
	sum := sha256.Sum256([]byte(`{"alpha":"a","amount":120,"order":1049,"zeta":1.50}`))
	contains(t, "output", out, "args SHA-256 "+hex.EncodeToString(sum[:]))

	j, closeJ := f.store()
	defer closeJ()
	recs, _ := j.History(context.Background(), f.run)
	_, call, ok := agent.FindToolCall(recs, f.call)
	if !ok || call.Name != "refund" {
		t.Fatalf("journal call: %+v", call)
	}
	subject := agent.ApprovalSubject{RunID: f.run, ToolUseID: f.call, ToolName: call.Name, Args: call.Args}
	verify := func(id string) (agent.ApproverVerifier, bool) { v, ok := f.pubs[id]; return v, ok }
	tally, checks := agent.TallyApprovals(recs, subject, *twoOfThree, verify)
	if tally.Approved != 1 || len(checks) != 1 || !checks[0].Counted {
		t.Fatalf("the recorded decision does not count under the gate's rule: %+v %+v", tally, checks)
	}
}

// TestRefusals: every refusal records nothing and says why.
func TestRefusals(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	rec := func(name string, wantCode int, wantErr string, stdin string, args ...string) {
		t.Helper()
		before := len(f.decisions())
		code, _, errs := f.run_(stdin, args...)
		if code != wantCode || !strings.Contains(errs, wantErr) {
			t.Errorf("%s: code %d (want %d), stderr %q (want %q)", name, code, wantCode, errs, wantErr)
		}
		if after := len(f.decisions()); after != before {
			t.Errorf("%s: recorded %d decisions", name, after-before)
		}
	}
	rec("unknown call", 1, "has no approval gate on call nope; its gated calls: c2", "", "-run", f.run, "-call", "nope", "-as", "ops", "-key", f.keys["ops"], "-yes")
	rec("unknown run", 1, "is not in this journal", "", "-run", "ghost", "-call", f.call, "-as", "ops", "-key", f.keys["ops"], "-yes")
	rec("not an approver", 1, `"mallory" is not an approver of the gate on call c2; its approvers are: ops, finance, risk`, "", f.as("ops", "-yes", "-as", "mallory")...)
	rec("not confirmed", 1, "not confirmed", "no\n", f.as("ops")...)
	rec("end of input is not confirmation", 1, "not confirmed", "", f.as("ops")...)
	rec("another id is not confirmation", 1, "not confirmed", "finance\n", f.as("ops")...)
	rec("as without key", 2, "-as and -key go together", "", "-run", f.run, "-call", f.call, "-as", "ops")
	rec("no call", 2, "no -call given", "", "-run", f.run)
	rec("two journals", 2, "exactly one of", "", "-postgres-env", "X", "-run", f.run, "-call", f.call)

	// Already decided: ops decides, then asks again.
	if code, _, errs := f.run_("", f.as("ops", "-yes")...); code != 0 {
		t.Fatalf("ops: %s", errs)
	}
	rec("already recorded (same verdict, verified)", 0, "", "", f.as("ops", "-yes")...)
	rec("already decided, rejecting", 1, "ops already decided on call c2: approved at 20", "", f.as("ops", "-yes", "-reject")...)
	// The refusal names the recorded time, and with -approver-keys it checks the signature.
	rec("already decided (unverified)", 1, "already decided", "", f.asUnverified("ops", "-yes")...)
	rec("already recorded (verified)", 0, "", "", f.as("ops", "-yes", "-approver-keys", f.keysJSON)...)

	// A single-approver gate takes neither -as nor -key; and a missing -as on an m-of-n gate.
	rec("m-of-n without -as", 1, "is a 2-of-3 gate: give -as", "", "-run", f.run, "-call", f.call, "-yes")
}

// TestWrongKeyIsRefusedBeforeRecording: with -approver-keys, a key that is not the one enrolled for
// the approver (another approver's, a tampered file, a fresh key) records nothing.
func TestWrongKeyIsRefusedBeforeRecording(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	// ops signs with finance's key.
	code, _, errs := f.run_("", "-run", f.run, "-call", f.call, "-as", "ops", "-key", f.keys["finance"], "-yes", "-approver-keys", f.keysJSON)
	if code != 1 || !strings.Contains(errs, `is not the key enrolled for "ops"`) {
		t.Fatalf("finance's key as ops: %d %s", code, errs)
	}
	// A tampered key file: one character of ops's seed changed.
	seed, _ := os.ReadFile(f.keys["ops"])
	bad := []byte(strings.TrimSpace(string(seed)))
	if bad[0] == '0' {
		bad[0] = '1'
	} else {
		bad[0] = '0'
	}
	tampered := filepath.Join(f.dir, "tampered.key")
	if err := os.WriteFile(tampered, append(bad, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs = f.run_("", "-run", f.run, "-call", f.call, "-as", "ops", "-key", tampered, "-yes", "-approver-keys", f.keysJSON)
	if code != 1 || !strings.Contains(errs, `is not the key enrolled for "ops"`) {
		t.Fatalf("tampered key: %d %s", code, errs)
	}
	// An approver the keys file does not list.
	short := filepath.Join(f.dir, "short.json")
	_ = os.WriteFile(short, []byte(`{"finance":"`+publicKeyTextOf(t, f.keys["finance"])+`"}`), 0o600)
	code, _, errs = f.run_("", f.as("ops", "-yes", "-approver-keys", short)...)
	if code != 1 || !strings.Contains(errs, `has no key for "ops"`) {
		t.Fatalf("unlisted approver: %d %s", code, errs)
	}
	// A keys file that is not a key fails closed.
	junk := filepath.Join(f.dir, "junk.json")
	_ = os.WriteFile(junk, []byte(`{"ops":"nonsense"}`), 0o600)
	if code, _, errs = f.run_("", f.as("ops", "-yes", "-approver-keys", junk)...); code != 1 {
		t.Fatalf("junk keys: %d %s", code, errs)
	}
	if n := len(f.decisions()); n != 0 {
		t.Fatalf("%d decisions recorded", n)
	}

	// Without -approver-keys the wrong key cannot be known: the decision is recorded, never counts
	// (the gate's rule), and does not take the real approver's place; the output warned about it.
	code, out, _ := f.run_("", "-run", f.run, "-call", f.call, "-as", "ops", "-key", f.keys["finance"], "-yes", "-unverified")
	if code != 0 {
		t.Fatal("expected the unchecked wrong-key decision to be recorded")
	}
	contains(t, "warning", out, "UNVERIFIED", "not checked against the key enrolled")
	recs := f.allRecords()
	subject := agent.ApprovalSubject{RunID: f.run, ToolUseID: f.call, ToolName: "refund", Args: json.RawMessage(refundArgs)}
	verify := func(id string) (agent.ApproverVerifier, bool) { v, ok := f.pubs[id]; return v, ok }
	if tally, _ := agent.TallyApprovals(recs, subject, *twoOfThree, verify); tally.Approved != 0 {
		t.Fatalf("a wrong-key decision counted: %+v", tally)
	}
	// With the keys file, the real ops is not blocked by that junk record.
	if code, _, errs := f.run_("", f.as("ops", "-yes", "-approver-keys", f.keysJSON)...); code != 0 {
		t.Fatalf("real ops after a junk record: %d %s", code, errs)
	}
}

func (f *fixture) allRecords() []agent.Record {
	j, closeJ := f.store()
	defer closeJ()
	recs, err := j.History(context.Background(), f.run)
	if err != nil {
		f.t.Fatal(err)
	}
	return recs
}

func publicKeyTextOf(t *testing.T, keyFile string) string {
	t.Helper()
	k, err := LoadSigner(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return publicKeyText(k)
}

// TestRejectDeniesTheCall: two of three reject; the call can no longer pass and the resumed run
// never runs it.
func TestRejectDeniesTheCall(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	code, out, errs := f.run_("", f.as("ops", "-yes", "-reject", "-approver-keys", f.keysJSON)...)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	contains(t, "reject", out, "Rejected call c2", "verified tally: 0 of 2 needed approved; denied by ops")
	code, out, errs = f.run_("", f.as("finance", "-yes", "-reject", "-approver-keys", f.keysJSON)...)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	contains(t, "second reject", out, "can no longer pass", "the call is denied")
	if _, err := f.drive(twoOfThree, false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if f.charges.Load() != 0 {
		t.Fatal("a rejected call ran")
	}
}

// TestConfirmationPrompt: the prompt names the action; a rejection says REJECT.
func TestConfirmationPrompt(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	_, out, _ := f.run_("y\n", f.as("ops", "-reject")...)
	contains(t, "prompt", out, "REJECT this call?", "Type ops (or y) to REJECT: ")
}

// TestKeyFileModeRefused: a private key that group or others can read is refused, as ssh does.
func TestKeyFileModeRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not enforced on Windows")
	}
	f := newFixture(t, twoOfThree, refundArgs)
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o666, 0o707} {
		if err := os.Chmod(f.keys["ops"], mode); err != nil {
			t.Fatal(err)
		}
		code, _, errs := f.run_("", f.as("ops", "-yes")...)
		if code != 1 || !strings.Contains(errs, "accessible by others") || !strings.Contains(errs, "chmod 600") {
			t.Errorf("mode %04o: code %d: %s", mode, code, errs)
		}
	}
	if n := len(f.decisions()); n != 0 {
		t.Fatalf("%d decisions recorded", n)
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		if err := os.Chmod(f.keys["ops"], mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSigner(f.keys["ops"]); err != nil {
			t.Errorf("mode %04o refused: %v", mode, err)
		}
	}
}

// TestKeyFileErrorsNeverShowTheKey: a malformed key file is refused without echoing any of it,
// including the one character a hex decoder names.
func TestKeyFileErrorsNeverShowTheKey(t *testing.T) {
	dir := t.TempDir()
	secret := strings.Repeat("ab", 31) + "zq" // right length, not hex
	for name, content := range map[string]string{
		"not hex":   secret + "\n",
		"short":     strings.Repeat("ab", 16) + "\n",
		"long":      strings.Repeat("ab", 40) + "\n",
		"empty":     "",
		"binary":    "\x00\x01\x02",
		"huge":      strings.Repeat("a", 5000),
		"with text": "key: " + strings.Repeat("ab", 32),
		"padded":    strings.Repeat("ab", 32) + strings.Repeat(" ", 2000),
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadSigner(path)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		msg := err.Error()
		if strings.Contains(msg, "invalid byte") || strings.Contains(msg, "abab") || strings.Contains(msg, "zq") || strings.Contains(msg, "'z'") {
			t.Errorf("%s: the error shows key material: %s", name, msg)
		}
	}
	if _, err := LoadSigner(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a directory: %v", err)
	}
	if _, err := LoadSigner(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file was accepted")
	}
}

// TestDisplayIsSafe: arguments with terminal escapes and bidirectional controls are shown escaped,
// and a long argument is cut with a note that the signature binds all of it.
func TestDisplayIsSafe(t *testing.T) {
	long := strings.Repeat("x", 5000)
	args, _ := json.Marshal(map[string]any{"memo": "\x1b[2J\x1b[Happroved by finance \u202e \u009b31m \x7f \u200b", "long": long, "bell": "\a"})
	f := newFixture(t, twoOfThree, string(args))
	_, out, _ := f.run_("n\n", f.as("ops")...)
	if strings.ContainsAny(out, "\x1b\a\u202e\u009b\x7f\u200b") {
		t.Fatalf("raw control characters reached the terminal: %q", out)
	}
	contains(t, "output", out, `\u202e`, "more bytes not shown", "-full shows everything")
	_, full, _ := f.run_("n\n", f.as("ops", "-full")...)
	if !strings.Contains(full, long) || strings.Contains(full, "not shown") {
		t.Fatal("-full did not show all of the arguments")
	}
	if strings.ContainsAny(full, "\x1b\a\u202e\u009b\x7f\u200b") {
		t.Fatalf("raw control characters reached the terminal with -full: %q", full)
	}
}

// TestSingleApproverGate: a gate with one decision (agent.Approve) is decided, unsigned, with no -as
// or -key.
func TestSingleApproverGate(t *testing.T) {
	f := newFixture(t, agent.SingleApproval(), refundArgs)
	_, out, _ := f.run_("", "-list")
	contains(t, "-list", out, "one approver, one decision", "bide approve -db "+word(f.db)+" -run refund-1049 -call c2\n")
	if strings.Contains(out, "-as") {
		t.Fatalf("a single gate's command names an approver: %s", out)
	}
	if code, _, errs := f.run_("", f.as("ops", "-yes")...); code != 1 || !strings.Contains(errs, "single-approver gate") {
		t.Fatalf("-as on a single gate: %d %s", code, errs)
	}
	code, out, errs := f.run_("y\n", "-run", f.run, "-call", f.call)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	contains(t, "single", out, "Type y to approve", "unsigned", "the gate has its approvals")
	if code, _, errs := f.run_("", "-run", f.run, "-call", f.call, "-yes"); code != 1 || !strings.Contains(errs, "ended") && !strings.Contains(errs, "already has its decision") {
		t.Fatalf("second decision: %d %s", code, errs)
	}
	if _, err := f.drive(agent.SingleApproval(), false); err != nil {
		t.Fatal(err)
	}
	if f.charges.Load() != 1 {
		t.Fatalf("ran %d times", f.charges.Load())
	}
}

// TestListJSON: -json is an array with the commands, and a postgres DSN never appears in a command.
func TestListJSON(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	code, out, errs := f.run_("", "-list", "-json")
	if code != 0 {
		t.Fatal(errs)
	}
	var got []waiting
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 1 {
		t.Fatalf("%v: %s", err, out)
	}
	w := got[0]
	if w.Run != f.run || w.Call != f.call || w.Tool != "refund" || w.Need != 2 || len(w.Pending) != 3 || len(w.Commands) != 3 {
		t.Fatalf("%+v", w)
	}

	// A literal -postgres DSN: the printed commands name the variable, never the DSN.
	var pg bytes.Buffer
	stub := Stores{Postgres: func(dsn string, readOnly bool) (agent.Store, io.Closer, error) { return stores.SQLite(f.db, readOnly) }}
	code = Main([]string{"-postgres", "postgres://u:hunter2@h/db", "-list", "-color", "never"}, strings.NewReader(""), &pg, io.Discard, stub)
	if code != 0 || strings.Contains(pg.String(), "hunter2") || !strings.Contains(pg.String(), "-postgres-env BIDE_POSTGRES_DSN") {
		t.Fatalf("%d: %s", code, pg.String())
	}
	// An open error never shows the DSN.
	var errb bytes.Buffer
	bad := Stores{Postgres: func(dsn string, readOnly bool) (agent.Store, io.Closer, error) {
		return nil, nil, errors.New("dial " + dsn)
	}}
	code = Main([]string{"-postgres", "postgres://u:hunter2@h/db", "-list"}, strings.NewReader(""), io.Discard, &errb, bad)
	if code != 1 || strings.Contains(errb.String(), "hunter2") {
		t.Fatalf("%d: %s", code, errb.String())
	}
	// -postgres-env with an unset variable.
	t.Setenv("BIDE_TEST_UNSET_DSN", "")
	code = Main([]string{"-postgres-env", "BIDE_TEST_UNSET_DSN", "-list"}, strings.NewReader(""), io.Discard, &errb, stub)
	if code != 1 || !strings.Contains(errb.String(), "BIDE_TEST_UNSET_DSN") {
		t.Fatalf("%d: %s", code, errb.String())
	}
}

// TestMissingJournalIsNotCreated: a mistyped -db is an error, not a new empty journal.
func TestMissingJournalIsNotCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "typo.db")
	var errb bytes.Buffer
	code := Main([]string{"-db", path, "-run", "r", "-call", "c", "-as", "ops", "-key", "k", "-yes"}, strings.NewReader(""), io.Discard, &errb, stores)
	if code != 1 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("bide approve created the journal")
	}
	code = Main([]string{"-db", path, "-list"}, strings.NewReader(""), io.Discard, &errb, stores)
	if code != 1 {
		t.Fatalf("-list code %d", code)
	}
}

// TestDecisionTimeIsLocalCanonical: the decision is stamped by the local clock in canonical form,
// and is signed with it.
func TestDecisionTimeIsLocalCanonical(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 14, 3, 123_456_789, time.FixedZone("x", 3600))
	old := now
	now = func() time.Time { return at }
	defer func() { now = old }()
	f := newFixture(t, twoOfThree, refundArgs)
	if code, _, errs := f.run_("", f.as("ops", "-yes", "-approver-keys", f.keysJSON)...); code != 0 {
		t.Fatal(errs)
	}
	ds := f.decisions()
	if len(ds) != 1 || ds[0].DecidedAt() != "2026-10-13T11:14:03.123Z" {
		t.Fatalf("%+v", ds)
	}
}

// TestKeygen: the private key is 0600 and never printed; the public key is printed in the form
// policies read; nothing is overwritten.
func TestKeygen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ops.key")
	var out, errb bytes.Buffer
	if code := KeygenMain([]string{"-out", path, "-as", "ops", "-color", "never"}, &out, &errb); code != 0 {
		t.Fatalf("%d: %s", code, errb.String())
	}
	seed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String()+errb.String(), strings.TrimSpace(string(seed))) {
		t.Fatal("the private key was printed")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("private key mode %04o", fi.Mode().Perm())
		}
	}
	priv, err := LoadSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	pub := publicKeyText(priv)
	contains(t, "keygen output", out.String(), "public key  "+pub, `{"ops": "`+pub+`"}`)
	pubFile, _ := os.ReadFile(path + ".pub")
	if strings.TrimSpace(string(pubFile)) != pub {
		t.Fatalf(".pub = %q", pubFile)
	}
	v, err := audit.ParsePublicKey(pub)
	if err != nil {
		t.Fatalf("the printed public key does not parse: %v", err)
	}
	msg := []byte("m")
	sig, err := priv.Sign(msg)
	if err != nil || !v.Verify(msg, sig) {
		t.Fatalf("the key pair does not match: %v", err)
	}
	// No overwrite, of either file, and nothing is left behind by a refusal.
	for _, victim := range []string{path, path + ".pub"} {
		out.Reset()
		errb.Reset()
		other := filepath.Join(dir, "second.key")
		if victim == path+".pub" {
			_ = os.Remove(other)
			_ = os.WriteFile(other+".pub", []byte("keep"), 0o644)
		}
		target := path
		if victim != path {
			target = other
		}
		before, _ := os.ReadFile(victim)
		if code := KeygenMain([]string{"-out", target}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "does not overwrite") {
			t.Fatalf("%s: %d %s", victim, code, errb.String())
		}
		after, _ := os.ReadFile(victim)
		if !bytes.Equal(before, after) {
			t.Fatalf("%s was changed", victim)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "second.key")); err == nil {
		t.Fatal("a refused keygen left a private key behind")
	}
	// Usage errors.
	for _, args := range [][]string{{}, {"-out", filepath.Join(dir, "x"), "-as", "has space"}, {"extra"}} {
		if code := KeygenMain(args, io.Discard, io.Discard); code != 2 {
			t.Errorf("%v: %d", args, code)
		}
	}
}

// TestArgsDigest: the shown digest is the SHA-256 of the canonical arguments, taken from the signed
// bytes.
func TestArgsDigest(t *testing.T) {
	s := agent.ApprovalSubject{RunID: "r", ToolUseID: "c", ToolName: "t", Args: json.RawMessage(` {"b": 1, "a": "<&>"} `)}
	sum := sha256.Sum256([]byte(`{"a":"<&>","b":1}`))
	if got := argsDigest(s); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("got %s", got)
	}
	msg := agent.ApprovalDecisionBytes(s, "ops", true, time.Now())
	if !digestAt(msg, s, argsDigest(s)) {
		t.Fatal("digestAt disagrees with the signed layout")
	}
	s.Args = nil
	empty := sha256.Sum256([]byte(`{}`))
	if got := argsDigest(s); got != hex.EncodeToString(empty[:]) {
		t.Fatalf("empty args: %s", got)
	}
}

// TestKeysRequired: an m-of-n decision needs the enrolled keys (-approver-keys), or an explicit
// -unverified. Without either it is refused before the key file is read and nothing is recorded; the
// refusal names the file bide view reads and where bide keygen prints the entry.
func TestKeysRequired(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	args := []string{"-run", f.run, "-call", f.call, "-as", "ops", "-key", f.keys["ops"], "-yes"}
	code, _, errs := f.run_("", args...)
	if code != 1 {
		t.Fatalf("code %d: %s", code, errs)
	}
	contains(t, "refusal", errs, "-approver-keys", "bide view", "bide keygen", "-unverified")
	// Refused before the key is read: a missing key file gives the same refusal.
	if code, _, errs := f.run_("", "-run", f.run, "-call", f.call, "-as", "ops", "-key", filepath.Join(f.dir, "absent.key"), "-yes"); code != 1 || !strings.Contains(errs, "-approver-keys") {
		t.Fatalf("missing key, no keys: %d %s", code, errs)
	}
	if n := len(f.decisions()); n != 0 {
		t.Fatalf("%d decisions recorded", n)
	}
	// The two are exclusive, and -unverified is for deciding only.
	if code, _, errs := f.run_("", f.as("ops", "-yes", "-unverified")...); code != 2 || !strings.Contains(errs, "-unverified") {
		t.Fatalf("both: %d %s", code, errs)
	}
	if code, _, errs := f.run_("", "-list", "-unverified"); code != 2 || !strings.Contains(errs, "-unverified") {
		t.Fatalf("-list -unverified: %d %s", code, errs)
	}
	// -unverified keeps the old behavior: recorded, warned, and the tally is labelled unverified.
	code, out, errs := f.run_("", f.asUnverified("ops", "-yes")...)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	contains(t, "unverified", out, "UNVERIFIED", "not checked against the key enrolled", "recorded decisions: 1 of 2 needed approved by ops", "counted from the journal")
	if n := len(f.decisions()); n != 1 {
		t.Fatalf("%d decisions", n)
	}
}

// TestSingleGateNeedsNoKeys: a single-approver gate has no signature to check, so neither flag.
func TestSingleGateNeedsNoKeys(t *testing.T) {
	f := newFixture(t, agent.SingleApproval(), refundArgs)
	if code, _, errs := f.run_("", "-run", f.run, "-call", f.call, "-yes", "-unverified"); code != 1 && code != 2 || !strings.Contains(errs, "-unverified") {
		t.Fatalf("-unverified on a single gate: %d %s", code, errs)
	}
	if code, _, errs := f.run_("", "-run", f.run, "-call", f.call, "-yes"); code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
}

// TestSchemes: an approver's key can be Ed25519, ML-DSA-65 or a hybrid. bide keygen makes each, the
// key file round-trips through the audit.Signer the command signs with, and a 2-of-3 gate of keys
// in that scheme passes and runs the call, with the tally verified under the scheme.
func TestSchemes(t *testing.T) {
	for _, alg := range algs {
		t.Run(string(alg), func(t *testing.T) {
			f := newFixtureAlg(t, twoOfThree, refundArgs, alg)
			sg, err := LoadSigner(f.keys["ops"])
			if err != nil || sg.Alg() != alg {
				t.Fatalf("LoadSigner: %v %v", sg, err)
			}
			pubFile, _ := os.ReadFile(f.keys["ops"] + ".pub")
			if v, err := audit.ParsePublicKey(string(pubFile)); err != nil || v.Alg() != alg {
				t.Fatalf("the .pub file does not parse under %s: %v", alg, err)
			}
			for _, id := range []string{"ops", "finance"} {
				if code, out, errs := f.run_("", f.as(id, "-yes")...); code != 0 {
					t.Fatalf("%s: %d %s\n%s", id, code, errs, out)
				} else if id == "finance" {
					contains(t, "tally", out, "verified tally: 2 of 2 needed approved by ops, finance")
				}
			}
			recs := f.decisions()
			if len(recs) != 2 || recs[0].ApproverAlg() != alg {
				t.Fatalf("recorded under %q, want %q", recs[0].ApproverAlg(), alg)
			}
			if _, err := f.drive(twoOfThree, false); err != nil || f.charges.Load() != 1 {
				t.Fatalf("resume: %v, ran %d times", err, f.charges.Load())
			}
		})
	}
}

// TestSchemeMismatch: a key of another scheme than the one enrolled for the approver is refused
// before recording, and the refusal names both key identities, not the keys.
func TestSchemeMismatch(t *testing.T) {
	f := newFixtureAlg(t, twoOfThree, refundArgs, audit.AlgEd25519)
	other := filepath.Join(f.dir, "ml.key")
	if _, err := writeKeyPair(other, audit.AlgMLDSA65); err != nil {
		t.Fatal(err)
	}
	code, _, errs := f.run_("", "-run", f.run, "-call", f.call, "-as", "ops", "-key", other, "-yes", "-approver-keys", f.keysJSON)
	if code != 1 || !strings.Contains(errs, "ml-dsa-65") || !strings.Contains(errs, "is not the key enrolled") {
		t.Fatalf("%d %s", code, errs)
	}
	if len(errs) > 1000 {
		t.Fatalf("the refusal prints whole public keys (%d bytes)", len(errs))
	}
	if n := len(f.decisions()); n != 0 {
		t.Fatalf("%d decisions", n)
	}
}

// TestKeyLinesNeverShowKeyMaterial: malformed hybrid and ML-DSA lines are refused without echoing
// any part of them.
func TestKeyLinesNeverShowKeyMaterial(t *testing.T) {
	dir := t.TempDir()
	good := strings.Repeat("ab", 32)
	for name, content := range map[string]string{
		"ml not hex":      "ml-dsa-65:" + strings.Repeat("ab", 31) + "zq",
		"ml short":        "ml-dsa-65:" + strings.Repeat("ab", 16),
		"hybrid one seed": "ed25519+ml-dsa-65:" + good,
		"hybrid three":    "ed25519+ml-dsa-65:" + good + ":" + good + ":" + good,
		"hybrid bad half": "ed25519+ml-dsa-65:" + good + ":" + strings.Repeat("cd", 31) + "zq",
		"p256 zero":       "ecdsa-p256:" + strings.Repeat("00", 32),
		"p256 above n":    "ecdsa-p256:" + strings.Repeat("ff", 32),
		"p256 short":      "ecdsa-p256:" + strings.Repeat("ab", 16),
		"p256 not hex":    "ecdsa-p256:" + strings.Repeat("ab", 31) + "zq",
		"p256 two":        "ecdsa-p256:" + good + ":" + good,
		"unknown scheme":  "rsa:" + good,
		"ed25519 two":     good + ":" + good,
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadSigner(path)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		for _, leak := range []string{"zq", "abab", "cdcd", "invalid byte"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: the error shows key material (%q): %v", name, leak, err)
			}
		}
	}
}

// TestKeygenAlg: -alg picks the scheme; an unknown one is a usage error; the private key is never
// printed in any scheme.
func TestKeygenAlg(t *testing.T) {
	dir := t.TempDir()
	for _, alg := range algs {
		path := filepath.Join(dir, strings.NewReplacer("+", "_", "-", "_").Replace(string(alg))+".key")
		var out, errb bytes.Buffer
		if code := KeygenMain([]string{"-out", path, "-alg", string(alg), "-as", "ops", "-color", "never"}, &out, &errb); code != 0 {
			t.Fatalf("%s: %d %s", alg, code, errb.String())
		}
		raw, _ := os.ReadFile(path)
		for _, part := range strings.Split(strings.TrimSpace(string(raw)), ":") {
			if len(part) == seedHexLen && strings.Contains(out.String(), part) {
				t.Fatalf("%s: the private key was printed", alg)
			}
		}
		contains(t, string(alg), out.String(), "public key  "+string(alg)+":")
	}
	if code := KeygenMain([]string{"-out", filepath.Join(dir, "x"), "-alg", "rsa"}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("unknown -alg: %d", code)
	}
}

// receiptsIn returns the receipt files in dir.
func receiptsIn(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "*.receipt.json"))
	return m
}

func readReceipt(t *testing.T, path string) audit.DecisionReceipt {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r audit.DecisionReceipt
	if err := audit.UnmarshalStrict(b, &r); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return r
}

// TestReceipt: a signed decision leaves a receipt next to the key: mode 0600, no key material, the
// journal position of the record, and verifiable on its own.
func TestReceipt(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	code, out, errs := f.run_("", f.as("ops", "-yes")...)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	got := receiptsIn(t, f.dir)
	if len(got) != 1 {
		t.Fatalf("receipts: %v", got)
	}
	if base := filepath.Base(got[0]); filepath.Dir(got[0]) != f.dir || !strings.HasPrefix(base, "refund-1049-c2-ops-20") || !strings.HasSuffix(base, "Z.receipt.json") {
		t.Fatalf("default path %s", got[0])
	}
	contains(t, "output", out, "receipt "+got[0], "signed into the audit log at the app's next write to this run")
	if strings.Contains(out, "re-run it") {
		t.Fatalf("the audit line tells the approver to re-run: %s", out)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(got[0]); fi.Mode().Perm() != 0o600 {
			t.Fatalf("receipt mode %04o", fi.Mode().Perm())
		}
	}
	raw, _ := os.ReadFile(got[0])
	seed, _ := os.ReadFile(f.keys["ops"])
	if strings.Contains(string(raw), strings.TrimSpace(string(seed))) {
		t.Fatal("the receipt holds key material")
	}
	r := readReceipt(t, got[0])
	if err := r.Verify(f.pubs["ops"]); err != nil {
		t.Fatal(err)
	}
	if r.RunID != f.run || r.ToolUseID != f.call || r.Approver != "ops" || !r.Approved || r.ToolName != "refund" || r.Record == "" || r.RecordedAt == "" {
		t.Fatalf("%+v", r)
	}
	// The record is where the receipt says.
	recs := f.allRecords()
	if r.Index <= 0 || r.Index >= len(recs) || recs[r.Index].Name != r.Record {
		t.Fatalf("index %d does not point at record %q", r.Index, r.Record)
	}
	// It checks against the journal: not yet in any signed head, which is pending, not a failure.
	if fnd := audit.CheckReceipt(r, recs, nil, f.pubs["ops"]); fnd.Verdict != audit.ReceiptPending {
		t.Fatalf("%+v", fnd)
	}

	// -verify-receipt checks it on its own.
	var o, e bytes.Buffer
	if code := Main([]string{"-verify-receipt", got[0], "-approver-keys", f.keysJSON, "-color", "never"}, strings.NewReader(""), &o, &e, stores); code != 0 {
		t.Fatalf("verify: %d %s", code, e.String())
	}
	contains(t, "verify", o.String(), "signature verifies", "ops approved call c2", "args SHA-256 "+r.ArgsSHA)
	// Any change to what it states fails it.
	r.Approved = false
	b, _ := json.Marshal(r)
	tampered := filepath.Join(f.dir, "tampered.json")
	_ = os.WriteFile(tampered, b, 0o600)
	o.Reset()
	e.Reset()
	if code := Main([]string{"-verify-receipt", tampered, "-approver-keys", f.keysJSON}, strings.NewReader(""), &o, &e, stores); code != 1 {
		t.Fatalf("tampered receipt: %d %s", code, o.String())
	}
	// The wrong approver's keys, no keys, and a journal flag beside it are refused.
	for _, args := range [][]string{
		{"-verify-receipt", got[0]},
		{"-verify-receipt", got[0], "-approver-keys", f.keysJSON, "-db", f.db},
		{"-verify-receipt", got[0], "-approver-keys", f.keysJSON, "-run", "x"},
		{"-verify-receipt", got[0], "-approver-keys", f.keysJSON, "-list"},
	} {
		if code := Main(args, strings.NewReader(""), io.Discard, io.Discard, stores); code != 2 {
			t.Errorf("%v: %d, want 2", args, code)
		}
	}
	if code := Main([]string{"-verify-receipt", got[0], "-approver-keys", f.keysJSON}, nil, io.Discard, io.Discard, stores); code != 0 {
		t.Errorf("nil stdin: %d", code)
	}
	// Another approver's key does not verify it.
	other := filepath.Join(f.dir, "finance-only.json")
	_ = os.WriteFile(other, []byte(`{"ops":"`+publicKeyTextOf(t, f.keys["finance"])+`"}`), 0o600)
	if code := Main([]string{"-verify-receipt", got[0], "-approver-keys", other}, strings.NewReader(""), io.Discard, io.Discard, stores); code != 1 {
		t.Errorf("another key verified the receipt: %d", code)
	}
}

// TestReceiptPath: -receipt names the file; one that exists is never overwritten, and that is found
// before anything is signed or recorded.
func TestReceiptPath(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	taken := filepath.Join(f.dir, "mine.json")
	if err := os.WriteFile(taken, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := f.run_("", f.as("ops", "-yes", "-receipt", taken)...)
	if code != 1 || !strings.Contains(errs, "exists") || !strings.Contains(errs, "-receipt") {
		t.Fatalf("%d %s", code, errs)
	}
	if n := len(f.decisions()); n != 0 {
		t.Fatalf("%d decisions recorded before the receipt path was refused", n)
	}
	if b, _ := os.ReadFile(taken); string(b) != "keep" {
		t.Fatal("the existing file was overwritten")
	}
	fresh := filepath.Join(f.dir, "sub-nonexistent-dir", "r.json")
	if code, _, errs := f.run_("", f.as("ops", "-yes", "-receipt", fresh)...); code != 1 || len(f.decisions()) != 0 {
		t.Fatalf("a receipt path in a missing directory: %d %s (%d decisions)", code, errs, len(f.decisions()))
	}
	ok := filepath.Join(f.dir, "ok.json")
	if code, out, errs := f.run_("", f.as("ops", "-yes", "-receipt", ok)...); code != 0 || !strings.Contains(out, ok) {
		t.Fatalf("%d %s\n%s", code, errs, out)
	}
	if _, err := os.Stat(ok); err != nil {
		t.Fatal(err)
	}
	// Ids with path characters cannot place the default receipt anywhere but next to the key.
	name := defaultReceiptName("../x/y", "node:review", "ops", "2026-10-13T12:14:03.123Z")
	if strings.ContainsAny(name, `/\:`) || !strings.HasSuffix(name, ".receipt.json") {
		t.Fatalf("default name %q has path characters", name)
	}
}

// TestSingleGateWritesNoReceipt: an unsigned decision has no signature to keep.
func TestSingleGateWritesNoReceipt(t *testing.T) {
	f := newFixture(t, agent.SingleApproval(), refundArgs)
	if code, _, errs := f.run_("", "-run", f.run, "-call", f.call, "-yes"); code != 0 {
		t.Fatal(errs)
	}
	if got := receiptsIn(t, f.dir); len(got) != 0 {
		t.Fatalf("receipts: %v", got)
	}
}

// TestReceiptOnAnAuditedJournal: the application audits its journal (audit.AuditedStore); bide
// approve writes the decision through the plain store. Until the application next writes to the run,
// no signed head covers the decision and its receipt is pending. The application's resume then signs
// a head over the journal, and the receipt is anchored: the decision is signed into the audit log at
// the app's next write.
func TestReceiptOnAnAuditedJournal(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	for _, id := range []string{"ops", "finance"} {
		if code, _, errs := f.run_("", f.as(id, "-yes")...); code != 0 {
			t.Fatalf("%s: %s", id, errs)
		}
	}
	rc := readReceipt(t, receiptsIn(t, f.dir)[0])
	logPub, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	logKey := audit.Ed25519Verifier{Pub: logPub}
	anchor := audit.NewMemAnchorLog()
	heads := func() []audit.SignedTreeHead {
		var hs []audit.SignedTreeHead
		for _, e := range anchor.Entries() {
			hs = append(hs, e.STH)
		}
		return hs
	}
	if fnd := audit.CheckReceipt(rc, f.allRecords(), heads(), logKey); fnd.Verdict != audit.ReceiptPending {
		t.Fatalf("before the app writes: %+v", fnd)
	}

	// The application resumes the run through its audited store.
	s, err := sqlite.Open(f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	as, err := audit.NewAuditedStore(s, audit.Ed25519Signer{Priv: logPriv}, anchor)
	if err != nil {
		t.Fatal(err)
	}
	verify := func(id string) (agent.ApproverVerifier, bool) { v, ok := f.pubs[id]; return v, ok }
	refund := agent.MustFunc("refund", "refund an order", func(context.Context, json.RawMessage) (string, error) { return "refunded", nil }, agent.WithApproval(twoOfThree))
	m := agenttest.NewScriptedModel(agenttest.ToolTurn(f.call, "refund", f.args), agenttest.TextTurn("done"))
	if _, err := agenttest.MustNew(m, agenttest.MustJournal(as), agent.WithTools(refund), agent.WithApproverVerifiers(verify)).Run(context.Background(), f.run, agent.UserText("refund")); err != nil {
		t.Fatal(err)
	}
	if fnd := audit.CheckReceipt(rc, f.allRecords(), heads(), logKey); fnd.Verdict != audit.ReceiptAnchored {
		t.Fatalf("after the app writes: %+v", fnd)
	}
}

// word is how a printed command writes path p: as is on Unix temp paths, single-quoted when it holds
// a character the POSIX quoting in approveline does not leave bare (a Windows path's backslashes).
func word(p string) string {
	if strings.ContainsAny(p, "\\ ") {
		return "'" + p + "'"
	}
	return p
}

// TestRetryIsIdempotent: under every scheme, deciding again what the approver's counted decision
// already says (approve after approve, reject after reject) exits 0 with "already recorded", signs
// nothing, adds no record and writes no second receipt; the opposite verdict is still refused as
// already decided. ecdsa-p256 signs differently each time, so there a retry could not be told
// apart by its signature, even at one decision time.
func TestRetryIsIdempotent(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 14, 3, 123_000_000, time.UTC)
	old := now
	now = func() time.Time { return at }
	defer func() { now = old }()
	for _, alg := range algs {
		for _, reject := range []bool{false, true} {
			verb := map[bool]string{false: "approve", true: "reject"}[reject]
			t.Run(string(alg)+"/"+verb, func(t *testing.T) {
				f := newFixtureAlg(t, twoOfThree, refundArgs, alg)
				flags := func(extra ...string) []string {
					a := f.as("ops", append([]string{"-yes"}, extra...)...)
					if reject {
						a = append(a, "-reject")
					}
					return a
				}
				first, second := filepath.Join(f.dir, "r1.json"), filepath.Join(f.dir, "r2.json")
				if code, _, errs := f.run_("", flags("-receipt", first)...); code != 0 {
					t.Fatal(errs)
				}
				code, out, errs := f.run_("", flags("-receipt", second)...)
				if code != 0 || !strings.Contains(out, "already recorded: your "+verb+" at 2026-10-13T12:14:03.123Z counts; nothing to do") {
					t.Fatalf("retry: %d %s\n%s", code, errs, out)
				}
				if n := len(f.decisions()); n != 1 {
					t.Fatalf("%d decision records after a retry, want 1", n)
				}
				if _, err := os.Stat(second); err == nil {
					t.Fatal("the retry wrote a receipt")
				}
				r := readReceipt(t, first)
				if err := r.Verify(f.pubs["ops"]); err != nil {
					t.Fatal(err)
				}
				// The other verdict is a different decision: refused.
				other := f.as("ops", "-yes")
				if !reject {
					other = append(other, "-reject")
				}
				if code, _, errs := f.run_("", other...); code != 1 || !strings.Contains(errs, "already decided") {
					t.Fatalf("the opposite verdict: %d %s", code, errs)
				}
				if n := len(f.decisions()); n != 1 {
					t.Fatalf("%d decision records", n)
				}
			})
		}
	}
}

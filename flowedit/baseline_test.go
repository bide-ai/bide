package flowedit

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/runview"
)

// weakFlow has a gate that needs 2 of 3, requires the requester out, has a deadline, and states
// the assertion the gate keeps true.
const weakFlow = `version: 1
flow: refund
entry: validate
nodes:
  - {name: validate, block: validate, safety: readonly}
  - name: charge
    block: charge
    safety: side_effect
    approval:
      need: 2
      approvers: [ops, finance, risk]
      exclude_principal: true
      timeout: 24h
  - {name: receipt, block: receipt, safety: idempotent}
wiring:
  - edge: [validate, charge]
  - edge: [charge, receipt]
assert:
  - every path to charge passes approval >= 2 of 3
`

func gate(need int, approvers []string, exclude bool, timeout string) *Approval {
	return &Approval{Need: need, Approvers: approvers, ExcludePrincipal: exclude, Timeout: timeout}
}

// What the editor calls weakened is what `bide check -diff -fail-on-weaken` calls weakened, on the
// same two texts: the editor shows what the classifier returns and has no classifier of its own.
// Each case is an operation, or a text change the operations do not offer yet, on weakFlow.
func TestWeakeningAgreesWithCheckDiff(t *testing.T) {
	three := []string{"ops", "finance", "risk"}
	type edit struct {
		name string
		op   *Operation
		text func(string) string
		weak bool
	}
	replace := func(a, b string) func(string) string {
		return func(s string) string {
			if !strings.Contains(s, a) {
				t.Fatalf("sample lacks %q", a)
			}
			return strings.Replace(s, a, b, 1)
		}
	}
	for _, c := range []edit{
		{"remove the gate", &Operation{Op: "clear_approval", Node: "charge"}, nil, true},
		{"lower the need", &Operation{Op: "set_approval", Node: "charge", Approval: gate(1, three, true, "24h")}, nil, true},
		{"add an approver without raising the need", &Operation{Op: "set_approval", Node: "charge", Approval: gate(2, append(three[:3:3], "audit"), true, "24h")}, nil, true},
		{"drop exclude_principal", &Operation{Op: "set_approval", Node: "charge", Approval: gate(2, three, false, "24h")}, nil, true},
		{"remove the deadline", &Operation{Op: "set_approval", Node: "charge", Approval: gate(2, three, true, "")}, nil, true},
		{"lengthen the deadline", &Operation{Op: "set_approval", Node: "charge", Approval: gate(2, three, true, "72h")}, nil, true},
		{"remove an assertion", nil, replace("assert:\n  - every path to charge passes approval >= 2 of 3\n", ""), true},
		{"loosen an assertion", nil, replace("passes approval >= 2 of 3", "passes approval >= 1 of 3"), true},
		{"a new route around the gate to an effect", nil, func(s string) string {
			s = replace("safety: idempotent", "safety: side_effect")(s)
			return replace("  - edge: [charge, receipt]\n", "  - edge: [charge, receipt]\n  - edge: [validate, receipt]\n")(s)
		}, true},
		{"raise the need", &Operation{Op: "set_approval", Node: "charge", Approval: gate(3, three, true, "24h")}, nil, false},
		{"shorten the deadline", &Operation{Op: "set_approval", Node: "charge", Approval: gate(2, three, true, "12h")}, nil, false},
		{"replace an approver at the same need", &Operation{Op: "set_approval", Node: "charge", Approval: gate(2, []string{"ops", "finance", "treasury"}, true, "24h")}, nil, true},
		{"lower a safety label (reported by itself, #342)", &Operation{Op: "set_safety", Node: "receipt", Value: "readonly"}, nil, true},
		{"raise an ungated node to a side effect (judged by the more dangerous label, #342)", &Operation{Op: "set_safety", Node: "validate", Value: "side_effect"}, nil, true},
		{"no change", &Operation{Op: "set_safety", Node: "validate", Value: "readonly"}, nil, false},
	} {
		text := weakFlow
		switch {
		case c.op != nil:
			r, err := ApplyOperation("refund.yaml", []byte(weakFlow), *c.op)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			text = string(r.Text)
		default:
			text = c.text(weakFlow)
		}
		w := Compare("refund.yaml", "test", []byte(weakFlow), []byte(text))
		oldJSON, _ := ToJSON([]byte(weakFlow))
		newJSON, _ := ToJSON([]byte(text))
		var out bytes.Buffer
		weakened, err := runview.CheckDiff(&out, runview.Diff{Old: runview.Config{Name: "old", Data: oldJSON}, New: runview.Config{Name: "new", Data: newJSON}})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := len(w.Weakened) > 0; got != weakened || got != c.weak {
			t.Errorf("%s: the editor says weakened=%v, CheckDiff %v, expected %v\n%q\n%s", c.name, got, weakened, c.weak, w.Weakened, out.String())
		}
		for _, item := range w.Weakened {
			if !strings.Contains(out.String(), item) {
				t.Errorf("%s: CheckDiff's text lacks the editor's item %q:\n%s", c.name, item, out.String())
			}
		}
		if c.weak && (w.Word == "" || w.Line == "") {
			t.Errorf("%s: the banner has no words: %+v", c.name, w)
		}
		if !c.weak && (w.Word != "" || len(w.Weakened) > 0) {
			t.Errorf("%s: a banner on a text that is not weaker: %+v", c.name, w)
		}
	}
}

func TestWeakeningWords(t *testing.T) {
	op := Operation{Op: "set_approval", Node: "charge", Approval: gate(1, []string{"ops", "finance", "risk"}, true, "24h")}
	r, _ := ApplyOperation("refund.yaml", []byte(weakFlow), op)
	w := Compare("refund.yaml", "git HEAD", []byte(weakFlow), r.Text)
	if w.Word != "APPROVAL GATE WEAKENED" || len(w.Weakened) == 0 || w.Weakened[0] != "charge: approval 2 of 3 excluding the requester, times out after 24h -> 1 of 3 excluding the requester, times out after 24h" {
		t.Errorf("%+v", w)
	}
	// A text that does not parse cannot be compared: that is said, and is not "not weaker".
	w = Compare("refund.yaml", "git HEAD", []byte(weakFlow), []byte("nodes: ["))
	if w.CompareError == "" || len(w.Weakened) != 0 {
		t.Errorf("%+v", w)
	}
	w = Compare("refund.yaml", "git HEAD", []byte("nodes: ["), []byte(weakFlow))
	if !strings.Contains(w.CompareError, "baseline") {
		t.Errorf("%+v", w)
	}
}

// Without git the baseline is the file as the editor opened it; every view is compared with it.
func TestBaselineWithoutGit(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", weakFlow)
	h := s.Handler()
	var d Doc
	if err := json.Unmarshal(call(h, s, "/api/doc", "{}", nil).Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Weakening == nil || d.Weakening.Baseline != "the file as it was when the editor opened it" || len(d.Weakening.Weakened) != 0 {
		t.Errorf("%+v", d.Weakening)
	}
	// Two edits in a row, each from the text before: the second is still weaker than the baseline.
	text := weakFlow
	for i, op := range []Operation{
		{Op: "set_approval", Node: "charge", Approval: gate(2, []string{"ops", "finance", "risk"}, false, "24h")},
		{Op: "set_approval", Node: "charge", Approval: gate(2, []string{"ops", "finance", "risk"}, false, "")},
	} {
		var e Edited
		if err := json.Unmarshal(call(h, s, "/api/apply", applyReq(text, op), nil).Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		text = e.Text
		if e.Weakening == nil || len(e.Weakening.Weakened) != 1 || e.Weakening.Word != "APPROVAL GATE WEAKENED" {
			t.Errorf("edit %d: %+v", i, e.Weakening)
		}
	}
	// The baseline stays the opening text when the file on disk changes under the editor.
	if err := os.WriteFile(s.path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(call(h, s, "/api/doc", "{}", nil).Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Weakening.Weakened) != 1 {
		t.Errorf("a file weakened on disk compares with the opening text: %+v", d.Weakening)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// In a git working tree the baseline is the committed file: the file at HEAD, or at the revision
// -base names. An edit that weakens the working file is weakening against the commit, however many
// saves it took.
func TestBaselineFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	sub := filepath.Join(dir, "flows")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "refund.yaml")
	if err := os.WriteFile(p, []byte(weakFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "policy")
	first := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	weaker := strings.Replace(weakFlow, "need: 2", "need: 1", 1)
	if err := os.WriteFile(p, []byte(weaker), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "commit", "-q", "-am", "weaken")
	stronger := strings.Replace(weakFlow, "need: 2", "need: 3", 1)
	if err := os.WriteFile(p, []byte(stronger), 0o600); err != nil {
		t.Fatal(err)
	}

	// Default: the file at HEAD (need 1), so the working file (need 3) is stronger.
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Doc()
	if err != nil {
		t.Fatal(err)
	}
	if d.Weakening.Baseline != "git HEAD" || len(d.Weakening.Weakened) != 0 || len(d.Weakening.Strengthened) != 1 {
		t.Errorf("against HEAD: %+v", d.Weakening)
	}
	// -base: the first commit (need 2): need 3 is stronger there too; HEAD~1 is the same commit.
	for _, rev := range []string{first, "HEAD~1"} {
		s, err := New(p, WithBase(rev))
		if err != nil {
			t.Fatal(err)
		}
		if d, _ := s.Doc(); d.Weakening.Baseline != "git "+rev || len(d.Weakening.Strengthened) != 1 {
			t.Errorf("against %s: %+v", rev, d.Weakening)
		}
	}
	// The committed policy is the baseline, not the previous save: editing the working file down
	// to need 1 is weaker than the first commit.
	s, err = New(p, WithBase(first))
	if err != nil {
		t.Fatal(err)
	}
	var e Edited
	body := applyReq(stronger, Operation{Op: "set_approval", Node: "charge", Approval: gate(1, []string{"ops", "finance", "risk"}, true, "24h")})
	if err := json.Unmarshal(call(s.Handler(), s, "/api/apply", body, nil).Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	// the gate, and the assertion that held at the first commit and is violated now
	if len(e.Weakening.Weakened) != 2 || e.Weakening.Baseline != "git "+first {
		t.Errorf("%+v", e.Weakening)
	}
	// A file git does not have falls back to the file as opened; a revision that does not have it is an error.
	untracked := filepath.Join(sub, "new.yaml")
	if err := os.WriteFile(untracked, []byte(weakFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := New(untracked); err != nil || s.baselineSource != "the file as it was when the editor opened it" {
		t.Errorf("untracked file: %v %v", err, s)
	}
	for _, rev := range []string{"nosuchrev", "HEAD:flows/refund.yaml", "-x", "--output=/tmp/x", "a\nb"} {
		if _, err := New(p, WithBase(rev)); err == nil {
			t.Errorf("-base %q was accepted", rev)
		}
	}
	// An option-looking revision must not reach git as an option: --output= would write a file.
	if runtime.GOOS != "windows" { // the trap is a directory with a colon in its name
		trap := filepath.Join(sub, "o:.")
		if err := os.Mkdir(trap, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := New(p, WithBase("--output=o")); err == nil {
			t.Error("an option as a revision was accepted")
		}
		if _, err := os.Stat(filepath.Join(trap, "refund.yaml")); err == nil {
			t.Error("git was run with an option taken from -base and wrote a file")
		}
	}
	if _, err := New(untracked, WithBase("HEAD")); err == nil {
		t.Error("-base for a file that is not in the revision was accepted")
	}
}

// fakeGit puts a git on PATH that prints msg to stderr and exits 128, for the commands the baseline
// runs.
func fakeGit(t *testing.T, msg string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for git")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho '" + msg + "' >&2\nexit 128\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A git that fails for any reason but "the file is not in git" does not quietly turn the baseline into
// "the file as opened": the reason is in the baseline's name (which the page shows every time) and in
// BaselineNote (which bide edit prints). An untracked file and a non-repository are ordinary.
func TestBaselineSaysWhyGitWasNotUsed(t *testing.T) {
	for name, c := range map[string]struct {
		msg  string
		note bool
	}{
		"dubious ownership": {"fatal: detected dubious ownership in repository at '/x'", true},
		"a corrupt repo":    {"fatal: bad object HEAD", true},
		"not a repository":  {"fatal: not a git repository (or any of the parent directories): .git", false},
		"untracked":         {"fatal: path 'f.yaml' exists on disk, but not in 'HEAD'", false},
		"no such path":      {"fatal: path 'f.yaml' does not exist in 'HEAD'", false},
	} {
		fakeGit(t, c.msg)
		s, _ := newTestServer(t, "refund.yaml", weakFlow)
		d, err := s.Doc()
		if err != nil {
			t.Fatal(err)
		}
		if c.note {
			if s.BaselineNote() == "" || !strings.Contains(d.Weakening.Baseline, "git HEAD was not used: ") || !strings.Contains(d.Weakening.Baseline, strings.SplitN(c.msg, "'", 2)[0]) {
				t.Errorf("%s: note %q, baseline %q", name, s.BaselineNote(), d.Weakening.Baseline)
			}
		} else if s.BaselineNote() != "" || d.Weakening.Baseline != "the file as it was when the editor opened it" {
			t.Errorf("%s: note %q, baseline %q", name, s.BaselineNote(), d.Weakening.Baseline)
		}
	}
}

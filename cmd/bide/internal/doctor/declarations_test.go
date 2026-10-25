package doctor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/declarations"
)

const golden = "../../../../internal/declarations/testdata/agent.golden.json"

var twoOfTwo = &declarations.Gate{Need: 2, Approvers: []string{"ops", "risk"}, ExcludePrincipal: true}

func decl(p *declarations.Policy, tools ...declarations.Tool) declarations.Document {
	d := declarations.Document{Format: declarations.Format, Policy: p, Tools: tools}
	d.Digest = d.Recompute()
	return d
}

// write encodes d into a file of the test's directory.
func write(t *testing.T, name string, d declarations.Document) string {
	t.Helper()
	var buf bytes.Buffer
	if err := declarations.Encode(&buf, d); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runMain(args ...string) (code int, out, errs string) {
	var o, e bytes.Buffer
	code = Main("", append([]string{"-color", "never"}, args...), &o, &e)
	return code, o.String(), e.String()
}

func TestDeclarations_ReportOnTheAgentsDocument(t *testing.T) {
	code, out, errs := runMain("-declarations", golden)
	if code != 0 {
		t.Fatalf("code %d: %s", code, errs)
	}
	for _, want := range []string{"taint policy: an escalation gate", "2 tool(s)", "tool refund", "1 cap rule(s)", "declares no provenance"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
	// The default checks did not run.
	if strings.Contains(out, "READY") || strings.Contains(out, "SQLite") {
		t.Errorf("the first-run checks ran:\n%s", out)
	}
}

func TestDeclarations_AnAgentWithNoPolicyIsFlagged(t *testing.T) {
	path := write(t, "d.json", decl(nil, declarations.Tool{Name: "pay", Effect: true}))
	code, out, _ := runMain("-declarations", path)
	if code != 0 || !strings.Contains(out, "no taint policy") || !strings.Contains(out, "tool pay") {
		t.Fatalf("code %d:\n%s", code, out)
	}
}

func TestDeclarations_RefusesADocumentThatDoesNotRead(t *testing.T) {
	dir := t.TempDir()
	bad := map[string]string{
		"unknown.json": `{"format":"bide.declarations.v1","digest":"","tools":[],"future":1}`,
		"format.json":  `{"format":"bide.declarations.v9","digest":"","tools":[]}`,
		"digest.json":  `{"format":"bide.declarations.v1","digest":"abc","tools":[]}`,
	}
	for name, content := range bad {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, _, errs := runMain("-declarations", p); code != 1 || errs == "" {
			t.Errorf("%s: code %d, stderr %q; want 1 and a message", name, code, errs)
		}
		if code, _, _ := runMain("-diff", p, p); code != 1 {
			t.Errorf("%s: -diff code %d, want 1", name, code)
		}
	}
	if code, _, _ := runMain("-declarations", filepath.Join(dir, "missing.json")); code != 1 {
		t.Errorf("a missing file: code %d, want 1", code)
	}
}

func TestDiff_ExitsOneWhenAnythingIsWeakened(t *testing.T) {
	pol := &declarations.Policy{Gate: twoOfTwo}
	old := write(t, "old.json", decl(pol, declarations.Tool{Name: "read", Untrusted: "crm"}, declarations.Tool{Name: "write", Exempt: ""}))
	weaker := write(t, "new.json", decl(pol, declarations.Tool{Name: "read"}, declarations.Tool{Name: "write", Exempt: "reviewed"}))
	code, out, _ := runMain("-diff", old, weaker)
	if code != 1 || !strings.Contains(out, "WEAKENED") || !strings.Contains(out, "Untrusted") || !strings.Contains(out, "TaintExempt") || !strings.Contains(out, "2 change(s) weaken") {
		t.Fatalf("weakened: code %d:\n%s", code, out)
	}
	// The other way is only a strengthening: exit 0.
	code, out, _ = runMain("-diff", weaker, old)
	if code != 0 || !strings.Contains(out, "strengthened") || strings.Contains(out, "WEAKENED") {
		t.Fatalf("strengthened: code %d:\n%s", code, out)
	}
	// Equal documents.
	if code, out, _ := runMain("-diff", old, old); code != 0 || !strings.Contains(out, "no difference") {
		t.Fatalf("equal: code %d:\n%s", code, out)
	}
}

func TestDiff_ListsAChangeNotInTheDigest(t *testing.T) {
	pol := &declarations.Policy{Gate: twoOfTwo}
	a := write(t, "a.json", decl(pol, declarations.Tool{Name: "x"}))
	b := write(t, "b.json", decl(pol, declarations.Tool{Name: "x", Effect: true}))
	code, out, _ := runMain("-diff", a, b)
	if code != 0 || !strings.Contains(out, "now has side effects") || !strings.Contains(out, "not in the digest") {
		t.Fatalf("code %d:\n%s", code, out)
	}
}

func TestDiff_Usage(t *testing.T) {
	if code, _, errs := runMain("-diff", "only-one.json"); code != 2 || !strings.Contains(errs, "two files") {
		t.Errorf("one file: code %d %q", code, errs)
	}
	if code, _, _ := runMain("-diff", "-declarations", "x.json", "a", "b"); code != 2 {
		t.Errorf("both reports: code %d, want 2", code)
	}
	if code, _, _ := runMain("stray"); code != 2 {
		t.Errorf("a stray argument: code %d, want 2", code)
	}
	if code, _, _ := runMain("-declarations", golden, "stray"); code != 2 {
		t.Errorf("a stray argument with -declarations: code %d, want 2", code)
	}
}

// RaisesSafety is listed by -declarations; -diff reports adding it as WEAKENED (exit 1) and removing it as
// strengthened (exit 0).
func TestRaisesSafety_IsListedAndDiffed(t *testing.T) {
	pol := &declarations.Policy{Refuse: true}
	plain := write(t, "plain.json", decl(pol, declarations.Tool{Name: "quote", Effect: true, Trusted: "ours"}))
	raised := write(t, "raised.json", decl(pol, declarations.Tool{Name: "quote", Effect: true, Trusted: "ours", RaisesSafety: "reads the quote only"}))
	idem := write(t, "idem.json", decl(pol, declarations.Tool{Name: "quote", Effect: true, Idempotent: true, Trusted: "ours", RaisesSafety: "reads the quote only"}))
	code, out, errs := runMain("-declarations", raised)
	if code != 0 || !strings.Contains(out, "tool quote") || !strings.Contains(out, "RaisesSafety") || !strings.Contains(out, "reads the quote only") {
		t.Errorf("-declarations: code %d, out %q, err %q", code, out, errs)
	}
	code, out, _ = runMain("-diff", plain, raised)
	if code != 1 || !strings.Contains(out, "WEAKENED") || !strings.Contains(out, "RaisesSafety") {
		t.Errorf("adding RaisesSafety: code %d, out %q", code, out)
	}
	// Effect is unchanged, so only RaisesSafety makes these weakened.
	code, out, _ = runMain("-diff", raised, idem)
	if code != 1 || !strings.Contains(out, "WEAKENED") || !strings.Contains(out, "now Idempotent") {
		t.Errorf("raising to Idempotent: code %d, out %q", code, out)
	}
	code, out, _ = runMain("-diff", raised, plain)
	if code != 0 || !strings.Contains(out, "strengthened") || strings.Contains(out, "WEAKENED") || !strings.Contains(out, "RaisesSafety removed") {
		t.Errorf("removing RaisesSafety: code %d, out %q", code, out)
	}
}

// A baseline from before the idempotent member is not read as "no tool is Idempotent": -diff says to
// regenerate it and exits 1.
func TestDiff_AnOldBaselineIsToBeRegenerated(t *testing.T) {
	pol := &declarations.Policy{Refuse: true}
	old := write(t, "old.json", decl(pol, declarations.Tool{Name: "w", Effect: true, Trusted: "ours"}))
	newer := decl(pol, declarations.Tool{Name: "w", Effect: true, Idempotent: true, Trusted: "ours"})
	newer.TracksIdempotent = true
	code, out, _ := runMain("-diff", old, write(t, "new.json", newer))
	if code != 1 || !strings.Contains(out, "regenerate the baseline") || strings.Contains(out, "now Idempotent") {
		t.Errorf("code %d, out %q", code, out)
	}
}

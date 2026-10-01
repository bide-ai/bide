package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The marker prefix is built, not written, so this file holds no line comment a scan of the
// repository would read as a marker (the scanner reads comments only, but this keeps grep quiet
// too).
const mk = "// " + "protocol:"

const fixtureSpec = `---- MODULE Claims ----
(* --algorithm claims
process driver \in Drivers
begin
Claim:
  skip;
ClaimInsert:
  skip;
Start:
  skip;
end process;
end algorithm; *)
Claim(self) == TRUE
ClaimInsert(self) == TRUE
Crash(p) == TRUE
====
`

const fixtureReadme = `# Formal models

## Model 1

<!-- modelsync: no-code claims Start Crash -->
<!-- modelsync: map claims -->
| Label | Go |
|---|---|
| ` + "`Start`" + ` | the caller |
| ` + "`Claim`" + ` | Journal.claim |
| ` + "`ClaimInsert`" + ` | Journal.insert |
| ` + "`Crash(p)`" + ` | a process dies |
`

var fixtureGo = `package agent

func unrelated() int {
	return 1
}

func claim() int {
	` + mk + `claims begin Claim ClaimInsert
	x := 1
	x++
	` + mk + `claims end
	return x
}

func after() int {
	return 2
}
`

// fixture is a git repository holding a model, its README map and marked Go code, with one
// commit (the base).
type fixture struct {
	t    *testing.T
	dir  string
	base string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir()}
	f.git("init", "-q", "-b", "main")
	f.write("spec/tla/claims/Claims.tla", fixtureSpec)
	f.write("spec/tla/README.md", fixtureReadme)
	f.write("spec/tla/flows/Flows.tla", "---- MODULE Flows ----\nBegin == TRUE\n====\n") // a second model, with no map and no markers
	f.write("agent/claim.go", fixtureGo)
	f.base = f.commit("base")
	return f
}

func (f *fixture) git(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", f.dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) write(path, content string) {
	f.t.Helper()
	p := filepath.Join(f.dir, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(path string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, path))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

// edit replaces old with new in path, which must hold old exactly once.
func (f *fixture) edit(path, old, new string) {
	f.t.Helper()
	s := f.read(path)
	if strings.Count(s, old) != 1 {
		f.t.Fatalf("%s holds %q %d times", path, old, strings.Count(s, old))
	}
	f.write(path, strings.Replace(s, old, new, 1))
}

func (f *fixture) commit(msg string) string {
	f.t.Helper()
	f.git("add", "-A")
	f.git("commit", "-q", "--allow-empty", "-m", msg)
	return f.git("rev-parse", "HEAD")
}

// run runs modelsync on the fixture and returns its exit status and output.
func (f *fixture) run(args ...string) (int, string) {
	f.t.Helper()
	t := f.t
	t.Setenv("GITHUB_ACTIONS", "")
	var out, errb bytes.Buffer
	code := run(append([]string{"-root", f.dir}, args...), &out, &errb)
	return code, out.String() + errb.String()
}

func expect(t *testing.T, code int, out string, want int, contains ...string) {
	t.Helper()
	if code != want {
		t.Fatalf("exit status %d, want %d; output:\n%s", code, want, out)
	}
	for _, c := range contains {
		if !strings.Contains(out, c) {
			t.Fatalf("output does not contain %q:\n%s", c, out)
		}
	}
}

func TestConsistentFixturePasses(t *testing.T) {
	f := newFixture(t)
	code, out := f.run()
	expect(t, code, out, 0, "modelsync: ok")
	code, out = f.run("-base", f.base)
	expect(t, code, out, 0, "no marked region changed")
}

// The path rule: a change inside a marked region with no model change fails.
func TestRegionChangeWithoutModelChangeFails(t *testing.T) {
	f := newFixture(t)
	f.edit("agent/claim.go", "x++", "x += 2")
	f.commit("change the claim")
	code, out := f.run("-base", f.base)
	expect(t, code, out, 1, "model claims: marked code changed", "agent/claim.go:8-11 (Claim ClaimInsert)")
}

// The same change passes with the model changed in the same change.
func TestRegionChangeWithModelChangePasses(t *testing.T) {
	f := newFixture(t)
	f.edit("agent/claim.go", "x++", "x += 2")
	f.edit("spec/tla/claims/Claims.tla", "Crash(p) == TRUE", "Crash(p) == FALSE")
	f.commit("change the claim and the model")
	code, out := f.run("-base", f.base)
	expect(t, code, out, 0, "ok: spec/tla/claims/ changed")
}

// The same change passes with an override, in a commit message or in the description, and the
// override is printed.
func TestRegionChangeWithOverridePasses(t *testing.T) {
	for _, tc := range []struct {
		name, msg, body string
		from            string
	}{
		{"commit, every model", "refactor\n\nProtocol-Impact: none (renames a local variable)\n", "", "commit "},
		{"commit, named model", "refactor\n\nProtocol-Impact: claims none (renames a local variable)\n", "", "commit "},
		{"description", "refactor", "Some text.\n\nProtocol-Impact: none (renames a local variable)\n", "description "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.edit("agent/claim.go", "x++", "x += 1")
			f.commit(tc.msg)
			args := []string{"-base", f.base}
			if tc.body != "" {
				p := filepath.Join(t.TempDir(), "body.txt")
				if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
					t.Fatal(err)
				}
				args = append(args, "-body", p)
			}
			code, out := f.run(args...)
			expect(t, code, out, 0, "warning: model claims: marked code changed without a model change; Protocol-Impact override from "+tc.from, "renames a local variable")
		})
	}
}

// An override that names another model, has no reason, or names no model that exists does not
// excuse the change.
func TestBadOverrideFails(t *testing.T) {
	for _, tc := range []struct{ msg, want string }{
		{"Protocol-Impact: flows none (another model)", "model claims: marked code changed"},
		{"Protocol-Impact: nosuch none (no such model)", `override names model "nosuch"`},
		{"Protocol-Impact: none ()", "malformed override"},
		{"Protocol-Impact: none", "malformed override"},
		{"Protocol-Impact: maybe (it is fine)", "malformed override"},
	} {
		t.Run(tc.msg, func(t *testing.T) {
			f := newFixture(t)
			f.edit("agent/claim.go", "x++", "x += 1")
			f.commit("refactor\n\n" + tc.msg)
			code, out := f.run("-base", f.base)
			expect(t, code, out, 1, tc.want, "model claims: marked code changed")
		})
	}
}

// Changes outside every region, and to a region's surroundings, pass.
func TestChangeOutsideRegionPasses(t *testing.T) {
	f := newFixture(t)
	f.edit("agent/claim.go", "return 1", "return 3")
	f.edit("agent/claim.go", "return 2", "return 4")
	f.edit("agent/claim.go", "\treturn x\n", "\tx--\n\treturn x\n")
	f.commit("unrelated")
	code, out := f.run("-base", f.base)
	expect(t, code, out, 0, "no marked region changed")
}

// Deleting a region with its markers, deleting lines from it, editing a marker or adding lines
// inside it all count as touching it.
func TestRegionEditsAreSeen(t *testing.T) {
	for name, edit := range map[string]func(f *fixture){
		"delete region": func(f *fixture) {
			s := f.read("agent/claim.go")
			i, j := strings.Index(s, "\t"+mk+"claims begin"), strings.Index(s, mk+"claims end\n")
			f.write("agent/claim.go", s[:i]+s[j+len(mk+"claims end\n"):])
		},
		"delete a line": func(f *fixture) { f.edit("agent/claim.go", "\tx++\n", "") },
		"insert a line": func(f *fixture) { f.edit("agent/claim.go", "\tx++\n", "\tx++\n\tx++\n") },
		"edit a marker": func(f *fixture) {
			f.edit("agent/claim.go", "begin Claim ClaimInsert", "begin Claim")
			f.edit("spec/tla/README.md", "<!-- modelsync: no-code claims Start Crash -->", "<!-- modelsync: no-code claims Start Crash ClaimInsert -->")
		},
		"move the file": func(f *fixture) {
			f.write("agent/claim2.go", f.read("agent/claim.go"))
			if err := os.Remove(filepath.Join(f.dir, "agent/claim.go")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			edit(f)
			f.commit(name)
			code, out := f.run("-base", f.base)
			expect(t, code, out, 1, "model claims: marked code changed")
		})
	}
}

// A renamed action fails the consistency check, wherever the rename is made and not followed.
func TestRenamedActionFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(f *fixture)
		want []string
	}{
		{"spec only", func(f *fixture) {
			f.edit("spec/tla/claims/Claims.tla", "ClaimInsert:", "ClaimPut:")
			f.edit("spec/tla/claims/Claims.tla", "ClaimInsert(self) ==", "ClaimPut(self) ==")
		}, []string{"marker names claims.ClaimInsert, which spec/tla/claims/Claims.tla does not define", "the claims map lists ClaimInsert, which spec/tla/claims/Claims.tla does not define"}},
		{"spec and map", func(f *fixture) {
			f.edit("spec/tla/claims/Claims.tla", "ClaimInsert:", "ClaimPut:")
			f.edit("spec/tla/claims/Claims.tla", "ClaimInsert(self) ==", "ClaimPut(self) ==")
			f.edit("spec/tla/README.md", "`ClaimInsert`", "`ClaimPut`")
		}, []string{"marker names claims.ClaimInsert, which spec/tla/claims/Claims.tla does not define", "the claims map lists ClaimPut, but no Go region is marked"}},
		{"code only", func(f *fixture) {
			f.edit("agent/claim.go", "begin Claim ClaimInsert", "begin Claim ClaimPut")
		}, []string{"marker names claims.ClaimPut, which spec/tla/claims/Claims.tla does not define", "the claims map lists ClaimInsert, but no Go region is marked"}},
		{"no-code entry gone from the map", func(f *fixture) {
			f.edit("spec/tla/README.md", "| `Start` | the caller |\n", "")
		}, []string{"the claims no-code list names Start, which its map does not list"}},
		{"marked action not in the map", func(f *fixture) {
			f.edit("spec/tla/README.md", "| `Claim` | Journal.claim |\n", "")
		}, []string{"marker names claims.Claim, which the claims map in spec/tla/README.md does not list"}},
		{"marked action listed as no-code", func(f *fixture) {
			f.edit("spec/tla/README.md", "no-code claims Start Crash", "no-code claims Start Crash Claim")
		}, []string{"marker names claims.Claim, which spec/tla/README.md lists as having no Go region"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.edit(f)
			code, out := f.run()
			expect(t, code, out, 1, tc.want...)
		})
	}
}

// Broken or malformed markers fail; a marker inside a string literal is not a marker.
func TestMarkerProblems(t *testing.T) {
	for _, tc := range []struct {
		name, old, new, want string
	}{
		{"unclosed", "\t" + mk + "claims end\n", "", "begin marker of claims is never closed"},
		{"unopened", "\t" + mk + "claims begin Claim ClaimInsert\n", "", "end marker of claims with no open region"},
		{"nested", "\tx++\n", "\tx++\n\t" + mk + "claims begin Claim\n", "regions of one model do not nest"},
		{"unknown model", "claims begin Claim", "claim begin Claim", `marker names model "claim"`},
		{"no action", "begin Claim ClaimInsert", "begin", "begin marker of claims names no action"},
		{"end with action", "claims end", "claims end Claim", "end marker of claims names actions"},
		{"bad spelling", "claims end", "claims  end", "malformed marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.edit("agent/claim.go", tc.old, tc.new)
			code, out := f.run()
			expect(t, code, out, 1, tc.want)
		})
	}
	t.Run("string literal", func(t *testing.T) {
		f := newFixture(t)
		f.write("agent/lit.go", "package agent\n\nvar s = `\n"+mk+"claims begin Nope\n`\n")
		code, out := f.run()
		expect(t, code, out, 0, "modelsync: ok")
	})
}

// Under GitHub Actions, findings and overrides are annotations.
func TestAnnotations(t *testing.T) {
	f := newFixture(t)
	f.edit("agent/claim.go", "x++", "x += 1")
	f.commit("refactor\n\nProtocol-Impact: none (a rename)")
	var out, errb bytes.Buffer
	t.Setenv("GITHUB_ACTIONS", "true")
	code := run([]string{"-root", f.dir, "-base", f.base}, &out, &errb)
	expect(t, code, out.String()+errb.String(), 0, "::warning title=modelsync override::model claims")
}

func TestParseDiff(t *testing.T) {
	out := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -3 +3,2 @@ func\n-a\n+b\n+c\n@@ -10,0 +12 @@\n+d\n" +
		"diff --git a/y.go b/y.go\ndeleted file mode 100644\n--- a/y.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-a\n-b\n"
	got := parseDiff(out)
	if len(got) != 2 || got[0].oldPath != "x.go" || got[0].newPath != "x.go" || got[1].newPath != "" {
		t.Fatalf("parseDiff: %+v", got)
	}
	want := []hunk{{3, 1, 3, 2}, {10, 0, 12, 1}}
	for i, h := range want {
		if got[0].hunks[i] != h {
			t.Fatalf("hunk %d = %+v, want %+v", i, got[0].hunks[i], h)
		}
	}
	if !hit(got[0].hunks, region{begin: 4, end: 5}, false) || hit(got[0].hunks, region{begin: 5, end: 11}, false) {
		t.Fatal("hit: wrong new-side intersection")
	}
	if hit(got[0].hunks, region{begin: 4, end: 9}, true) || !hit(got[0].hunks, region{begin: 1, end: 3}, true) {
		t.Fatal("hit: wrong old-side intersection")
	}
}

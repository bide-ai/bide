package flowedit

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// bombYAML is a document of a few hundred bytes whose aliases expand to 9^depth items.
func bombYAML(depth int) string {
	var b strings.Builder
	b.WriteString("a0: &a0 [x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i <= depth; i++ {
		fmt.Fprintf(&b, "a%d: &a%d [%s]\n", i, i, strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*a%d, ", i-1), 9), ", "))
	}
	return b.String() + "z: 1\n"
}

var setZ = Op{Kind: Set, Path: []any{"z"}, Value: 2}

// A document with an anchor or an alias anywhere is refused before anything walks it: a
// self-referential alias would overflow the stack (a fatal error no recover catches) and an alias
// chain expands exponentially. The test would kill the process, or time out, if either were walked.
func TestAnchorsAndAliasesAreRefusedWhereverTheyAre(t *testing.T) {
	for name, src := range map[string]string{
		"a self-referential alias":     "a: &x [*x]\nz: 1\n",
		"a root anchor, aliased":       "&r\na: 1\nz: *r\n",
		"an alias cycle through a map": "a: &x {b: *x}\nz: 1\n",
		"an expansion bomb":            bombYAML(12),
		"an anchor nobody uses":        "a: &x 1\nz: 1\n",
		"an alias in an unrelated key": "a: &x 1\nb: *x\nz: 1\n",
	} {
		start := time.Now()
		done := make(chan error, 1)
		go func() { _, err := Apply([]byte(src), setZ); done <- err }()
		var err error
		select {
		case err = <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("%s: still expanding after 5s (the goroutine is abandoned)", name)
			continue
		}
		var rf *Refusal
		if !errors.As(err, &rf) || !strings.Contains(rf.Reason, "anchors and aliases") {
			t.Errorf("%s: %v", name, err)
		}
		if time.Since(start) > time.Second {
			t.Errorf("%s: took %v", name, time.Since(start))
		}
	}
}

// The engine refuses a document, and a result, that the loader (ToJSON, which bide check and bide
// view -config use) refuses for what is in it: a repeated key, a key that is not a string. Every
// case is checked both ways: the loader's verdict and the engine's agree where the loader refuses,
// and where the engine refuses for another reason (anchors, tags) the table says so.
func TestEngineRefusesWhatTheLoaderRefuses(t *testing.T) {
	for _, c := range []struct {
		name, src     string
		loaderRefuses bool
		engineRefuses bool // for a document the loader accepts
	}{
		{"a plain document", "a: 1\nz: 1\n", false, false},
		{"a repeated key", "a: 1\nz: 1\na: 3\n", true, true},
		{"a repeated key, quoted and bare", "a: 1\n\"a\": 2\nz: 1\n", true, true},
		{"a repeated key in JSON", "{\"a\": 1, \"a\": 2, \"z\": 1}", true, true},
		{"a repeated key deep inside", "m:\n  - {k: 1, k: 2}\nz: 1\n", true, true},
		{"an integer key", "1: a\nz: 1\n", true, true},
		{"a key YAML 1.2 reads as a string", "yes: a\nz: 1\n", false, false},
		{"a null key", "~: a\nz: 1\n", true, true},
		{"a flow-sequence key", "? [a]\n: 1\nz: 1\n", true, true},
		{"a string key that looks like a number, quoted", "\"1\": a\nz: 1\n", false, false},
		{"a NaN value", "a: .nan\nz: 1\n", true, true},
		{"an infinity value", "a: -.inf\nz: 1\n", true, true},
		{"a float that is finite", "a: 1.5e3\nz: 1\n", false, false},
	} {
		_, loadErr := ToJSON([]byte(c.src))
		if (loadErr != nil) != c.loaderRefuses {
			t.Errorf("%s: the loader's verdict is %v, the table says refuses=%v", c.name, loadErr, c.loaderRefuses)
		}
		_, err := Apply([]byte(c.src), setZ)
		var rf *Refusal
		if refused := errors.As(err, &rf); refused != (c.loaderRefuses || c.engineRefuses) {
			t.Errorf("%s: the engine refused=%v (%v), want %v", c.name, refused, err, c.loaderRefuses || c.engineRefuses)
		}
	}
	// An operation cannot produce what the loader refuses either.
	for name, op := range map[string]Op{
		"an inserted map with a repeated key": {Kind: Insert, Path: nil, Key: "m", Value: Map{{"x", 1}, {"x", 2}}},
		"an inserted key that is there":       {Kind: Insert, Path: nil, Key: "z", Value: 3},
		"an inserted merge key":               {Kind: Insert, Path: nil, Key: "<<", Value: 1},
	} {
		for _, src := range []string{"a: 1\nz: 1\n", "{\"a\": 1, \"z\": 1}"} {
			r, err := Apply([]byte(src), op)
			if err == nil {
				if _, lerr := ToJSON(r.Text); lerr != nil {
					t.Errorf("%s: produced text the loader refuses (%v):\n%s", name, lerr, r.Text)
				}
				t.Errorf("%s: applied to %q:\n%s", name, src, r.Text)
			}
		}
	}
}

// A string that is not UTF-8 is refused in both formats, never written as !!binary.
func TestInvalidUTF8ValueIsRefused(t *testing.T) {
	for _, src := range []string{"a: 1\nz: 1\n", "{\"a\": 1, \"z\": 1}", "a: 'q'\nz: 'r'\n"} {
		if r, err := Apply([]byte(src), Op{Kind: Set, Path: []any{"z"}, Value: "x\xffy"}); err == nil {
			t.Errorf("%q: applied as %q", src, r.Text)
		}
	}
}

// A character a YAML 1.1 reader reads as a line break, or a control or format character, is written
// escaped, and reads back as the same string.
func TestLineBreakRunesAreWrittenEscaped(t *testing.T) {
	for _, v := range []string{"\u2028", "x\u2029y", "\u0085", "\ufeff", "\x00", "\x1b[0m"} {
		for _, src := range []string{"a: old\nz: 1\n", "a: 'old'\nz: 'r'\n", "a: \"old\"\nz: \"r\"\n", "a: [old, z]\nz: 1\n", "{a: old, z: 1}"} {
			r, err := Apply([]byte(src), Op{Kind: Set, Path: []any{"z"}, Value: v})
			if err != nil {
				continue
			}
			for _, bad := range []string{"\u2028", "\u2029", "\u0085", "\ufeff", "\x00", "\x1b"} {
				if strings.Contains(string(r.Frag), bad) {
					t.Errorf("%q in %q: the fragment %q holds the raw rune", v, src, r.Frag)
				}
			}
			var got map[string]any
			if err := yaml.Unmarshal(r.Text, &got); err != nil || got["z"] != v {
				t.Errorf("%q in %q: reads back as %v (%v)", v, src, got["z"], err)
			}
		}
	}
}

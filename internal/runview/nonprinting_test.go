package runview

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode"

	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/plan"
)

const spoofName = "refund\u202egnp.json" // a file name holding a direction override

const spoofCfg = `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a"}],"wiring":[]}`

func noRaw(t *testing.T, what, s string) {
	t.Helper()
	for _, r := range s {
		if r == '\u202e' {
			t.Errorf("%s: a raw direction override is in the output: %q", what, s)
			return
		}
	}
	if !strings.Contains(s, `\u{202E}`) {
		t.Errorf("%s: the override is not written out: %q", what, s)
	}
}

// A name that reaches the output unrefused (a file name; a journal's topology) is written out in
// bide check, in the diff, in the serving pages and in the usage and error lines, never drawn.
func TestOutputWritesNonPrintingCharactersOut(t *testing.T) {
	c := Config{Name: spoofName, Data: []byte(spoofCfg)}
	bad := Config{Name: spoofName, Data: []byte(`{"version":1,"flow":"f","nodes":[{"name":"","block":"a"}],"wiring":[]}`)}
	var out bytes.Buffer
	Check(&out, bad)
	noRaw(t, "bide check", out.String())

	for _, st := range []term.Style{{}, term.For(io.Discard, term.Always)} {
		out.Reset()
		if _, err := CheckDiffStyled(&out, Diff{Old: Config{Name: "old.json", Data: []byte(spoofCfg)}, New: bad}, st); err != nil {
			t.Fatal(err)
		}
		for _, r := range out.String() {
			if r == '\u202e' {
				t.Errorf("bide check -diff: a raw override in %q", out.String())
			}
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://127.0.0.1/", nil)
	Handler(Options{Config: &c}).ServeHTTP(rec, req)
	noRaw(t, "the config page", rec.Body.String())
}

// With color on, the command's own color sequences still pass through.
func TestColorSurvivesTheEscape(t *testing.T) {
	var out bytes.Buffer
	st := term.For(io.Discard, term.Always)
	Check(&out, Config{Name: "x.json", Data: []byte(`{"version":1,"flow":"f","nodes":[{"name":"","block":"a"}],"wiring":[]}`)})
	CheckStyled(&out, Config{Name: "x.json", Data: []byte(`{"version":1,"flow":"f","nodes":[{"name":"","block":"a"}],"wiring":[]}`)}, st)
	if !strings.Contains(out.String(), "\x1b[") {
		t.Errorf("no color sequence in %q", out.String())
	}
}

// The viewer draws names a journal holds, which no config check covered: the flow's name, its steps,
// a predicate, the run's id. Every HTML page it serves has them written out, in every place, never
// drawn; the JSON endpoints are for programs and are left as they are.
func TestViewerPagesWriteJournalNamesOut(t *testing.T) {
	ctx := context.Background()
	b := plan.New[order, string]("fl\u202eow")
	validate := b.Step("va\u200blidate", func(_ context.Context, o order) (order, error) { return o, nil }, plan.ReadOnly())
	charge := b.Step("ch\u202egra", func(_ context.Context, o order) (order, error) { return o, nil })
	skip := b.Step("sk\u2028ip", func(_ context.Context, o order) (string, error) { return "none", nil }, plan.ReadOnly())
	receipt := b.Step("re\u00adceipt", func(_ context.Context, o order) (string, error) { return "ok", nil }, plan.ReadOnly())
	b.Switch(validate, plan.When(func(o order) bool { return o.Amount > 100 }, charge).Named("la\u202erge"), plan.Else(skip))
	b.Edge(charge, receipt)
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MemJournal()
	runID := "run\u202e1"
	if _, err := f.Run(ctx, j, runID, order{ID: "o\u200b1", Amount: 250}); err != nil {
		t.Fatal(err)
	}
	h := Handler(Options{Source: JournalSource{J: j}})
	for _, target := range []string{"/", "/run?id=" + url.QueryEscape(runID), "/attention"} {
		code, body := get(t, h, target)
		if code != 200 {
			t.Errorf("GET %s = %d", target, code)
			continue
		}
		for _, r := range body {
			if r != '\n' && r != '\t' && r != '\r' && !unicode.IsPrint(r) {
				t.Errorf("GET %s: a raw %U is in the page", target, r)
				break
			}
		}
		if strings.HasPrefix(target, "/run?") && !strings.Contains(body, "data-detail=") {
			t.Errorf("GET %s: the drawing has no data-detail, which view.js renders; the check below would prove nothing", target)
		}
		if target != "/attention" && !strings.Contains(body, `\u{202E}`) {
			t.Errorf("GET %s: the names are not written out", target)
		}
	}
	// A run page is served from the same escaped writer when fetched whole, as the live script does.
	if code, body := get(t, h, "/run.json?id="+url.QueryEscape(runID)); code != 200 || !strings.Contains(body, "\u202e") {
		t.Errorf("the JSON endpoint should carry the names as they are: %d", code)
	}
}

// The text/plain body of an error carries the names too (a file name in a failed diff); it is written
// out like a page, and the JSON endpoints are left alone.
func TestErrorBodiesWriteNamesOut(t *testing.T) {
	c := Config{Name: spoofName, Data: []byte(spoofCfg)}
	h := guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, c.Name+": it failed", http.StatusInternalServerError)
	}))
	code, body := get(t, h, "/")
	if code != 500 || strings.ContainsRune(body, '\u202e') || !strings.Contains(body, `\u{202E}`) {
		t.Errorf("%d %q", code, body)
	}
}

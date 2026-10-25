package editcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/runview"
)

const flow = "version: 1\nflow: f\nentry: a\nnodes:\n  - {name: a, block: a, safety: readonly}\nwiring: []\n"

func write(t *testing.T, name, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUsageErrors(t *testing.T) {
	good := write(t, "f.yaml", flow)
	for name, c := range map[string]struct {
		args []string
		code int
	}{
		"no file":                      {nil, 2},
		"two files":                    {[]string{good, good}, 2},
		"an unknown flag":              {[]string{"-nope", good}, 2},
		"a public address":             {[]string{"-addr", "0.0.0.0:8585", good}, 2},
		"an empty host":                {[]string{"-addr", ":8585", good}, 2},
		"a name, not an IP":            {[]string{"-addr", "example.com:8585", good}, 2},
		"a missing file":               {[]string{filepath.Join(t.TempDir(), "none.yaml")}, 1},
		"a wrong file suffix":          {[]string{write(t, "f.txt", flow)}, 1},
		"help":                         {[]string{"-h"}, 0},
		"a revision git does not have": {[]string{"-base", "nosuchrev", good}, 1},
		"a base that is an option":     {[]string{"-base", "--output=/tmp/x", good}, 1},
	} {
		var out, errb bytes.Buffer
		if got := run(context.Background(), c.args, &out, &errb, runview.Stores{}); got != c.code {
			t.Errorf("%s: exit %d, want %d\n%s%s", name, got, c.code, out.String(), errb.String())
		}
		if c.code == 2 && !strings.Contains(errb.String(), "usage: bide edit") && !strings.Contains(errb.String(), "loopback") {
			t.Errorf("%s: stderr %q", name, errb.String())
		}
	}
}

// syncBuffer is a bytes.Buffer the server goroutine writes while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// bide edit prints an address with the session token in its fragment, serves the file on it, and
// stops with its context.
func TestServes(t *testing.T) {
	p := write(t, "f.yaml", flow)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "127.0.0.1:0", "-color", "never", p}, &out, io.Discard, runview.Stores{})
	}()
	re := regexp.MustCompile(`serving (http://127\.0\.0\.1:\d+)/#t=([0-9a-f]{32}) `)
	var m []string
	for i := 0; i < 400 && m == nil; i++ {
		m = re.FindStringSubmatch(out.String())
		time.Sleep(10 * time.Millisecond)
	}
	if m == nil {
		t.Fatalf("no address in %q", out.String())
	}
	base, token := m[1], m[2]
	// A request is built for each call: its body is read by the first one.
	post := func(token string) (*http.Response, error) {
		req, _ := http.NewRequest("POST", base+"/api/doc", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", base)
		if token != "" {
			req.Header.Set("X-Bide-Token", token)
		}
		return http.DefaultClient.Do(req)
	}
	res, err := post(token)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Text string `json:"text"`
		Flow string `json:"flow"`
	}
	err = json.NewDecoder(res.Body).Decode(&d)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 || d.Text != flow || d.Flow != "f" {
		t.Fatalf("doc: %d %v %+v", res.StatusCode, err, d)
	}
	// The page itself needs no token; the API does.
	if r, err := http.Get(base + "/"); err != nil || r.StatusCode != 200 {
		t.Errorf("index: %v %v", r, err)
	} else {
		r.Body.Close()
	}
	if r, err := post(""); err != nil || r.StatusCode != 401 {
		t.Errorf("no token: %v %v", r, err)
	} else {
		r.Body.Close()
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d after a clean stop", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
}

const blocks = `{"version":1,"blocks":[{"name":"a","kind":"step","in":"x.In","out":"x.Out","safety":"readonly"},{"name":"spare","kind":"step","in":"x.Out","out":"x.Out","safety":"side_effect","gate":{"need":2,"approvers":["ops","fin"]}}],"predicates":[],"merges":[]}`

// -blocks makes the editor typed: its doc carries the palette, the digest and the label, and the
// description file is read once and checked before the editor starts.
func TestBlocksFlag(t *testing.T) {
	good := write(t, "f.yaml", flow)
	for name, args := range map[string][]string{
		"a missing description": {"-blocks", filepath.Join(t.TempDir(), "none.json"), good},
		"an invalid one":        {"-blocks", write(t, "b.json", `{"version":9}`), good},
		"an unknown field":      {"-blocks", write(t, "b.json", `{"version":1,"blocks":[],"predicates":[],"merges":[],"x":1}`), good},
	} {
		var out, errb bytes.Buffer
		if got := run(context.Background(), append([]string{"-addr", "127.0.0.1:0"}, args...), &out, &errb, runview.Stores{}); got != 1 || errb.String() == "" {
			t.Errorf("%s: exit %d, stderr %q", name, got, errb.String())
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "127.0.0.1:0", "-color", "never", "-blocks", write(t, "b.json", blocks), good}, &out, io.Discard, runview.Stores{})
	}()
	re := regexp.MustCompile(`serving (http://127\.0\.0\.1:\d+)/#t=([0-9a-f]{32}) `)
	var m []string
	for i := 0; i < 400 && m == nil; i++ {
		m = re.FindStringSubmatch(out.String())
		time.Sleep(10 * time.Millisecond)
	}
	if m == nil {
		t.Fatalf("no address in %q", out.String())
	}
	req, _ := http.NewRequest("POST", m[1]+"/api/doc", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", m[1])
	req.Header.Set("X-Bide-Token", m[2])
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Errors int    `json:"errors"`
		Mode   string `json:"mode"`
		Typed  *struct {
			Source  string `json:"source"`
			Digest  string `json:"digest"`
			Palette []struct {
				Name string `json:"name"`
				Gate string `json:"gate"`
				Used bool   `json:"used"`
			} `json:"palette"`
		} `json:"typed"`
	}
	err = json.NewDecoder(res.Body).Decode(&d)
	res.Body.Close()
	if err != nil || d.Typed == nil || d.Typed.Source != "description" || d.Typed.Digest == "" || d.Errors != 0 ||
		len(d.Typed.Palette) != 2 || !d.Typed.Palette[0].Used || d.Typed.Palette[1].Used || d.Typed.Palette[1].Gate != "2 of 2: ops, fin" ||
		!strings.Contains(d.Mode, "not checked against the program") {
		t.Fatalf("doc: %v %+v", err, d)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
}

const acmeTheme = `{"bide_theme":1,"name":"acme-night","base":"dark","surface":{"bg":"#0e1116","accent":"#79b8ff"}}`

// serveStyle runs bide edit with args and returns its style.css.
func serveStyle(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, append([]string{"-addr", "127.0.0.1:0", "-color", "never"}, args...), &out, io.Discard, runview.Stores{})
	}()
	re := regexp.MustCompile(`serving (http://127\.0\.0\.1:\d+)/#t=`)
	var m []string
	for i := 0; i < 400 && m == nil; i++ {
		m = re.FindStringSubmatch(out.String())
		time.Sleep(10 * time.Millisecond)
	}
	if m == nil {
		t.Fatalf("no address in %q", out.String())
	}
	res, err := http.Get(m[1] + "/assets/style.css")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
	return string(b)
}

// -theme adds a theme file to the editor's list, and -theme-default starts the page in a theme.
func TestThemeFlags(t *testing.T) {
	p := write(t, "f.yaml", flow)
	th := write(t, "acme.json", acmeTheme)
	css := serveStyle(t, "-theme", th, p)
	if !strings.Contains(css, `"light dark high-contrast acme-night"`) || !strings.Contains(css, `:root[data-theme="acme-night"]`) {
		t.Error("-theme: the theme is not in the editor's stylesheet")
	}
	css = serveStyle(t, "-theme", th, "-theme-default", "acme-night", p)
	if !strings.Contains(css, `--bide-default: "acme-night";`) {
		t.Error("-theme-default: the page does not start in the theme")
	}
	css = serveStyle(t, "-theme-default", "high-contrast", p)
	if !strings.Contains(css, `--bide-default: "high-contrast";`) {
		t.Error("-theme-default names a shipped theme: not the start")
	}
}

// A theme file that is not valid, or fails a check, is refused before anything is served; a default
// that names no theme is a usage error.
func TestThemeFlagErrors(t *testing.T) {
	p := write(t, "f.yaml", flow)
	faint := write(t, "faint.json", `{"bide_theme":1,"name":"faint","base":"light","surface":{"text":"#f6f6f2"}}`)
	for name, c := range map[string]struct {
		args []string
		code int
		want string
	}{
		"a missing theme file":     {[]string{"-theme", filepath.Join(t.TempDir(), "none.json"), p}, 1, "none.json"},
		"a theme that fails C1":    {[]string{"-theme", faint, p}, 1, "C1"},
		"a default naming nothing": {[]string{"-theme-default", "nope", p}, 2, `"nope" names no theme`},
	} {
		var out, errb bytes.Buffer
		if got := run(context.Background(), append([]string{"-color", "never"}, c.args...), &out, &errb, runview.Stores{}); got != c.code {
			t.Errorf("%s: exit %d, want %d\n%s%s", name, got, c.code, out.String(), errb.String())
		}
		if !strings.Contains(errb.String(), c.want) {
			t.Errorf("%s: stderr %q lacks %q", name, errb.String(), c.want)
		}
		if strings.Contains(out.String(), "serving") {
			t.Errorf("%s: served anyway: %q", name, out.String())
		}
	}
}

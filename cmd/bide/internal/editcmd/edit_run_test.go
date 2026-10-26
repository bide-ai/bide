package editcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
)

// -run and the store flags go together, name one store, and refuse a reserved run, before anything is
// opened; a value that may be a DSN is never echoed.
func TestRunFlagUsageErrors(t *testing.T) {
	good := write(t, "f.yaml", flow)
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"-run alone":            {[]string{"-run", "r1", good}, "-run needs a store"},
		"two kinds of store":    {[]string{"-run", "r1", "-db", "x.db", "-journal", "y.json", good}, "give exactly one of"},
		"both postgres flags":   {[]string{"-run", "r1", "-postgres", "p", "-postgres-env", "V", good}, "not both"},
		"a reserved run":        {[]string{"-run", agent.StopControlRun, "-journal", "y.json", good}, "reserved"},
		"a DSN as the env name": {[]string{"-run", "r1", "-postgres-env", "postgres://u:hunter2@h/db", good}, "environment variable"},
	} {
		var out, errb bytes.Buffer
		// A flag check that wrongly lets the command through would serve until stopped: the deadline stops
		// it (exit 0), so the failure is the assertion on the exit code and the message, not a hang.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		got := run(ctx, append([]string{"-color", "never", "-addr", "127.0.0.1:0"}, c.args...), &out, &errb, runview.Stores{})
		cancel()
		if got != 2 {
			t.Errorf("%s: exit %d, want 2", name, got)
		}
		if !strings.Contains(errb.String(), c.want) || strings.Contains(errb.String(), "hunter2") || strings.Contains(out.String(), "serving") {
			t.Errorf("%s: stderr %q", name, errb.String())
		}
	}
	// A store that cannot be opened is an input error naming what failed, not a served page.
	var out, errb bytes.Buffer
	if got := run(context.Background(), []string{"-color", "never", "-run", "r1", "-db", "x.db", good}, &out, &errb, runview.Stores{}); got != 1 || !strings.Contains(errb.String(), "this build cannot read that store") {
		t.Errorf("no opener: exit %d %q", got, errb.String())
	}
	if got := run(context.Background(), []string{"-color", "never", "-run", "r1", "-journal", filepath.Join(t.TempDir(), "none.json"), good}, &out, &errb, runview.Stores{}); got != 1 {
		t.Errorf("a missing export: exit %d", got)
	}
}

// bide edit -run serves the comparison: the page asks /api/run and gets the run's facts and the claim.
func TestServesTheRunComparison(t *testing.T) {
	cfg := `{"version":1,"flow":"f","in":"int","out":"int","entry":"a","nodes":[{"name":"a","block":"a"}],"wiring":[]}`
	reg := plan.NewRegistry()
	if err := reg.RegisterStep("a", func(_ context.Context, n int) (int, error) { return n, nil }); err != nil {
		t.Fatal(err)
	}
	fl, err := plan.Load[int, int]([]byte(cfg), reg)
	if err != nil {
		t.Fatal(err)
	}
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fl.Run(context.Background(), j, "r1", 1); err != nil {
		t.Fatal(err)
	}
	exp, err := audit.ExportJournal(context.Background(), j, "r1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(exp)
	export := write(t, "r1.json", string(b))
	file := write(t, "f.json", cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "127.0.0.1:0", "-color", "never", "-run", "r1", "-journal", export, file}, &out, io.Discard, runview.Stores{})
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
	req, _ := http.NewRequest("POST", m[1]+"/api/run", strings.NewReader(`{"text":`+string(mustJSON(cfg))+`}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", m[1])
	req.Header.Set("X-Bide-Token", m[2])
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Configured bool   `json:"configured"`
		Status     string `json:"status"`
		Claim      string `json:"claim"`
		Run        struct {
			ID, Flow string
		} `json:"run"`
	}
	err = json.NewDecoder(res.Body).Decode(&r)
	res.Body.Close()
	if err != nil || !r.Configured || r.Status != "none_seen" || r.Run.ID != "r1" || r.Run.Flow != "f" || !strings.Contains(r.Claim, "No difference") {
		t.Errorf("%v %+v", err, r)
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

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// A store without -run is allowed: the page has the list of unfinished runs and no run panel.
func TestServesAStoreWithoutARun(t *testing.T) {
	cfg := `{"version":1,"flow":"f","in":"int","out":"int","entry":"a","nodes":[{"name":"a","block":"a"}],"wiring":[]}`
	reg := plan.NewRegistry()
	if err := reg.RegisterStep("a", func(_ context.Context, n int) (int, error) { return n, nil }); err != nil {
		t.Fatal(err)
	}
	fl, err := plan.Load[int, int]([]byte(cfg), reg)
	if err != nil {
		t.Fatal(err)
	}
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fl.Run(context.Background(), j, "r1", 1); err != nil {
		t.Fatal(err)
	}
	exp, err := audit.ExportJournal(context.Background(), j, "r1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(exp)
	export := write(t, "r1.json", string(b))
	file := write(t, "f.json", cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "127.0.0.1:0", "-color", "never", "-journal", export, file}, &out, io.Discard, runview.Stores{})
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
	ask := func(path, body string) string {
		req, _ := http.NewRequest("POST", m[1]+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", m[1])
		req.Header.Set("X-Bide-Token", m[2])
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(res.Body)
		return buf.String()
	}
	if got := ask("/api/run", `{"text":""}`); !strings.Contains(got, `"configured":false`) {
		t.Errorf("a store without a run has a run panel: %s", got)
	}
	if got := ask("/api/affected", `{"text":`+string(mustJSON(cfg))+`,"check":true}`); !strings.Contains(got, `"configured":true`) || !strings.Contains(got, `"checked":true`) {
		t.Errorf("no list of unfinished runs: %s", got)
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

// Two -journal exports of one run are a usage error naming both files, not a page served from
// whichever came last.
func TestEditRefusesTwoExportsOfOneRun(t *testing.T) {
	cfg := `{"version":1,"flow":"f","in":"int","out":"int","entry":"a","nodes":[{"name":"a","block":"a"}],"wiring":[]}`
	reg := plan.NewRegistry()
	if err := reg.RegisterStep("a", func(_ context.Context, n int) (int, error) { return n, nil }); err != nil {
		t.Fatal(err)
	}
	fl, err := plan.Load[int, int]([]byte(cfg), reg)
	if err != nil {
		t.Fatal(err)
	}
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fl.Run(context.Background(), j, "r1", 1); err != nil {
		t.Fatal(err)
	}
	exp, err := audit.ExportJournal(context.Background(), j, "r1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(exp)
	one, two := write(t, "a.json", string(b)), write(t, "b.json", string(b))
	// Bounded, so that a build that serves the page fails the test instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	got := run(ctx, []string{"-color", "never", "-run", "r1", "-journal", one, "-journal", two, write(t, "f.json", cfg)}, &out, &errb, runview.Stores{})
	if got != 2 || !strings.Contains(errb.String(), one) || !strings.Contains(errb.String(), two) || strings.Contains(out.String(), "serving") {
		t.Errorf("exit %d, stderr %q", got, errb.String())
	}
}

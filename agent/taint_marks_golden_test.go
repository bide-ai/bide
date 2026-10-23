package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// T9: the records that carry the new members (a result's Safety.Untrusted, run:start's untrusted_input
// and trusted_input, a retrieval step's untrusted) are pinned byte for byte in testdata, and a reader
// that does not know the members reads them: it ignores a member it does not know in a record, in
// Safety, in the settings and in the retrieval step. The golden was also read by the binary built from
// main before this change (see the pull request): old readers decode it. BIDE_UPDATE_GOLDEN=1 rewrites
// the file.
func TestUntrustedMarks_RecordsGoldenAndUnknownMembers(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	j := agenttest.MemJournal()
	ok := func(context.Context, struct{}) (string, error) { return "ok", nil }
	boom := func(context.Context, struct{}) (string, error) { return "", os.ErrNotExist }
	m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "read", `{}`), agenttest.ToolTurn("c2", "fail", `{}`), agenttest.TextTurn("done"))
	a := agenttest.MustNew(m, j, agent.WithClock(func() time.Time { return now }),
		agent.WithTools(agent.MustFunc("read", "read", ok, agent.Untrusted("crm-notes")), agent.MustFunc("fail", "fail", boom, agent.Untrusted("crm-notes"))),
		agent.WithRetrieval(staticRetriever{{ID: "d1", Text: "doc"}}, 1, agent.RetrievalUntrusted("kb")))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithUntrustedInput("inbound-email"), agent.WithSaga()); err == nil {
		// the failing tool in a saga aborts the run; either way the records below exist
		t.Log("the run finished without aborting")
	}
	recs, err := j.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	var lines [][]byte
	for _, r := range recs {
		switch {
		case r.Name == "run:start", strings.HasPrefix(r.Name, "@retrieval/"), r.Kind == agent.StepToolResult, r.Kind == agent.StepSagaFail:
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			b = regexp.MustCompile(`"(salt|claim)":"[^"]*"`).ReplaceAll(b, []byte(`"$1":"-"`))
			b = regexp.MustCompile(`"started_at":"[^"]*"`).ReplaceAll(b, []byte(`"started_at":"-"`))
			lines = append(lines, b)
		}
	}
	got := append(bytes.Join(lines, []byte("\n")), '\n')
	const path = "testdata/untrusted_marks_golden.jsonl"
	if os.Getenv("BIDE_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the records carrying marks changed (golden %s):\ngot:\n%s\nwant:\n%s", path, got, want)
	}
	for _, m := range []string{`"untrusted":"crm-notes"`, `"untrusted_input":"inbound-email"`, `"untrusted":"kb"`} {
		if !bytes.Contains(want, []byte(m)) {
			t.Errorf("the golden has no %s", m)
		}
	}

	// A reader that does not know a member ignores it, wherever a later version adds one.
	for _, l := range bytes.Split(bytes.TrimSpace(want), []byte("\n")) {
		var rec map[string]any
		if err := json.Unmarshal(l, &rec); err != nil {
			t.Fatal(err)
		}
		rec["x_future"] = 1
		rec["salt"] = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // the golden's salt is normalised
		if s, ok := rec["safety"].(map[string]any); ok {
			s["x_future"] = 1
		}
		b, _ := json.Marshal(rec)
		if _, err := agent.DecodeRecord(b); err != nil {
			t.Errorf("a record with an unknown member (%s) does not decode: %v", l[:min(60, len(l))], err)
		}
	}
	start := func() agent.RunStart {
		var rs agent.RunStart
		for _, l := range bytes.Split(bytes.TrimSpace(want), []byte("\n")) {
			var rec struct {
				Name   string          `json:"name"`
				Result json.RawMessage `json:"result"`
			}
			if json.Unmarshal(l, &rec) == nil && rec.Name == "run:start" {
				var m map[string]any
				_ = json.Unmarshal(rec.Result, &m)
				m["settings"].(map[string]any)["x_future"] = 1
				m["x_future"] = 1
				b, _ := json.Marshal(m)
				if err := json.Unmarshal(b, &rs); err != nil {
					t.Fatalf("run:start with unknown members does not decode: %v", err)
				}
			}
		}
		return rs
	}()
	if start.Settings.UntrustedInput == nil || *start.Settings.UntrustedInput != "inbound-email" {
		t.Errorf("decoded settings = %+v", start.Settings)
	}
}

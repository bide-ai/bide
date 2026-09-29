package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A tool's error text is journaled and sent to the model as the call's result, so it is also
// hashed into audit proofs. A URL in it can carry a credential: net/http's *url.Error quotes
// the whole request URL, query string and userinfo included.

var urlSecrets = []string{"SK-QUERY-SECRET", "PW-USERINFO-SECRET", "USER-TOKEN-SECRET", "FRAGMENT-SECRET"}

// seen collects every text a run's journal holds and every message its model was sent.
type seen struct {
	mu    sync.Mutex
	texts []string
}

func (s *seen) middleware(next agent.ModelHandler) agent.ModelHandler {
	return func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
		b, _ := json.Marshal(req.Messages)
		s.mu.Lock()
		s.texts = append(s.texts, string(b))
		s.mu.Unlock()
		return next(ctx, req)
	}
}

func (s *seen) journal(t *testing.T, st agent.Durable, runIDs ...string) {
	t.Helper()
	for _, id := range runIDs {
		recs, err := st.History(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			b, err := agent.EncodeRecord(r)
			if err != nil {
				t.Fatal(err)
			}
			s.texts = append(s.texts, string(b))
		}
	}
}

// check fails for any secret in what was journaled or sent, and for any wanted text missing
// from it (the redacted error must still tell the model what went wrong).
func (s *seen) check(t *testing.T, secrets []string, want ...string) {
	t.Helper()
	all := strings.Join(s.texts, "\n")
	for _, sec := range secrets {
		if strings.Contains(all, sec) {
			t.Errorf("%q was journaled or sent to the model:\n%s", sec, all)
		}
	}
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("%q missing from what was journaled and sent:\n%s", w, all)
		}
	}
}

func fetchTool(rawURL string) agent.Tool {
	type in struct{}
	return agent.Func("fetch", "fetch", agent.Safety{ReadOnly: true}, func(ctx context.Context, _ in) (string, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
		if err != nil {
			return "", err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err // what most tools do: the *url.Error quotes the request URL
		}
		resp.Body.Close()
		return "ok", nil
	})
}

const secretURL = "http://user:PW-USERINFO-SECRET@127.0.0.1:1/v1/items?key=SK-QUERY-SECRET&page=2#FRAGMENT-SECRET"

func TestToolErrorURLCredentialsNotJournaled(t *testing.T) {
	var s seen
	st := agent.NewMemStore()
	a := agent.New(agent.NewScriptedModel(agent.ToolTurn("tu1", "fetch", `{}`), agent.TextTurn("done")), st, fetchTool(secretURL)).Use(s.middleware)
	if _, err := a.Run(context.Background(), "r1", "go"); err != nil {
		t.Fatal(err)
	}
	s.journal(t, st, "r1")
	// The model still learns the operation, where it went, which parameters it sent, and why it
	// failed.
	s.check(t, urlSecrets, `Get \"http://REDACTED@127.0.0.1:1/v1/items?key=REDACTED`, "page=REDACTED#REDACTED", "connection refused")
}

// A URL written into an error's text by hand (not a *url.Error) is redacted the same way.
func TestToolErrorURLInTextNotJournaled(t *testing.T) {
	type in struct{}
	tool := agent.Func("call", "call", agent.Safety{ReadOnly: true}, func(context.Context, in) (string, error) {
		return "", fmt.Errorf("upstream https://USER-TOKEN-SECRET@api.test/v2?token=SK-QUERY-SECRET returned 503 (see https://status.test/page)")
	})
	var s seen
	st := agent.NewMemStore()
	a := agent.New(agent.NewScriptedModel(agent.ToolTurn("tu1", "call", `{}`), agent.TextTurn("done")), st, tool).Use(s.middleware)
	if _, err := a.Run(context.Background(), "r1", "go"); err != nil {
		t.Fatal(err)
	}
	s.journal(t, st, "r1")
	s.check(t, urlSecrets, "upstream https://REDACTED@api.test/v2?token=REDACTED returned 503 (see https://status.test/page)")
}

// A sub-agent's failure becomes the parent's tool error: its text is redacted before the
// parent journals it.
func TestSubAgentErrorURLCredentialsNotJournaled(t *testing.T) {
	failing := agent.NewScriptedModel(agent.ErrorTurn(&url.Error{Op: "Post", URL: "https://api.test/v1/chat?key=SK-QUERY-SECRET", Err: errors.New("EOF")}))
	st := agent.NewMemStore()
	sub := agent.New(failing, st)
	var s seen
	parent := agent.New(agent.NewScriptedModel(agent.ToolTurn("tu1", "helper", `{"task":"x"}`), agent.TextTurn("done")), st,
		agent.SubAgent("helper", "helps", sub)).Use(s.middleware)
	if _, err := parent.Run(context.Background(), "r1", "go"); err != nil {
		t.Fatal(err)
	}
	s.journal(t, st, "r1")
	s.check(t, urlSecrets, `Post \"https://api.test/v1/chat?key=REDACTED\": EOF`)
}

// In a saga, a failing tool's error is journaled twice: as the saga's failure record and as the
// terminal marker of the finished rollback. Both are redacted; the caller still gets the error.
func TestSagaToolErrorURLCredentialsNotJournaled(t *testing.T) {
	st := agent.NewMemStore()
	a := agent.New(agent.NewScriptedModel(agent.ToolTurn("tu1", "fetch", `{}`), agent.TextTurn("done")), st, fetchTool(secretURL))
	_, err := a.RunSaga(context.Background(), "r1", "go")
	var aborted *agent.SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("RunSaga = %v, want *SagaAborted", err)
	}
	if !strings.Contains(aborted.Cause.Error(), "SK-QUERY-SECRET") {
		t.Errorf("SagaAborted.Cause = %v, want the tool's own error", aborted.Cause)
	}
	var s seen
	s.journal(t, st, "r1")
	s.check(t, urlSecrets, `key=REDACTED`)
	recs, _ := st.History(context.Background(), "r1")
	var failure, marker string
	for _, r := range recs {
		switch {
		case r.Kind == agent.StepSagaFail:
			failure = string(r.Result)
		case r.Name == "run:aborted":
			marker = string(r.Result)
		}
	}
	if failure == "" || marker != failure {
		t.Errorf("the rollback's terminal marker records %s, want the saga failure's text %s", marker, failure)
	}
	// A resumed saga reads the cause back from the journal: the terminal marker holds it redacted.
	_, err = a.RunSaga(context.Background(), "r1", "go")
	if !errors.As(err, &aborted) || strings.Contains(aborted.Cause.Error(), "SK-QUERY-SECRET") {
		t.Errorf("resumed RunSaga = %v, want *SagaAborted with the journaled (redacted) cause", err)
	}
}

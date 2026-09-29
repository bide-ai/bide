package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestRedactURLs(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"no url here", "no url here"},
		{"see https://docs.test/errors/42.", "see https://docs.test/errors/42."},
		{"https://u:p@h.test/x", "https://REDACTED@h.test/x"},
		{"https://tok@h.test", "https://REDACTED@h.test"},
		{"https://h.test/a@b/c", "https://h.test/a@b/c"},
		{"https://h.test/x?a=1&b=&c", "https://h.test/x?a=REDACTED&b=REDACTED&REDACTED"}, // a bare parameter can be the token itself
		{"https://h.test/x?", "https://h.test/x?"},
		{"https://h.test/x?&a=1&", "https://h.test/x?&a=REDACTED&"},
		{"https://h.test/x#access_token=T", "https://h.test/x#REDACTED"},
		{"https://h.test?k=v#f", "https://h.test?k=REDACTED#REDACTED"},
		{"(at https://h.test/x?k=v).", "(at https://h.test/x?k=REDACTED)."},
		{"a postgres://u:pw@db:5432/app?sslmode=off and ftp://h/f", "a postgres://REDACTED@db:5432/app?sslmode=REDACTED and ftp://h/f"},
	} {
		if got := redactURLs(c.in); got != c.want {
			t.Errorf("redactURLs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A *url.Error's URL is redacted as a whole, even where a scan of the text would end it early (a
// quote in the query) and leave the rest of the credential behind.
func TestToolErrorTextURLError(t *testing.T) {
	ue := &url.Error{Op: "Get", URL: `https://h.test/x?q=a"b&key=SK-SECRET`, Err: errors.New("timeout")}
	got := toolErrorText(nil, "fetch", fmt.Errorf("fetch page: %w", ue))
	if strings.Contains(got, "SK-SECRET") || !strings.Contains(got, `fetch page: Get "https://h.test/x?q=REDACTED&key=REDACTED": timeout`) {
		t.Errorf("toolErrorText = %q", got)
	}
}

// WithToolErrorRedactor replaces the text journaled for a failed call; URL credentials are still
// redacted from whatever it returns.
func TestWithToolErrorRedactor(t *testing.T) {
	type in struct{}
	tool := Func("lookup", "lookup", Safety{ReadOnly: true}, func(context.Context, in) (string, error) {
		return "", errors.New("account ACCT-NUMBER-SECRET not found")
	})
	var gotTool string
	var gotErr error
	redact := func(tool string, err error) string {
		gotTool, gotErr = tool, err
		return "lookup failed: not found (see https://docs.test/errors?session=SK-QUERY-SECRET)"
	}
	st := NewMemStore()
	a := New(NewScriptedModel(ToolTurn("tu1", "lookup", `{}`), TextTurn("done")), st, tool).WithToolErrorRedactor(redact)
	if _, err := a.Run(context.Background(), "r1", "go"); err != nil {
		t.Fatal(err)
	}
	if gotTool != "lookup" || gotErr == nil || !strings.Contains(gotErr.Error(), "ACCT-NUMBER-SECRET") {
		t.Errorf("redactor called with (%q, %v), want the tool's name and its own error", gotTool, gotErr)
	}
	recs, _ := st.History(context.Background(), "r1")
	var result string
	for _, r := range recs {
		if r.Kind == StepToolResult {
			result = string(r.Result)
		}
	}
	if want := `"lookup failed: not found (see https://docs.test/errors?session=REDACTED)"`; result != want {
		t.Errorf("journaled result = %s, want %s", result, want)
	}
	// A derived agent (RunTyped clones the agent) keeps the redactor.
	if c := a.clone(); c.toolErrRedact == nil {
		t.Error("a cloned agent dropped its tool-error redactor")
	}
}

// ToolErrorText gives a tool middleware the text the agent journals for a failed call: the
// agent's redactor's, with URLs redacted. Outside an agent's tool call it redacts URLs only.
func TestToolErrorTextInMiddleware(t *testing.T) {
	type in struct{}
	tool := Func("lookup", "lookup", Safety{ReadOnly: true}, func(context.Context, in) (string, error) {
		return "", errors.New("account ACCT-NUMBER-SECRET: https://h.test/x?key=SK-SECRET")
	})
	var seen string
	st := NewMemStore()
	a := New(NewScriptedModel(ToolTurn("tu1", "lookup", `{}`), TextTurn("done")), st, tool).
		WithToolErrorRedactor(func(_ string, err error) string {
			return strings.ReplaceAll(err.Error(), "ACCT-NUMBER-SECRET", "ACCT")
		}).
		UseTool(func(next ToolHandler) ToolHandler {
			return func(ctx context.Context, tu ToolUse) (json.RawMessage, error) {
				res, err := next(ctx, tu)
				if err != nil {
					seen = ToolErrorText(ctx, tu.Name, err)
				}
				return res, err
			}
		})
	if _, err := a.Run(context.Background(), "r1", "go"); err != nil {
		t.Fatal(err)
	}
	recs, _ := st.History(context.Background(), "r1")
	var journaled string
	for _, r := range recs {
		if r.Kind == StepToolResult {
			_ = json.Unmarshal(r.Result, &journaled)
		}
	}
	if want := "account ACCT: https://h.test/x?key=REDACTED"; seen != want || journaled != want {
		t.Errorf("middleware saw %q and the journal holds %q, want both %q", seen, journaled, want)
	}
	if got := ToolErrorText(context.Background(), "lookup", errors.New("at https://h.test/x?key=SK-SECRET")); got != "at https://h.test/x?key=REDACTED" {
		t.Errorf("ToolErrorText outside a tool call = %q", got)
	}
	if got := RedactURLs("https://u:p@h.test/x?key=SK-SECRET"); got != "https://REDACTED@h.test/x?key=REDACTED" {
		t.Errorf("RedactURLs = %q", got)
	}
}

package openai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/schema"
)

type tagsArgs struct {
	Tags map[string]string `json:"tags"`
}

// A tool whose arguments strict mode cannot express (here a map) fails the request with an error
// that names the tool, instead of silently sending a non-strict schema under WithStrictSchema.
func TestStrictSchema_InexpressibleToolIsAnError(t *testing.T) {
	tool := agent.MustFunc("tag", "sets tags", func(context.Context, tagsArgs) (string, error) { return "", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
	_, err := New("k", WithStrictSchema()).buildRequest(agent.Request{Messages: []agent.Message{agent.UserText("hi")}, Tools: []agent.ToolSpec{tool.Spec()}})
	if !errors.Is(err, schema.ErrStrictUnsupported) || !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `"tag"`) {
		t.Fatalf("buildRequest = %v; want ErrStrictUnsupported and ErrConfig naming tool \"tag\"", err)
	}
	// Without WithStrictSchema the neutral schema is sent as is.
	if _, err := New("k").buildRequest(agent.Request{Messages: []agent.Message{agent.UserText("hi")}, Tools: []agent.ToolSpec{tool.Spec()}}); err != nil {
		t.Fatalf("non-strict buildRequest = %v", err)
	}
}

// A response format strict mode cannot express fails the request, rather than sending the
// neutral schema marked strict:true (which the API rejects) or a closed schema whose only valid
// answer is empty.
func TestStrictSchema_InexpressibleResponseFormatIsAnError(t *testing.T) {
	sch, err := schema.For[tagsArgs]()
	if err != nil {
		t.Fatal(err)
	}
	_, err = New("k").buildRequest(agent.Request{Messages: []agent.Message{agent.UserText("hi")},
		ResponseFormat: &agent.ResponseFormat{Name: "response", Schema: json.RawMessage(sch)}})
	if !errors.Is(err, schema.ErrStrictUnsupported) || !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("buildRequest = %v; want ErrStrictUnsupported and ErrConfig", err)
	}
}

// RunTypedNative with a result type strict mode cannot express reports it before any request.
func TestRunTypedNative_InexpressibleTypeIsAnError(t *testing.T) {
	a := agenttest.MustNew(New("k", WithBaseURL("http://127.0.0.1:1")), agenttest.MemJournal())
	_, _, err := a.RunTyped[tagsArgs](context.Background(), "r", agent.UserText("hi"), agent.WithOutputMode(agent.OutputNative))
	if !errors.Is(err, schema.ErrStrictUnsupported) {
		t.Fatalf("RunTypedNative = %v; want ErrStrictUnsupported", err)
	}
}

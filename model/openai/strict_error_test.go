package openai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/schema"
)

type tagsArgs struct {
	Tags map[string]string `json:"tags"`
}

// A tool whose arguments strict mode cannot express (here a map) fails the request with an error
// that names the tool, instead of silently sending a non-strict schema under WithStrictSchema.
func TestStrictSchema_InexpressibleToolIsAnError(t *testing.T) {
	tool := agent.Func("tag", "sets tags", agent.Safety{ReadOnly: true}, func(context.Context, tagsArgs) (string, error) { return "", nil })
	_, err := New("k", WithStrictSchema()).buildRequest(agent.Request{Messages: []agent.Message{agent.UserText("hi")}, Tools: []agent.ToolSpec{agent.SpecOf(tool)}})
	if !errors.Is(err, schema.ErrStrictUnsupported) || !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `"tag"`) {
		t.Fatalf("buildRequest = %v; want ErrStrictUnsupported and ErrConfig naming tool \"tag\"", err)
	}
	// Without WithStrictSchema the neutral schema is sent as is.
	if _, err := New("k").buildRequest(agent.Request{Messages: []agent.Message{agent.UserText("hi")}, Tools: []agent.ToolSpec{agent.SpecOf(tool)}}); err != nil {
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
	a := agent.New(New("k", WithBaseURL("http://127.0.0.1:1")), agent.NewMemStore())
	_, err := agent.RunTypedNative[tagsArgs](context.Background(), a, "r", "hi")
	if !errors.Is(err, schema.ErrStrictUnsupported) {
		t.Fatalf("RunTypedNative = %v; want ErrStrictUnsupported", err)
	}
}

package agent

import (
	"context"
	"testing"
)

// RunTypedNative sets a JSON-schema ResponseFormat on the request and decodes the model's
// direct JSON output (no final_answer tool).
func TestRunTypedNative_ResponseFormatAndDecode(t *testing.T) {
	type R struct {
		Answer string `json:"answer"`
		Score  int    `json:"score"`
	}
	var got Request
	m := &captureModel{inner: &scriptModel{turns: [][]Emit{textTurn(`{"answer":"42","score":7}`)}}, got: &got}
	a := New(m, NewMemStore())

	out, err := RunTypedNative[R](context.Background(), a, "r", "q")
	if err != nil {
		t.Fatalf("RunTypedNative: %v", err)
	}
	if out.Answer != "42" || out.Score != 7 {
		t.Fatalf("out = %+v, want {42 7}", out)
	}
	// The request carried a native response-format constraint, and no final_answer tool
	// was injected (native mode, not the tool-based path).
	if got.ResponseFormat == nil || got.ResponseFormat.Name != "response" || len(got.ResponseFormat.Schema) == 0 {
		t.Fatalf("ResponseFormat = %+v, want set with a schema", got.ResponseFormat)
	}
	for _, tl := range got.Tools {
		if tl.Name == finalAnswerTool {
			t.Fatal("native mode must not inject the final_answer tool")
		}
	}
}

// The caller's agent is untouched (RunTypedNative works on a clone).
func TestRunTypedNative_DoesNotMutateAgent(t *testing.T) {
	type R struct {
		X int `json:"x"`
	}
	m := &scriptModel{turns: [][]Emit{textTurn(`{"x":1}`)}}
	a := New(m, NewMemStore())
	if _, err := RunTypedNative[R](context.Background(), a, "r", "q"); err != nil {
		t.Fatal(err)
	}
	if a.responseFormat != nil {
		t.Fatal("RunTypedNative must not set responseFormat on the caller's agent")
	}
}

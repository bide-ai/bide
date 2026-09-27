package openai

import (
	"encoding/json"
	"testing"

	agent "github.com/blackwell-systems/bide"
)

// A Request.ResponseFormat maps to an OpenAI strict json_schema response_format.
func TestBuildRequest_ResponseFormat(t *testing.T) {
	m := New("k")
	body, err := m.buildRequest(agent.Request{
		Messages: []agent.Message{agent.UserText("hi")},
		ResponseFormat: &agent.ResponseFormat{
			Name:   "response",
			Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	rf, ok := p["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_schema" {
		t.Fatalf("response_format = %v, want type json_schema", p["response_format"])
	}
	js := rf["json_schema"].(map[string]any)
	if js["name"] != "response" || js["strict"] != true {
		t.Fatalf("json_schema = %v, want name=response strict=true", js)
	}
	// OpenAI strict requires the schema be closed.
	sch := js["schema"].(map[string]any)
	if sch["additionalProperties"] != false {
		t.Errorf("schema not closed (additionalProperties): %v", sch)
	}
}

// With no ResponseFormat, the key is absent.
func TestBuildRequest_NoResponseFormat(t *testing.T) {
	m := New("k")
	body, _ := m.buildRequest(agent.Request{Messages: []agent.Message{agent.UserText("hi")}})
	var p map[string]any
	_ = json.Unmarshal(body, &p)
	if _, present := p["response_format"]; present {
		t.Error("response_format should be absent when unset")
	}
}

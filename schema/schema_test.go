package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

type Address struct {
	City string `json:"city" desc:"city name"`
	Zip  string `json:"zip,omitempty"`
}

type Person struct {
	Name    string   `json:"name" desc:"full name"`
	Age     int      `json:"age"`
	Nick    *string  `json:"nick,omitempty"` // pointer → optional
	Tags    []string `json:"tags,omitempty"`
	Home    Address  `json:"home"` // nested struct → inlined
	private int      // unexported → skipped
}

func TestFor_ReflectsStructInline(t *testing.T) {
	raw, err := For[Person]()
	if err != nil {
		t.Fatal(err)
	}
	// No $ref/$defs anywhere — nested structs must be inlined (LLM APIs reject refs).
	if strings.Contains(string(raw), "$ref") || strings.Contains(string(raw), "$defs") {
		t.Fatalf("schema contains $ref/$defs (must be inlined): %s", raw)
	}

	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s["type"] != "object" {
		t.Fatalf("root type = %v", s["type"])
	}
	props := s["properties"].(map[string]any)
	if _, ok := props["private"]; ok {
		t.Error("unexported field leaked into schema")
	}
	if props["name"].(map[string]any)["description"] != "full name" {
		t.Error("desc tag not applied to name")
	}
	if props["age"].(map[string]any)["type"] != "integer" {
		t.Error("age should be integer")
	}
	// nested struct inlined as an object with its own properties
	home := props["home"].(map[string]any)
	if home["type"] != "object" || home["properties"].(map[string]any)["city"] == nil {
		t.Errorf("home not inlined as object: %v", home)
	}

	// required = non-pointer, non-omitempty fields only
	req := toStringSet(s["required"])
	for _, want := range []string{"name", "age", "home"} {
		if !req[want] {
			t.Errorf("expected %q required", want)
		}
	}
	for _, notWant := range []string{"nick", "tags"} { // pointer / omitempty
		if req[notWant] {
			t.Errorf("%q should not be required", notWant)
		}
	}
}

func TestOpenAIStrict_ClosesObjectsAndRequiresAll(t *testing.T) {
	neutral, err := For[Person]()
	if err != nil {
		t.Fatal(err)
	}
	strict, err := OpenAIStrict(neutral)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(strict, &s); err != nil {
		t.Fatal(err)
	}
	if s["additionalProperties"] != false {
		t.Error("root object must have additionalProperties:false in strict mode")
	}
	// strict mode: every property required, even the omitempty/pointer ones
	req := toStringSet(s["required"])
	for _, want := range []string{"name", "age", "nick", "tags", "home"} {
		if !req[want] {
			t.Errorf("strict mode should require %q", want)
		}
	}
	// recursion into nested objects
	home := s["properties"].(map[string]any)["home"].(map[string]any)
	if home["additionalProperties"] != false {
		t.Error("nested object must also be closed in strict mode")
	}
}

func toStringSet(v any) map[string]bool {
	out := map[string]bool{}
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			out[e.(string)] = true
		}
	}
	return out
}

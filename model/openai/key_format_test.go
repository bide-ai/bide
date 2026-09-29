package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The adapter holds the provider API key. Formatting it (a config dumped with %+v, a struct
// logged by a structured logger) must not print the key, and its JSON form carries no field.
func TestModelDoesNotDiscloseAPIKey(t *testing.T) {
	const key = "sk-SECRET-key-123"
	m := New(key, WithModel("m-1"))
	want := `openai.Model{model:"m-1", apiKey:[redacted]}`
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		for _, v := range []any{m, *m} {
			if got := fmt.Sprintf(verb, v); got != want {
				t.Errorf("%T formatted with %s = %q, want %q", v, verb, got, want)
			}
		}
	}
	if s, ok := any(m).(fmt.Stringer); !ok || s.String() != want {
		t.Errorf("*Model has no String method returning %q", want)
	}
	out, err := json.Marshal(struct{ M *Model }{m})
	if err != nil || strings.Contains(string(out), key) {
		t.Errorf("json.Marshal = %s, %v; want no key", out, err)
	}
}

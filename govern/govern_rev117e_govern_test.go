package govern_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern"
)

// The spec of a config-built tool is what the old positional form's method set answered.
func TestRev117e_EventToolSpecMatchesOld(t *testing.T) {
	gov, digest := buildCreditGov(t)
	for _, pd := range []string{"", digest} {
		s := agent.SpecOf(govern.EventTool(gov, govern.EventToolConfig{Name: "credit", Description: "d", Event: "credit", PolicyDigest: pd, Safety: agent.Safety{Idempotent: true}}))
		if s.Name != "credit" || s.Description != "d" || s.Safety != (agent.Safety{Idempotent: true}) || s.Approval != nil || s.Timeout != 0 {
			t.Fatalf("pd %q: spec = %+v", pd, s)
		}
	}
}

// Old AttestedEventTool(gov, n, d, e, "", s) recorded state_digest and the acting identity even
// with an empty policy digest. EventToolConfig{PolicyDigest: ""} is the plain EventTool, so a
// migrated call with an empty digest would lose both without a word. A config that asks for the
// attested form (Attested) with an empty PolicyDigest is refused with ErrConfig; the plain form
// still records neither.
func TestRev117e_EmptyDigestAttestedFormLost(t *testing.T) {
	gov, digest := buildCreditGov(t)
	func() {
		defer func() {
			err, _ := recover().(error)
			if !errors.Is(err, agent.ErrConfig) {
				t.Fatalf("EventTool{Attested, PolicyDigest: \"\"}: recover = %v, want ErrConfig", err)
			}
		}()
		govern.EventTool(gov, govern.EventToolConfig{Name: "credit", Event: "credit", Attested: true})
	}()
	ctx := agent.WithIdentity(context.Background(), agent.Identity{Actor: "a"})
	for _, tc := range []struct {
		cfg      govern.EventToolConfig
		attested bool
	}{
		{govern.EventToolConfig{Name: "credit", Event: "credit"}, false},
		{govern.EventToolConfig{Name: "credit", Event: "credit", Attested: true, PolicyDigest: digest}, true},
		{govern.EventToolConfig{Name: "credit", Event: "credit", PolicyDigest: digest}, true},
	} {
		raw, err := govern.EventTool(gov, tc.cfg).Call(ctx, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		_ = json.Unmarshal(raw, &got)
		_, sd := got["state_digest"]
		if sd != tc.attested || (got["actor"] == "a") != tc.attested {
			t.Fatalf("%+v: result %v, want attested=%v", tc.cfg, got, tc.attested)
		}
	}
}

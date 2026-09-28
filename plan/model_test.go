package plan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blackwell-systems/bide/agent"
)

// A Model node binds a real model to the flow: it renders the node's prompt from the
// typed input, calls the bound model, and decodes the model's JSON text response into
// the node's output type. These tests use a DETERMINISTIC fake agent.Model (a canned
// structured response, no network, no API key) so the render/call/decode path is
// exercised end to end without a provider.

// fakeModel is a deterministic agent.Model. It captures the last rendered prompt it
// was sent (the concatenated user text) and streams back a canned assistant message:
// reply is the assistant text (a JSON document the Model node decodes into O). If
// failWith is non-nil, Stream returns it instead, exercising the generate-error path.
type fakeModel struct {
	reply    string // canned assistant text (JSON) the flow decodes into O
	failWith error  // if set, Stream returns this error
	lastSeen *string
}

func (m *fakeModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	if m.failWith != nil {
		return nil, m.failWith
	}
	if m.lastSeen != nil {
		// Record the rendered prompt (the last user message's text) so a test can assert
		// the input was rendered into it.
		var b strings.Builder
		for _, msg := range req.Messages {
			b.WriteString(msg.Text())
		}
		*m.lastSeen = b.String()
	}
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: m.reply}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	close(ch)
	return agent.NewStream(ch), nil
}

// review is a structured Model output: a JSON-shaped struct the fake model returns
// and the Model node decodes into.
type review struct {
	Verdict string `json:"verdict"`
	Score   int    `json:"score"`
}

// ticket is a structured Model input rendered into the prompt via text/template.
type ticket struct {
	Subject string
	Body    string
}

// TestModel_RendersCallsAndDecodes proves the whole path: a bound Model node renders
// the prompt from the typed input (a {{.Field}} template), calls the model, and
// decodes the model's structured JSON response into O. The flow Runs to a typed Out.
func TestModel_RendersCallsAndDecodes(t *testing.T) {
	var seen string
	fake := &fakeModel{
		reply:    `{"verdict":"approve","score":91}`,
		lastSeen: &seen,
	}

	b := New[ticket, review]("triage").WithModel(fake)
	b.Model[ticket, review]("assess", "Subject: {{.Subject}}\nBody: {{.Body}}\nRespond with JSON {verdict, score}.")
	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	out, err := flow.Run(context.Background(), agent.NewMemStore(), "run1", ticket{Subject: "refund", Body: "please refund my order"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The prompt was rendered from the typed input's fields.
	if !strings.Contains(seen, "Subject: refund") || !strings.Contains(seen, "Body: please refund my order") {
		t.Errorf("model saw prompt %q; want the input rendered via the template", seen)
	}
	// The structured response was decoded into O.
	if out.Verdict != "approve" || out.Score != 91 {
		t.Errorf("decoded output = %+v, want {approve 91}", out)
	}
}

// TestModel_NoBoundModel_ErrorsAtBuild proves a flow declaring a Model node with NO
// bound model returns a clear error at Build, naming the node, rather than a nil
// dereference at Run.
func TestModel_NoBoundModel_ErrorsAtBuild(t *testing.T) {
	b := New[ticket, review]("triage")
	b.Model[ticket, review]("assess", "prompt")
	_, err := b.Build()
	if err == nil {
		t.Fatal("Build succeeded with no bound model; want an error")
	}
	if !strings.Contains(err.Error(), "assess") || !strings.Contains(err.Error(), "no bound model") {
		t.Errorf("Build error = %v; want it to name the node %q and say it has no bound model", err, "assess")
	}
}

// TestModel_LoadedFlow_BindsModel proves a Model node built via the config loader
// (RegisterModel + Load) runs once a model is bound to the loaded flow with
// WithLoadedModel: it renders, calls, and decodes exactly like a hand-built flow.
func TestModel_LoadedFlow_BindsModel(t *testing.T) {
	var seen string
	fake := &fakeModel{reply: `{"verdict":"deny","score":10}`, lastSeen: &seen}

	reg := NewRegistry()
	if err := RegisterModel[ticket, review](reg, "assessBlock", "Subject: {{.Subject}}"); err != nil {
		t.Fatalf("RegisterModel: %v", err)
	}
	cfg := `{"flow":"triage","entry":"assess","nodes":[{"name":"assess","block":"assessBlock"}],"wiring":[]}`

	flow, err := Load[ticket, review]([]byte(cfg), reg, WithLoadedModel(fake))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := flow.Run(context.Background(), agent.NewMemStore(), "loaded1", ticket{Subject: "spam"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(seen, "Subject: spam") {
		t.Errorf("loaded model saw prompt %q; want the input rendered", seen)
	}
	if out.Verdict != "deny" || out.Score != 10 {
		t.Errorf("decoded output = %+v, want {deny 10}", out)
	}
}

// TestModel_LoadedFlow_NoBoundModel_ErrorsAtBuild proves a config declaring a Model
// block loaded WITHOUT a bound model fails Build (via Load), naming the node.
func TestModel_LoadedFlow_NoBoundModel_ErrorsAtBuild(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterModel[ticket, review](reg, "assessBlock", "prompt"); err != nil {
		t.Fatalf("RegisterModel: %v", err)
	}
	cfg := `{"flow":"triage","entry":"assess","nodes":[{"name":"assess","block":"assessBlock"}],"wiring":[]}`

	_, err := Load[ticket, review]([]byte(cfg), reg) // no WithLoadedModel
	if err == nil {
		t.Fatal("Load succeeded with no bound model; want an error")
	}
	if !strings.Contains(err.Error(), "assess") || !strings.Contains(err.Error(), "no bound model") {
		t.Errorf("Load error = %v; want it to name the node and say it has no bound model", err)
	}
}

// buildModelFlow builds a single-node Model flow bound to fake, for the crash sweep.
// The input is a plain int rendered into the prompt; the output is review.
func buildModelFlow(fake agent.Model) (*Flow[int, review], error) {
	b := New[int, review]("model-flow").WithModel(fake)
	b.Model[int, review]("assess", "score {{.}}")
	return b.Build()
}

// TestModel_DefaultHaltsOnAmbiguousCrash proves a Model node keeps the conservative
// halt-on-ambiguous-crash default (Safety default preserved): a model call is
// non-idempotent, so on a crash that persists the attempt marker but loses the result
// Run returns *HaltAmbiguous and does NOT re-call the model. This mirrors the DST
// crash-sweep in flow_dst_test.go (crashFlowStore, errCrash, per-write sweep).
func TestModel_DefaultHaltsOnAmbiguousCrash(t *testing.T) {
	var mem agent.Durable
	var haltCalls int // how many times the model was called at the halt point
	found := false
	for crashAt := 1; crashAt <= 32; crashAt++ {
		calls := 0
		m := agent.NewMemStore()
		store := &crashFlowStore{inner: m, crashAt: crashAt}
		fake := &countingModel{reply: `{"verdict":"ok","score":1}`, calls: &calls}
		flow, err := buildModelFlow(fake)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		_, err = flow.Run(context.Background(), store, "model-crash", 0)
		if !errors.Is(err, errCrash) {
			continue
		}
		// The ambiguous window is the "assess" result write: attempt marker persisted,
		// result missing.
		recs, hErr := m.History(context.Background(), "model-crash")
		if hErr != nil {
			t.Fatalf("History: %v", hErr)
		}
		var haveAttempt, haveResult bool
		for _, r := range recs {
			switch r.Name {
			case "attempt:assess":
				haveAttempt = true
			case "assess":
				haveResult = true
			}
		}
		if haveAttempt && !haveResult {
			mem = m
			haltCalls = calls
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no crash point produced the model-result ambiguous window")
	}

	// Resume with no further crash: the default (non-idempotent) Model node must HALT
	// rather than re-call the model.
	calls := haltCalls
	fake := &countingModel{reply: `{"verdict":"ok","score":1}`, calls: &calls}
	flow, err := buildModelFlow(fake)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	_, err = flow.Run(context.Background(), mem, "model-crash", 0)
	var halt *HaltAmbiguous
	if !errors.As(err, &halt) {
		t.Fatalf("default Model node did not halt: err = %v; want *HaltAmbiguous", err)
	}
	if halt.Step != "assess" {
		t.Fatalf("halt named %q, want %q", halt.Step, "assess")
	}
	if calls != haltCalls {
		t.Fatalf("Model node re-called the model on resume (calls %d -> %d); a non-idempotent model call must halt", haltCalls, calls)
	}
}

// countingModel is a deterministic fake that counts how many times Stream was called,
// so a crash-sweep test can assert the model was not re-called across a halt.
type countingModel struct {
	reply string
	calls *int
}

func (m *countingModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	*m.calls++
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: m.reply}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	close(ch)
	return agent.NewStream(ch), nil
}

// TestModel_GenerateErrorPropagates proves a model that errors surfaces a clear,
// node-named error from Run rather than an opaque failure.
func TestModel_GenerateErrorPropagates(t *testing.T) {
	fake := &fakeModel{failWith: errors.New("provider down")}
	b := New[ticket, review]("triage").WithModel(fake)
	b.Model[ticket, review]("assess", "prompt")
	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	_, err = flow.Run(context.Background(), agent.NewMemStore(), "err1", ticket{Subject: "x"})
	if err == nil {
		t.Fatal("Run succeeded despite a failing model; want an error")
	}
	if !strings.Contains(err.Error(), "assess") || !strings.Contains(err.Error(), "provider down") {
		t.Errorf("Run error = %v; want it to name the node and wrap the provider error", err)
	}
}

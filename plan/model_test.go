package plan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
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

	out, err := flow.Run(context.Background(), agenttest.MemJournal(), "run1", ticket{Subject: "refund", Body: "please refund my order"})
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
	if err := reg.RegisterModel[ticket, review]("assessBlock", "Subject: {{.Subject}}"); err != nil {
		t.Fatalf("RegisterModel: %v", err)
	}
	cfg := `{"version":1,"flow":"triage","entry":"assess","nodes":[{"name":"assess","block":"assessBlock"}],"wiring":[]}`

	flow, err := Load[ticket, review]([]byte(cfg), reg, WithLoadedModel(fake))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := flow.Run(context.Background(), agenttest.MemJournal(), "loaded1", ticket{Subject: "spam"})
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
	if err := reg.RegisterModel[ticket, review]("assessBlock", "prompt"); err != nil {
		t.Fatalf("RegisterModel: %v", err)
	}
	cfg := `{"version":1,"flow":"triage","entry":"assess","nodes":[{"name":"assess","block":"assessBlock"}],"wiring":[]}`

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

// TestModel_DefaultRetriesOnAmbiguousCrash proves a Model node is retry-safe by
// default: a model call has no external side effect, so on a crash that loses its
// result Run re-calls the model on resume and completes, rather than halting with
// *agent.OutcomeUnknown. A retry-safe node writes no attempt marker, so the ambiguous
// window is "the model was called, the journal holds no node:assess result". This
// mirrors the DST crash-sweep in flow_dst_test.go (crashFlowStore, errCrash, per-write
// sweep).
func TestModel_DefaultRetriesOnAmbiguousCrash(t *testing.T) {
	var mem *agent.Journal
	var crashCalls int // how many times the model was called before the crash
	for crashAt := 1; crashAt <= 32 && mem == nil; crashAt++ {
		calls := 0
		j := agenttest.MustJournal(agent.NewMemStore())
		store := agenttest.MustJournal(&crashFlowStore{inner: j, crashAt: crashAt})
		flow, err := buildModelFlow(&countingModel{reply: `{"verdict":"ok","score":1}`, calls: &calls})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if _, err := flow.Run(context.Background(), store, "model-crash", 0); !errors.Is(err, errCrash) {
			continue
		}
		recs, hErr := j.History(context.Background(), "model-crash")
		if hErr != nil {
			t.Fatalf("History: %v", hErr)
		}
		if hasRecord(recs, "attempt:step:node:assess") {
			t.Fatalf("a default Model node wrote an attempt marker; want it retry-safe (no marker)")
		}
		if calls >= 1 && !hasRecord(recs, "node:assess") {
			mem, crashCalls = j, calls
		}
	}
	if mem == nil {
		t.Fatal("no crash point produced the model-result ambiguous window")
	}

	calls := crashCalls
	flow, err := buildModelFlow(&countingModel{reply: `{"verdict":"ok","score":1}`, calls: &calls})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := flow.Run(context.Background(), mem, "model-crash", 0)
	if err != nil {
		t.Fatalf("resume after the crash: %v; want the Model node re-called", err)
	}
	if calls != crashCalls+1 {
		t.Fatalf("model calls %d -> %d on resume; want exactly one re-call", crashCalls, calls)
	}
	if out.Verdict != "ok" || out.Score != 1 {
		t.Fatalf("resumed output = %+v, want {ok 1}", out)
	}
}

// scriptedModel replies with replies[i] on its i-th call (the last reply repeats), so a
// test can make a first call return what a real model sometimes does (fenced JSON, a
// number in a string field) and a later call return a valid document.
type scriptedModel struct {
	replies []string
	calls   int
}

func (m *scriptedModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	reply := m.replies[min(m.calls, len(m.replies)-1)]
	m.calls++
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: reply}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	close(ch)
	return agent.NewStream(ch), nil
}

// A Model node whose call fails (the provider errors, or the reply does not decode into
// O) records nothing, and the next drive of the run calls the model again rather than
// halting with "unknown outcome and is not retry-safe". This is the run a fresh user hit
// with a real model: a fenced reply, then a number in a string field, then a halted
// resume. Each case runs the Builder flow and the Load flow (RegisterModel), whose
// defaults must agree.
func TestModel_FailedCallRetriesOnTheNextDrive(t *testing.T) {
	good := `{"verdict":"ok","score":1}`
	for name, first := range map[string]string{
		"fenced JSON": "```json\n" + good + "\n```",
		"wrong type":  `{"verdict":7,"score":1}`,
	} {
		builds := map[string]func(agent.Model) (*Flow[int, review], error){
			"builder": buildModelFlow,
			"loaded": func(m agent.Model) (*Flow[int, review], error) {
				reg := NewRegistry()
				if err := reg.RegisterModel[int, review]("assess", "score {{.}}"); err != nil {
					return nil, err
				}
				cfg := `{"version":1,"flow":"model-flow","nodes":[{"name":"assess","block":"assess"}],"wiring":[]}`
				return Load[int, review]([]byte(cfg), reg, WithLoadedModel(m))
			},
		}
		for bname, build := range builds {
			m := &scriptedModel{replies: []string{first, good}}
			flow, err := build(m)
			if err != nil {
				t.Fatalf("%s/%s: build: %v", name, bname, err)
			}
			j := agenttest.MemJournal()
			if _, err := flow.Run(context.Background(), j, "r", 0); err == nil {
				t.Fatalf("%s/%s: first drive succeeded on a bad reply; want a decode error", name, bname)
			}
			out, err := flow.Run(context.Background(), j, "r", 0)
			if err != nil {
				t.Fatalf("%s/%s: second drive: %v; want the model called again", name, bname, err)
			}
			if m.calls != 2 || out.Verdict != "ok" {
				t.Fatalf("%s/%s: calls = %d, out = %+v; want 2 calls and {ok 1}", name, bname, m.calls, out)
			}
		}
	}
}

// The default is an override point, not a rule: a config "safety": "side_effect" lowers a
// Model node back to halt-on-unknown-outcome, and a side-effect Step still halts after a
// failed body. Both keep the marker, so the next drive halts and does not run the body again.
func TestModel_SideEffectStillHaltsAfterAFailedCall(t *testing.T) {
	bad := "```json\n{}\n```"
	m := &scriptedModel{replies: []string{bad, `{"verdict":"ok","score":1}`}}
	reg := NewRegistry()
	if err := reg.RegisterModel[int, review]("assess", "score {{.}}"); err != nil {
		t.Fatalf("RegisterModel: %v", err)
	}
	cfg := `{"version":1,"flow":"model-flow","nodes":[{"name":"assess","block":"assess","safety":"side_effect"}],"wiring":[]}`
	modelFlow, err := Load[int, review]([]byte(cfg), reg, WithLoadedModel(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	runs := 0
	b := New[int, int]("tool-flow")
	b.Step("charge", func(context.Context, int) (int, error) {
		runs++
		return 0, errors.New("connection reset after the charge was sent")
	})
	stepFlow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for name, drive := range map[string]func(*agent.Journal) error{
		"model marked side_effect": func(j *agent.Journal) error { _, err := modelFlow.Run(context.Background(), j, "r", 0); return err },
		"side-effect step":         func(j *agent.Journal) error { _, err := stepFlow.Run(context.Background(), j, "r", 0); return err },
	} {
		j := agenttest.MemJournal()
		if err := drive(j); err == nil {
			t.Fatalf("%s: first drive succeeded; want the body's error", name)
		}
		var halt *agent.OutcomeUnknown
		if err := drive(j); !errors.As(err, &halt) {
			t.Fatalf("%s: second drive err = %v; want *agent.OutcomeUnknown", name, err)
		}
	}
	if m.calls != 1 || runs != 1 {
		t.Fatalf("model calls = %d, step runs = %d; want each body run once, then a halt", m.calls, runs)
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
	_, err = flow.Run(context.Background(), agenttest.MemJournal(), "err1", ticket{Subject: "x"})
	if err == nil {
		t.Fatal("Run succeeded despite a failing model; want an error")
	}
	if !strings.Contains(err.Error(), "assess") || !strings.Contains(err.Error(), "provider down") {
		t.Errorf("Run error = %v; want it to name the node and wrap the provider error", err)
	}
}

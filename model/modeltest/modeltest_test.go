package modeltest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// sseAdapter is a minimal SSE-reading agent.Model. Each "data: x" line is a text delta and
// "data: [DONE]" ends the turn. A well-behaved adapter closes the body when the consumer stops
// and fails a response that ends before [DONE].
type sseAdapter struct {
	baseURL string
	client  *http.Client
	// leak makes the adapter never close the response body and read it on a goroutine that
	// blocks on an unbuffered channel, the bug Run exists to catch.
	leak bool
	// eofIsDone makes the adapter end the turn when the body ends, the truncation bug.
	eofIsDone bool
	// lax makes the adapter skip the tool-name and tool-choice checks.
	lax bool
}

var toolNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func (a sseAdapter) validate(req agent.Request) error {
	seen := map[string]bool{}
	for _, tl := range req.Tools {
		if !toolNameRE.MatchString(tl.Name) {
			return fmt.Errorf("%w: tool name %q", agent.ErrConfig, tl.Name)
		}
		if seen[tl.Name] {
			return fmt.Errorf("%w: duplicate tool %q", agent.ErrConfig, tl.Name)
		}
		seen[tl.Name] = true
	}
	c := req.ToolChoice
	if c == nil {
		return nil
	}
	switch c.Mode {
	case "auto", "none":
	case "required":
		if len(req.Tools) == 0 {
			return fmt.Errorf("%w: required with no tools", agent.ErrConfig)
		}
	case "tool":
		if !seen[c.Name] {
			return fmt.Errorf("%w: tool choice %q not declared", agent.ErrConfig, c.Name)
		}
	default:
		return fmt.Errorf("%w: mode %q", agent.ErrConfig, c.Mode)
	}
	return nil
}

func (a sseAdapter) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	if !a.lax {
		if err := a.validate(req); err != nil {
			return nil, err
		}
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(hr)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%w: status %d", agent.ErrModel, resp.StatusCode)
	}
	if a.leak {
		ch := make(chan agent.Emit) // nobody receives once the consumer is gone
		go func() {
			defer close(ch)
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				ch <- agent.Emit{Event: agent.TextDelta{Text: sc.Text()}}
			}
		}()
		return agent.NewStream(ch), nil
	}
	return agent.NewStreamFunc(ctx, func(send func(agent.Emit) bool) {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "data: [DONE]":
				send(agent.Emit{Event: agent.Finish{Reason: agent.FinishStop}})
				return
			case strings.HasPrefix(line, "data: "):
				if !send(agent.Emit{Event: agent.TextDelta{Text: line[len("data: "):]}}) {
					return
				}
			}
		}
		if a.eofIsDone {
			send(agent.Emit{Event: agent.Finish{Reason: agent.FinishStop}})
			return
		}
		send(agent.Emit{Err: fmt.Errorf("%w: stream ended before [DONE]", agent.ErrModel)})
	}), nil
}

func newAdapter(opts func(*sseAdapter)) func(string, *http.Client) agent.Model {
	return func(baseURL string, c *http.Client) agent.Model {
		a := sseAdapter{baseURL: baseURL, client: c}
		if opts != nil {
			opts(&a)
		}
		return a
	}
}

func chunk(i int) string { return fmt.Sprintf("data: chunk%d\n\n", i) }

// runChild re-executes the test binary on one of the child tests below and returns its output
// and whether it exited non-zero. Run, ToolConfig and ToolNames report through *testing.T, so a
// check that must fail is observed from outside the test that runs it.
func runChild(t *testing.T, child string) (out string, failed bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+child+"$", "-test.v")
	cmd.Env = append(os.Environ(), "MODELTEST_CHILD=1")
	b, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("running %s: %v", child, err)
	}
	return string(b), err != nil
}

func child(t *testing.T) {
	t.Helper()
	if os.Getenv("MODELTEST_CHILD") == "" {
		t.Skip("runs only as a child of the test that expects it to fail")
	}
}

func TestRun_AcceptsAWellBehavedAdapter(t *testing.T) {
	Run(t, newAdapter(nil), chunk)
}

func TestChildRunLeaky(t *testing.T) {
	child(t)
	Run(t, newAdapter(func(a *sseAdapter) { a.leak = true }), chunk)
}

func TestChildRunEOFIsDone(t *testing.T) {
	child(t)
	Run(t, newAdapter(func(a *sseAdapter) { a.eofIsDone = true }), chunk)
}

// Run fails an adapter that never closes the body once the consumer stops, on both the break and
// the cancel paths, and does not blame it for truncation.
func TestRun_RejectsALeakingAdapter(t *testing.T) {
	t.Parallel()
	out, failed := runChild(t, "TestChildRunLeaky")
	if !failed {
		t.Fatalf("a leaking adapter passed Run:\n%s", out)
	}
	for _, want := range []string{
		"--- FAIL: TestChildRunLeaky/BreakReleases",
		"--- FAIL: TestChildRunLeaky/CancelReleases",
		"2s after the consumer broke out of Events",
		"2s after the stream's context was cancelled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Run fails an adapter that reads a response cut before the end-of-turn marker as complete.
func TestRun_RejectsATruncationAccepted(t *testing.T) {
	t.Parallel()
	out, failed := runChild(t, "TestChildRunEOFIsDone")
	if !failed {
		t.Fatalf("an adapter that accepts a truncated turn passed Run:\n%s", out)
	}
	if !strings.Contains(out, "--- FAIL: TestChildRunEOFIsDone/TruncatedIsAnError") ||
		!strings.Contains(out, "ended before the end-of-turn marker was accepted as complete") {
		t.Errorf("want the truncation check to fail:\n%s", out)
	}
	for _, other := range []string{"BreakReleases", "CancelReleases"} {
		if strings.Contains(out, "--- FAIL: TestChildRunEOFIsDone/"+other) {
			t.Errorf("%s failed for an adapter that releases its body:\n%s", other, out)
		}
	}
}

// The hook turns a non-200 answer into an error before the adapter sees a body, so a check
// against a server that refuses fails at Stream.
func TestBodyCloseHook_RefusesANon200(t *testing.T) {
	srvHits := 0
	rt := bodyCloseHook{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		srvHits++
		return &http.Response{StatusCode: http.StatusTeapot, Status: "418 I'm a teapot", Body: io.NopCloser(strings.NewReader(""))}, nil
	}), closed: make(chan struct{})}
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)
	resp, err := rt.RoundTrip(req)
	if err == nil || resp != nil || !strings.Contains(err.Error(), "418 I'm a teapot") {
		t.Fatalf("RoundTrip = %v, %v; want a modeltest error naming the status", resp, err)
	}
	if srvHits != 1 {
		t.Errorf("next transport hit %d times, want 1", srvHits)
	}
}

func TestBodyCloseHook_PassesTransportErrors(t *testing.T) {
	boom := errors.New("boom")
	rt := bodyCloseHook{next: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, boom }), closed: make(chan struct{})}
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)
	if _, err := rt.RoundTrip(req); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the transport's error", err)
	}
}

// Closing a hooked body signals once, however many times it is closed, and closes the inner body.
func TestHookedBody_ClosesOnce(t *testing.T) {
	closed := make(chan struct{})
	inner := &countingCloser{Reader: strings.NewReader("x")}
	b := &hookedBody{ReadCloser: inner, closed: closed}
	for range 3 {
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-closed:
	default:
		t.Fatal("the closed channel was not signalled")
	}
	if inner.n != 3 {
		t.Errorf("inner body closed %d times, want 3 (every Close reaches it)", inner.n)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type countingCloser struct {
	io.Reader
	n int
}

func (c *countingCloser) Close() error { c.n++; return nil }

func TestToolConfig_AcceptsAValidatingAdapter(t *testing.T) {
	ToolConfig(t, newAdapter(nil))
}

func TestChildToolConfigLax(t *testing.T) {
	child(t)
	ToolConfig(t, newAdapter(func(a *sseAdapter) { a.lax = true }))
}

func TestToolConfig_RejectsAnAdapterThatAcceptsBadTools(t *testing.T) {
	t.Parallel()
	out, failed := runChild(t, "TestChildToolConfigLax")
	if !failed {
		t.Fatalf("a non-validating adapter passed ToolConfig:\n%s", out)
	}
	for _, want := range []string{
		"space in name: err = ", "duplicate names: err = ", "unknown mode: err = ",
		"tool, undeclared: err = ", "the request reached the provider",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "valid names: err") {
		t.Errorf("a valid request was blamed on the adapter:\n%s", out)
	}
}

// An adapter that refuses a name every provider accepts is caught by the accepted-side checks.
func TestChildToolConfigOverStrict(t *testing.T) {
	child(t)
	ToolConfig(t, func(string, *http.Client) agent.Model { return refuseAll{} })
}

type refuseAll struct{}

func (refuseAll) Stream(context.Context, agent.Request) (*agent.Stream, error) {
	return nil, fmt.Errorf("%w: refuse everything", agent.ErrConfig)
}

func TestToolConfig_RejectsAnAdapterThatRefusesValidTools(t *testing.T) {
	t.Parallel()
	out, failed := runChild(t, "TestChildToolConfigOverStrict")
	if !failed {
		t.Fatalf("an adapter refusing valid requests passed ToolConfig:\n%s", out)
	}
	for _, want := range []string{"valid names: err = ", "auto, no tools: err = ", "tool, declared: err = "} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestNamedTool_IsAWellFormedReadOnlySpec(t *testing.T) {
	s := namedTool("x")
	if s.Name != "x" || !s.Safety.ReadOnly || !strings.Contains(string(s.Input), `"required":["q"]`) {
		t.Errorf("namedTool = %+v", s)
	}
}

func TestToolNames_AcceptsAMatchingRule(t *testing.T) {
	ToolNames(t, newAdapter(nil), []string{"has space", "dot.name", ""}, []string{"ok_name", "a-b"})
}

func TestChildToolNamesLax(t *testing.T) {
	child(t)
	ToolNames(t, newAdapter(func(a *sseAdapter) { a.lax = true }), []string{"has space"}, []string{"fine"})
}

func TestChildToolNamesWrongQuote(t *testing.T) {
	child(t)
	// Refuses with ErrConfig but without quoting the offending name.
	ToolNames(t, func(string, *http.Client) agent.Model { return refuseAll{} }, []string{"bad name"}, nil)
}

func TestToolNames_RejectsAnAdapterThatAcceptsARefusedName(t *testing.T) {
	t.Parallel()
	out, failed := runChild(t, "TestChildToolNamesLax")
	if !failed || !strings.Contains(out, `"has space": err = `) || !strings.Contains(out, "the request reached the provider") {
		t.Fatalf("failed=%v; want the refused name flagged and the leak to the provider:\n%s", failed, out)
	}
}

func TestToolNames_RequiresTheErrorToNameTheTool(t *testing.T) {
	t.Parallel()
	out, failed := runChild(t, "TestChildToolNamesWrongQuote")
	if !failed || !strings.Contains(out, `"bad name": err = `) || !strings.Contains(out, "naming the tool") {
		t.Fatalf("failed=%v; want an error that does not name the tool to be flagged:\n%s", failed, out)
	}
}

func TestCheckSSEPrefix(t *testing.T) {
	// A reader that succeeds with whatever deltas it saw, ending the turn at EOF, turns a cut
	// response into a different success; CheckSSEPrefix must say so.
	eofDone := func(body io.ReadCloser, send func(agent.Emit) bool) {
		defer body.Close()
		b, _ := io.ReadAll(body)
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" && !send(agent.Emit{Event: agent.TextDelta{Text: l}}) {
				return
			}
		}
		send(agent.Emit{Event: agent.Finish{Reason: agent.FinishStop}})
	}
	strict := func(body io.ReadCloser, send func(agent.Emit) bool) {
		defer body.Close()
		b, _ := io.ReadAll(body)
		if !strings.HasSuffix(string(b), "END\n") {
			send(agent.Emit{Err: fmt.Errorf("%w: cut", agent.ErrModel)})
			return
		}
		send(agent.Emit{Event: agent.TextDelta{Text: strings.TrimSuffix(string(b), "END\n")}})
		send(agent.Emit{Event: agent.Finish{Reason: agent.FinishStop}})
	}
	body := []byte("one\ntwo\nEND\n")
	if msg := failsOn(t, func(tb testing.TB) { CheckSSEPrefix(tb, eofDone, body, 4) }); !strings.Contains(msg, "succeeded with a different message") {
		t.Errorf("a reader that completes a cut response passed: %q", msg)
	}
	if msg := failsOn(t, func(tb testing.TB) { CheckSSEPrefix(tb, strict, body, 4) }); msg != "" {
		t.Errorf("a reader that fails a cut response was refused: %q", msg)
	}
	// An out-of-range cut is clamped to the body, so the prefix is the whole response.
	for _, cut := range []int{-5, 1 << 20} {
		if msg := failsOn(t, func(tb testing.TB) { CheckSSEPrefix(tb, strict, body, cut) }); msg != "" {
			t.Errorf("cut %d: %q", cut, msg)
		}
	}
}

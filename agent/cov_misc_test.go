package agent

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

// doneCause is the cancellation cause when there is one, and DeadlineExceeded for a context whose
// deadline has passed but whose timer has not run yet (no cause recorded).
func TestDoneCause(t *testing.T) {
	why := errors.New("operator stop")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(why)
	if got := doneCause(ctx); !errors.Is(got, why) {
		t.Errorf("doneCause(cancelled with a cause) = %v; want the cause", got)
	}
	if got := doneCause(context.Background()); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("doneCause(no cause) = %v; want DeadlineExceeded", got)
	}
}

type covRules struct{ Model }

func (covRules) ToolNameRule() *regexp.Regexp   { return regexp.MustCompile(`^[a-z]+$`) }
func (covRules) RequiresToolsForRequired() bool { return true }

type covModelWrapper struct{ inner Model }

func (w covModelWrapper) Stream(ctx context.Context, r Request) (*Stream, error) {
	return w.inner.Stream(ctx, r)
}
func (w covModelWrapper) Unwrap() Model { return w.inner }

// The tool rules a model declares are found through its wrappers, and a nil model, a model that
// declares none, and a wrapper that unwraps to itself declare none rather than looping.
func TestToolRulesOf(t *testing.T) {
	base := covRules{&scriptModel{}}
	if r, ok := toolRulesOf(covModelWrapper{covModelWrapper{base}}); !ok || !r.RequiresToolsForRequired() {
		t.Errorf("rules behind two wrappers = %v, %v", r, ok)
	}
	if _, ok := toolRulesOf(nil); ok {
		t.Error("a nil model declared rules")
	}
	if _, ok := toolRulesOf(&scriptModel{}); ok {
		t.Error("a model with none declared rules")
	}
	var self covSelfWrapper
	self.inner = &self
	if _, ok := toolRulesOf(self); ok {
		t.Error("a self-wrapping model declared rules")
	}
	// New enforces the declared name rule: a tool whose name the provider would refuse is ErrConfig.
	bad := &countingTool{name: "Bad_Name", calls: new(int)}
	if _, err := New(base, memJournal(), WithTools(bad)); !errors.Is(err, ErrConfig) {
		t.Errorf("New with a tool name the model refuses = %v; want ErrConfig", err)
	}
}

type covSelfWrapper struct{ inner Model }

func (w covSelfWrapper) Stream(ctx context.Context, r Request) (*Stream, error) {
	return w.inner.Stream(ctx, r)
}
func (w covSelfWrapper) Unwrap() Model { return w.inner }

// An agent with no tools offers the model none.
func TestRequestTools_NoToolsOffersNone(t *testing.T) {
	a := mustNew(&scriptModel{}, memJournal())
	if got := a.requestTools(); got != nil {
		t.Errorf("requestTools = %v; want nil", got)
	}
}

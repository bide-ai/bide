package agent

import (
	"context"
	"errors"
	"testing"
)

// A provider reporting negative token counts must not lower the run's totals: the response is
// rejected as a provider protocol fault.
func TestRun_NegativeUsageIsRejected(t *testing.T) {
	neg := Usage{InputTokens: -1000, OutputTokens: 5}
	for name, turn := range map[string][]Emit{
		"answer":   textTurnWithUsage("done", neg),
		"cache":    textTurnWithUsage("done", Usage{CacheReadTokens: -1}),
		"truncate": truncatedTurn(neg),
	} {
		m := &scriptModel{turns: [][]Emit{turn}}
		store := NewMemStore()
		_, err := New(m, store).Run(context.Background(), "r", "go")
		if !errors.Is(err, ErrProtocol) || !errors.Is(err, ErrModel) {
			t.Fatalf("%s: err = %v, want a model protocol error", name, err)
		}
		recs, _ := store.History(context.Background(), "r")
		for _, r := range recs {
			for _, u := range []*Usage{r.Usage, r.DiscardedUsage} {
				if u != nil && (u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0) {
					t.Fatalf("%s: journaled negative usage %+v", name, *u)
				}
			}
		}
	}
}

// A middleware returning negative usage is rejected the same way.
func TestRun_NegativeUsageFromMiddlewareIsRejected(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	neg := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			resp, err := next(ctx, call)
			resp.Usage = Usage{OutputTokens: -7}
			return resp, err
		}
	}
	_, err := New(m, NewMemStore()).Use(neg).Run(context.Background(), "r", "go")
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want a protocol error", err)
	}
}

// A stream that fails after reporting negative usage reports none of the negative part.
func TestStream_FailedStreamNeverReportsNegativeUsage(t *testing.T) {
	ch := make(chan Emit, 3)
	ch <- Emit{Event: TextDelta{Text: "x"}}
	ch <- Emit{Event: Finish{Reason: "stop", Usage: Usage{InputTokens: -3, OutputTokens: 2}}}
	ch <- Emit{Event: TextDelta{Text: "late"}}
	close(ch)
	_, u, err := NewStream(ch).Message()
	if !errors.Is(err, ErrStreamProtocol) {
		t.Fatalf("err = %v, want ErrStreamProtocol", err)
	}
	if u != (Usage{OutputTokens: 2}) {
		t.Fatalf("usage = %+v, want the non-negative part {OutputTokens: 2}", u)
	}
}

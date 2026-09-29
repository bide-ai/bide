package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type refundArgs struct {
	Amount int    `json:"amount"`
	Memo   string `json:"memo,omitempty"`
}

// compensateWith calls a CompensatedFunc's Compensate with recorded args and returns what undo saw.
func compensateWith(t *testing.T, args string) (refundArgs, error) {
	t.Helper()
	var saw refundArgs
	tool := CompensatedFunc("charge", "charge", Safety{},
		func(context.Context, refundArgs) (string, error) { return "ok", nil },
		func(_ context.Context, in refundArgs, _ string) error { saw = in; return nil })
	err := tool.(Compensator).Compensate(context.Background(), json.RawMessage(args), json.RawMessage(`"ok"`))
	return saw, err
}

// Compensation decodes the recorded arguments as the forward call did. A record the strict
// forward decoder accepts yields the same value it gave the forward call.
func TestCompensate_StrictRecordIsTheForwardValue(t *testing.T) {
	for args, want := range map[string]refundArgs{
		`{"amount":5}`:                {Amount: 5},
		`{"amount":5,"memo":"m"}`:     {Amount: 5, Memo: "m"},
		` {"amount":0,"memo":null} `:  {Amount: 0},
		`{"memo":"x","amount":12345}`: {Amount: 12345, Memo: "x"},
	} {
		got, err := compensateWith(t, args)
		if err != nil || got != want {
			t.Errorf("Compensate(%s) saw %+v, %v; want %+v", args, got, err, want)
		}
		// The forward call accepts the same record and decodes the same value.
		var fwd refundArgs
		if err := decodeArgs(json.RawMessage(args), &fwd); err != nil || fwd != got {
			t.Errorf("forward decode of %s = %+v, %v; compensation saw %+v", args, fwd, err, got)
		}
	}
}

// A record written before tool arguments decoded strictly may hold arguments only encoding/json
// accepts (a case variant, an unknown or duplicate name, a missing or null required field). The
// forward call of that time decoded them with encoding/json, so compensation decodes them the same
// way and undoes the value the call acted on, rather than refusing to compensate.
func TestCompensate_OlderLooseRecordIsTheValueItsCallDecoded(t *testing.T) {
	for args, want := range map[string]refundArgs{
		`{"AMOUNT":7}`:              {Amount: 7},
		`{"amount":7,"extra":true}`: {Amount: 7},
		`{"amount":1,"amount":7}`:   {Amount: 7},
		`{"memo":"m"}`:              {Memo: "m"},
		`{"amount":null}`:           {},
		``:                          {},
	} {
		got, err := compensateWith(t, args)
		if err != nil || got != want {
			t.Errorf("Compensate(%q) saw %+v, %v; want %+v", args, got, err, want)
		}
	}
}

// Arguments no decoder accepts are ErrProtocol: compensation never guesses a value.
func TestCompensate_UndecodableRecordIsAnError(t *testing.T) {
	for _, args := range []string{`{"amount":"five"}`, `[1]`, `{"amount":5} {}`} {
		if got, err := compensateWith(t, args); !errors.Is(err, ErrProtocol) {
			t.Errorf("Compensate(%s) saw %+v, %v; want ErrProtocol", args, got, err)
		}
	}
}

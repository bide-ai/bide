package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// Compensator is an optional interface a Tool implements to declare how to UNDO its side
// effect. In a saga run (RunSaga), if a step fails after earlier writes succeeded, the
// completed compensatable writes are rolled back in reverse order — automatically, and
// recursively through sub-agent trees.
//
// This is the transactional / saga agent: "charged the card and booked the flight, then
// failed on the hotel → cleanly refund + cancel." It's the gsm compensation idea applied
// to the agent runtime — the journal records exactly which writes completed, so
// well-founded reverse compensation falls out of the durable substrate.
//
// A saga step must be ATOMIC: it must not leave a partial external side effect before
// returning an error, because the failing step itself is not compensated (there's no
// recorded result to drive Compensate). Make forward steps all-or-nothing, or idempotent.
type Compensator interface {
	// Compensate undoes a completed call. args are the arguments the tool accepted (after tool
	// middleware; see CompensatedFunc); result
	// is what Call returned. Must be idempotent — on a crash mid-rollback it may re-run.
	Compensate(ctx context.Context, args, result json.RawMessage) error
}

// CompensatedFunc is a typed tool that declares both its forward action and its
// compensator. undo receives the same typed input and the output do produced: the call's
// recorded arguments decoded as the forward call decoded them, strictly (see Func). A record
// written before tool arguments decoded strictly may hold arguments only encoding/json accepts;
// the forward call of that time decoded them with encoding/json, so compensation does too, and
// undoes the value that call acted on. Arguments neither decodes are ErrProtocol.
//
// The arguments are the ones the tool accepted. When a tool middleware changes a compensable
// call's arguments in a saga, the arguments the tool receives are journaled before it runs
// (under a key of their own), and compensation undoes those; otherwise the model's arguments
// are the ones the tool received. A journal written before accepted arguments were journaled
// has no such record, so compensation there falls back to the model's arguments. A middleware
// that rewrites a retry-safe compensable call's arguments must rewrite them the same way every
// time: the first record is kept when the call runs again.
//
// opts set the rest of the tool's spec, as for Func.
func CompensatedFunc[In, Out any](
	name, description string,
	safety Safety,
	do func(context.Context, In) (Out, error),
	undo func(context.Context, In, Out) error,
	opts ...ToolOption,
) Tool {
	return &compTool[In, Out]{funcTool: newFuncTool(name, description, safety, do, opts), undo: undo}
}

type compTool[In, Out any] struct {
	*funcTool[In, Out]
	undo func(context.Context, In, Out) error
}

func (t *compTool[In, Out]) Compensate(ctx context.Context, args, result json.RawMessage) error {
	var in In
	if err := decodeRecordedArgs(args, &in); err != nil {
		return fmt.Errorf("saga compensate %q: decode recorded args: %w (%w)", t.Name(), err, ErrProtocol)
	}
	var out Out
	if len(result) > 0 {
		if err := json.Unmarshal(result, &out); err != nil {
			return fmt.Errorf("saga compensate %q: decode recorded result: %w (%w)", t.Name(), err, ErrProtocol)
		}
	}
	return t.undo(ctx, in, out)
}

// decodeRecordedArgs decodes a call's recorded arguments into in as its forward call decoded
// them: strictly, with decodeArgs, which every call journaled since tool arguments decode strictly
// passed. A record that does not decode strictly was written before that, when the forward call
// decoded with encoding/json (empty arguments as the zero value), so it is decoded that way.
func decodeRecordedArgs[In any](args json.RawMessage, in *In) error {
	if err := decodeArgs(args, in); err == nil {
		return nil
	}
	if len(args) == 0 {
		return nil
	}
	return json.Unmarshal(args, in)
}

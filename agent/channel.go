// channel.go adds ordered per-run message channels: the multi-message form of a signal
// (see pause.go's single-shot Signal/Await). Enqueue appends deduped by key; Receive returns
// the oldest not-yet-acked message in delivery order and pauses (*SignalPending) when the channel
// is drained; Ack marks a message consumed so Receive advances. Enqueue, Ack, and the records
// Receive reads all ride Durable.Do and History, so intake is at-most-once and consumption is
// replay-safe and exactly-once by construction, with no new persistence model.
//
// Replay-safety: Receive writes nothing (only Ack writes), so on a tool re-run Receive returns
// the same oldest-unacked message deterministically. A caller consumes exactly once by looping
// Receive -> durably handle -> Ack: the tool re-runs from the top on resume, and the journaled
// ack is what makes Receive advance past a handled message.

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Enqueue appends a message to an ordered per-run channel, deduped by key: a redelivery with
// the same (runID, channel, key) is a no-op and the first payload wins (at-most-once intake over
// at-least-once transport). Safe to call from any process; the store's PK / ON CONFLICT is the
// cross-process dedup, exactly as for Signal. After enqueueing, re-invoke a run paused on the
// channel (*SignalPending) with the pause's RootRunID.
func Enqueue[T any](ctx context.Context, d Durable, runID, channel, key string, payload T) error {
	if runID == "" {
		return fmt.Errorf("Enqueue: empty runID: %w", ErrConfig)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("agent: encode channel message %q/%q: %w (%w)", channel, key, err, ErrConfig)
	}
	_, err = d.Do(ctx, runID, chanStep(channel, key), func(context.Context) (Record, error) {
		return Record{Kind: StepSignal, Result: b}, nil
	})
	return err
}

// Send is the former name of Enqueue.
//
// Deprecated: transitional; renamed by the 1.0 rewrite. Use Enqueue.
func Send[T any](ctx context.Context, d Durable, runID, channel, key string, payload T) error {
	return Enqueue(ctx, d, runID, channel, key, payload)
}

// Received is one message pulled from a channel.
type Received[T any] struct {
	Key     string
	Payload T
}

// Receive returns the oldest not-yet-acked message on channel, in delivery order. If the
// channel has no unacked message it returns *SignalPending and the run pauses durably. The run must
// Ack(channel, msg.Key) after it has durably handled the message; until then Receive keeps
// returning the SAME message, which is what makes processing replay-safe and exactly-once. Call
// from a retry-safe tool (Safety.ReadOnly or Idempotent), like Await.
func Receive[T any](ctx context.Context, channel string) (Received[T], error) {
	var zero Received[T]
	d, runID, ok := runContext(ctx)
	if !ok {
		return zero, fmt.Errorf("agent: Receive called outside a running agent: %w", ErrConfig)
	}
	recs, err := d.History(ctx, runID)
	if err != nil {
		return zero, fmt.Errorf("agent: receive %q: %w (%w)", channel, err, ErrStorage)
	}

	// Which message keys have been acked. Ack records are journaled StepValue steps.
	acked := map[string]bool{}
	ackPrefix := chanAckPrefix(channel)
	for _, r := range recs {
		if r.Kind == StepValue && strings.HasPrefix(r.Name, ackPrefix) {
			acked[strings.TrimPrefix(r.Name, ackPrefix)] = true
		}
	}

	// The channel's messages are the StepSignal records under chanPrefix(channel), in history
	// (delivery) order. Return the first whose ack is absent.
	msgPrefix := chanPrefix(channel)
	for _, r := range recs {
		if r.Kind != StepSignal || !strings.HasPrefix(r.Name, msgPrefix) {
			continue
		}
		key := strings.TrimPrefix(r.Name, msgPrefix)
		if acked[key] {
			continue
		}
		var v T
		if len(r.Result) > 0 {
			if err := json.Unmarshal(r.Result, &v); err != nil {
				return zero, fmt.Errorf("agent: decode channel message %q/%q: %w (%w)", channel, key, err, ErrProtocol)
			}
		}
		return Received[T]{Key: key, Payload: v}, nil
	}
	// Drained (or empty): pause durably, reusing *SignalPending like single-shot Await.
	return zero, &SignalPending{RunRef: RunRef{RunID: runID, RootRunID: rootRunID(ctx, runID)}, Name: channel}
}

// Ack marks a message consumed so Receive advances past it. Idempotent (the first ack for a
// (runID, channel, key) wins) and journaled, so the advance survives a crash and a resume.
func Ack(ctx context.Context, d Durable, runID, channel, key string) error {
	if runID == "" {
		return fmt.Errorf("Ack: empty runID: %w", ErrConfig)
	}
	_, err := d.Do(ctx, runID, chanAckStep(channel, key), func(context.Context) (Record, error) {
		return Record{Kind: StepValue}, nil
	})
	return err
}

// chanPrefix is the exact step-name boundary for a channel's messages: "chan:", the channel
// name's length in bytes, ':', the channel name, ':'. The length makes the boundary exact even
// when names contain ':', so no two (channel, key) pairs share a step name: channel "orders"
// never matches a message on "orders:vip", and key "b:c" on channel "a" is not key "c" on
// channel "a:b". The remainder after the prefix is the key.
func chanPrefix(channel string) string { return "chan:" + lengthPrefixed(channel) + ":" }

func chanStep(channel, key string) string { return chanPrefix(channel) + key }

// chanAckPrefix is chanPrefix for a channel's acks, under "chanack:".
func chanAckPrefix(channel string) string { return "chanack:" + lengthPrefixed(channel) + ":" }

func chanAckStep(channel, key string) string { return chanAckPrefix(channel) + key }

func lengthPrefixed(s string) string { return strconv.Itoa(len(s)) + ":" + s }

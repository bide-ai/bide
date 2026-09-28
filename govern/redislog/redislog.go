// Package redislog is a durable, networked govern.EventLog backed by Redis Streams — the
// "no SQL database required" adapter. Redis Streams is a purpose-built, ordered, append-
// only log (XADD/XRANGE), so it maps almost 1:1 onto the two-method EventLog port and gives
// a PersistentGovernor multi-process / multi-node event sourcing without a relational DB.
// It satisfies govern.EventLog structurally, so this package doesn't import govern.
package redislog

import (
	"context"
	"fmt"

	"github.com/bide-ai/bide/agent"
	"github.com/redis/go-redis/v9"
)

// Log is a Redis-Streams-backed event log; each entity is one stream (prefix+entity).
type Log struct {
	rc     *redis.Client
	prefix string
}

// Open connects to Redis at addr (e.g. "localhost:6379") and pings it. Streams are keyed
// "govern:<entity>".
func Open(ctx context.Context, addr string) (*Log, error) {
	rc := redis.NewClient(&redis.Options{Addr: addr})
	if err := rc.Ping(ctx).Err(); err != nil {
		_ = rc.Close()
		return nil, err
	}
	return &Log{rc: rc, prefix: "govern:"}, nil
}

// NewWithClient wraps an existing redis.Client (for shared clients / custom options).
func NewWithClient(rc *redis.Client, prefix string) *Log { return &Log{rc: rc, prefix: prefix} }

// Close closes the underlying Redis client.
func (l *Log) Close() error { return l.rc.Close() }

func (l *Log) key(entity string) string { return l.prefix + entity }

// Append durably appends an event to the entity's stream (auto-assigned monotonic ID).
func (l *Log) Append(ctx context.Context, entity, event string) error {
	return l.rc.XAdd(ctx, &redis.XAddArgs{
		Stream: l.key(entity),
		Values: map[string]any{"event": event},
	}).Err()
}

// Events returns the entity's events in append (stream ID) order.
func (l *Log) Events(ctx context.Context, entity string) ([]string, error) {
	msgs, err := l.rc.XRange(ctx, l.key(entity), "-", "+").Result()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		e, ok := m.Values["event"].(string)
		if !ok {
			return nil, fmt.Errorf("redislog: stream %q entry %s missing string 'event' field: %w", l.key(entity), m.ID, agent.ErrProtocol)
		}
		out = append(out, e)
	}
	return out, nil
}

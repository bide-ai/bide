// Package redislog is a durable, networked govern.EventLog backed by Redis Streams — the
// "no SQL database required" adapter. Redis Streams is a purpose-built, ordered, append-
// only log (XADD/XRANGE), so it maps almost 1:1 onto the two-method EventLog port and gives
// a PersistentGovernor multi-process / multi-node event sourcing without a relational DB.
// It satisfies govern.EventLog structurally, so this package doesn't import govern.
package redislog

import (
	"context"
	"fmt"
	"strings"

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

// key is the entity's stream. idsKey is the hash of the entity's append ids, each mapped to its
// position; its name ends in a NUL byte, which no entity name may contain, so it is never another
// entity's stream.
func (l *Log) key(entity string) string    { return l.prefix + entity }
func (l *Log) idsKey(entity string) string { return l.prefix + entity + "\x00append-ids" }

func checkEntity(entity string) error {
	if strings.Contains(entity, "\x00") {
		return fmt.Errorf("redislog: entity name %q contains a NUL byte: %w", entity, agent.ErrConfig)
	}
	return nil
}

// appendScript appends one event under an append id and returns its position and the event
// recorded at it. The position is the stream's length before the append, and the entry is stored
// under the explicit ID "<position+1>-0", so an entity's entries sit at IDs 1-0, 2-0, ... and
// reading from a position is one XRANGE. The id is recorded in the entity's id hash in the same
// script; an id already there records nothing and returns the position (and event) recorded for
// it. Redis runs a script atomically, so concurrent appends from any number of processes get
// distinct, dense positions, and a script run twice for one append (a client retry after a lost
// reply) records its event once.
var appendScript = redis.NewScript(`
local recorded = redis.call('HGET', KEYS[2], ARGV[2])
if recorded then
	local id = (tonumber(recorded) + 1) .. '-0'
	local entry = redis.call('XRANGE', KEYS[1], id, id)
	local fields = entry[1] and entry[1][2] or {}
	for i = 1, #fields, 2 do
		if fields[i] == 'event' then
			return {tonumber(recorded), fields[i + 1]}
		end
	end
	return redis.error_reply('redislog: append id ' .. ARGV[2] .. ' maps to a missing stream entry ' .. id)
end
local n = redis.call('XLEN', KEYS[1])
redis.call('XADD', KEYS[1], (n + 1) .. '-0', 'event', ARGV[1])
redis.call('HSET', KEYS[2], ARGV[2], n)
return {n, ARGV[1]}
`)

// Append durably appends an event to the entity's stream under id and returns its position, or
// returns the position already recorded for id. The stream must be written only through this
// adapter: an entry added with an auto-assigned ID would break the position-to-ID mapping.
//
// The id makes the append safe to send more than once. go-redis retries a command whose reply was
// lost (Options.MaxRetries, 3 by default), including after Redis ran it; the retry carries the
// same id, so the script records the event once and returns its position. That holds for any
// client passed to NewWithClient, whatever its retry settings. The id hash keeps one field per
// append for the life of the stream.
func (l *Log) Append(ctx context.Context, entity, id, event string) (int64, error) {
	if id == "" {
		return 0, fmt.Errorf("redislog: empty append id: %w", agent.ErrConfig)
	}
	if err := checkEntity(entity); err != nil {
		return 0, err
	}
	res, err := appendScript.Run(ctx, l.rc, []string{l.key(entity), l.idsKey(entity)}, event, id).Slice()
	if err != nil {
		return 0, err
	}
	if len(res) != 2 {
		return 0, fmt.Errorf("redislog: append script returned %v: %w", res, agent.ErrProtocol)
	}
	pos, ok := res[0].(int64)
	recorded, ok2 := res[1].(string)
	if !ok || !ok2 {
		return 0, fmt.Errorf("redislog: append script returned %v: %w", res, agent.ErrProtocol)
	}
	if recorded != event {
		return 0, fmt.Errorf("redislog: append id %q holds event %q, not %q: %w", id, recorded, event, agent.ErrConfig)
	}
	return pos, nil
}

// Events returns the entity's events at positions from onward, in log order.
func (l *Log) Events(ctx context.Context, entity string, from int64) ([]string, error) {
	if err := checkEntity(entity); err != nil {
		return nil, err
	}
	if from < 0 {
		from = 0
	}
	msgs, err := l.rc.XRange(ctx, l.key(entity), fmt.Sprintf("%d-0", from+1), "+").Result()
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

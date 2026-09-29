package redislog

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// clusterSlot is the Redis Cluster hash slot of key: CRC16 (XMODEM) of the key's hash tag (the
// text between its first '{' and the next '}', when that is not empty) or of the whole key,
// modulo 16384.
func clusterSlot(key string) int {
	if s := strings.IndexByte(key, '{'); s >= 0 {
		if e := strings.IndexByte(key[s+1:], '}'); e > 0 {
			key = key[s+1 : s+1+e]
		}
	}
	var crc uint16
	for i := 0; i < len(key); i++ {
		crc ^= uint16(key[i]) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return int(crc) % 16384
}

// The append script touches an entity's stream and its id hash, so on Redis Cluster both must
// hash to one slot, or the script fails with CROSSSLOT.
func TestKeysOfAnEntityShareAClusterSlot(t *testing.T) {
	if clusterSlot("123456789") != 12739 { // the CRC16 check value (0x31C3) the Redis spec gives
		t.Fatal("clusterSlot does not compute Redis's CRC16")
	}
	l := &Log{prefix: "govern:"}
	for _, entity := range []string{"orders", "", "a", "a}b", "a{b", "x{}y", "}", "{", "account:42"} {
		if a, b := clusterSlot(l.key(entity)), clusterSlot(l.idsKey(entity)); a != b {
			t.Errorf("entity %q: stream %q in slot %d, ids %q in slot %d", entity, l.key(entity), a, l.idsKey(entity), b)
		}
	}
}

// A name that would leave an entity's keys without a shared hash tag is refused before anything
// is sent: a prefix with a brace, whose tag would come first.
func TestNamesWithoutASharedTagAreRefused(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ prefix, entity string }{{"g{x}:", "e"}, {"g{", "e"}, {"g}", "e"}} {
		l := &Log{prefix: c.prefix} // no client: a refused call never reaches Redis
		if _, err := l.Append(ctx, c.entity, "id", "ev"); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("Append with prefix %q, entity %q: %v, want ErrConfig", c.prefix, c.entity, err)
		}
		if _, err := l.Events(ctx, c.entity, 0); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("Events with prefix %q, entity %q: %v, want ErrConfig", c.prefix, c.entity, err)
		}
	}
}

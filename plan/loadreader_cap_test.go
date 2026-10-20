package plan

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// countingReader yields n bytes of spaces and counts how many it was asked to hand over.
type countingReader struct{ left, read int }

func (c *countingReader) Read(p []byte) (int, error) {
	if c.left == 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.left)
	for i := range n {
		p[i] = ' '
	}
	c.left -= n
	c.read += n
	return n, nil
}

// LoadReader reads at most MaxConfigSize bytes: a reader with one more is refused with ErrConfig
// naming the limit, and it is not read past the byte that proves it too long.
func TestLoadReader_RefusesOversizeConfig(t *testing.T) {
	r := &countingReader{left: 1 << 30}
	_, err := LoadReader[string, string](r, NewRegistry())
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "larger than 16777216 bytes") {
		t.Fatalf("LoadReader = %v, want ErrConfig naming the 16 MiB size", err)
	}
	if r.read > 16<<20+1 {
		t.Fatalf("read %d bytes, want at most %d", r.read, 16<<20+1)
	}
}

// A config of exactly the limit is read in full (here it is blank, so it fails later, as a config
// and not as an oversize one).
func TestLoadReader_AcceptsConfigAtTheLimit(t *testing.T) {
	r := &countingReader{left: 16 << 20}
	_, err := LoadReader[string, string](r, NewRegistry())
	if err == nil || strings.Contains(err.Error(), "larger than") {
		t.Fatalf("LoadReader = %v, want a config error that is not about size", err)
	}
	if r.read != 16<<20 {
		t.Fatalf("read %d bytes, want all %d", r.read, 16<<20)
	}
}

package provider

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// FuzzSSEScanner: the shared SSE framing never panics, never yields a line over MaxSSELine, fails
// only with bufio.ErrTooLong (which SSEReadError reports as agent.ErrResponseTooLarge), and SSEPayload
// returns a non-empty, trimmed payload exactly for data: lines that carry one.
func FuzzSSEScanner(f *testing.F) {
	f.Add([]byte("event: x\ndata: {\"a\":1}\n\ndata:   \ndata:[DONE]\r\n"))
	f.Add([]byte("data:\x00\xff\n"))
	f.Add([]byte("data: a\r\n\r\ndata:b\rdata: c"))
	f.Add([]byte(": comment\nid: 1\nretry: 10\ndata\ndata :x\n"))
	// A line longer than the scanner's initial buffer (one over MaxSSELine, 32MB, is too large for
	// a seed; provider_http_test.go covers it).
	f.Add([]byte("data: " + strings.Repeat("a", 80<<10) + "\n"))
	f.Fuzz(func(t *testing.T, body []byte) {
		sc := NewSSEScanner(bytes.NewReader(body))
		for sc.Scan() {
			line := sc.Text()
			if len(line) > MaxSSELine {
				t.Fatalf("line of %d bytes exceeds the cap", len(line))
			}
			p, ok := SSEPayload(line)
			if !ok {
				if strings.HasPrefix(line, "data:") && strings.TrimSpace(line[5:]) != "" {
					t.Fatalf("non-empty data line %q skipped", line)
				}
				continue
			}
			if !strings.HasPrefix(line, "data:") || p == "" || p != strings.TrimSpace(p) || p != strings.TrimSpace(line[5:]) {
				t.Fatalf("SSEPayload(%q) = %q", line, p)
			}
		}
		if err := sc.Err(); err != nil {
			if !errors.Is(err, bufio.ErrTooLong) {
				t.Fatalf("scanner failed with %v on an in-memory body", err)
			}
			if re := SSEReadError("p", err); !errors.Is(re, agent.ErrResponseTooLarge) || !errors.Is(re, agent.ErrModel) {
				t.Fatalf("SSEReadError(%v) = %v, want agent.ErrResponseTooLarge", err, re)
			}
		}
	})
}

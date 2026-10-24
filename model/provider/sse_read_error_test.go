package provider

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/bide-ai/bide/agent"
)

// A read that fails partway through a line ends the scan with that error: the cut-off line is not returned
// as a token, so an adapter never decodes half an event and reports "unexpected end of JSON input" in place
// of the failed read. The error is the retryable one a cut connection is.
func TestNewSSEScanner_ReadErrorWithholdsThePartialLine(t *testing.T) {
	body := io.MultiReader(strings.NewReader("data: {\"a\":1}\ndata: {\"b\""), iotest.ErrReader(io.ErrUnexpectedEOF))
	sc := NewSSEScanner(body)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 1 || lines[0] != `data: {"a":1}` {
		t.Fatalf("lines = %q, want only the complete line", lines)
	}
	if err := sc.Err(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Err = %v, want the read error", err)
	}
	if err := SSEReadError("p", sc.Err()); !errors.Is(err, agent.ErrModel) || errors.Is(err, agent.ErrResponseTooLarge) {
		t.Fatalf("SSEReadError = %v, want a plain ErrModel (a cut stream is retried)", err)
	}
}

// A clean end of the body still returns a last line that has no newline.
func TestNewSSEScanner_CleanEOFReturnsTheLastLine(t *testing.T) {
	sc := NewSSEScanner(strings.NewReader("data: {\"a\":1}\ndata: {\"b\":2}"))
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 2 || sc.Err() != nil {
		t.Fatalf("lines = %q, err = %v, want both lines and no error", lines, sc.Err())
	}
}

// A line over MaxSSELine is a non-retryable agent.ErrResponseTooLarge, read in memory (no socket): the
// scanner gives up at its cap whatever the body does after.
func TestNewSSEScanner_OverlongLineIsNotRetryable(t *testing.T) {
	body := io.MultiReader(strings.NewReader("data: "), io.LimitReader(repeatByte('A'), MaxSSELine+1<<20), strings.NewReader("\n"))
	sc := NewSSEScanner(body)
	for sc.Scan() {
		t.Fatal("scanned a line over the cap")
	}
	if err := SSEReadError("p", sc.Err()); !errors.Is(err, agent.ErrResponseTooLarge) {
		t.Fatalf("SSEReadError = %v, want ErrResponseTooLarge", err)
	}
}

type repeatByte byte

func (b repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

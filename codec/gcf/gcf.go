// Package gcf provides an agent.ToolResultCodec that encodes tool results as
// GCF (Graph Compact Format) instead of JSON when they are sent to the model.
// GCF is more token-efficient and better comprehended by frontier models than
// JSON for structured data, so this can cut prompt tokens on tool output. It is
// opt-in per model (model adapters default to JSON), and it changes only what
// the model reads: bide's journal and audit trail keep the canonical JSON.
package gcf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/bide-ai/bide/agent"
	gcfgo "github.com/blackwell-systems/gcf-go"
)

// Codec renders a tool result's JSON bytes as a GCF document.
type Codec struct{}

var _ agent.ToolResultCodec = Codec{}

// New returns a GCF tool-result codec. Wire it into a model adapter with that
// adapter's WithToolResultCodec option, for example:
//
//	openai.New(key, openai.WithToolResultCodec(gcf.New()))
func New() agent.ToolResultCodec { return Codec{} }

// EncodeToolResult encodes the canonical JSON as GCF, keeping object keys in the tool's order and
// every number at exactly the value the tool returned. Integers are carried as int64 and other
// numbers as float64, in GCF's canonical spelling (1e20 is written 1e+20, 1.50 as 1.5). A number
// GCF cannot carry exactly (an integer outside int64, or a decimal such as 0.30000000000000000001
// that no float64 equals) is an error rather than a silently different value, as is a JSON decode
// error, so the caller can fall back to JSON (agent.EncodeToolResultOr does this). Empty input
// yields an empty string.
func (Codec) EncodeToolResult(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if err := checkNumbers(raw); err != nil {
		return "", err
	}
	// ParseJSONOrdered decodes objects in key order and numbers with UseNumber: a bare integer
	// becomes an int64 (or an out-of-range error), anything else a float64.
	v, err := gcfgo.ParseJSONOrdered(raw)
	if err != nil {
		return "", err
	}
	return gcfgo.EncodeGenericChecked(v)
}

// checkNumbers reports an error for the first non-integer number in raw whose float64 does not
// print back as the same decimal value, the text GCF would show the model in its place. It also
// rejects invalid JSON and trailing data.
func checkNumbers(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		n, ok := tok.(json.Number)
		if !ok || !strings.ContainsAny(string(n), ".eE") {
			continue // integers: ParseJSONOrdered keeps them exact or reports them out of range
		}
		// A literal outside the float64 range parses to an infinity or zero, which prints as a
		// different value and so is rejected below with the rest.
		f, _ := strconv.ParseFloat(string(n), 64)
		if magnitude(string(n)) != magnitude(strconv.FormatFloat(f, 'e', -1, 64)) {
			return fmt.Errorf("gcf: number %s has no exact float64 form (it would read %s)", n, strconv.FormatFloat(f, 'g', -1, 64))
		}
	}
}

// magnitude returns a canonical form of a number literal's absolute value: its significant digits
// without leading or trailing zeros and the exponent of the last one. Zero is "0". The sign is
// dropped because the literal and the float64 parsed from it always share one. An exponent too
// large for an int counts as zero; such a literal parses to an infinity or zero, which never
// yields the same digits unless the literal is itself zero.
func magnitude(s string) string {
	s = strings.TrimPrefix(s, "-")
	exp := 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exp, _ = strconv.Atoi(s[i+1:])
		s = s[:i]
	}
	intPart, frac, _ := strings.Cut(s, ".")
	digits := strings.TrimLeft(intPart+frac, "0")
	exp -= len(frac)
	for strings.HasSuffix(digits, "0") {
		digits = digits[:len(digits)-1]
		exp++
	}
	if digits == "" {
		return "0"
	}
	return fmt.Sprintf("%se%d", digits, exp)
}

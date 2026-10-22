package amount

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bide-ai/bide/internal/strictjson"
)

var (
	// ErrArgs: the arguments are not strict. A duplicate member name, or two names that differ
	// only in case, at any depth: a tool that decodes its own arguments (a custom Tool, an MCP
	// server) may take the last duplicate, or match a name case-insensitively, and so read a value
	// other than the one the cap read. Also invalid UTF-8, an escaped lone surrogate, trailing
	// data, arguments that are not an object, and nesting deeper than a tool's arguments go.
	ErrArgs = notAssessable("arguments are not strict JSON")
	// ErrAbsent: the member is not there.
	ErrAbsent = notAssessable("amount argument is absent")
)

// maxArgsDepth bounds the nesting Lookup walks. Tool arguments carrying an amount are shallow;
// this is far above any, and far below the point where recursion is a cost.
const maxArgsDepth = 32

// Lookup returns the JSON value at path (object members, outermost first) of a tool call's
// arguments, exactly as written, after checking that the arguments are strict. The check is of
// the whole document, not only the path: a duplicate or case-variant name anywhere makes the
// call not assessable, because a tool decoding the arguments its own way may differ about any of
// them. Empty arguments are the empty object, as for a Func tool. A null at the path is returned
// as null (Assess then refuses it); an empty path, an empty member name, a missing member, or a
// step through a value that is not an object is ErrAbsent. A name that differs from the path's
// only in case is not the member (the lookup is exact); it is absent.
//
// The strict options are those internal/strictjson applies to Func arguments in agent/tool.go
// (duplicate names, one spelling, UTF-8, trailing data), plus the case-variant check, which
// strictjson makes only against a struct's fields and which needs no struct here.
func Lookup(args []byte, path []string) (json.RawMessage, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		args = []byte("{}")
	}
	if !utf8.Valid(args) {
		return nil, fmt.Errorf("%w: invalid UTF-8", ErrArgs)
	}
	if err := strictjson.Check(args, reflect.TypeFor[map[string]any](), nil); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrArgs, err)
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("%w: not an object", ErrArgs)
	}
	dec = json.NewDecoder(bytes.NewReader(args))
	if err := noCaseVariants(dec, 0); err != nil {
		return nil, err
	}
	if len(path) == 0 {
		return nil, ErrAbsent
	}
	cur := json.RawMessage(args)
	for _, name := range path {
		if name == "" {
			return nil, ErrAbsent
		}
		var members map[string]json.RawMessage
		if json.Unmarshal(cur, &members) != nil {
			return nil, ErrAbsent
		}
		next, ok := members[name]
		if !ok {
			return nil, ErrAbsent
		}
		cur = next
	}
	return bytes.Clone(cur), nil
}

// noCaseVariants consumes one JSON value from dec and refuses an object, at any depth, with two
// member names that are equal under Unicode simple case folding, the folding encoding/json
// matches names with. (Exact duplicates were refused by strictjson before this runs.)
func noCaseVariants(dec *json.Decoder, depth int) error {
	if depth > maxArgsDepth {
		return fmt.Errorf("%w: nested deeper than %d levels", ErrArgs, maxArgsDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrArgs, err)
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return fmt.Errorf("%w: %v", ErrArgs, err)
			}
			name, _ := k.(string)
			key := foldKey(name)
			if seen[key] {
				return fmt.Errorf("%w: two member names differ only in case", ErrArgs)
			}
			seen[key] = true
			if err := noCaseVariants(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := noCaseVariants(dec, depth+1); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil { // the closing delimiter
		return fmt.Errorf("%w: %v", ErrArgs, err)
	}
	return nil
}

// foldKey maps a name to one string per case-folding class: each rune becomes the least rune of
// its unicode.SimpleFold orbit, so two names have the same key exactly when strings.EqualFold
// says they are equal. It is linear, where comparing every pair of names is not.
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		min := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < min {
				min = f
			}
		}
		b.WriteRune(min)
	}
	return b.String()
}

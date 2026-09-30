package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"unicode/utf8"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/strictjson"
)

// UnmarshalStrict decodes a JSON proof artifact (a bundle, certificate, evidence package, or tree
// head) so that what a person reads in the file is exactly what is verified. encoding/json accepts
// a duplicate key and keeps the last value, matches keys case-insensitively (so "Size" and "SIZE"
// are the same field), ignores unknown fields, and rewrites invalid UTF-8; each lets a file show a
// reader one value while the verifier checks another. UnmarshalStrict rejects all four: duplicate
// names (at any depth, including inside embedded raw JSON), names that do not match a field
// exactly, unknown fields, and invalid UTF-8.
//
// It also rejects the spellings encoding/json decodes lossily, so each decoded value has one
// spelling: an escaped lone surrogate ("\ud800"), which becomes U+FFFD, in any string decoded into
// Go, and, in a []byte field, base64 that is not the standard encoding of the bytes it decodes to
// (line breaks, which encoding/json skips, or unused bits set in the last character). JSON kept
// verbatim (json.RawMessage) is checked for duplicate names only: it is committed as written.
//
// A message (agent.Message) decodes with its own UnmarshalJSON, which matches names loosely, so
// UnmarshalStrict checks it against its wire shape: exactly "role" and "parts", and each part
// exactly the fields of the part its "type" names.
//
// An artifact with a format (ProofBundle, AbsenceBundle, RunCertificate, CurrentGrantProof,
// EventInclusion, EvidencePackage, SignedTreeHead, AnchorEntry, JournalExport) is checked for its "format" first,
// at the top level and wherever one artifact carries another: one that has none, or not the one this
// version reads, is refused with an error wrapping ErrFormat, before any rule above can report a
// field its own format names differently. Every other error wraps ErrMalformed.
//
// It uses only the standard encoding/json: the input is checked token by token against the target
// type before it is decoded, so the rules above hold without depending on encoding/json/v2.
func UnmarshalStrict(data []byte, v any) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("audit: strict json: invalid UTF-8: %w", ErrMalformed)
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("audit: strict json: decode target must be a non-nil pointer, got %T: %w", v, agent.ErrConfig)
	}
	if f, ok := reflect.Zero(rv.Type().Elem()).Interface().(formatted); ok {
		if err := checkDataFormat(data, f); err != nil {
			return fmt.Errorf("audit: strict json: %w", err)
		}
	}
	if err := strictjson.Check(data, rv.Type().Elem(), strictOptions); err != nil {
		return malformed(err)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return malformed(err)
	}
	return nil
}

// malformed wraps a strict-decoding error in ErrMalformed, unless it already is one (ErrFormat).
func malformed(err error) error {
	if errors.Is(err, ErrMalformed) {
		return fmt.Errorf("audit: strict json: %w", err)
	}
	return fmt.Errorf("audit: strict json: %w (%w)", err, ErrMalformed)
}

// formattedWire maps each formatted artifact type to a copy of it without methods, the type its
// JSON is checked against once its format has been checked (see checkFormatted).
var formattedWire = map[reflect.Type]reflect.Type{
	reflect.TypeFor[ProofBundle]():       reflect.TypeFor[proofBundleWire](),
	reflect.TypeFor[AbsenceBundle]():     reflect.TypeFor[absenceBundleWire](),
	reflect.TypeFor[RunCertificate]():    reflect.TypeFor[runCertificateWire](),
	reflect.TypeFor[CurrentGrantProof](): reflect.TypeFor[currentGrantProofWire](),
	reflect.TypeFor[EventInclusion]():    reflect.TypeFor[eventInclusionWire](),
	reflect.TypeFor[EvidencePackage]():   reflect.TypeFor[evidencePackageWire](),
	reflect.TypeFor[SignedTreeHead]():    reflect.TypeFor[signedTreeHeadWire](),
	reflect.TypeFor[AnchorEntry]():       reflect.TypeFor[anchorEntryWire](),
	reflect.TypeFor[JournalExport]():     reflect.TypeFor[journalExportWire](),
}

type (
	proofBundleWire       ProofBundle
	absenceBundleWire     AbsenceBundle
	runCertificateWire    RunCertificate
	currentGrantProofWire CurrentGrantProof
	eventInclusionWire    EventInclusion
	evidencePackageWire   EvidencePackage
	signedTreeHeadWire    SignedTreeHead
	anchorEntryWire       AnchorEntry
	journalExportWire     JournalExport
)

// checkFormatted returns the strict check of a formatted artifact of type t, wherever it sits in
// the input: its "format" first (ErrFormat), then the rest of it against its fields.
func checkFormatted(t reflect.Type) func([]byte, string) error {
	f := reflect.Zero(t).Interface().(formatted)
	wire := formattedWire[t]
	return func(raw []byte, path string) error {
		if string(raw) == "null" {
			return nil
		}
		if err := checkDataFormat(raw, f); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return strictjson.CheckValue(raw, wire, path, strictOptions)
	}
}

// strictOptions check an agent.Message against its wire shape, and each part against the wire
// form of the part its "type" names. A struct's names are strictjson.ExactFields: none required.
var strictOptions = &strictjson.Options{
	Shapes: map[reflect.Type]reflect.Type{
		reflect.TypeFor[agent.Message](): reflect.TypeFor[messageShape](),
		reflect.TypeFor[agent.Record]():  recordShape,
	},
}

// recordShape is the wire form of agent.Record (see its MarshalJSON), which UnmarshalStrict checks
// a record against in place of its UnmarshalJSON: the record's journaled fields, then "claim" and
// "salt". It is built from the type, so it names every field a record journals.
var recordShape = func() reflect.Type {
	rt := reflect.TypeFor[agent.Record]()
	var fields []reflect.StructField
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.IsExported() && f.Tag.Get("json") != "-" {
			fields = append(fields, reflect.StructField{Name: f.Name, Type: f.Type, Tag: f.Tag})
		}
	}
	fields = append(fields,
		reflect.StructField{Name: "Claim", Type: reflect.TypeFor[string](), Tag: `json:"claim,omitempty"`},
		reflect.StructField{Name: "Salt", Type: reflect.TypeFor[[]byte](), Tag: `json:"salt,omitempty"`})
	return reflect.StructOf(fields)
}()

func init() {
	strictOptions.Hooks = map[reflect.Type]func([]byte, string) error{reflect.TypeFor[partShape](): checkPartWith(strictOptions)}
	for t := range formattedWire {
		strictOptions.Hooks[t] = checkFormatted(t)
	}
	recordBytesOptions.Hooks = map[reflect.Type]func([]byte, string) error{reflect.TypeFor[partShape](): checkPartWith(recordBytesOptions)}
}

// recordBytesOptions check a proven record's stored bytes (checkRecordBytes): the strict rules of
// UnmarshalStrict, except that a name the record type does not have is tolerated, as a record a
// later release wrote carries one.
var recordBytesOptions = &strictjson.Options{Shapes: strictOptions.Shapes, AllowUnknown: true}

// checkRecordBytes checks a record's stored bytes before they are read for display or a role
// check, so that what they say to this package is what they say to any JSON reader: valid UTF-8,
// no duplicate name at any depth, no name that is a case variant of a record field (which
// encoding/json would read into that field), and no escaped lone surrogate. A name no record field
// has is tolerated: the leaf is the bytes, and a later release may add fields.
func checkRecordBytes(b []byte) error {
	if !utf8.Valid(b) {
		return errors.New("invalid UTF-8")
	}
	return strictjson.Check(b, recordShape, recordBytesOptions)
}

// messageShape is the wire form of agent.Message (see its MarshalJSON), which UnmarshalStrict
// checks a message against in place of its loosely matching UnmarshalJSON.
type messageShape struct {
	Role  agent.Role  `json:"role"`
	Parts []partShape `json:"parts,omitempty"`
}

// partShape stands for one part of a message: the fields it may have depend on its "type".
type partShape struct{}

// partShapes maps each part "type" to the wire form of that part: the part's own fields and the
// "type" tag.
var partShapes = map[string]reflect.Type{
	"text": reflect.TypeFor[struct {
		Type string `json:"type"`
		agent.Text
	}](),
	"reasoning": reflect.TypeFor[struct {
		Type string `json:"type"`
		agent.Reasoning
	}](),
	"tool_use": reflect.TypeFor[struct {
		Type string `json:"type"`
		agent.ToolUse
	}](),
	"tool_result": reflect.TypeFor[struct {
		Type string `json:"type"`
		agent.ToolResult
	}](),
	"image": reflect.TypeFor[struct {
		Type string `json:"type"`
		agent.Image
	}](),
}

// checkPart checks one message part (raw, already checked for duplicate names) against the wire
// form of the part its "type" names.
func checkPartWith(opts *strictjson.Options) func([]byte, string) error {
	return func(raw []byte, path string) error { return checkPart(raw, path, opts) }
}

func checkPart(raw []byte, path string, opts *strictjson.Options) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("%s: a message part must be an object: %w", path, err)
	}
	var kind string
	if err := json.Unmarshal(probe["type"], &kind); err != nil {
		return fmt.Errorf("%s: a message part needs a string \"type\"", path)
	}
	shape, ok := partShapes[kind]
	if !ok {
		return fmt.Errorf("%s: unknown message part type %q", path, kind)
	}
	return strictjson.CheckValue(raw, shape, path, opts)
}

// Every leaf, grant, and seal in this package is hashed or signed over a JSON encoding. JSON
// encoding is injective only over valid UTF-8: encoding/json rewrites each invalid byte in a string
// to U+FFFD, so two values that differ only in invalid bytes would encode, hash, and verify
// identically. checkUTF8 rejects such a value before it is committed or verified. Byte slices are
// exempt (they encode as base64, which is injective) and so is json.RawMessage (it is copied
// verbatim).

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// checkUTF8 returns an error naming the first string in v (any exported field, map key or value,
// slice element, or pointer target, recursively) that is not valid UTF-8.
func checkUTF8(v any) error {
	return checkUTF8Value(reflect.ValueOf(v), "")
}

func checkUTF8Value(v reflect.Value, path string) error {
	switch v.Kind() {
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("invalid UTF-8 in %s%q", pathPrefix(path), v.String())
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return checkUTF8Value(v.Elem(), path)
		}
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			if f := t.Field(i); f.IsExported() {
				if err := checkUTF8Value(v.Field(i), path+"."+f.Name); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type() == rawMessageType || v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := range v.Len() {
			if err := checkUTF8Value(v.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			if err := checkUTF8Value(it.Key(), path+" key"); err != nil {
				return err
			}
			if err := checkUTF8Value(it.Value(), fmt.Sprintf("%s[%v]", path, it.Key())); err != nil {
				return err
			}
		}
	}
	return nil
}

func pathPrefix(path string) string {
	if path == "" {
		return ""
	}
	return path[1:] + " "
}

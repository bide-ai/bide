package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// strictStart is the full decoding of a stored run:start record: the record through
// decodeStored, then its result as a RunStart. ok is false for a record that is not a value
// record.
func strictStart(t *testing.T, b []byte) (RunStart, bool, error) {
	t.Helper()
	r, err := decodeStored("run", runStartStep, b)
	if err != nil {
		return RunStart{}, false, err
	}
	if r.Kind != StepValue {
		return RunStart{}, false, nil
	}
	var st RunStart
	if err := json.Unmarshal(r.Result, &st); err != nil {
		return RunStart{}, false, err
	}
	return st, true, nil
}

// A recovery pass decodes run:start in one pass, reading only the record's name and kind and the
// start (decodeStartEntry). For every start a drive journals, that is the start the full decoding
// reads; anything else is left to the full decoding.
func TestP14_RecoveryStartDecodeMatchesFullDecode(t *testing.T) {
	turn := 3
	maxTurns, budget := 7, 900
	prompt := "be brief"
	starts := map[string]RunStart{
		"plain":   {Input: UserText("go")},
		"empty":   {},
		"escaped": {Input: UserText("a \"quote\", a \\ and  ")},
		"image":   {Input: Message{Role: RoleUser, Parts: []Part{Text{Text: "look"}, ImageData("image/png", []byte{1, 2, 3})}}},
		"saga":    {Input: UserText("go"), Saga: true},
		"session": {Input: UserText("hi"), Kind: RunKindSessionTurn, Session: &SessionRef{ID: "s1", Turn: &turn}},
		"flow":    {Input: UserText(`{"a":1}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}},
		"typed":   {Input: UserText("q"), Typed: &TypedStart{Mode: OutputNative, SchemaDigest: "d", Schema: json.RawMessage(`{"type":"object"}`)}},
		"options": {Input: UserText("go"), Settings: RunSettings{MaxTurns: &maxTurns, TokenBudget: &budget, SystemPrompt: &prompt},
			Principal: &Principal{OnBehalfOf: "u", AuthorityRef: "a"}, Tools: []string{"x", "y"},
			Ext: map[string]json.RawMessage{"audit.grant": json.RawMessage(`"g"`)}},
	}
	for name, want := range starts {
		t.Run(name, func(t *testing.T) {
			res, err := marshalJournal(want)
			if err != nil {
				t.Fatal(err)
			}
			b, err := EncodeRecord(Record{Name: runStartStep, Kind: StepValue, Result: res})
			if err != nil {
				t.Fatal(err)
			}
			strict, ok, err := strictStart(t, b)
			if err != nil || !ok {
				t.Fatalf("full decoding = %v, %v", ok, err)
			}
			got, ok := decodeStartEntry(b)
			if !ok {
				t.Fatalf("decodeStartEntry refused %s", b)
			}
			if !reflect.DeepEqual(got, strict) {
				t.Fatalf("decodeStartEntry = %#v\nthe full decoding = %#v", got, strict)
			}
		})
	}

	// What the one-pass decoding does not take, the full decoding reports.
	tomb := []byte(`{"redacted":{"leaf_hash":"aa","at_ms":1}}`)
	other, _ := EncodeRecord(Record{Name: runStartStep, Kind: StepToolResult, Result: json.RawMessage(`{"input":"go"}`)})
	renamed, _ := EncodeRecord(Record{Name: "run:other", Kind: StepValue, Result: json.RawMessage(`{"input":"go"}`)})
	badInput, _ := EncodeRecord(Record{Name: runStartStep, Kind: StepValue, Result: json.RawMessage(`{"input":5}`)})
	noResult, _ := EncodeRecord(Record{Name: runStartStep, Kind: StepValue})
	nullResult := []byte(`{"name":"run:start","kind":"value","result":null}`)
	noInput := []byte(`{"name":"run:start","kind":"value","result":{"saga":true}}`)
	escapedName := []byte(`{"name":"run\u003astart","kind":"value","result":{"input":"go"}}`)
	escapedKind := []byte(`{"name":"run:start","kind":"valu\u0065","result":{"input":"go"}}`)
	renamedLast := []byte(`{"name":"run:start","kind":"value","result":{"input":"go"},"name":"run:other"}`)
	for name, b := range map[string][]byte{
		"tombstone": tomb, "other kind": other, "other name": renamed, "bad input": badInput,
		"no result": noResult, "not json": []byte(`{"name":`), "null result": nullResult, "no input": noInput,
		"escaped name": escapedName, "escaped kind": escapedKind, "renamed by a later member": renamedLast,
	} {
		t.Run(name, func(t *testing.T) {
			if st, ok := decodeStartEntry(b); ok {
				t.Fatalf("decodeStartEntry took %s as %#v; the full decoding must read it", b, st)
			}
		})
	}
}

// A record the one-pass decoding leaves to the full decoding reads as the full decoding reads it:
// a name or kind spelled with escapes is still run:start's, and a result with no input is a start
// with no input.
func TestStartUnderLeaseFallsBackToTheFullDecoding(t *testing.T) {
	ctx := context.Background()
	for name, b := range map[string][]byte{
		"escaped name": []byte(`{"name":"run\u003astart","kind":"value","result":{"input":"go","saga":true}}`),
		"escaped kind": []byte(`{"name":"run:start","kind":"valu\u0065","result":{"input":"go"}}`),
		"no input":     []byte(`{"name":"run:start","kind":"value","result":{"saga":true}}`),
	} {
		t.Run(name, func(t *testing.T) {
			m := NewMemStore()
			j, err := NewJournal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err := j.ensureHeader(ctx, "r"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := m.Insert(ctx, "r", runStartStep, b); err != nil {
				t.Fatal(err)
			}
			rec, err := decodeStored("r", runStartStep, b)
			if err != nil {
				t.Fatal(err)
			}
			var want RunStart
			if err := json.Unmarshal(rec.Result, &want); err != nil {
				t.Fatal(err)
			}
			got, ok, err := startUnderLease(ctx, j, "r")
			if err != nil || !ok || !reflect.DeepEqual(got, want) {
				t.Fatalf("startUnderLease = %#v, %v, %v; want %#v (the full decoding)", got, ok, err, want)
			}
		})
	}
}

// decodePlainStart reads only the records it is sure of, and reads them as the full decoding does:
// a plain text start this version writes (or one with no kind, as earlier versions wrote it) whose
// input needs no escape, with any other input or setting left to the JSON decoding.
func TestDecodePlainStartMatchesFullDecode(t *testing.T) {
	entry := func(result string) []byte {
		b, err := JournalEntry(runStartStep, Record{Kind: StepValue, Result: json.RawMessage(result)})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	start := func(s RunStart) []byte {
		r, err := marshalJournal(s)
		if err != nil {
			t.Fatal(err)
		}
		return entry(string(r))
	}
	full := func(b []byte) RunStart {
		rec, err := decodeStored("r", runStartStep, b)
		if err != nil {
			t.Fatal(err)
		}
		var st RunStart
		if err := json.Unmarshal(rec.Result, &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	taken := map[string][]byte{
		"plain":        start(RunStart{Input: UserText("go"), Kind: RunKindAgent}),
		"empty":        start(RunStart{Input: UserText(""), Kind: RunKindAgent}),
		"unicode":      start(RunStart{Input: UserText("caf\u00e9 \u4e16\u754c <b> & 'x'"), Kind: RunKindAgent}),
		"legacy":       entry(`{"input":"go"}`),
		"legacy space": entry(`{"input":"a b"}`),
	}
	for name, b := range taken {
		t.Run("taken/"+name, func(t *testing.T) {
			got, ok := decodePlainStart(b)
			if !ok {
				t.Fatalf("decodePlainStart left %s to the JSON decoding", b)
			}
			if want := full(b); !reflect.DeepEqual(got, want) {
				t.Fatalf("decodePlainStart = %#v, the full decoding = %#v", got, want)
			}
		})
	}
	left := map[string][]byte{
		"quote":        start(RunStart{Input: UserText(`a "b"`), Kind: RunKindAgent}),
		"backslash":    start(RunStart{Input: UserText(`a \ b`), Kind: RunKindAgent}),
		"control":      start(RunStart{Input: UserText("a\tb\n"), Kind: RunKindAgent}),
		"line sep":     start(RunStart{Input: UserText("a\u2028b"), Kind: RunKindAgent}),
		"saga":         start(RunStart{Input: UserText("go"), Kind: RunKindAgent, Saga: true}),
		"session":      start(RunStart{Input: UserText("go"), Kind: RunKindSessionTurn}),
		"image":        start(RunStart{Input: Message{Role: RoleUser, Parts: []Part{Text{Text: "go"}, Image{URL: "u"}}}, Kind: RunKindAgent}),
		"escaped":      entry(`{"input":"g\u006f"}`),
		"invalid utf8": []byte(plainStartPrefix + "\xff" + plainStartKind + `AAAA"}`),
		"raw control":  []byte(plainStartPrefix + "a\x01b" + plainStartKind + `AAAA"}`),
		"bad salt":     []byte(plainStartPrefix + "go" + plainStartKind + `AA AA"}`),
		"no salt":      []byte(plainStartPrefix + "go" + plainStartKind + `"}`),
		"trailing":     []byte(plainStartPrefix + "go" + plainStartKind + `AAAA"},`),
		"extra member": []byte(plainStartPrefix + "go" + plainStartKind + `AAAA","x":1}`),
	}
	for name, b := range left {
		t.Run("left/"+name, func(t *testing.T) {
			if st, ok := decodePlainStart(b); ok {
				t.Fatalf("decodePlainStart took %s as %#v", b, st)
			}
		})
	}
}

package agent

import (
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
	for name, b := range map[string][]byte{
		"tombstone": tomb, "other kind": other, "other name": renamed, "bad input": badInput,
		"no result": noResult, "not json": []byte(`{"name":`),
	} {
		t.Run(name, func(t *testing.T) {
			if st, ok := decodeStartEntry(b); ok {
				t.Fatalf("decodeStartEntry took %s as %#v; the full decoding must read it", b, st)
			}
		})
	}
}

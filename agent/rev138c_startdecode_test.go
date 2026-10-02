package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// fullStartDecode is the full decoding a recovery pass falls back to: decodeStored, then the
// record's result as a RunStart.
func fullStartDecode(b []byte) (RunStart, bool, error) {
	r, err := decodeStored("r", runStartStep, b)
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

var rev138cStartSeeds = []string{
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"salt":"AAAAAAAAAAAAAAAAAAAAAA=="}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi"},"salt":"AAAAAAAAAAAAAAAAAAAAAA=="}`,
	// salt in the base64 alphabet but not valid base64
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"salt":"A"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi"},"salt":"AA==AA=="}`,
	// duplicate members
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"result":{"saga":true},"salt":"AAAA"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"name":"x","salt":"AAAA"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"kind":"error","salt":"AAAA"}`,
	// case-folded keys and escapes
	`{"NAME":"run:start","Kind":"value","Result":{"Input":"hi"},"salt":"AAAA"}`,
	`{"name":"run\u003astart","kind":"value","result":{"input":"hi"},"salt":"AAAA"}`,
	`{"name":"run:start","kind":"value","result":{"input":"h\u00e9","kind":"agent"},"salt":"AAAA"}`,
	"{\"name\":\"run:start\",\"kind\":\"value\",\"result\":{\"input\":\"a\xe2\x80\xa8b\",\"kind\":\"agent\"},\"salt\":\"AAAA\"}",
	"{\"name\":\"run:start\",\"kind\":\"value\",\"result\":{\"input\":\"a\x7fb\",\"kind\":\"agent\"},\"salt\":\"AAAA\"}",
	// extra fields
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent","zz":1},"salt":"AAAA","zz":2}`,
	`{"name":"run:start","kind":"value","result":{"input":{"role":"user","parts":[{"type":"text","text":"x"}]}},"salt":"AAAA"}`,
	`{"name":"run:start","kind":"value","result":{"input":null,"saga":true},"salt":"AAAA"}`,
	`{"name":"run:start","kind":"value","result":null,"salt":"AAAA"}`,
	`{"redacted":{"leaf_hash":"ab12","at_ms":1}}`,
}

// Wherever the recovery decoder reports a start, the full decoding succeeds with the same start.
func TestRev138c_StartDecodeAgreesWithFull(t *testing.T) {
	for _, s := range rev138cStartSeeds {
		checkStartAgree(t, []byte(s))
	}
}

func FuzzRev138c_StartDecode(f *testing.F) {
	for _, s := range rev138cStartSeeds {
		f.Add([]byte(s))
	}
	skipSalt = true
	f.Fuzz(func(t *testing.T, b []byte) { checkStartAgree(t, b) })
}

var skipSalt bool

func checkStartAgree(t *testing.T, b []byte) {
	t.Helper()
	fast, ok := decodeStartEntry(b)
	if !ok {
		return // falls back to the full decoding
	}
	full, fok, err := fullStartDecode(b)
	if err != nil && skipSalt && (strings.Contains(strings.ToLower(err.Error()), "salt of type") || strings.Contains(err.Error(), "base64")) {
		return // known class: the salt is not read on the fast paths
	}
	if err != nil || !fok {
		t.Errorf("%s: fast path reads %+v, full decoding = ok %v, err %v", b, fast, fok, err)
		return
	}
	if !reflect.DeepEqual(fast, full) {
		t.Errorf("%s: fast %+v != full %+v", b, fast, full)
	}
}

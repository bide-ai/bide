package agent

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
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
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"result":{"saga":true},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"name":"x","salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"kind":"error","salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	// case-folded keys and escapes
	`{"NAME":"run:start","Kind":"value","Result":{"Input":"hi"},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"name":"run\u003astart","kind":"value","result":{"input":"hi"},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"name":"run:start","kind":"value","result":{"input":"h\u00e9","kind":"agent"},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	"{\"name\":\"run:start\",\"kind\":\"value\",\"result\":{\"input\":\"a\xe2\x80\xa8b\",\"kind\":\"agent\"},\"salt\":\"AAAA\"}",
	"{\"name\":\"run:start\",\"kind\":\"value\",\"result\":{\"input\":\"a\x7fb\",\"kind\":\"agent\"},\"salt\":\"AAAA\"}",
	// extra fields
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent","zz":1},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","zz":2}`,
	`{"name":"run:start","kind":"value","result":{"input":{"role":"user","parts":[{"type":"text","text":"x"}]}},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"name":"run:start","kind":"value","result":{"input":null,"saga":true},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"name":"run:start","kind":"value","result":null,"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"redacted":{"leaf_hash":"ab12","at_ms":1}}`,
	// salts of the right length that are not one, or not a plain string
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"salt":"AAAAAAAAAAAAAAAAAAAA=AAAAAAAAAAAAAAAAAAAAAAA"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\u003d"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi"},"salt":null}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi"},"salt":5}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi"}}`,
	"{\"name\":\"run:start\",\"kind\":\"value\",\"result\":{\"input\":\"\"},\"salt\":\"00000000000000000000000000000000000000000000\"}",
	// a member the fast paths do not read, of a type the full decoding refuses
	`{"name":"run:start","kind":"value","result":{"input":"hi"},"attempted_at":"x","salt":"` + "`" + ` + rev138cSalt + ` + "`" + `"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi"},"Salt":"` + "`" + ` + rev138cSalt + ` + "`" + `","salt":"` + "`" + ` + rev138cSalt + ` + "`" + `"}`,
	` {"name" : "run:start" , "kind":"value","result":{"input":"hi"},"salt":"` + "`" + ` + rev138cSalt + ` + "`" + `"} `,
}

// rev138cSalt is a salt as the journal writes one: SaltSize bytes, base64.
const rev138cSalt = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

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
	f.Fuzz(func(t *testing.T, b []byte) { checkStartAgree(t, b) })
}

func checkStartAgree(t *testing.T, b []byte) {
	t.Helper()
	fast, ok := decodeStartEntry(b)
	if !ok {
		return // falls back to the full decoding
	}
	full, fok, err := fullStartDecode(b)
	if err != nil || !fok {
		t.Errorf("%s: fast path reads %+v, full decoding = ok %v, err %v", b, fast, fok, err)
		return
	}
	if !reflect.DeepEqual(fast, full) {
		t.Errorf("%s: fast %+v != full %+v", b, fast, full)
	}
}

// saltLen is the length the journal's encoding of a salt has.
func TestRev138c_SaltLen(t *testing.T) {
	want := base64.StdEncoding.EncodedLen(SaltSize)
	for _, got := range []int{saltLen, len(rev138cSalt)} {
		if got != want {
			t.Fatalf("saltLen = %d, rev138cSalt has %d, want %d", saltLen, len(rev138cSalt), want)
		}
	}
}

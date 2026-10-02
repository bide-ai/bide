package agent

import (
	"encoding/base64"
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

// The fast paths still read the records the journal writes, escapes and all: both decode them.
func TestRev138c_StartDecodeTakesJournaledStarts(t *testing.T) {
	for _, st := range []RunStart{
		{Input: UserText("go"), Kind: RunKindAgent},
		{Input: UserText(`say "hi`), Saga: true},
		{Input: UserText(`a\b "c" {d} [e], f`), Kind: RunKindAgent, Tools: []string{`x"`, `]`}},
		{Input: UserText("hi"), Ext: map[string]json.RawMessage{"k": json.RawMessage(`{"a":"}\""}`)}},
	} {
		res, err := marshalJournal(st)
		if err != nil {
			t.Fatal(err)
		}
		b, err := EncodeRecord(Record{Name: runStartStep, Kind: StepValue, Result: res, salt: []byte("0123456789abcdef0123456789abcdef")})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := decodeStartEntry(b); !ok {
			t.Errorf("decodeStartEntry refused %s", b)
		}
		checkStartAgree(t, b)
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

// validSalt takes exactly what the journal writes for a salt: SaltSize bytes, base64, padded.
func TestRev138c_ValidSalt(t *testing.T) {
	written := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	for s, want := range map[string]bool{
		written:                    true,
		rev138cSalt:                true,
		"AAAAAAAAAAAAAAAAAAAAAA==": false, // 16 bytes
		rev138cSalt[:43]:           false, // unpadded
		rev138cSalt + "AAAA":       false, // 35 bytes (and longer than any salt)
		rev138cSalt[:42] + "==":    false, // 31 bytes
		"=" + rev138cSalt[1:]:      false, // padding first
		rev138cSalt[:20] + "=" + rev138cSalt[21:]: false,
		rev138cSalt[:40] + "\r\nA=":               false, // Decode skips the newline: 33 others, 24 bytes
		rev138cSalt[:42] + "-=":                   false, // outside the standard alphabet
		"":                                        false,
		strings.Repeat("A", 48):                   false, // 36 bytes: more than Decode may write into a salt's buffer
	} {
		if got := validSalt([]byte(s)); got != want {
			t.Errorf("validSalt(%q) = %v, want %v", s, got, want)
		}
	}
}

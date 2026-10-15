package flowedit

import (
	"reflect"
	"testing"
)

// FuzzApply: whatever the text and the operation, Apply either refuses or returns text that
// decodes to what the independent oracle says, and never panics.
func FuzzApply(f *testing.F) {
	f.Add([]byte(sample), uint16(0))
	for i, g := range generated(20) {
		f.Add([]byte(g.text), uint16(i*37))
	}
	f.Fuzz(func(t *testing.T, text []byte, pick uint16) {
		list := ops(string(text))
		if len(list) == 0 {
			return
		}
		op := list[int(pick)%len(list)]
		r, err := Apply(text, op)
		if err != nil {
			return
		}
		want, ok := oracle(string(text), op)
		if !ok {
			return // the data oracle models string keys only; Verify has checked the result
		}
		got, derr := decode(r.Text)
		if derr != nil || !reflect.DeepEqual(want, got) {
			t.Fatalf("applied text disagrees with the oracle\nop %+v\n%s\n=====\n%s", op, text, r.Text)
		}
	})
}

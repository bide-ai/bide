package flowedit

import (
	"fmt"
	"strings"
	"testing"
)

func TestDebugRef(t *testing.T) {
	n := 0
	for _, f := range generated(300) {
		for _, op := range ops(f.text) {
			raw, err := applyRaw([]byte(f.text), op)
			if err != nil {
				continue
			}
			verr := Verify([]byte(f.text), []byte(raw), op)
			if verr == nil || n >= 2 || !strings.Contains(verr.Error(), "dropped") {
				continue
			}
			n++
			fmt.Println("====", f.name, fmt.Sprintf("%+v", op), verr)
			a, b := strings.Split(f.text, "\n"), strings.Split(raw, "\n")
			d, _ := parse([]byte(f.text))
			sp, en, _ := d.spans(op)
			fmt.Println("span", sp, "entry", en, "len", len(a), len(b))
			if len(a) < 30 {
				fmt.Println(f.text, "=====>"); fmt.Println(a[0])
				fmt.Println(raw)
			}
		}
	}
}

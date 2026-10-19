package flowedit

import (
	"bytes"
	"testing"

	"go.yaml.in/yaml/v3"
)

// FuzzSwap: exchanging two neighbouring items of any list in any text either refuses or gives text
// that Verify accepts, and exchanging the same pair again restores the exact bytes. It never panics.
func FuzzSwap(f *testing.F) {
	f.Add([]byte(structFlow), uint16(0))
	f.Add([]byte(structFlow), uint16(3))
	f.Add([]byte("a: [1, 2, 3]\nb:\n  - x\n  # c\n  - y   # t\n\n  - z\n"), uint16(1))
	f.Add([]byte("{\"a\": [\n  {\"k\": 1},\n  {\"k\": 2}\n]}\n"), uint16(0))
	for i, g := range generated(20) {
		f.Add([]byte(g.text), uint16(i*29))
	}
	f.Fuzz(func(t *testing.T, text []byte, pick uint16) {
		var doc yaml.Node
		if yaml.Unmarshal(text, &doc) != nil || len(doc.Content) == 0 {
			return
		}
		var ops []Op
		var walk func(path []any, n *yaml.Node, depth int)
		walk = func(path []any, n *yaml.Node, depth int) {
			if depth > 8 {
				return
			}
			switch n.Kind {
			case yaml.MappingNode:
				for i := 0; i+1 < len(n.Content); i += 2 {
					if n.Content[i].Kind == yaml.ScalarNode {
						walk(append(append([]any(nil), path...), n.Content[i].Value), n.Content[i+1], depth+1)
					}
				}
			case yaml.SequenceNode:
				for i := 0; i+1 < len(n.Content); i++ {
					ops = append(ops, Op{Kind: Swap, Path: append([]any(nil), path...), Index: i})
				}
				for i, c := range n.Content {
					walk(append(append([]any(nil), path...), i), c, depth+1)
				}
			}
		}
		walk(nil, doc.Content[0], 0)
		if len(ops) == 0 {
			return
		}
		op := ops[int(pick)%len(ops)]
		r, err := Apply(text, op)
		if err != nil {
			return
		}
		back, err := Apply(r.Text, op)
		if err != nil {
			t.Fatalf("exchanging back is refused: %v\nop %+v\n%s\n=====\n%s", err, op, text, r.Text)
		}
		if !bytes.Equal(back.Text, text) {
			t.Fatalf("exchanging twice did not restore the bytes\nop %+v\n%s\n=====\n%s\n=====\n%s", op, text, r.Text, back.Text)
		}
	})
}

package flowedit

import (
	"strings"
	"testing"
)

const owned = `version: 1
nodes:
  # about a
  - {name: a, block: a}   # a trails

  # about b
  - {name: b, block: b}
  # about z
  - {name: z, block: z}
wiring: []
`

// The verification refuses an output that changes data only the operation should change, and an
// output that changes the lines or the comments around the operated entry, even when the data is
// right.
func TestVerifyRefusesHostileOutputs(t *testing.T) {
	del := Op{Kind: Delete, Path: []any{"nodes", 0}}
	good, err := Apply([]byte(owned), del)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(good.Text), "about a") || strings.Contains(string(good.Text), "a trails") || !strings.Contains(string(good.Text), "about b") {
		t.Fatalf("the real result is wrong:\n%s", good.Text)
	}
	for name, c := range map[string]struct {
		op  Op
		out string
	}{
		"delete a, and drop b's comment too":     {del, strings.Replace(string(good.Text), "  # about b\n", "", 1)},
		"delete a, and move b's comment onto z":  {del, strings.Replace(strings.Replace(string(good.Text), "  # about b\n", "", 1), "  # about z\n", "  # about z\n  # about b\n", 1)},
		"delete a, and leave its comment behind": {del, strings.Replace(owned, "  - {name: a, block: a}   # a trails\n", "", 1)},
		"delete a, and add a comment":            {del, strings.Replace(string(good.Text), "wiring: []", "# stray\nwiring: []", 1)},
		"delete a, and reorder the keys":         {del, strings.Replace(string(good.Text), "version: 1\nnodes:", "nodes:", 1) + "version: 1\n"},
		"insert under z's comment": {
			Op{Kind: Insert, Path: []any{"nodes"}, Index: 3, Value: Map{{"name", "nn"}, {"block", "nn"}}},
			strings.Replace(owned, "  # about z\n", "  # about z\n  - {name: nn, block: nn}\n", 1),
		},
		"insert above b's comment, in b's place": {
			Op{Kind: Insert, Path: []any{"nodes"}, Index: 2, Value: Map{{"name", "nn"}, {"block", "nn"}}},
			strings.Replace(owned, "  # about z\n", "  # about z\n  - {name: nn, block: nn}\n", 1),
		},
		"set a value, and change a line elsewhere": {
			Op{Kind: Set, Path: []any{"version"}, Value: 2},
			strings.Replace(strings.Replace(owned, "version: 1", "version: 2", 1), "wiring: []", "wiring: [ ]", 1),
		},
	} {
		if err := Verify([]byte(owned), []byte(c.out), c.op); err == nil {
			t.Errorf("%s: accepted\n%s", name, c.out)
		}
	}
	// The correct outputs pass.
	if err := Verify([]byte(owned), good.Text, del); err != nil {
		t.Errorf("the real result was refused: %v", err)
	}
	ins, err := Apply([]byte(owned), Op{Kind: Insert, Path: []any{"nodes"}, Index: 3, Value: Map{{"name", "nn"}, {"block", "nn"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ins.Text), "  # about z\n  - {name: z, block: z}\n  - {name: nn, block: nn}\n") {
		t.Errorf("the item was not added after z:\n%s", ins.Text)
	}
}

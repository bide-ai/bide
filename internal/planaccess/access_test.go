package planaccess

import "testing"

func TestRegister(t *testing.T) {
	oldInspect := inspect
	defer func() { inspect = oldInspect }()
	RegisterInspect(func([]byte, any, any) []Record { return []Record{{Class: "type", Message: "m"}} })
	if got := Inspect(nil, nil, nil); len(got) != 1 || got[0].Message != "m" {
		t.Errorf("Inspect() = %v", got)
	}
}

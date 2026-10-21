package planaccess

import (
	"errors"
	"testing"
)

func TestRegisterParse(t *testing.T) {
	old := parse
	defer func() { parse = old }()
	parse = nil
	if ParseError(nil) != nil {
		t.Error("ParseError without plan loaded")
	}
	want := errors.New("bad")
	RegisterParse(func([]byte) error { return want })
	if ParseError(nil) != want {
		t.Error("ParseError does not return the registered function's error")
	}
}

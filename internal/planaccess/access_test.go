package planaccess

import "testing"

func TestRegister(t *testing.T) {
	old := allTypeProblems
	defer func() { allTypeProblems = old }()
	Register("opt")
	if AllTypeProblems() != "opt" {
		t.Errorf("AllTypeProblems() = %v", AllTypeProblems())
	}
}

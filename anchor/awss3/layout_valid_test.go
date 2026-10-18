package awss3

import "testing"

func TestLayoutIsValid(t *testing.T) {
	if err := layout.Validate(); err != nil {
		t.Fatal(err)
	}
}

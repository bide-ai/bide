package pkg

import (
	"os"
	"testing"
)

func gate(t *testing.T) {
	if os.Getenv("BIDE_FAKE_BROWSER") != "1" {
		t.Skip()
	}
}

func drive(t *testing.T) { gate(t) }

func TestOneInChrome(t *testing.T) { drive(t) }

func TestTwoMissesThePattern(t *testing.T) { drive(t) }

func TestNotGated(t *testing.T) {}

func TestUnsetVar(t *testing.T) {
	_ = os.Getenv("BIDE_NOWORKFLOW_BROWSER")
}

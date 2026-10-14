package hsmkeys

import (
	"errors"
	"os"
	"testing"
)

func testBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// TestPromptChild runs only in the child TestPromptRefusedWithoutATerminal starts without a
// controlling terminal.
func TestPromptChild(t *testing.T) {
	if os.Getenv("HSMKEYS_PROMPT_CHILD") == "" {
		return // not the child: a helper process, not a test (a skip would fail the integration job)
	}
	pin, err := readPINFromTerminal("PIN: ")
	if !errors.Is(err, ErrNoTerminal) || pin != "" {
		t.Fatalf("pin %q, err %v: want ErrNoTerminal", pin, err)
	}
}

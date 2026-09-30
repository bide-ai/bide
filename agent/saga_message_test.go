package agent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Uncompensated lists every write the rollback did not undo, for whatever reason: one with no
// compensator, one whose outcome is unknown (the rollback halted on it, compensator or not), or one
// whose tool is gone. The message says that, not that each lacked a compensator.
func TestSagaAborted_MessageDoesNotBlameAMissingCompensator(t *testing.T) {
	e := &SagaAborted{RunID: "r", Cause: errors.New("boom"), Uncompensated: []string{"charge"},
		CompensateErr: toolHalt("r", "r", "c1", "charge", time.Time{}, HaltCrashed)}
	msg := e.Error()
	if strings.Contains(msg, "no compensator") || !strings.Contains(msg, "charge") || !strings.Contains(msg, "INCOMPLETE") {
		t.Fatalf("Error() = %q; want the uncompensated writes listed without blaming a missing compensator", msg)
	}
}

package notifycmd

import (
	"errors"
	"fmt"
	"testing"
)

// On Plan 9 there is no syscall.ECONNREFUSED, and a refused connection is known by how the system
// words it. refusedByText is that rule, built on every OS so it is tested here.
func TestRefusedByText(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":             {nil, false},
		"plan 9 wording":  {fmt.Errorf("dial tcp 127.0.0.1:1: %w", errors.New("connection refused")), true},
		"capitalised":     {errors.New("Connection Refused"), true},
		"windows wording": {errors.New("No connection could be made because the target machine actively refused it."), true},
		"a timeout":       {errors.New("i/o timeout"), false},
		"no such host":    {errors.New("no such host"), false},
	} {
		if got := refusedByText(tc.err); got != tc.want {
			t.Errorf("%s: refusedByText(%v) = %v, want %v", name, tc.err, got, tc.want)
		}
	}
}

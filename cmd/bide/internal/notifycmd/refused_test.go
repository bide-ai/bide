package notifycmd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
)

// On Plan 9 there is no syscall.ECONNREFUSED, and a refused connection is known by how the system
// words it: a dial error (*net.OpError, Op "dial") whose innermost message ends in "connection
// refused". Any other error that merely contains the words is not one. refusedByText is that rule,
// built on every OS so it is tested here.
func TestRefusedByText(t *testing.T) {
	dial := func(err error) error {
		return fmt.Errorf("notify: POST to 127.0.0.1:1 failed: %w", &net.OpError{Op: "dial", Net: "tcp", Err: err})
	}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":                    {nil, false},
		"plan 9 dial":            {dial(errors.New("connection refused")), true},
		"dial through a syscall": {dial(os.NewSyscallError("connect", errors.New("connection refused"))), true},
		"a read, not a dial":     {fmt.Errorf("x: %w", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection refused")}), false},
		"words and more":         {dial(errors.New("connection refused by the proxy policy")), false},
		"proxy policy, no dial":  {errors.New("connection refused by the proxy policy"), false},
		"rate limiter":           {errors.New("request refused by rate limiter"), false},
		"bare words, no dial":    {errors.New("connection refused"), false},
		"capitalised":            {dial(errors.New("Connection Refused")), false},
		"windows wording":        {dial(errors.New("No connection could be made because the target machine actively refused it.")), false},
		"a timeout in a dial":    {dial(errors.New("i/o timeout")), false},
		"no such host in a dial": {dial(errors.New("no such host")), false},
	} {
		if got := refusedByText(tc.err); got != tc.want {
			t.Errorf("%s: refusedByText(%v) = %v, want %v", name, tc.err, got, tc.want)
		}
	}
}

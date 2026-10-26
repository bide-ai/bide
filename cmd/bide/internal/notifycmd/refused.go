package notifycmd

import (
	"errors"
	"net"
	"strings"
)

// refusedByText reports a refused connection by how the system words it: a dial error (*net.OpError,
// Op "dial") whose own error message ends in "connection refused". It is the rule where the syscall
// package has no error number for it (Plan 9, isRefused). An error that only contains the words
// ("connection refused by the proxy policy", "request refused by rate limiter") is not one. It is built
// everywhere so its test runs everywhere.
func refusedByText(err error) bool {
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		return false
	}
	return op.Err != nil && strings.HasSuffix(op.Err.Error(), "connection refused")
}

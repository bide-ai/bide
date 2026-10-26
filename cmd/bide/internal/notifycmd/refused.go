package notifycmd

import "strings"

// refusedByText reports a refused connection by how the system words it ("connection refused",
// "actively refused it"). It is the rule where the syscall package has no error number for it (Plan 9,
// isRefused); it is built everywhere so its test runs everywhere.
func refusedByText(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "refused")
}

package notifycmd

import "errors"

// errConnRefused is Plan 9's refused-connection error: it has no error number, only the wording.
var errConnRefused = errors.New("connection refused")

// refusedByErrno is false: the rule in force on Plan 9 is the wording (refusedByText, tested on every OS),
// so a test of what the number rule refuses does not apply.
const refusedByErrno = false

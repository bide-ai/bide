package notifycmd

import "errors"

// errConnRefused is Plan 9's refused-connection error: it has no error number, only the wording.
var errConnRefused = errors.New("connection refused")

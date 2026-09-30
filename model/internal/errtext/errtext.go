// Package errtext bounds provider-supplied text that an adapter quotes in an error.
package errtext

import "strconv"

// maxQuoted bounds a quoted value. The values adapters quote (a finish reason, an event type) are
// a few bytes long from a working provider; a longer one is cut so a broken or hostile endpoint
// cannot turn one error into megabytes of log line.
const maxQuoted = 128

// Quote returns s quoted as %q would, cut to its first maxQuoted bytes, with the full length noted
// when it was cut.
func Quote(s string) string {
	if len(s) <= maxQuoted {
		return strconv.Quote(s)
	}
	return strconv.Quote(s[:maxQuoted]) + "...(" + strconv.Itoa(len(s)) + " bytes)"
}

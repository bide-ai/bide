package agent

import (
	"strings"
	"testing"
)

// An APIError or RateLimited built with a huge body or message still prints a bounded line: a
// provider that echoes the prompt back must not put megabytes into every log line and trace.
func TestProviderErrors_BoundTheirText(t *testing.T) {
	huge := strings.Repeat("x", 32<<20)
	if n := len((&APIError{StatusCode: 500, Body: huge, Err: ErrModel}).Error()); n > 2*maxErrorBody {
		t.Errorf("APIError.Error() is %d bytes", n)
	}
	if n := len((&APIError{StatusCode: 500, Message: huge, Err: ErrModel}).Error()); n > 2*maxErrorBody {
		t.Errorf("APIError.Error() with a huge message is %d bytes", n)
	}
	if n := len((&RateLimited{Message: huge, Err: ErrModel}).Error()); n > 2*maxErrorBody {
		t.Errorf("RateLimited.Error() is %d bytes", n)
	}
}

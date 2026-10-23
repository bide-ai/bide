package relay

import (
	"crypto/fips140"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/notify"
)

// New refuses a secret shorter than 14 bytes under GODEBUG=fips140=only, as a configuration error
// and not a panic when the first request is verified; without that mode it is accepted as before.
func TestNewRefusesAShortSecretUnderFIPSEnforcement(t *testing.T) {
	const short = "0123456789abc"
	ok := &funcSink{name: "a"}
	_, err := New(Config{Secrets: []string{secret, short}, Sinks: []Sink{ok}})
	if fips140.Enforced() {
		if !errors.Is(err, notify.ErrShortSecret) || strings.Contains(err.Error(), short) {
			t.Fatalf("New = %v, want an error wrapping ErrShortSecret without the secret", err)
		}
	} else if err != nil {
		t.Fatalf("without enforcement New = %v, want nil", err)
	}
}

// A mis-set secret is named as such in the log, not as an unreadable request.
func TestRejectionClassNamesAShortSecret(t *testing.T) {
	got := RejectionClass(fmt.Errorf("wrapped: %w", notify.ErrShortSecret))
	if !strings.Contains(got, "secret is too short") {
		t.Fatalf("RejectionClass = %q", got)
	}
	if RejectionClass(errors.New("x")) != "unreadable request" {
		t.Fatal("the default class changed")
	}
}

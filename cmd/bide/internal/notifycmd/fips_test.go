package notifycmd

import (
	"crypto/fips140"
	"strings"
	"testing"
)

// A 13-byte secret is a startup error under GODEBUG=fips140=only (crypto/hmac would panic on it
// there), naming the variable but never the value; without that mode it loads as before. Both
// verbs that verify (listen, relay) and the sender (test) read their secrets through load.
func TestShortSecretIsAStartupErrorUnderFIPSEnforcement(t *testing.T) {
	const short = "0123456789abc"
	for _, tc := range []struct{ name, cur, prev string }{
		{"current", short, ""},
		{"previous", "0123456789abcdef0123", short},
	} {
		src := &secretSource{env: "S", prevEnv: "P"}
		env := map[string]string{"S": tc.cur, "P": tc.prev}
		got, err := src.load(func(k string) string { return env[k] }, nil)
		if fips140.Enforced() {
			if err == nil || !strings.Contains(err.Error(), "14 bytes") {
				t.Errorf("%s: load = %v, want an error naming the 14 byte minimum", tc.name, err)
			}
			if err != nil && strings.Contains(err.Error(), short) {
				t.Errorf("%s: the error holds the secret: %v", tc.name, err)
			}
		} else if err != nil || got.current != tc.cur || got.previous != tc.prev {
			t.Errorf("%s: without enforcement load = %+v, %v, want the secrets unchanged", tc.name, got, err)
		}
	}
}

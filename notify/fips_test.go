package notify

import (
	"bytes"
	"crypto/fips140"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// shortSecret is 13 bytes: below the 112 bits (14 bytes) crypto/hmac accepts under
// GODEBUG=fips140=only, where it panics rather than returning an error.
const shortSecret = "0123456789abc"

// okSecret is 14 bytes, the shortest crypto/hmac accepts under fips140=only.
const okSecret = "0123456789abcd"

// Under fips140=only a secret shorter than 14 bytes is refused as configuration, never reached as a
// panic at Sign or Verify time. Without enforcement the behaviour is unchanged: it works.
// (The assertion follows fips140.Enforced(), so it runs, and proves something, in both modes.)
func TestShortSecretUnderFIPSEnforcement(t *testing.T) {
	enforced := fips140.Enforced()

	for _, tc := range []struct {
		name          string
		current, prev string
	}{{"current", shortSecret, ""}, {"previous", okSecret, shortSecret}} {
		_, err := NewWebhook("https://example.com/hook", WithSecrets(tc.current, tc.prev))
		if enforced {
			if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "14 bytes") {
				t.Errorf("%s: NewWebhook error = %v, want an ErrConfig naming the 14 byte minimum", tc.name, err)
			}
			if strings.Contains(err.Error(), shortSecret) {
				t.Errorf("%s: the error holds the secret: %v", tc.name, err)
			}
		} else if err != nil {
			t.Errorf("%s: without enforcement a short secret is accepted as before, got %v", tc.name, err)
		}
	}
	if _, err := NewWebhook("https://example.com/hook", WithSecrets(okSecret, okSecret+"x")); err != nil {
		t.Errorf("14 and 15 byte secrets must be accepted under any mode: %v", err)
	}

	// Verify with a short secret: ErrShortSecret under enforcement (no panic), a normal check otherwise.
	ts := time.Now()
	body := []byte(`{"id":"d1"}`)
	sig := Sign([]string{shortSecret}, ts.Unix(), body)
	err := VerifyBody(itoa(ts.Unix()), sig, body, []string{shortSecret}, 0, ts)
	if enforced {
		if !errors.Is(err, ErrShortSecret) {
			t.Errorf("VerifyBody = %v, want ErrShortSecret", err)
		}
		if sig != "" {
			t.Errorf("Sign with only a short secret under enforcement = %q, want no entry", sig)
		}
	} else if err != nil {
		t.Errorf("VerifyBody without enforcement = %v, want nil", err)
	}

	// A usable secret next to a short one still signs and verifies.
	sig = Sign([]string{okSecret, shortSecret}, ts.Unix(), body)
	if err := VerifyBody(itoa(ts.Unix()), sig, body, []string{okSecret}, 0, ts); err != nil {
		t.Errorf("14 byte secret: %v", err)
	}

	// The http path too.
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	r.Header.Set(HeaderTimestamp, itoa(ts.Unix()))
	r.Header.Set(HeaderSignature, Sign([]string{okSecret}, ts.Unix(), body))
	r.Header.Set(HeaderDelivery, "d1")
	if err := Verify(r, []string{okSecret}, 0); err != nil {
		t.Errorf("Verify with a 14 byte secret = %v", err)
	}
}

// checkSecret is the rule itself, testable without GODEBUG.
func TestCheckSecretRule(t *testing.T) {
	for _, tc := range []struct {
		secret   string
		enforced bool
		ok       bool
	}{
		{shortSecret, true, false}, {okSecret, true, true}, {"", true, true}, // empty is handled by the callers
		{shortSecret, false, true}, {okSecret, false, true},
	} {
		if got := checkSecret(tc.secret, tc.enforced) == nil; got != tc.ok {
			t.Errorf("checkSecret(len %d, enforced=%v) ok=%v, want %v", len(tc.secret), tc.enforced, got, tc.ok)
		}
	}
}

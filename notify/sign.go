package notify

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Header names of a webhook request.
const (
	HeaderDelivery  = "Bide-Delivery"
	HeaderTimestamp = "Bide-Timestamp"
	HeaderSignature = "Bide-Signature"
)

// DefaultTolerance is how far a request's timestamp may be from the receiver's clock before Verify
// rejects it, when the caller passes a tolerance of zero.
const DefaultTolerance = 5 * time.Minute

// maxVerifyBody bounds what Verify reads, so a hostile sender cannot make a receiver buffer an
// unbounded body before the signature is checked.
const maxVerifyBody = 1 << 20

// Verification errors. A receiver can tell them apart, but should answer all of them with the same
// 4xx so a sender learns nothing about which check failed.
var (
	ErrNoSignature   = errors.New("notify: missing signature or timestamp header")
	ErrBadTimestamp  = errors.New("notify: timestamp is not a Unix time in seconds")
	ErrStale         = errors.New("notify: timestamp outside the tolerance")
	ErrBadSignature  = errors.New("notify: no signature matches")
	ErrNoSecrets     = errors.New("notify: no secrets to verify with")
	ErrBodyTooLarge  = errors.New("notify: request body too large to verify")
	errSignatureRead = errors.New("notify: reading the request body")
)

// Sign returns the Bide-Signature header value for body sent at ts: one "v1=<hex>" entry per
// secret, space separated, where each is HMAC-SHA256(secret, ts + "." + body). Empty secrets are
// skipped. With two secrets (current first, previous second) a receiver holding either verifies the
// request, which is how a secret is rotated without dropping deliveries.
func Sign(secrets []string, ts int64, body []byte) string {
	var parts []string
	for _, s := range secrets {
		if s == "" {
			continue
		}
		parts = append(parts, "v1="+hex.EncodeToString(mac(s, ts, body)))
	}
	return strings.Join(parts, " ")
}

func mac(secret string, ts int64, body []byte) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(strconv.FormatInt(ts, 10)))
	h.Write([]byte("."))
	h.Write(body)
	return h.Sum(nil)
}

// Verify checks a request from a bide webhook in one call, for use in a receiver. It reads the
// whole body (up to 1 MiB), checks that the Bide-Timestamp is within tolerance of now (zero means
// DefaultTolerance, 5 minutes), and that at least one v1 signature in Bide-Signature equals the
// HMAC-SHA256 of timestamp + "." + body under one of secrets, compared in constant time. Pass both
// the new and the old secret during a rotation.
//
// On return r.Body is replaced by a reader over the same bytes, so the handler can decode it. Verify
// returns nil only when the request is authentic and fresh. Treat the delivery ID header
// (Bide-Delivery) as the dedupe key: the same event can be delivered more than once.
func Verify(r *http.Request, secrets []string, tolerance time.Duration) error {
	return verifyAt(r, secrets, tolerance, time.Now())
}

func verifyAt(r *http.Request, secrets []string, tolerance time.Duration, now time.Time) error {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxVerifyBody+1))
	if err != nil {
		return fmt.Errorf("%w: %v", errSignatureRead, err)
	}
	if len(body) > maxVerifyBody {
		return ErrBodyTooLarge
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return VerifyBody(r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderSignature), body, secrets, tolerance, now)
}

// VerifyBody is Verify over parts the caller already holds, such as a stored body and its headers
// when checking a delivery later as evidence. now is the time to judge freshness against; pass the
// time of receipt, or a very large tolerance to check the signature alone.
func VerifyBody(timestamp, signature string, body []byte, secrets []string, tolerance time.Duration, now time.Time) error {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	var live []string
	for _, s := range secrets {
		if s != "" {
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		return ErrNoSecrets
	}
	if timestamp == "" || signature == "" {
		return ErrNoSignature
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrBadTimestamp
	}
	// Signature first, then age: both always run, so the answer does not leak which one failed
	// through timing, and a forged-but-fresh request is not told it was merely stale.
	matched := false
	for _, entry := range strings.Fields(signature) {
		val, ok := strings.CutPrefix(entry, "v1=")
		if !ok {
			continue // an unknown scheme: ignore, so a later v2 can ride alongside
		}
		got, err := hex.DecodeString(val)
		if err != nil {
			continue
		}
		for _, s := range live {
			if hmac.Equal(got, mac(s, ts, body)) {
				matched = true
			}
		}
	}
	age := now.Sub(time.Unix(ts, 0))
	if age < 0 {
		age = -age
	}
	switch {
	case !matched:
		return ErrBadSignature
	case age > tolerance:
		return ErrStale
	}
	return nil
}

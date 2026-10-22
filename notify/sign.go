package notify

import (
	"bytes"
	"crypto/fips140"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// maxSignatureEntries bounds how many v1 entries of Bide-Signature are looked at; a sender never
// sends more than two (current and previous), so the rest is noise or an attack.
const maxSignatureEntries = 8

// Verification errors. A receiver can tell them apart, but should answer all of them with the same
// 4xx so a sender learns nothing about which check failed.
var (
	ErrNoSignature   = errors.New("notify: missing signature or timestamp header")
	ErrBadTimestamp  = errors.New("notify: timestamp is not a Unix time in seconds")
	ErrStale         = errors.New("notify: timestamp outside the tolerance")
	ErrBadSignature  = errors.New("notify: no signature matches")
	ErrNoSecrets     = errors.New("notify: no secrets to verify with")
	ErrShortSecret   = errors.New("notify: secret is shorter than the 14 bytes FIPS 140-3 mode allows for an HMAC key")
	ErrBodyTooLarge  = errors.New("notify: request body too large to verify")
	ErrDeliveryID    = errors.New("notify: Bide-Delivery is missing or does not match the signed id")
	errSignatureRead = errors.New("notify: reading the request body")
)

// Sign returns the Bide-Signature header value for body sent at ts: one "v1=<hex>" entry per
// secret, space separated, where each is HMAC-SHA256(secret, ts + "." + body). Empty secrets are
// skipped, and so is a secret shorter than 14 bytes under GODEBUG=fips140=only (see ErrShortSecret;
// crypto/hmac panics on one there). With two secrets (current first, previous second) a receiver holding either verifies the
// request, which is how a secret is rotated without dropping deliveries.
func Sign(secrets []string, ts int64, body []byte) string {
	var parts []string
	for _, s := range secrets {
		if s == "" || checkSecret(s, fips140.Enforced()) != nil {
			continue
		}
		parts = append(parts, "v1="+hex.EncodeToString(mac(s, ts, body)))
	}
	return strings.Join(parts, " ")
}

// minFIPSSecret is the shortest HMAC key (112 bits) crypto/hmac accepts under GODEBUG=fips140=only.
// bide recommends at least 32 bytes of random data for every deployment; this is only the floor that
// keeps FIPS enforcement from panicking.
const minFIPSSecret = 14

// checkSecret refuses a non-empty secret shorter than minFIPSSecret when enforced (fips140.Enforced()).
// Outside strict FIPS enforcement every secret is accepted, as before.
func checkSecret(secret string, enforced bool) error {
	if enforced && secret != "" && len(secret) < minFIPSSecret {
		return ErrShortSecret
	}
	return nil
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
	// Whatever happens next, the handler (or an error page) still sees the bytes that were read.
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
	if err != nil {
		return fmt.Errorf("%w: %v", errSignatureRead, err)
	}
	if len(body) > maxVerifyBody {
		return ErrBodyTooLarge
	}
	if err := VerifyBody(r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderSignature), body, secrets, tolerance, now); err != nil {
		return err
	}
	// The signature covers the body, not the headers, so the delivery ID a receiver dedupes on must
	// be the signed one: otherwise a captured request replayed with a fresh (or deleted) header
	// passes dedupe. bide always sends the header, so its absence is as bad as a mismatch.
	var p struct {
		ID string `json:"id"`
	}
	if h := r.Header.Get(HeaderDelivery); h == "" || json.Unmarshal(body, &p) != nil || p.ID != h {
		return ErrDeliveryID
	}
	return nil
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
		if err := checkSecret(s, fips140.Enforced()); err != nil {
			return err
		}
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
	// One MAC per secret, however many entries the header holds; entries are compared to those.
	macs := make([][]byte, len(live))
	for i, sec := range live {
		macs[i] = mac(sec, ts, body)
	}
	matched := false
	for n, entry := range strings.Fields(signature) {
		if n >= maxSignatureEntries {
			break
		}
		val, ok := strings.CutPrefix(entry, "v1=")
		if !ok {
			continue // an unknown scheme: ignore, so a later v2 can ride alongside
		}
		got, err := hex.DecodeString(val)
		if err != nil {
			continue
		}
		for _, m := range macs {
			if hmac.Equal(got, m) {
				matched = true
			}
		}
	}
	// Compare the instants themselves: a subtraction saturates for a far-future timestamp.
	tolSec := int64(tolerance / time.Second)
	if tolerance%time.Second != 0 {
		tolSec++
	}
	nowSec := now.Unix()
	stale := ts > nowSec+tolSec || ts < nowSec-tolSec
	switch {
	case !matched:
		return ErrBadSignature
	case stale:
		return ErrStale
	}
	return nil
}

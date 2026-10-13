package notify

// Confirmation pass of the webhook review, against 9278eeef.

import (
	"errors"
	"io"
	"testing"
	"time"
)

// W1b. ErrDeliveryID binds the header to the signed id only when the header is present. The guide's
// receiver dedupes on r.Header.Get(notify.HeaderDelivery) after Verify, so a captured request
// replayed with the header removed passes Verify and dedupes under "" instead of its id: the replay
// bypass of W1 still works by deletion. bide always sends the header; Verify should require it.
func TestReview_VerifyRequiresTheDeliveryHeader(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"schema":"bide.notification.v1","id":"aaaa"}`)
	r := reviewRequest(t, []string{"s3cret"}, now.Unix(), body, "aaaa")
	r.Header.Del(HeaderDelivery)
	if err := verifyAt(r, []string{"s3cret"}, 0, now); !errors.Is(err, ErrDeliveryID) && !errors.Is(err, ErrNoSignature) {
		t.Fatalf("Verify = %v for a request whose Bide-Delivery (the guide's dedupe key) was stripped", err)
	}
}

// Held: Verify can run twice on one request (the body is restored after each), a too-large body is
// still readable in full after ErrBodyTooLarge, and entries past the 8th are not looked at.
func TestReviewHeld_ConfirmationFixes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"id":"aaaa"}`)
	r := reviewRequest(t, []string{"k"}, now.Unix(), body, "aaaa")
	for i := range 2 {
		if err := verifyAt(r, []string{"k"}, 0, now); err != nil {
			t.Fatalf("verify %d: %v", i, err)
		}
	}
	if got, _ := io.ReadAll(r.Body); string(got) != string(body) {
		t.Fatalf("body after two verifies = %q", got)
	}
	big := make([]byte, maxVerifyBody+10)
	rb := reviewRequest(t, []string{"k"}, now.Unix(), big, "x")
	if err := verifyAt(rb, []string{"k"}, 0, now); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("big: %v", err)
	}
	if got, _ := io.ReadAll(rb.Body); len(got) != len(big) {
		t.Fatalf("big body after verify: %d bytes, want %d", len(got), len(big))
	}
	ts := now.Unix()
	sig := "v1=00 v1=00 v1=00 v1=00 v1=00 v1=00 v1=00 v1=00 " + Sign([]string{"k"}, ts, body)
	if err := VerifyBody("1800000000", sig, body, []string{"k"}, 0, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a valid entry in 9th place: %v (the cap should ignore it)", err)
	}
	// Tolerance edges by the second, and a huge tolerance for evidence checks.
	for _, c := range []struct {
		ts    int64
		stale bool
	}{{ts - 300, false}, {ts + 300, false}, {ts - 301, true}, {ts + 301, true}} {
		err := VerifyBody(itoa(c.ts), Sign([]string{"k"}, c.ts, body), body, []string{"k"}, 0, now)
		if errors.Is(err, ErrStale) != c.stale {
			t.Errorf("ts %+d: %v", c.ts-ts, err)
		}
	}
	if err := VerifyBody("1", Sign([]string{"k"}, 1, body), body, []string{"k"}, time.Duration(1<<62), now); err != nil {
		t.Errorf("huge tolerance: %v", err)
	}
}

func itoa(n int64) string { return formatInt(n) }

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

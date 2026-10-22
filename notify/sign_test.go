package notify

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// signedRequest signs a payload-shaped body that carries body as a field, with the matching
// Bide-Delivery header, as bide sends it.
func signedRequest(secrets []string, ts time.Time, body string) *http.Request {
	body = `{"id":"d1","x":"` + body + `"}`
	r := httptest.NewRequest("POST", "/hook", strings.NewReader(body))
	r.Header.Set(HeaderDelivery, "d1")
	r.Header.Set(HeaderTimestamp, strconv.FormatInt(ts.Unix(), 10))
	r.Header.Set(HeaderSignature, Sign(secrets, ts.Unix(), []byte(body)))
	return r
}

func TestSignCoversTimestampAndBody(t *testing.T) {
	body := []byte(`{"id":"x"}`)
	base := Sign([]string{"k-fips-padded-secret"}, 100, body)
	if base == Sign([]string{"k-fips-padded-secret"}, 101, body) {
		t.Fatal("signature does not cover the timestamp: a captured request could be replayed with a fresh one")
	}
	if base == Sign([]string{"k-fips-padded-secret"}, 100, []byte(`{"id":"y"}`)) {
		t.Fatal("signature does not cover the body")
	}
	if base == Sign([]string{"k2-fips-padded-secret"}, 100, body) {
		t.Fatal("signature does not depend on the secret")
	}
	// ts and body are joined by a dot, so ("1", "00.x") and ("100", ".x")-style splices differ.
	if Sign([]string{"k-fips-padded-secret"}, 1, []byte("00.x")) == Sign([]string{"k-fips-padded-secret"}, 100, []byte(".x")) {
		t.Fatal("timestamp and body are not separated")
	}
	// An independent computation of the documented construction.
	want := "v1=" + hexMAC("k-fips-padded-secret", "100."+string(body))
	if base != want {
		t.Fatalf("Sign = %q, want %q", base, want)
	}
}

func TestSignTwoSecretsAndSkipsEmpty(t *testing.T) {
	h := Sign([]string{"new-fips-padded-secret", "old-fips-padded-secret"}, 5, []byte("b"))
	parts := strings.Split(h, " ")
	if len(parts) != 2 || parts[0] != "v1="+hexMAC("new-fips-padded-secret", "5.b") || parts[1] != "v1="+hexMAC("old-fips-padded-secret", "5.b") {
		t.Fatalf("rotation header: %q", h)
	}
	if got := Sign([]string{"new-fips-padded-secret", ""}, 5, []byte("b")); strings.Contains(got, " ") {
		t.Fatalf("empty secret signed: %q", got)
	}
}

func TestVerifyAccepts(t *testing.T) {
	now := time.Now()
	r := signedRequest([]string{"s-fips-padded-secret"}, now, "plain")
	if err := Verify(r, []string{"s-fips-padded-secret"}, 0); err != nil {
		t.Fatal(err)
	}
	// The body is still readable by the handler.
	b, _ := io.ReadAll(r.Body)
	if string(b) != `{"id":"d1","x":"plain"}` {
		t.Fatalf("body after Verify: %q", b)
	}
}

func TestVerifyRejects(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		req  func() *http.Request
		sec  []string
		want error
	}{
		{"wrong secret", func() *http.Request { return signedRequest([]string{"s-fips-padded-secret"}, now, "b") }, []string{"other-fips-padded-secret"}, ErrBadSignature},
		{"tampered body", func() *http.Request {
			r := signedRequest([]string{"s-fips-padded-secret"}, now, "b")
			r.Body = io.NopCloser(strings.NewReader("B"))
			return r
		}, []string{"s-fips-padded-secret"}, ErrBadSignature},
		{"stale timestamp", func() *http.Request {
			return signedRequest([]string{"s-fips-padded-secret"}, now.Add(-6*time.Minute), "b")
		}, []string{"s-fips-padded-secret"}, ErrStale},
		{"future timestamp", func() *http.Request {
			return signedRequest([]string{"s-fips-padded-secret"}, now.Add(6*time.Minute), "b")
		}, []string{"s-fips-padded-secret"}, ErrStale},
		{"timestamp swapped for a fresh one", func() *http.Request {
			r := signedRequest([]string{"s-fips-padded-secret"}, now.Add(-time.Hour), "b")
			r.Header.Set(HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
			return r
		}, []string{"s-fips-padded-secret"}, ErrBadSignature},
		{"no signature", func() *http.Request {
			r := signedRequest([]string{"s-fips-padded-secret"}, now, "b")
			r.Header.Del(HeaderSignature)
			return r
		}, []string{"s-fips-padded-secret"}, ErrNoSignature},
		{"no timestamp", func() *http.Request {
			r := signedRequest([]string{"s-fips-padded-secret"}, now, "b")
			r.Header.Del(HeaderTimestamp)
			return r
		}, []string{"s-fips-padded-secret"}, ErrNoSignature},
		{"bad timestamp", func() *http.Request {
			r := signedRequest([]string{"s-fips-padded-secret"}, now, "b")
			r.Header.Set(HeaderTimestamp, "yesterday")
			return r
		}, []string{"s-fips-padded-secret"}, ErrBadTimestamp},
		{"garbage signature", func() *http.Request {
			r := signedRequest([]string{"s-fips-padded-secret"}, now, "b")
			r.Header.Set(HeaderSignature, "v1=zz")
			return r
		}, []string{"s-fips-padded-secret"}, ErrBadSignature},
		{"unknown scheme only", func() *http.Request {
			r := signedRequest([]string{"s-fips-padded-secret"}, now, "b")
			r.Header.Set(HeaderSignature, "v2=00")
			return r
		}, []string{"s-fips-padded-secret"}, ErrBadSignature},
		{"no secrets", func() *http.Request { return signedRequest([]string{"s-fips-padded-secret"}, now, "b") }, nil, ErrNoSecrets},
		{"only empty secrets", func() *http.Request { return signedRequest([]string{"s-fips-padded-secret"}, now, "b") }, []string{""}, ErrNoSecrets},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := Verify(c.req(), c.sec, 0); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestVerifyTolerance(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	for _, c := range []struct {
		age  time.Duration
		tol  time.Duration
		want error
	}{
		{4*time.Minute + 59*time.Second, 0, nil},        // default 5 minutes
		{5*time.Minute + 1*time.Second, 0, ErrStale},    // default 5 minutes
		{5*time.Minute + 1*time.Second, time.Hour, nil}, // explicit
		{30 * time.Second, 10 * time.Second, ErrStale},  // explicit, tighter
		{-4*time.Minute - 59*time.Second, 0, nil},       // clock skew the other way
		{-5*time.Minute - 1*time.Second, 0, ErrStale},
	} {
		r := signedRequest([]string{"s-fips-padded-secret"}, now.Add(-c.age), "b")
		if err := verifyAt(r, []string{"s-fips-padded-secret"}, c.tol, now); !errors.Is(err, c.want) {
			t.Errorf("age %v tol %v: got %v, want %v", c.age, c.tol, err, c.want)
		}
	}
}

func TestVerifyRotationWindow(t *testing.T) {
	now := time.Now()
	// A receiver that has only the old secret, only the new one, or both, accepts it.
	for _, rx := range [][]string{{"old-fips-padded-secret"}, {"new-fips-padded-secret"}, {"new-fips-padded-secret", "old-fips-padded-secret"}, {"old-fips-padded-secret", "new-fips-padded-secret"}} {
		r := signedRequest([]string{"new-fips-padded-secret", "old-fips-padded-secret"}, now, "b")
		if err := Verify(r, rx, 0); err != nil {
			t.Errorf("receiver %v: %v", rx, err)
		}
	}
	// One that has neither does not.
	if err := Verify(signedRequest([]string{"new-fips-padded-secret", "old-fips-padded-secret"}, now, "b"), []string{"third-fips-padded-secret"}, 0); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("got %v", err)
	}
	// A sender signing with only the new secret is not accepted by a receiver that has only the old.
	if err := Verify(signedRequest([]string{"new-fips-padded-secret"}, now, "b"), []string{"old-fips-padded-secret"}, 0); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyBodyTooLarge(t *testing.T) {
	r := httptest.NewRequest("POST", "/", bytes.NewReader(make([]byte, maxVerifyBody+10)))
	r.Header.Set(HeaderTimestamp, "1")
	r.Header.Set(HeaderSignature, "v1=00")
	if err := Verify(r, []string{"s-fips-padded-secret"}, 0); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyBodyStoredEvidence(t *testing.T) {
	// Checking a stored delivery later: the signature alone, with a huge tolerance.
	ts := time.Now().Add(-30 * 24 * time.Hour)
	sig := Sign([]string{"s-fips-padded-secret"}, ts.Unix(), []byte("b"))
	if err := VerifyBody(strconv.FormatInt(ts.Unix(), 10), sig, []byte("b"), []string{"s-fips-padded-secret"}, 365*24*time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyBindsDeliveryHeaderToSignedID(t *testing.T) {
	now := time.Now()
	body := `{"schema":"bide.notification.v1","id":"abc123","key":"k"}`
	mk := func(delivery string) *http.Request {
		r := httptest.NewRequest("POST", "/hook", strings.NewReader(body))
		r.Header.Set(HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
		r.Header.Set(HeaderSignature, Sign([]string{"s-fips-padded-secret"}, now.Unix(), []byte(body)))
		if delivery != "" {
			r.Header.Set(HeaderDelivery, delivery)
		}
		return r
	}
	if err := Verify(mk("abc123"), []string{"s-fips-padded-secret"}, 0); err != nil {
		t.Fatalf("matching header: %v", err)
	}
	if err := Verify(mk(""), []string{"s-fips-padded-secret"}, 0); !errors.Is(err, ErrDeliveryID) {
		t.Fatalf("no header: %v", err)
	}
	if err := Verify(mk("other"), []string{"s-fips-padded-secret"}, 0); !errors.Is(err, ErrDeliveryID) {
		t.Fatalf("replay with a fresh header: %v", err)
	}
	// A body that is not a payload cannot vouch for any header.
	r := signedRequest([]string{"s-fips-padded-secret"}, now, "not json")
	r.Header.Set(HeaderDelivery, "x")
	if err := Verify(r, []string{"s-fips-padded-secret"}, 0); !errors.Is(err, ErrDeliveryID) {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyOnlyLooksAtTheFirstEntries(t *testing.T) {
	now := time.Now()
	body := []byte("b")
	good := Sign([]string{"s-fips-padded-secret"}, now.Unix(), body)
	fake := strings.TrimSpace(strings.Repeat("v1="+strings.Repeat("ab", 32)+" ", maxSignatureEntries))
	if err := VerifyBody(strconv.FormatInt(now.Unix(), 10), fake+" "+good, body, []string{"s-fips-padded-secret"}, 0, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("an entry past the cap was honored: %v", err)
	}
	if err := VerifyBody(strconv.FormatInt(now.Unix(), 10), good+" "+fake, body, []string{"s-fips-padded-secret"}, 0, now); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyTooLargeLeavesBodyReadable(t *testing.T) {
	big := bytes.Repeat([]byte("x"), maxVerifyBody+100)
	r := httptest.NewRequest("POST", "/", bytes.NewReader(big))
	r.Header.Set(HeaderTimestamp, "1")
	r.Header.Set(HeaderSignature, "v1=00")
	if err := Verify(r, []string{"s-fips-padded-secret"}, 0); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r.Body)
	if len(got) != len(big) {
		t.Fatalf("body after ErrBodyTooLarge: %d of %d bytes", len(got), len(big))
	}
}

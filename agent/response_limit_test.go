package agent

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

// A reply of exactly the cap is read whole; one byte more fails with ErrResponseTooLarge after
// exactly the cap has been read.
func TestLimitResponse_CapIsExact(t *testing.T) {
	for _, n := range []int{0, 1, 99, 100} {
		b, err := io.ReadAll(LimitResponse(&closeRecorder{Reader: bytes.NewReader(make([]byte, n))}, 100))
		if err != nil || len(b) != n {
			t.Errorf("%d-byte reply under a 100-byte cap: read %d, err %v; want all of it", n, len(b), err)
		}
	}
	for _, n := range []int{101, 5000} {
		b, err := io.ReadAll(LimitResponse(&closeRecorder{Reader: bytes.NewReader(make([]byte, n))}, 100))
		if !errors.Is(err, ErrResponseTooLarge) || len(b) != 100 {
			t.Errorf("%d-byte reply under a 100-byte cap: read %d, err %v; want 100 and ErrResponseTooLarge", n, len(b), err)
		}
	}
}

// Once the cap is passed, every later read returns no bytes and the error again.
func TestLimitResponse_StaysFailed(t *testing.T) {
	r := LimitResponse(&closeRecorder{Reader: bytes.NewReader(make([]byte, 50))}, 10)
	buf := make([]byte, 64)
	if n, err := r.Read(buf); n != 10 || !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("first read: %d, %v; want 10 and ErrResponseTooLarge", n, err)
	}
	for range 3 {
		if n, err := r.Read(buf); n != 0 || !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("later read: %d, %v; want 0 and ErrResponseTooLarge", n, err)
		}
	}
}

// A cap of zero or less is the default cap, not no cap.
func TestLimitResponse_NonPositiveIsTheDefault(t *testing.T) {
	for _, max := range []int64{0, -1} {
		b, err := io.ReadAll(LimitResponse(&closeRecorder{Reader: bytes.NewReader(make([]byte, DefaultMaxResponseBytes))}, max))
		if err != nil || len(b) != DefaultMaxResponseBytes {
			t.Errorf("max %d: a reply of exactly the default cap read %d, err %v", max, len(b), err)
		}
		_, err = io.ReadAll(LimitResponse(&closeRecorder{Reader: bytes.NewReader(make([]byte, DefaultMaxResponseBytes+1))}, max))
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Errorf("max %d: a reply one byte over the default cap: err %v, want ErrResponseTooLarge", max, err)
		}
	}
}

func TestLimitResponse_ClosesTheBody(t *testing.T) {
	c := &closeRecorder{Reader: strings.NewReader("x")}
	if err := LimitResponse(c, 10).Close(); err != nil || !c.closed {
		t.Fatalf("Close: err %v, body closed %v", err, c.closed)
	}
}

// The scanner never hands out the line the cap cut through: the scan ends with the cap's error.
func TestNewSSEScanner_CutLineIsNotReturned(t *testing.T) {
	body := "data: a\n\ndata: {\"long\":\"bbbbbbbbbbbbbbbbbbbb\"}\n\n"
	sc := NewSSEScanner(LimitResponse(&closeRecorder{Reader: strings.NewReader(body)}, 20))
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 2 || lines[0] != "data: a" || lines[1] != "" {
		t.Errorf("lines = %q, want only the lines before the cut", lines)
	}
	err := SSEReadError("prov", sc.Err())
	if !errors.Is(err, ErrResponseTooLarge) || !strings.HasPrefix(err.Error(), "prov: the reply exceeded 20 bytes") {
		t.Fatalf("err = %v, want the provider's ErrResponseTooLarge", err)
	}
	// Without the cap the same body scans whole.
	sc = NewSSEScanner(strings.NewReader(body))
	n := 0
	for sc.Scan() {
		n++
	}
	if n != 4 || sc.Err() != nil {
		t.Fatalf("uncapped: %d lines, err %v", n, sc.Err())
	}
}

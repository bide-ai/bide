package gcs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestClassify(t *testing.T) {
	api := func(code int) error { return &googleapi.Error{Code: code} }
	for _, tc := range []struct {
		name string
		err  error
		is   error
		not  []error
	}{
		{"precondition failed is ErrExists", api(http.StatusPreconditionFailed), ErrExists, []error{ErrUnavailable}},
		{"404", api(http.StatusNotFound), ErrNoObject, []error{ErrUnavailable}},
		{"object not exist", storage.ErrObjectNotExist, ErrNoObject, nil},
		{"bucket not exist", storage.ErrBucketNotExist, ErrNoObject, nil},
		{"403 is denied", api(http.StatusForbidden), ErrDenied, []error{ErrUnavailable}},
		{"401 is denied", api(http.StatusUnauthorized), ErrDenied, []error{ErrUnavailable}},
		{"429 is unavailable", api(http.StatusTooManyRequests), ErrUnavailable, []error{ErrDenied}},
		{"408 is unavailable", api(http.StatusRequestTimeout), ErrUnavailable, nil},
		{"503 is unavailable", api(http.StatusServiceUnavailable), ErrUnavailable, nil},
		{"500 is unavailable", api(http.StatusInternalServerError), ErrUnavailable, nil},
		{"cancelled", context.Canceled, ErrUnavailable, nil},
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), ErrUnavailable, nil},
		{"no API answer", errors.New("connection reset"), ErrUnavailable, nil},
		{"gRPC permission denied", status.Error(codes.PermissionDenied, "no"), ErrDenied, []error{ErrUnavailable}},
		{"gRPC unauthenticated", status.Error(codes.Unauthenticated, "no"), ErrDenied, []error{ErrUnavailable}},
		{"gRPC not found", status.Error(codes.NotFound, "no"), ErrNoObject, []error{ErrUnavailable}},
		{"gRPC unavailable", status.Error(codes.Unavailable, "down"), ErrUnavailable, []error{ErrDenied}},
		{"gRPC resource exhausted", status.Error(codes.ResourceExhausted, "slow"), ErrUnavailable, nil},
	} {
		got := classify(tc.err)
		if !errors.Is(got, tc.is) {
			t.Errorf("%s: classify = %v; want %v", tc.name, got, tc.is)
		}
		for _, n := range tc.not {
			if errors.Is(got, n) {
				t.Errorf("%s: classify = %v; must not be %v", tc.name, got, n)
			}
		}
		if !errors.Is(got, tc.err) {
			t.Errorf("%s: the original error is not in the chain", tc.name)
		}
	}
	// A 400 (a bad request, such as a CRC32C mismatch) is neither retryable nor a known state: it is returned as is.
	if got := classify(api(http.StatusBadRequest)); errors.Is(got, ErrUnavailable) || errors.Is(got, ErrExists) {
		t.Errorf("a 400 classified as %v", got)
	}
	if classify(nil) != nil {
		t.Error("classify(nil) is not nil")
	}
}

func TestLayout(t *testing.T) {
	a, b := partitionOf("journal", "r"), partitionOf("events", "r")
	if a == b || len(a) != 64 || partitionOf("journal", "r") != a {
		t.Fatal("partitions are not distinct, 64 hex digits and stable")
	}
	if partitionOf("ab", "c") == partitionOf("a", "bc") {
		t.Error("(kind, run) pairs share an encoding")
	}
	if !(invertedSize(9) < invertedSize(2)) {
		t.Error("larger sizes must sort first")
	}
	body := []byte("x")
	name := keyOf("p/", "journal", "r", 7, body)
	part, n, ok := splitKey("p/", name)
	if !ok || part != a {
		t.Fatalf("splitKey(%q) = %q, %q, %v", name, part, n, ok)
	}
	if pn, ok := parseName(n); !ok || pn.size != 7 {
		t.Fatalf("parseName(%q) = %v, %v", n, pn, ok)
	}
	for _, bad := range []string{"", "x.json", invertedSize(1) + "-ABCDEF.json", invertedSize(1) + "-" + string(make([]byte, 32)) + ".json"} {
		if _, ok := parseName(bad); ok {
			t.Errorf("parseName(%q) accepted", bad)
		}
	}
}

func TestSuspectID(t *testing.T) {
	for id, want := range map[string]bool{
		"alice@example.com": true, "x@localhost": true, "acct-123456789012": true, "4111 1111 1111 1111": true,
		"run-2026-10-17-0001": false, "notify:@open:2026-10-17": false, "a@b": false,
	} {
		if got := suspectID(id) != ""; got != want {
			t.Errorf("suspectID(%q) = %v, want %v", id, got, want)
		}
	}
}

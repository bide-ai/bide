package gcs_test

import (
	"context"
	"os"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/bide-ai/bide/anchor/gcs"
)

// TestSDKStoreAgainstAnEmulator runs the SDK adapter against a server at STORAGE_EMULATOR_HOST
// (storage-testbench or fake-gcs-server), for the shape of its requests only: create-only writes, a
// generation read, a listing with versions. Neither emulator is known to enforce retention, locked
// policies or the 412 precondition, so this test checks no guarantee; the guarantees are tested with
// the in-memory fake and, against a real bucket, by TestRealGCS. Skipped unless the variable and
// BIDE_GCS_EMULATOR_BUCKET (an existing bucket on the emulator) are set.
func TestSDKStoreAgainstAnEmulator(t *testing.T) {
	bucket := os.Getenv("BIDE_GCS_EMULATOR_BUCKET")
	if os.Getenv("STORAGE_EMULATOR_HOST") == "" || bucket == "" {
		t.Skip("STORAGE_EMULATOR_HOST and BIDE_GCS_EMULATOR_BUCKET are not set")
	}
	c, err := storage.NewClient(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := gcs.NewStore(c, bucket)
	body := []byte(`{"x":1}`)
	info, err := s.Create(ctx, "probe/a.json", body, gcs.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, _, err := s.Read(ctx, "probe/a.json", info.Generation, 1<<20)
	if err != nil || string(got) != string(body) {
		t.Fatalf("Read = %q, %v", got, err)
	}
	n := 0
	if err := s.List(ctx, "probe/", true, func(gcs.ObjectInfo) bool { n++; return true }); err != nil || n == 0 {
		t.Fatalf("List = %d objects, %v", n, err)
	}
}

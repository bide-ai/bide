package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Diagnostic: insert rate outside and inside a Runs loop.
func TestDiagListerInsertRate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	const n = 1100
	var lat []time.Duration
	t0 := time.Now()
	for i := range n {
		id := fmt.Sprintf("p/%04d", i)
		a := time.Now()
		s.Insert(ctx, id, "x", []byte("x"))
		lat = append(lat, time.Since(a))
	}
	setup := time.Since(t0)
	var inLoop []time.Duration
	t1 := time.Now()
	for id, err := range s.Runs(ctx, agent.RunFilter{Prefix: "p/", ExcludeHolding: []string{"done"}}) {
		if err != nil {
			t.Fatal(err)
		}
		a := time.Now()
		s.Insert(ctx, id, "done", []byte("x"))
		inLoop = append(inLoop, time.Since(a))
	}
	loop := time.Since(t1)
	slices.Sort(lat)
	slices.Sort(inLoop)
	t.Fatalf("DIAG setup: %d inserts in %v (p50 %v max %v); loop: %d inserts in %v (p50 %v max %v)", n, setup, lat[n/2], lat[n-1], len(inLoop), loop, inLoop[len(inLoop)/2], inLoop[len(inLoop)-1])
}

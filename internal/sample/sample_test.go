package sample

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// dump is every entry of every run the sample wrote, in order, as text.
func dump(t *testing.T, s agent.Store, res *Result) string {
	t.Helper()
	var b strings.Builder
	for _, r := range res.Runs {
		for e, err := range s.Load(context.Background(), r.ID, -1) {
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\n", r.ID, e.Name, Unsalted(e.Data))
		}
	}
	return b.String()
}

func write(t *testing.T) (string, *Result) {
	t.Helper()
	s := agent.NewMemStore()
	res, err := Write(context.Background(), s, "")
	if err != nil {
		t.Fatal(err)
	}
	return dump(t, s, res), res
}

// TestDeterministic: two calls write the same entries but for their salts, and the same keys.
func TestDeterministic(t *testing.T) {
	a, ra := write(t)
	b, rb := write(t)
	if a != b {
		la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
		for i := range la {
			if i >= len(lb) || la[i] != lb[i] {
				t.Fatalf("the journals differ at entry %d:\n%s\n%s", i, la[i], lb[min(i, len(lb)-1)])
			}
		}
		t.Fatal("the journals differ")
	}
	// The keys and the run set are the same; the evidence holds salted digests, so it differs.
	ra.Evidence, rb.Evidence = nil, nil
	ja, _ := json.Marshal(ra)
	jb, _ := json.Marshal(rb)
	if string(ja) != string(jb) {
		t.Fatal("the keys or runs differ between calls")
	}
}

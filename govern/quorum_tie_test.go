package govern

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent/agenttest"
)

// A tie for the most votes is not agreement. With four voters and k = 2, two for "approve" and
// two for "deny" each reach k, but neither decision has more support than the other; reporting
// "approve" as agreed only because it sorts first would let an arbitrary label commit.
func TestQuorum_TieIsNotAgreement(t *testing.T) {
	vote := func(name, d string) Voter {
		return Voter{Name: name, Decide: func(context.Context) (string, error) { return d, nil }}
	}
	res, err := Quorum(context.Background(), agenttest.MemJournal(), "r1", "q", 2,
		vote("a", "approve"), vote("b", "approve"), vote("c", "deny"), vote("d", "deny"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Agreed {
		t.Fatalf("a 2-2 split reported agreement on %q (%d of %d)", res.Decision, res.VotesFor, res.Total)
	}
}

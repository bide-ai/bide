package bideaudit_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bide-ai/bide/govern"
)

// The bide-audit CLI mirrors govern's wire types so it imports neither gsm nor govern. govern is a
// separate module, so the check that each mirror matches govern is split at the golden files in
// cmd/bide-audit/testdata/govern-wire. This half proves each golden file is govern's encoding of
// the value below (fully populated, and zero) and that each govern type has one field per key; the
// CLI's TestWireMirrorsMatchGovern proves each mirror reads and writes the same files.
func TestGovernWireMatchesCLIGoldens(t *testing.T) {
	cert, err := govern.ConfluenceCertificate{Machine: "m", PolicyDigest: "d", Converges: true, WFC: true, CC: true,
		MaxRepairLen: 1, PairsTotal: 2, PairsDisjoint: 3, PairsBrute: 4, PairsUndeclared: 6,
		CausalOrderRequired: []govern.EventPair{{First: "a", Second: "b"}}, NotIdempotent: []string{"c"},
		Saturations: []govern.Saturation{{Rule: "r", Var: "v", States: 7}}, States: 5, CompensationFree: true}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	zeroCert, err := govern.ConfluenceCertificate{}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	tally, _ := json.Marshal(govern.QuorumResult{Decision: "x", VotesFor: 1, Total: 2, Agreed: true,
		Votes: []govern.Vote{{Voter: "a", Decision: "x"}}})
	zeroTally, _ := json.Marshal(govern.QuorumResult{})
	vote, _ := json.Marshal(govern.Vote{Voter: "a", Decision: "x"})
	zeroVote, _ := json.Marshal(govern.Vote{})

	for name, c := range map[string]struct {
		full, zero []byte
		typ        reflect.Type
	}{
		"certificate": {cert, zeroCert, reflect.TypeFor[govern.ConfluenceCertificate]()},
		"tally":       {tally, zeroTally, reflect.TypeFor[govern.QuorumResult]()},
		"vote":        {vote, zeroVote, reflect.TypeFor[govern.Vote]()},
	} {
		want := readGolden(t, name+".json")
		if string(c.full) != string(want) {
			t.Errorf("%s: govern encodes %s, the CLI's golden is %s", name, c.full, want)
		}
		if want := readGolden(t, name+"-zero.json"); string(c.zero) != string(want) {
			t.Errorf("%s: govern encodes the zero value as %s, the CLI's golden is %s", name, c.zero, want)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(want, &keys); err != nil {
			t.Fatalf("%s: golden: %v", name, err)
		}
		if n := c.typ.NumField(); n != len(keys) {
			t.Errorf("%s: govern's type has %d fields, the CLI's golden %d keys", name, n, len(keys))
		}
	}
}

// readGolden reads a file of govern's wire encoding from the CLI's testdata.
func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "cmd", "bide-audit", "testdata", "govern-wire", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

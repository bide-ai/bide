package awss3

import (
	"sort"
	"strings"
	"testing"
)

func TestInvertedSizeSortsNewestFirst(t *testing.T) {
	sizes := []int{0, 1, 2, 9, 10, 11, 99, 100, 1 << 20, 1<<31 - 1}
	var names []string
	for _, s := range sizes {
		names = append(names, invertedSize(s))
	}
	sort.Strings(names) // S3's order
	for i, n := range names {
		want := sizes[len(sizes)-1-i]
		got, ok := parseName(n + "-" + strings.Repeat("a", digestHex) + objectSuffix)
		if !ok || got.size != want {
			t.Fatalf("listing position %d holds size %d (ok %v), want %d: sizes must list largest first", i, got.size, ok, want)
		}
		if len(n) != sizeDigits {
			t.Fatalf("inverted size %q is not %d digits", n, sizeDigits)
		}
	}
}

func TestPartitionNamesNeitherKindNorRunAndSeparatesThem(t *testing.T) {
	seen := map[string]string{}
	for _, c := range [][2]string{{"journal", "r"}, {"events", "r"}, {"journal", "r\x00"}, {"journal", "run/with/slash"}, {"journal", "notify:@open:2026-10-17"}, {"journal", ""}, {"journalr", ""}, {"journal", "r "}} {
		p := partitionOf(c[0], c[1])
		if len(p) != 64 || strings.ContainsAny(p, "/:@") {
			t.Fatalf("partition %q is not 64 hex digits", p)
		}
		if prev, dup := seen[p]; dup {
			t.Fatalf("%q and %q share partition %s", prev, c, p)
		}
		seen[p] = c[0] + "|" + c[1]
	}
}

func TestParseNameRejectsEverythingElse(t *testing.T) {
	good := invertedSize(5) + "-" + strings.Repeat("0", digestHex) + objectSuffix
	if _, ok := parseName(good); !ok {
		t.Fatal("a well-formed name was rejected")
	}
	for _, bad := range []string{"", good + "x", strings.ToUpper(good), strings.Replace(good, "-", "_", 1), invertedSize(5)[1:] + "-" + strings.Repeat("0", digestHex) + objectSuffix,
		invertedSize(5) + "-" + strings.Repeat("0", digestHex-1) + objectSuffix, invertedSize(5) + "-" + strings.Repeat("g", digestHex) + objectSuffix,
		"9999999999999999999-" + strings.Repeat("0", digestHex) + objectSuffix, "-" + good} {
		if _, ok := parseName(bad); ok {
			t.Errorf("parseName accepted %q", bad)
		}
	}
}

func TestSplitKey(t *testing.T) {
	p := strings.Repeat("a", 64)
	if got, name, ok := splitKey("bide-anchor/", "bide-anchor/"+p+"/x.json"); !ok || got != p || name != "x.json" {
		t.Errorf("splitKey = %q %q %v", got, name, ok)
	}
	for _, k := range []string{"other/" + p + "/x.json", "bide-anchor/" + p + "x.json", "bide-anchor/short/x.json", "bide-anchor/" + p + "/a/x.json"} {
		if _, _, ok := splitKey("bide-anchor/", k); ok {
			t.Errorf("splitKey accepted %q", k)
		}
	}
}

func TestSuspectID(t *testing.T) {
	for id, want := range map[string]bool{
		"jane@example.com": true, "refund:jane.doe@bank.co.uk": true, "acct-123456789012": true, "4111111111111111": true,
		"jane.doe@openbank.com": true, "jane@openmail.example.org": true, "4111-1111-1111-1111": true, "4111 1111 1111 1111": true,
		"acct:@jane.doe.com": true, "jane.doe@localhost": true, "notify:@open:jane@x.com": true, "refund/jane@x.io/7": true,
		"run-1": false, "notify:@open:2026-10-17": false, "notify:@open:index:2026": false, "2026-10-17T12:00:00Z-0001": false,
		"run-2026-10-17-0001": false, "9b2e7c1a-5d34-4f10-8a6e-0c1d2e3f4a5b": false, "@handle": false, "user@host": false, "wf/refund/7": false,
	} {
		if got := suspectID(id) != ""; got != want {
			t.Errorf("suspectID(%q) = %v, want %v", id, got, want)
		}
	}
}

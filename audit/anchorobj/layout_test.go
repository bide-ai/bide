package anchorobj_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/bide-ai/bide/audit/anchorobj"
)

func TestInvertedSizeSortsNewestFirst(t *testing.T) {
	sizes := []int{0, 1, 2, 9, 10, 11, 99, 100, 1 << 20, 1<<31 - 1}
	var names []string
	for _, s := range sizes {
		names = append(names, anchorobj.InvertedSize(s))
	}
	sort.Strings(names) // the stores' order
	for i, n := range names {
		want := sizes[len(sizes)-1-i]
		got, ok := anchorobj.ParseName(n + "-" + strings.Repeat("a", anchorobj.DigestHex) + anchorobj.ObjectSuffix)
		if !ok || got.Size != want {
			t.Fatalf("listing position %d holds size %d (ok %v), want %d: sizes must list largest first", i, got.Size, ok, want)
		}
		if len(n) != anchorobj.SizeDigits {
			t.Fatalf("inverted size %q is not %d digits", n, anchorobj.SizeDigits)
		}
	}
}

func TestPartitionNamesNeitherKindNorRunAndSeparatesThem(t *testing.T) {
	seen := map[string]string{}
	for _, c := range [][2]string{{"journal", "r"}, {"events", "r"}, {"journal", "r\x00"}, {"journal", "run/with/slash"}, {"journal", "notify:@open:2026-10-17"}, {"journal", ""}, {"journalr", ""}, {"journal", "r "}} {
		p := testLayout.Partition(c[0], c[1])
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
	good := anchorobj.InvertedSize(5) + "-" + strings.Repeat("0", anchorobj.DigestHex) + anchorobj.ObjectSuffix
	if _, ok := anchorobj.ParseName(good); !ok {
		t.Fatal("a well-formed name was rejected")
	}
	for _, bad := range []string{"", good + "x", strings.ToUpper(good), strings.Replace(good, "-", "_", 1), anchorobj.InvertedSize(5)[1:] + "-" + strings.Repeat("0", anchorobj.DigestHex) + anchorobj.ObjectSuffix,
		anchorobj.InvertedSize(5) + "-" + strings.Repeat("0", anchorobj.DigestHex-1) + anchorobj.ObjectSuffix, anchorobj.InvertedSize(5) + "-" + strings.Repeat("g", anchorobj.DigestHex) + anchorobj.ObjectSuffix,
		"9999999999999999999-" + strings.Repeat("0", anchorobj.DigestHex) + anchorobj.ObjectSuffix, "-" + good} {
		if _, ok := anchorobj.ParseName(bad); ok {
			t.Errorf("parseName accepted %q", bad)
		}
	}
}

func TestSplitKey(t *testing.T) {
	p := strings.Repeat("a", 64)
	if got, name, ok := anchorobj.SplitKey("bide-anchor/", "bide-anchor/"+p+"/x.json"); !ok || got != p || name != "x.json" {
		t.Errorf("splitKey = %q %q %v", got, name, ok)
	}
	for _, k := range []string{"other/" + p + "/x.json", "bide-anchor/" + p + "x.json", "bide-anchor/short/x.json", "bide-anchor/" + p + "/a/x.json"} {
		if _, _, ok := anchorobj.SplitKey("bide-anchor/", k); ok {
			t.Errorf("splitKey accepted %q", k)
		}
	}
}

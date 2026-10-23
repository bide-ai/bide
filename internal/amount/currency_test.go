package amount

import (
	"encoding/xml"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestExponent(t *testing.T) {
	for _, tc := range []struct {
		code string
		want int
	}{
		{"USD", 2}, {"EUR", 2}, {"GBP", 2}, {"JPY", 0}, {"KRW", 0}, {"VND", 0}, {"CLP", 0},
		{"KWD", 3}, {"BHD", 3}, {"JOD", 3}, {"OMR", 3}, {"TND", 3}, {"CLF", 4}, {"UYW", 4},
		{"XOF", 0}, {"XAF", 0}, {"XPF", 0},
	} {
		got, err := Exponent(tc.code)
		if err != nil || got != tc.want {
			t.Errorf("Exponent(%q) = %d, %v; want %d", tc.code, got, err, tc.want)
		}
	}
}

func TestExponent_Unknown(t *testing.T) {
	for _, code := range []string{
		"", "usd", "Usd", "US", "USDX", " USD", "USD ", "XYZ", "ZZZ",
		"XXX", "XAU", "XAG", "XDR", "XTS", // in the list with no minor unit
		"ՕSD", "USд", // lookalike letters
	} {
		if got, err := Exponent(code); !errors.Is(err, ErrUnknownCurrency) || got != 0 {
			t.Errorf("Exponent(%q) = %d, %v; want ErrUnknownCurrency", code, got, err)
		}
	}
}

func TestResolve(t *testing.T) {
	ptr := func(n int) *int { return &n }
	for _, tc := range []struct {
		name     string
		code     string
		override *int
		want     int
		err      error
	}{
		{"table", "USD", nil, 2, nil},
		{"table JPY", "JPY", nil, 0, nil},
		{"override states the table's value", "USD", ptr(2), 2, nil},
		{"override a code outside the table", "XYZ", ptr(2), 2, nil},
		{"override zero", "XYZ", ptr(0), 0, nil},
		{"override a no-minor-unit code", "XXX", ptr(3), 3, nil},
		{"override largest", "XYZ", ptr(18), 18, nil},
		{"override contradicts the table", "USD", ptr(3), 0, ErrExponent},
		{"override contradicts JPY", "JPY", ptr(2), 0, ErrExponent},
		{"unknown, no override", "XYZ", nil, 0, ErrUnknownCurrency},
		{"override negative", "XYZ", ptr(-1), 0, ErrExponent},
		{"override too large", "XYZ", ptr(19), 0, ErrExponent},
		{"override with a lower case code", "xyz", ptr(2), 0, ErrUnknownCurrency},
		{"override with a short code", "XY", ptr(2), 0, ErrUnknownCurrency},
		{"override with a long code", "XYZW", ptr(2), 0, ErrUnknownCurrency},
		{"override with digits", "X1Z", ptr(2), 0, ErrUnknownCurrency},
		{"override with a non-ASCII letter", "XYΖ", ptr(2), 0, ErrUnknownCurrency},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.code, tc.override)
			if tc.err == nil {
				if err != nil || got != tc.want {
					t.Fatalf("Resolve(%q) = %d, %v; want %d", tc.code, got, err, tc.want)
				}
				return
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("Resolve(%q) = %d, %v; want an error wrapping %v", tc.code, got, err, tc.err)
			}
		})
	}
}

// published is the shape of SIX's list one that the test reads.
type published struct {
	Pblshd  string `xml:"Pblshd,attr"`
	Entries []struct {
		Ccy  string `xml:"Ccy"`
		Mnr  string `xml:"CcyMnrUnts"`
		Name string `xml:"CtryNm"`
	} `xml:"CcyTbl>CcyNtry"`
}

// TestExponentTable_MatchesPublishedList is decision 9's "a test against the published list": the
// table must equal, code for code and exponent for exponent, the list SIX publishes (a dated copy
// in testdata; see iso4217.go for where it came from and how to refresh it).
func TestExponentTable_MatchesPublishedList(t *testing.T) {
	data, err := os.ReadFile("testdata/iso4217-list-one.xml")
	if err != nil {
		t.Fatal(err)
	}
	var list published
	if err := xml.Unmarshal(data, &list); err != nil {
		t.Fatalf("parse the published list: %v", err)
	}
	if list.Pblshd == "" || len(list.Entries) < 200 {
		t.Fatalf("the published list looks truncated: published %q, %d entries", list.Pblshd, len(list.Entries))
	}
	want := map[string]int{}
	noMinor := map[string]bool{}
	for _, e := range list.Entries {
		if e.Ccy == "" { // a territory with no universal currency
			continue
		}
		if e.Mnr == "N.A." {
			noMinor[e.Ccy] = true
			continue
		}
		n := int(e.Mnr[0] - '0')
		if len(e.Mnr) != 1 || n > 9 {
			t.Fatalf("%s: minor units %q", e.Ccy, e.Mnr)
		}
		if prev, seen := want[e.Ccy]; seen && prev != n {
			t.Fatalf("%s is listed with exponents %d and %d", e.Ccy, prev, n)
		}
		want[e.Ccy] = n
	}
	var diffs []string
	for code, n := range want {
		got, ok := exponents[code]
		switch {
		case !ok:
			diffs = append(diffs, code+": in the published list ("+list.Pblshd+") with exponent "+string(rune('0'+n))+", missing from the table")
		case got != n:
			diffs = append(diffs, code+": the table says "+string(rune('0'+got))+", the published list says "+string(rune('0'+n)))
		}
	}
	for code := range exponents {
		if _, ok := want[code]; !ok {
			diffs = append(diffs, code+": in the table, not in the published list")
		}
		if noMinor[code] {
			diffs = append(diffs, code+": in the table, but the list gives it no minor unit")
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Fatalf("the exponent table differs from the published list:\n%s", strings.Join(diffs, "\n"))
	}
	if len(noMinor) == 0 {
		t.Fatal("the published list has no N.A. codes; the test no longer reads the file it thinks it does")
	}
	for code, n := range exponents {
		if len(code) != 3 || code != strings.ToUpper(code) || n < 0 || n > 4 {
			t.Errorf("table entry %q: %d is not a three-letter upper-case code with an exponent of 0 to 4", code, n)
		}
	}
}

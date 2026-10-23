package amount

import (
	"errors"
	"strings"
	"testing"
)

func TestLookup(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		path []string
		want string
	}{
		{"top level string", `{"amount":"12.50"}`, []string{"amount"}, `"12.50"`},
		{"top level number kept verbatim", `{"amount":12.50}`, []string{"amount"}, `12.50`},
		{"a number literal is not rewritten", `{"amount":1.0e2}`, []string{"amount"}, `1.0e2`},
		{"nested", `{"order":{"total":5}}`, []string{"order", "total"}, `5`},
		{"deeper", `{"a":{"b":{"c":"1"}}}`, []string{"a", "b", "c"}, `"1"`},
		{"among others", `{"x":1,"amount":7,"y":[1,2]}`, []string{"amount"}, `7`},
		{"null is returned, not hidden", `{"amount":null}`, []string{"amount"}, `null`},
		{"whitespace around", " {\n \"amount\" : 7 \n} ", []string{"amount"}, `7`},
		{"same name at different depths", `{"amount":1,"o":{"amount":2}}`, []string{"o", "amount"}, `2`},
		{"object value", `{"amount":{"v":1}}`, []string{"amount"}, `{"v":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Lookup([]byte(tc.args), tc.path)
			if err != nil || string(got) != tc.want {
				t.Fatalf("Lookup(%s, %v) = %s, %v; want %s", tc.args, tc.path, got, err, tc.want)
			}
		})
	}
}

func TestLookup_NotAssessable(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 200) + `1` + strings.Repeat(`}`, 200)
	// Deeper than Lookup walks (maxArgsDepth) but within strictjson's own limit, so only
	// Lookup's bound refuses it.
	mid := strings.Repeat(`{"a":`, maxArgsDepth+8) + `1` + strings.Repeat(`}`, maxArgsDepth+8)
	for _, tc := range []struct {
		name string
		args string
		path []string
		want error
	}{
		// Duplicate names: encoding/json keeps the last, a server may keep the first.
		{"duplicate top level", `{"amount":1,"amount":2}`, []string{"amount"}, ErrArgs},
		{"duplicate with equal values", `{"amount":1,"amount":1}`, []string{"amount"}, ErrArgs},
		{"duplicate of another member", `{"amount":1,"note":"a","note":"b"}`, []string{"amount"}, ErrArgs},
		{"duplicate nested on the path", `{"o":{"total":1,"total":2}}`, []string{"o", "total"}, ErrArgs},
		{"duplicate nested off the path", `{"amount":1,"meta":{"k":1,"k":2}}`, []string{"amount"}, ErrArgs},
		{"duplicate inside an array", `{"amount":1,"l":[{"k":1,"k":2}]}`, []string{"amount"}, ErrArgs},
		{"duplicate through an escape", `{"amount":1,"amount":2}`, []string{"amount"}, ErrArgs},
		// Case variants: a server that matches names case-insensitively reads the other one.
		{"case variant", `{"amount":1,"Amount":2}`, []string{"amount"}, ErrArgs},
		{"case variant listed first", `{"AMOUNT":2,"amount":1}`, []string{"amount"}, ErrArgs},
		{"free-form nested map unrelated to the amount, case variants", `{"amount":1,"metadata":{"labels":{"Env":"a","env":"b"}}}`, []string{"amount"}, ErrArgs},
		{"free-form nested map unrelated to the amount, duplicates", `{"amount":1,"metadata":{"labels":{"env":"a","env":"b"}}}`, []string{"amount"}, ErrArgs},
		{"case variants of an unrelated member", `{"amount":1,"note":"a","Note":"b"}`, []string{"amount"}, ErrArgs},
		{"case variant nested on the path", `{"o":{"total":1,"Total":2}}`, []string{"o", "total"}, ErrArgs},
		{"case variant of the object on the path", `{"o":{"total":1},"O":{"total":2}}`, []string{"o", "total"}, ErrArgs},
		{"case variant nested off the path", `{"amount":1,"meta":{"k":1,"K":2}}`, []string{"amount"}, ErrArgs},
		{"case variant inside an array", `{"amount":1,"l":[{"k":1,"K":2}]}`, []string{"amount"}, ErrArgs},
		{"case variant by Kelvin sign", `{"amount":1,"k":1,"K":2}`, []string{"amount"}, ErrArgs},
		{"case variant through an escape", `{"amount":1,"Amount":2,"Amount":3}`, []string{"amount"}, ErrArgs},
		// The document itself.
		{"invalid utf-8", "{\"amount\":\"\xff\"}", []string{"amount"}, ErrArgs},
		{"invalid utf-8 in a name", "{\"am\xffount\":1}", []string{"amount"}, ErrArgs},
		{"lone surrogate", `{"amount":"\ud800"}`, []string{"amount"}, ErrArgs},
		{"trailing data", `{"amount":1} {"amount":2}`, []string{"amount"}, ErrArgs},
		{"not json", `amount=1`, []string{"amount"}, ErrArgs},
		{"truncated", `{"amount":1`, []string{"amount"}, ErrArgs},
		{"array", `[{"amount":1}]`, []string{"amount"}, ErrArgs},
		{"scalar", `5`, []string{"amount"}, ErrArgs},
		{"too deep", deep, []string{"a"}, ErrArgs},
		{"deeper than a tool's arguments go", mid, []string{"a"}, ErrArgs},
		// Absent.
		{"empty arguments are the empty object", ``, []string{"amount"}, ErrAbsent},
		{"blank arguments", " \n", []string{"amount"}, ErrAbsent},
		{"empty object", `{}`, []string{"amount"}, ErrAbsent},
		{"other member", `{"total":1}`, []string{"amount"}, ErrAbsent},
		{"a lone case variant is not the member", `{"Amount":1}`, []string{"amount"}, ErrAbsent},
		{"through a scalar", `{"o":5}`, []string{"o", "total"}, ErrAbsent},
		{"through null", `{"o":null}`, []string{"o", "total"}, ErrAbsent},
		{"through an array", `{"o":[{"total":1}]}`, []string{"o", "total"}, ErrAbsent},
		{"empty path", `{"amount":1}`, nil, ErrAbsent},
		{"empty segment", `{"":1}`, []string{""}, ErrAbsent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Lookup([]byte(tc.args), tc.path)
			if err == nil {
				t.Fatalf("Lookup(%.60s, %v) = %s; want an error wrapping %v", tc.args, tc.path, got, tc.want)
			}
			if !errors.Is(err, tc.want) || !errors.Is(err, ErrNotAssessable) {
				t.Fatalf("Lookup(%.60s, %v): %v; want an error wrapping %v and ErrNotAssessable", tc.args, tc.path, err, tc.want)
			}
			if got != nil {
				t.Fatalf("Lookup returned %s with an error", got)
			}
			if len(err.Error()) > 300 {
				t.Fatalf("the error is %d bytes; it must not echo the arguments", len(err.Error()))
			}
		})
	}
}

// TestLookup_ThenAssess is the path a cap takes: strict arguments, then the amount.
func TestLookup_ThenAssess(t *testing.T) {
	raw, err := Lookup([]byte(`{"amount":"12.50","currency":"USD"}`), []string{"amount"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Assess(raw, Major, 2); err != nil || got != 1250 {
		t.Fatalf("Assess = %d, %v; want 1250", got, err)
	}
}

package amount

import (
	"bufio"
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestParseRule_Accepts(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want Rule
	}{
		{
			"per call, two tools, currency member",
			`refund_per_call:  refund|refund_large.amount[minor, currency=currency] > 500 USD -> gate`,
			Rule{
				Name: "refund_per_call", Tools: []string{"refund", "refund_large"}, Arg: []string{"amount"},
				Unit: Minor, CurrencyArg: []string{"currency"}, Currency: "USD", Exponent: 2, Limit: 50000,
				Kind: PerCall, Action: Action{Kind: Gate},
			},
		},
		{
			"cumulative per day by key",
			`refund_per_day:   refund|refund_large.amount[minor, currency=currency] sum 5000 USD per day by customer_id -> refuse`,
			Rule{
				Name: "refund_per_day", Tools: []string{"refund", "refund_large"}, Arg: []string{"amount"},
				Unit: Minor, CurrencyArg: []string{"currency"}, Currency: "USD", Exponent: 2, Limit: 500000,
				Kind: Sum, Window: Day, Key: &Key{Path: []string{"customer_id"}}, Action: Action{Kind: Refuse},
			},
		},
		{
			"cumulative with no key",
			`refund_total:     refund|refund_large.amount[minor, currency=currency] sum 50000 USD per day -> refuse`,
			Rule{
				Name: "refund_total", Tools: []string{"refund", "refund_large"}, Arg: []string{"amount"},
				Unit: Minor, CurrencyArg: []string{"currency"}, Currency: "USD", Exponent: 2, Limit: 5000000,
				Kind: Sum, Window: Day, Action: Action{Kind: Refuse},
			},
		},
		{
			"major units, nested path, fixed currency, fractional limit",
			`big: pay.order.total[major] > 12.50 EUR -> refuse`,
			Rule{
				Name: "big", Tools: []string{"pay"}, Arg: []string{"order", "total"}, Unit: Major,
				Currency: "EUR", Exponent: 2, Limit: 1250, Kind: PerCall, Action: Action{Kind: Refuse},
			},
		},
		{
			"JPY has no fraction",
			`yen: pay.amount[minor] > 5000 JPY -> gate`,
			Rule{
				Name: "yen", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor,
				Currency: "JPY", Exponent: 0, Limit: 5000, Kind: PerCall, Action: Action{Kind: Gate},
			},
		},
		{
			"KWD has three",
			`dinar: pay.amount[major] > 0.5 KWD -> gate`,
			Rule{
				Name: "dinar", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Major,
				Currency: "KWD", Exponent: 3, Limit: 500, Kind: PerCall, Action: Action{Kind: Gate},
			},
		},
		{
			"named gate",
			`named: pay.amount[minor] > 5 USD -> gate(finance_pair)`,
			Rule{
				Name: "named", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor,
				Currency: "USD", Exponent: 2, Limit: 500, Kind: PerCall, Action: Action{Kind: Gate, GateName: "finance_pair"},
			},
		},
		{
			"by principal, per hour",
			`p: pay.amount[minor, currency=cur] sum 10 KWD per hour by principal -> refuse`,
			Rule{
				Name: "p", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor, CurrencyArg: []string{"cur"},
				Currency: "KWD", Exponent: 3, Limit: 10000, Kind: Sum, Window: Hour,
				Key: &Key{Principal: true}, Action: Action{Kind: Refuse},
			},
		},
		{
			"per run, normalised nested key",
			`r: pay.amount[minor] sum 1 USD per run by customer.id lower trim -> gate`,
			Rule{
				Name: "r", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor, Currency: "USD", Exponent: 2,
				Limit: 100, Kind: Sum, Window: Run, Key: &Key{Path: []string{"customer", "id"}, Lower: true, Trim: true},
				Action: Action{Kind: Gate},
			},
		},
		{
			"trim then lower, either order",
			`r: pay.amount[minor] sum 1 USD per run by k trim lower -> gate`,
			Rule{
				Name: "r", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor, Currency: "USD", Exponent: 2,
				Limit: 100, Kind: Sum, Window: Run, Key: &Key{Path: []string{"k"}, Lower: true, Trim: true},
				Action: Action{Kind: Gate},
			},
		},
		{
			"exponent override for a code outside the table",
			`x: pay.amount[minor, exponent=2] > 5 XYZ -> gate`,
			Rule{
				Name: "x", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor,
				Currency: "XYZ", Exponent: 2, Limit: 500, Kind: PerCall, Action: Action{Kind: Gate},
			},
		},
		{
			"options in any order, exponent restating the table",
			`x: pay.amount[minor, exponent=2, currency=c] > 5 USD -> gate`,
			Rule{
				Name: "x", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor, CurrencyArg: []string{"c"},
				Currency: "USD", Exponent: 2, Limit: 500, Kind: PerCall, Action: Action{Kind: Gate},
			},
		},
		{
			"no space where none is needed, tabs where there are",
			"x:pay.amount[minor]\t>5 USD->refuse",
			Rule{
				Name: "x", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor,
				Currency: "USD", Exponent: 2, Limit: 500, Kind: PerCall, Action: Action{Kind: Refuse},
			},
		},
		{
			"zero limit",
			`z: pay.amount[minor] > 0 USD -> gate`,
			Rule{
				Name: "z", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor,
				Currency: "USD", Exponent: 2, Limit: 0, Kind: PerCall, Action: Action{Kind: Gate},
			},
		},
		{
			"cumulative gate",
			`s: pay.amount[minor] sum 9.99 USD per day -> gate(g1)`,
			Rule{
				Name: "s", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor,
				Currency: "USD", Exponent: 2, Limit: 999, Kind: Sum, Window: Day, Action: Action{Kind: Gate, GateName: "g1"},
			},
		},
		{
			"largest limit",
			`m: pay.amount[minor] > 92233720368547758.07 USD -> refuse`,
			Rule{
				Name: "m", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor,
				Currency: "USD", Exponent: 2, Limit: 9223372036854775807, Kind: PerCall, Action: Action{Kind: Refuse},
			},
		},
		{
			"quoted tool and member names with dots",
			`q: "stripe.refund"|refund."amount.v2"[minor] > 5 USD -> gate`,
			Rule{
				Name: "q", Tools: []string{"stripe.refund", "refund"}, Arg: []string{"amount.v2"}, Unit: Minor,
				Currency: "USD", Exponent: 2, Limit: 500, Kind: PerCall, Action: Action{Kind: Gate},
			},
		},
		{
			"a quoted plain name is the plain name",
			`q: "pay".order."total"[minor] > 5 USD -> gate`,
			Rule{
				Name: "q", Tools: []string{"pay"}, Arg: []string{"order", "total"}, Unit: Minor,
				Currency: "USD", Exponent: 2, Limit: 500, Kind: PerCall, Action: Action{Kind: Gate},
			},
		},
		{
			"quoted segments in a nested path, the currency path and the key",
			`q: "mcp.pay".o."x.y".z[minor, currency="cur.code"] sum 5 USD per day by "customer.id".k lower -> refuse`,
			Rule{
				Name: "q", Tools: []string{"mcp.pay"}, Arg: []string{"o", "x.y", "z"}, Unit: Minor,
				CurrencyArg: []string{"cur.code"}, Currency: "USD", Exponent: 2, Limit: 500, Kind: Sum, Window: Day,
				Key: &Key{Path: []string{"customer.id", "k"}, Lower: true}, Action: Action{Kind: Refuse},
			},
		},
		{
			"escapes in a member name",
			`q: pay."a\"b\\c\u00e9"[minor] > 5 USD -> gate`,
			Rule{
				Name: "q", Tools: []string{"pay"}, Arg: []string{"a\"b\\c\u00e9"}, Unit: Minor,
				Currency: "USD", Exponent: 2, Limit: 500, Kind: PerCall, Action: Action{Kind: Gate},
			},
		},
		{
			"a quoted principal is a member, not the run's principal",
			`q: pay.amount[minor] sum 5 USD per day by "principal" -> refuse`,
			Rule{
				Name: "q", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor, Currency: "USD", Exponent: 2,
				Limit: 500, Kind: Sum, Window: Day, Key: &Key{Path: []string{"principal"}}, Action: Action{Kind: Refuse},
			},
		},
		{
			"a plain principal is still the principal",
			`q: pay.amount[minor] sum 5 USD per day by principal -> refuse`,
			Rule{
				Name: "q", Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor, Currency: "USD", Exponent: 2,
				Limit: 500, Kind: Sum, Window: Day, Key: &Key{Principal: true}, Action: Action{Kind: Refuse},
			},
		},
		{
			"a 128 byte tool name",
			`q: ` + strings.Repeat("t", 128) + `.amount[minor] > 5 USD -> gate`,
			Rule{
				Name: "q", Tools: []string{strings.Repeat("t", 128)}, Arg: []string{"amount"}, Unit: Minor,
				Currency: "USD", Exponent: 2, Limit: 500, Kind: PerCall, Action: Action{Kind: Gate},
			},
		},
		{
			"a 128 byte quoted tool name",
			`q: "` + strings.Repeat("u.", 64) + `".amount[minor] > 5 USD -> gate`,
			Rule{
				Name: "q", Tools: []string{strings.Repeat("u.", 64)}, Arg: []string{"amount"}, Unit: Minor,
				Currency: "USD", Exponent: 2, Limit: 500, Kind: PerCall, Action: Action{Kind: Gate},
			},
		},
		{
			"longest name",
			strings.Repeat("a", 64) + `: pay.amount[minor] > 1 USD -> gate`,
			Rule{
				Name: strings.Repeat("a", 64), Tools: []string{"pay"}, Arg: []string{"amount"}, Unit: Minor,
				Currency: "USD", Exponent: 2, Limit: 100, Kind: PerCall, Action: Action{Kind: Gate},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRule(tc.line)
			if err != nil {
				t.Fatalf("ParseRule(%q): %v", tc.line, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseRule(%q):\n got %+v (key %+v)\nwant %+v (key %+v)", tc.line, got, got.Key, tc.want, tc.want.Key)
			}
		})
	}
}

func TestParseRule_Refuses(t *testing.T) {
	const ok = `r: pay.amount[minor] > 5 USD -> gate`
	if _, err := ParseRule(ok); err != nil { // the base the cases below each break one way
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		line string
		in   string // a fragment the error must name, so a refusal for the wrong reason fails
	}{
		{"empty", ``, "empty"},
		{"blank", `   `, "empty"},
		{"no colon", `r pay.amount[minor] > 5 USD -> gate`, "':'"},
		{"empty name", `: pay.amount[minor] > 5 USD -> gate`, "name"},
		{"name with a space", `my rule: pay.amount[minor] > 5 USD -> gate`, "':'"},
		{"name starting with a digit", `1r: pay.amount[minor] > 5 USD -> gate`, "name"},
		{"name with an at sign", `@cap/r: pay.amount[minor] > 5 USD -> gate`, "name"},
		{"name with a hash", `r#1: pay.amount[minor] > 5 USD -> gate`, "name"},
		{"name with a colon", `r:s: pay.amount[minor] > 5 USD -> gate`, "tool"},
		{"name with a non-ASCII letter", "ré: pay.amount[minor] > 5 USD -> gate", "ASCII"},
		{"name too long", strings.Repeat("a", 65) + `: pay.amount[minor] > 5 USD -> gate`, "name"},
		{"no tool", `r: .amount[minor] > 5 USD -> gate`, "tool"},
		{"empty tool in a set", `r: a||b.amount[minor] > 5 USD -> gate`, "tool"},
		{"trailing bar", `r: a|.amount[minor] > 5 USD -> gate`, "tool"},
		{"duplicate tool", `r: a|a.amount[minor] > 5 USD -> gate`, "twice"},
		{"no argument", `r: pay[minor] > 5 USD -> gate`, "'.'"},
		{"empty path segment", `r: pay.a..b[minor] > 5 USD -> gate`, "argument"},
		{"trailing dot in path", `r: pay.a.[minor] > 5 USD -> gate`, "argument"},
		{"no brackets", `r: pay.amount > 5 USD -> gate`, "'['"},
		{"unclosed bracket", `r: pay.amount[minor > 5 USD -> gate`, "']'"},
		{"no unit", `r: pay.amount[currency=c] > 5 USD -> gate`, "minor"},
		{"empty bracket", `r: pay.amount[] > 5 USD -> gate`, "minor"},
		{"unknown unit", `r: pay.amount[cents] > 5 USD -> gate`, "minor"},
		{"upper case unit", `r: pay.amount[Minor] > 5 USD -> gate`, "minor"},
		{"unit twice", `r: pay.amount[minor, major] > 5 USD -> gate`, "option"},
		{"unknown option", `r: pay.amount[minor, scale=2] > 5 USD -> gate`, "scale"},
		{"currency twice", `r: pay.amount[minor, currency=a, currency=b] > 5 USD -> gate`, "twice"},
		{"exponent twice", `r: pay.amount[minor, exponent=2, exponent=2] > 5 XYZ -> gate`, "twice"},
		{"currency with no path", `r: pay.amount[minor, currency=] > 5 USD -> gate`, "currency"},
		{"currency with no equals", `r: pay.amount[minor, currency] > 5 USD -> gate`, "'='"},
		{"trailing comma", `r: pay.amount[minor,] > 5 USD -> gate`, "option"},
		{"exponent not a number", `r: pay.amount[minor, exponent=x] > 5 XYZ -> gate`, "exponent"},
		{"exponent negative", `r: pay.amount[minor, exponent=-1] > 5 XYZ -> gate`, "exponent"},
		{"exponent too large", `r: pay.amount[minor, exponent=19] > 5 XYZ -> gate`, "exponent"},
		{"exponent with a leading zero", `r: pay.amount[minor, exponent=02] > 5 XYZ -> gate`, "exponent"},
		{"exponent contradicts the table", `r: pay.amount[minor, exponent=3] > 5 USD -> gate`, "exponent"},
		{"unknown currency", `r: pay.amount[minor] > 5 XYZ -> gate`, "XYZ"},
		{"no minor unit currency", `r: pay.amount[minor] > 5 XAU -> gate`, "XAU"},
		{"lower case currency", `r: pay.amount[minor] > 5 usd -> gate`, "currency"},
		{"two letter currency", `r: pay.amount[minor] > 5 US -> gate`, "currency"},
		{"no currency", `r: pay.amount[minor] > 5 -> gate`, "currency"},
		{"no limit", `r: pay.amount[minor] > USD -> gate`, "amount"},
		{"no operator", `r: pay.amount[minor] 5 USD -> gate`, "'>'"},
		{"wrong operator", `r: pay.amount[minor] >= 5 USD -> gate`, "amount"},
		{"less than", `r: pay.amount[minor] < 5 USD -> gate`, "'>'"},
		{"negative limit", `r: pay.amount[minor] > -5 USD -> gate`, "amount"},
		{"plus limit", `r: pay.amount[minor] > +5 USD -> gate`, "amount"},
		{"exponent limit", `r: pay.amount[minor] > 1e3 USD -> gate`, "currency"},
		{"hex limit", `r: pay.amount[minor] > 0x10 USD -> gate`, "currency"},
		{"too many fraction digits in the limit", `r: pay.amount[minor] > 5.005 USD -> gate`, "fraction"},
		{"fraction in the limit of JPY", `r: pay.amount[minor] > 5.0 JPY -> gate`, "fraction"},
		{"leading zero in the limit", `r: pay.amount[minor] > 05 USD -> gate`, "amount"},
		{"limit overflows", `r: pay.amount[minor] > 92233720368547758.08 USD -> gate`, "overflow"},
		{"limit far overflows", `r: pay.amount[minor] > 99999999999999999999999 USD -> gate`, "overflow"},
		{"no arrow", `r: pay.amount[minor] > 5 USD gate`, "'->'"},
		{"single dash arrow", `r: pay.amount[minor] > 5 USD - gate`, "'->'"},
		{"no action", `r: pay.amount[minor] > 5 USD ->`, "action"},
		{"unknown action", `r: pay.amount[minor] > 5 USD -> allow`, "allow"},
		{"upper case action", `r: pay.amount[minor] > 5 USD -> Gate`, "Gate"},
		{"refuse takes no gate", `r: pay.amount[minor] > 5 USD -> refuse(g)`, "refuse"},
		{"empty gate name", `r: pay.amount[minor] > 5 USD -> gate()`, "gate name"},
		{"bad gate name", `r: pay.amount[minor] > 5 USD -> gate(a b)`, "')'"},
		{"unclosed gate name", `r: pay.amount[minor] > 5 USD -> gate(a`, "')'"},
		{"trailing text", `r: pay.amount[minor] > 5 USD -> gate now`, "trailing"},
		{"a second rule on the line", `r: pay.amount[minor] > 5 USD -> gate; s: x.y[minor] > 1 USD -> gate`, "trailing"},
		{"sum without per", `r: pay.amount[minor] sum 5 USD -> gate`, "'per'"},
		{"sum runs into the amount", `r: pay.amount[minor] sum5 USD per day -> gate`, "'>' or 'sum'"},
		{"per runs into the window", `r: pay.amount[minor] sum 5 USD perday -> gate`, "'per'"},
		{"by runs into the key", `r: pay.amount[minor] sum 5 USD per day byk -> gate`, "'->'"},
		{"unknown window", `r: pay.amount[minor] sum 5 USD per week -> gate`, "window"},
		{"upper case window", `r: pay.amount[minor] sum 5 USD per Day -> gate`, "window"},
		{"window on a per-call rule", `r: pay.amount[minor] > 5 USD per day -> gate`, "'->'"},
		{"by without a key", `r: pay.amount[minor] sum 5 USD per day by -> gate`, "key"},
		{"by on a per-call rule", `r: pay.amount[minor] > 5 USD by k -> gate`, "'->'"},
		{"key with an empty segment", `r: pay.amount[minor] sum 5 USD per day by a..b -> gate`, "key"},
		{"unknown normalisation", `r: pay.amount[minor] sum 5 USD per day by k upper -> gate`, "upper"},
		{"normalisation twice", `r: pay.amount[minor] sum 5 USD per day by k lower lower -> gate`, "twice"},
		{"normalisation of the principal", `r: pay.amount[minor] sum 5 USD per day by principal lower -> gate`, "principal"},
		{"two keys", `r: pay.amount[minor] sum 5 USD per day by a by b -> gate`, "'->'"},
		{"newline", "r: pay.amount[minor] > 5 USD\n-> gate", "line"},
		{"carriage return", "r: pay.amount[minor] > 5 USD -> gate\r", "line"},
		{"NUL", "r: pay.amount[minor] > 5 USD -> gate\x00", "ASCII"},
		{"non-ASCII space", "r: pay.amount[minor] > 5 USD -> gate", "ASCII"},
		{"invalid utf-8", "r: pay.amount[minor] > 5 USD -> gate\xff", "ASCII"},
		{"line too long", `r: pay.amount[minor] > 5 USD -> gate(` + strings.Repeat("g", 1100) + `)`, "long"},
		{"unterminated quote", `q: "a.amount[minor] > 5 USD -> gate`, "unterminated"},
		{"unterminated quote ending in a backslash", `q: pay."a\`, "unterminated"},
		{"empty quoted tool", `q: "".amount[minor] > 5 USD -> gate`, "quoted tool"},
		{"empty quoted member", `q: pay.""[minor] > 5 USD -> gate`, "quoted member"},
		{"empty quoted key member", `q: pay.amount[minor] sum 5 USD per day by "" -> gate`, "quoted member"},
		{"space in a quoted tool", `q: "a b".amount[minor] > 5 USD -> gate`, "quoted tool"},
		{"escape in a quoted tool", `q: "a\u002eb".amount[minor] > 5 USD -> gate`, "quoted tool"},
		{"slash in a quoted tool", `q: "a/b".amount[minor] > 5 USD -> gate`, "quoted tool"},
		{"quoted tool too long", `q: "` + strings.Repeat("a", 129) + `".amount[minor] > 5 USD -> gate`, "quoted tool"},
		{"plain tool too long", `q: ` + strings.Repeat("a", 129) + `.amount[minor] > 5 USD -> gate`, "at most"},
		{"bad escape in a member", `q: pay."a\x"[minor] > 5 USD -> gate`, "quoted member"},
		{"lone surrogate in a member", `q: pay."\ud800"[minor] > 5 USD -> gate`, "quoted member"},
		{"raw tab in a member", "q: pay.\"a\tb\"[minor] > 5 USD -> gate", "quoted member"},
		{"raw non-ASCII in a member", "q: pay.\"\u00e9\"[minor] > 5 USD -> gate", "ASCII"},
		{"text after a quoted tool", `q: "a"b.amount[minor] > 5 USD -> gate`, "'|' or '.'"},
		{"quoted and plain tool are the same tool", `q: "a"|a.amount[minor] > 5 USD -> gate`, "twice"},
		{"two quoted tools the same", `q: "a.b"|"a.b".amount[minor] > 5 USD -> gate`, "twice"},
		{"quoted rule name", `"q": pay.amount[minor] > 5 USD -> gate`, "name"},
		{"quoted gate name", `q: pay.amount[minor] > 5 USD -> gate("g")`, "gate name"},
		{"comment", `# r: pay.amount[minor] > 5 USD -> gate`, "name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRule(tc.line)
			if err == nil {
				t.Fatalf("ParseRule(%.80q) = %+v; want an error", tc.line, got)
			}
			if !errors.Is(err, ErrRule) {
				t.Fatalf("ParseRule(%.80q): %v does not wrap ErrRule", tc.line, err)
			}
			if !strings.Contains(err.Error(), tc.in) {
				t.Fatalf("ParseRule(%.80q): %q does not mention %q", tc.line, err, tc.in)
			}
			if len(err.Error()) > 300 {
				t.Fatalf("the error is %d bytes; it must not echo a long line", len(err.Error()))
			}
		})
	}
}

func TestParseRules(t *testing.T) {
	rules, err := ParseRules(
		`a: pay.amount[minor] > 5 USD -> gate`,
		`b: pay.amount[minor] sum 50 USD per day -> refuse`,
	)
	if err != nil || len(rules) != 2 || rules[0].Name != "a" || rules[1].Name != "b" {
		t.Fatalf("ParseRules = %+v, %v", rules, err)
	}
	if _, err := ParseRules(`a: pay.amount[minor] > 5 USD -> gate`, `a: pay.amount[minor] > 6 USD -> gate`); !errors.Is(err, ErrRule) || !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("a repeated name: %v; want ErrRule naming it", err)
	}
	// The name is the ledger key, so one differing only in case is a different ledger, and a
	// mix-up is likely enough to refuse.
	if _, err := ParseRules(`a: pay.amount[minor] > 5 USD -> gate`, `A: pay.amount[minor] > 6 USD -> gate`); !errors.Is(err, ErrRule) {
		t.Fatalf("names differing only in case: %v; want ErrRule", err)
	}
	_, err = ParseRules(`a: pay.amount[minor] > 5 USD -> gate`, `b: pay.amount[minor] > 5 USD -> allow`)
	if !errors.Is(err, ErrRule) || !strings.Contains(err.Error(), "rule 2") {
		t.Fatalf("a bad second rule: %v; want ErrRule naming rule 2", err)
	}
	if rules, err := ParseRules(); err != nil || len(rules) != 0 {
		t.Fatalf("ParseRules() = %+v, %v; want none and no error", rules, err)
	}
}

// exampleLine finds a cap rule in a document: it has a bracketed unit and an arrow. A line the
// grammar stops accepting, or a document that drifts from it, fails TestDocGrammar.
var exampleLine = regexp.MustCompile(`^\s*[A-Za-z][A-Za-z0-9_-]*:\s.*\[(minor|major)[],].*->`)

// TestDocGrammar parses every example rule in the documents that state the grammar, so the
// grammar in the docs is the grammar the parser accepts, and a syntax change shows up here first.
func TestDocGrammar(t *testing.T) {
	for _, doc := range []struct {
		path string
		min  int  // the rules the file must hold, so a rewrite that drops them is noticed
		need bool // a file that must exist
	}{
		{"../../docs/design/untrusted-content-and-amount-caps.md", 4, true},
		// The guide arrives with the engine wiring (PR 6); its examples are checked from then on.
		{"../../docs/guides/untrusted-content.md", 0, false},
		{"../../docs/guides/amount-caps.md", 0, false},
	} {
		f, err := os.Open(doc.path)
		if err != nil {
			if !doc.need && errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Fatal(err)
		}
		n, inFence := 0, false
		sc := bufio.NewScanner(f)
		for ln := 1; sc.Scan(); ln++ {
			line := sc.Text()
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inFence = !inFence
				continue
			}
			if !inFence || !exampleLine.MatchString(line) {
				continue
			}
			n++
			if _, err := ParseRule(line); err != nil {
				t.Errorf("%s:%d: the documented rule does not parse: %v\n\t%s", doc.path, ln, err, line)
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		if n < doc.min {
			t.Errorf("%s holds %d example rules, want at least %d: has the document or the line pattern changed?", doc.path, n, doc.min)
		}
	}
}

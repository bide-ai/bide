package theme_test

import (
	"fmt"

	"github.com/bide-ai/bide/theme"
)

// A theme names a shipped base and overrides some of its values. Parse checks the result against
// the contrast and distinctness rules; a theme that fails a check is refused, with each failure.
func ExampleParse() {
	th, report, err := theme.Parse([]byte(`{"bide_theme": 1, "name": "acme-night", "base": "dark",
	  "surface": {"bg": "#0e1116", "accent": "#79b8ff"}}`))
	fmt.Println(th.Name(), th.Dark(), th.Shipped(), err, len(report.Failures(theme.Error)))

	_, _, err = theme.Parse([]byte(`{"bide_theme": 1, "name": "acme-faint", "base": "light",
	  "surface": {"text": "#f6f6f2"}}`))
	fmt.Println(err != nil)
	// Output:
	// acme-night true false <nil> 0
	// true
}

// The shipped themes are always available by name.
func ExampleLookup() {
	th, ok := theme.Lookup("high-contrast")
	fmt.Println(th.Name(), ok, th.Shipped())
	_, ok = theme.Lookup("nope")
	fmt.Println(ok)
	// Output:
	// high-contrast true true
	// false
}

// Package planview embeds bide view's flow-config page in a program, with the program's
// plan.Registry, so the page checks a config the way plan.Load does: blocks, predicates, merges and
// types, as well as structure, and shows the digest a run of the flow would journal. The bide view
// command cannot import a program's Go types, so its -config page is structural; this package is
// the full check. Nothing runs, and the page is read-only.
package planview

import (
	"io"
	"net"
	"net/http"

	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
	"github.com/bide-ai/bide/theme"
)

// Option configures Handler and DiffHandler.
type Option func(*options)

type options struct{ themes runview.ThemeSet }

// WithThemes adds custom themes, each from theme.Parse or theme.Load (so each passed the checks
// in docs/design/themes.md), beside the shipped light, dark and high-contrast that the page's
// switch cycles through. A host passes values, never CSS: the page's stylesheet is generated from
// the checked values alone. Handler and DiffHandler panic when a theme is nil or not from Parse or
// Load, or when two themes (a shipped one included) have one name.
func WithThemes(themes ...*theme.Theme) Option {
	return func(o *options) { o.themes.Extra = append(o.themes.Extra, themes...) }
}

// WithDefaultTheme sets the theme a reader starts in, by name: a shipped theme or one added with
// WithThemes. Without it the reader's system setting picks light or dark. A reader who asks their
// system for more contrast or forced colors gets those, over the default. Handler and DiffHandler
// panic when the name is not a theme in the set or differs from the name given to WithThemeLock.
func WithDefaultTheme(name string) Option { return func(o *options) { o.themes.Default = name } }

// WithThemeLock makes the named theme the only one the page has: no switch, no remembered choice,
// and the reader's system setting no longer picks light or dark. A reader who asks their system for
// more contrast or forced colors still gets those, over the lock. Handler and DiffHandler panic
// when the name is not a theme in the set or differs from the name given to WithDefaultTheme.
func WithThemeLock(name string) Option { return func(o *options) { o.themes.Lock = name } }

// themeOptions applies opts and panics, naming the caller, on a set that cannot be served.
func themeOptions(caller string, opts []Option) runview.ThemeSet {
	var o options
	for _, f := range opts {
		f(&o)
	}
	if err := o.themes.Validate(); err != nil {
		panic("planview: " + caller + ": " + err.Error())
	}
	return o.themes
}

// Handler serves the config page for the flow config data (JSON), named name in messages, checked
// against reg (nil: structural only, as the bide view command checks it): the page at / and its
// view as JSON at /config.json. It answers only GET and HEAD requests addressed to a loopback host.
// The options choose the themes (WithThemes, WithDefaultTheme, WithThemeLock); Handler panics on a
// set that cannot be served, as http.ServeMux.Handle panics on a bad pattern.
func Handler(name string, data []byte, reg *plan.Registry, opts ...Option) http.Handler {
	t := themeOptions("Handler", opts)
	return runview.Handler(runview.Options{Config: &runview.Config{Name: name, Data: data, Registry: reg}, Themes: t.Extra, DefaultTheme: t.Default, ThemeLock: t.Lock})
}

// Listen listens on addr, which must name a loopback IP or localhost (any other host is refused), for
// serving Handler: http.Serve(ln, planview.Handler(...)).
func Listen(addr string) (net.Listener, error) { return runview.Listen(addr) }

// Check writes the config's problems to w as "name:node: message" lines ("name:from->to: message"
// for an edge or arm, "name: message" for the config as a whole), warnings marked "warning:", then
// a line counting the verdicts on the config's assert lines when it has any, and reports whether
// any problem is an error (a violated, undecided or invalid assertion is one): the bide view -check
// output, with reg's checks added. See plan.InspectConfig for the problems and the assertions.
func Check(w io.Writer, name string, data []byte, reg *plan.Registry) bool {
	return runview.Check(w, runview.Config{Name: name, Data: data, Registry: reg})
}

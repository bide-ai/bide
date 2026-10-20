package theme

import "github.com/bide-ai/bide/internal/themeaccess"

// The stylesheet generators are not part of the public API: a host passes checked themes to a
// handler and the handler writes the page's CSS. The module's own servers reach them through
// themeaccess, which takes the themes as any.
func init() {
	conv := func(themes []any) []*Theme {
		out := make([]*Theme, len(themes))
		for i, t := range themes {
			out[i] = t.(*Theme)
		}
		return out
	}
	themeaccess.Register(
		func(themes []any) string { return renderCSS(conv(themes)) },
		func(themes []any, def string) string { return cssDefault(conv(themes), def) },
		func(themes []any) string { return systemCSS(conv(themes)) },
	)
}

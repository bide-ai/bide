package planview

import (
	"io"
	"net/http"

	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
)

// Version is one version of a flow config: its name in messages, its JSON, and the Registry it
// loads against (nil: the structural check only). Load refuses a block registered and never used,
// so each version takes the Registry of the program as of that version; one Registry for two
// versions that use different blocks leaves a version with an error and no digest.
type Version struct {
	Name     string
	Data     []byte
	Registry *plan.Registry
}

func (v Version) config() runview.Config {
	return runview.Config{Name: v.Name, Data: v.Data, Registry: v.Registry}
}

// DiffHandler serves the diff page bide check -diff shows, with each version's checks: newer
// compared with old. The page draws the newer flow with added nodes and edges in green, removed
// ones as dashed red ghosts and changed nodes in amber, calls out an approval gate whose policy was
// weakened, and, with both Registries, shows the digest each version's runs journal. Its view is
// JSON at /diff.json. It answers only GET and HEAD requests addressed to a loopback host. The options
// are Handler's, and DiffHandler panics on a set of themes that cannot be served as Handler does.
func DiffHandler(old, newer Version, opts ...Option) http.Handler {
	t := themeOptions("DiffHandler", opts)
	return runview.Handler(runview.Options{Diff: &runview.Diff{Old: old.config(), New: newer.config()}, Themes: t.Extra, DefaultTheme: t.Default, ThemeLock: t.Lock})
}

// CheckDiff writes the diff of newer from old to w as bide check -diff -check prints it: one line
// per change, a line per weakened approval gate, and a summary line with the counts and the digest
// change. It reports whether an approval gate was removed or weakened (a set of approvals the new
// gate accepts would not have passed the old one), and fails only when a version cannot be read
// as a flow config.
func CheckDiff(w io.Writer, old, newer Version) (weakened bool, err error) {
	return runview.CheckDiff(w, runview.Diff{Old: old.config(), New: newer.config()})
}

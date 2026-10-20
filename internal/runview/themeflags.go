package runview

import (
	"fmt"
	"io"
	"strconv"

	"github.com/bide-ai/bide/internal/themeaccess"
	"github.com/bide-ai/bide/theme"
)

// The help text of the -theme and -theme-default flags of bide view and bide edit.
const (
	ThemeFlagHelp        = "a theme file (JSON, see docs/guides/themes.md; check it first with bide theme check) the pages offer beside light, dark and high-contrast; refused, with the failed checks, and nothing served, if it is not valid or fails a contrast, distinctness or polarity check (C1 to C4, C8); the colour-blindness check (C6) only prints a warning"
	ThemeDefaultFlagHelp = "the theme a reader starts in (light, dark, high-contrast, or the -theme file's name), instead of following the system setting; a reader's own choice still wins"
)

// ThemeFlags reads the -theme file and checks -theme-default for bide view and bide edit, after the
// other flags have been validated (a flag error is a usage error whatever the theme file holds) and
// before anything is opened or served. It returns the set of themes to serve; or a status of 1 when
// the file is refused (the failed checks are written to stderr after errPrefix); or a usage message
// when the default names no theme, which the caller reports as a usage error (status 2).
func ThemeFlags(file, def string, stderr io.Writer, errPrefix string) (set ThemeSet, usageMsg string, status int) {
	if file != "" {
		th, rep, err := theme.Load(file)
		if err != nil {
			fmt.Fprintf(stderr, "%s%s: %v\n", errPrefix, themeaccess.Escape(file), err) // err is built from quoted or escaped text
			return ThemeSet{}, "", 1
		}
		for _, m := range rep.Failures(theme.Warning) {
			fmt.Fprintf(stderr, "%s: warning %s %s: a reader with a colour-vision deficiency may not tell these apart; see bide theme check -v\n", themeaccess.Escape(file), m.Check, m.Subject)
		}
		set.Extra = append(set.Extra, th)
	}
	set.Default = def
	if def != "" && set.Validate() != nil {
		return ThemeSet{}, "-theme-default " + strconv.Quote(def) + " names no theme: light, dark, high-contrast or the -theme file's name", 0
	}
	return set, "", 0
}

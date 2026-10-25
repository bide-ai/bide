// Package editrun is how bide edit hands the editor (package flowedit) the run it compares the file
// with, without flowedit exporting a name for it. The editor's Option type cannot be built outside
// flowedit, so flowedit registers a constructor here when it is linked, and the command calls it:
// the run lives in that one editor's options, and nothing global holds a store or a run ID.
package editrun

import "github.com/bide-ai/bide/internal/runview"

// Run is the run to compare a file with: its ID and the read-only source its journal is read from. The
// command opens the source (runview.StoreFlags.Open) and closes it after the editor stops.
type Run struct {
	ID     string
	Source runview.Source
}

// NewOption returns the flowedit.Option that gives an editor run, as an any: this package cannot
// name the type. flowedit sets it in its init, so it is non-nil in any binary that links flowedit.
var NewOption func(Run) any

// Package planaccess is what the module's own tools may ask of the plan package that a host may not.
// plan is public, and its problem reports are likely to change shape, so it exports neither of the
// thing the flow editor needs and registers it here when it is loaded: the structured records of the
// problems the typed editor refuses an edit for adding.
package planaccess

// Record is one problem the typed editor compares before and after an edit: its class ("type" for a
// connection that does not fit, "safety" for a retry safety above what a block registers in Go, "loop"
// for a loop or wiring shape Load refuses), its kind within the class, the full names it is about
// (as the config has them, not shortened), the types or levels it is about, and the message to show.
// What identifies a problem is its Class, Kind, Names and Detail, never its message: the message
// shortens long names, and names an element by its position.
type Record struct {
	Class, Kind string
	Names       []string
	Detail      string
	Message     string
}

var inspect func(data []byte, registry, description any) []Record

// RegisterInspect is called once by the plan package's init with the function that returns the
// records of a config: against a *plan.Registry, or a *plan.RegistryDescription (one of the two,
// the other nil), or neither (the structure alone).
func RegisterInspect(f func(data []byte, registry, description any) []Record) { inspect = f }

// Inspect returns the records of the config in data, or nil if the plan package is not loaded.
func Inspect(data []byte, registry, description any) []Record {
	if inspect == nil {
		return nil
	}
	return inspect(data, registry, description)
}

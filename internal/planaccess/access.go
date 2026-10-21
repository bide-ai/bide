// Package planaccess is what the module's own tools may ask of the plan package that a host may not.
// plan is public and exports only what it promises; it registers here the one thing the flow editor
// needs that is not a promise: whether a config parses as plan reads it.
package planaccess

var parse func(data []byte) error

// RegisterParse is called once by the plan package's init with the function that reports why a config
// does not parse (its decoding refuses it), or nil if it does.
func RegisterParse(f func(data []byte) error) { parse = f }

// ParseError returns why the config in data does not parse as plan reads it, or nil if it does (or if
// the plan package is not loaded). A config that does not parse has no findings but "other": its
// problems are unknown, not absent.
func ParseError(data []byte) error {
	if parse == nil {
		return nil
	}
	return parse(data)
}

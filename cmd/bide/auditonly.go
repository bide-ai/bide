//go:build bide_audit_only

package main

// fullCommands is empty in a bide_audit_only build: it holds audit, version and help only, and
// links no store driver and no YAML decoder.
func fullCommands() []command { return nil }

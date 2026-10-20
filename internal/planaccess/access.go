// Package planaccess is what the module's own tools may ask of the plan package that a host may not:
// the option that makes InspectConfig list every type problem. plan is public and does not export it
// (the shape of its problem reports is likely to change); it registers the option here when it is
// loaded, and the flow editor, which compares two reports to find what an edit added, takes it from
// here.
package planaccess

var allTypeProblems any

// Register is called once by the plan package's init with its InspectOption that lifts the cap on the
// type problems a report lists.
func Register(allTypeProblemsOption any) { allTypeProblems = allTypeProblemsOption }

// AllTypeProblems returns that option, as the plan.InspectOption it was registered as, or nil if the
// plan package is not loaded.
func AllTypeProblems() any { return allTypeProblems }

package plan

import "testing"

// findingsOf is the findings InspectConfig reports for data against reg (nil for the structure alone),
// without those of kind "other": the tests that use it are about specific kinds.
func findingsOf(t *testing.T, data []byte, reg *Registry) []Finding {
	t.Helper()
	var out []Finding
	for _, f := range InspectConfig(data, reg, AllowUnusedBlocks()).Findings {
		if f.Kind != ProblemOther {
			out = append(out, f)
		}
	}
	return out
}

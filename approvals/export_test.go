package approvals

// BuildScriptForTest exposes the script builder to the external tests.
func BuildScriptForTest(withApp bool) (string, error) { return buildScript(withApp) }

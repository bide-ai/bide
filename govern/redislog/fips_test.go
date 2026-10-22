package redislog

import (
	"crypto/fips140"
	"testing"
)

// Importing this package and building its append script must not panic under GODEBUG=fips140=only
// (go-redis hashes a script with SHA-1, which that mode refuses outside fips140.WithoutEnforcement).
// Before the script was built lazily, the test binary itself panicked in package init.
func TestAppendScriptBuildsUnderFIPSOnly(t *testing.T) {
	s := appendScript()
	if s == nil || s.Hash() == "" {
		t.Fatalf("appendScript() = %v, want a script with a hash", s)
	}
	t.Logf("fips140 enabled=%v enforced=%v", fips140.Enabled(), fips140.Enforced())
}

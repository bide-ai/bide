package audit

import (
	"runtime/debug"
	"testing"
)

func TestBuildInfoValues(t *testing.T) {
	rev, def := buildInfoValues([]debug.BuildSetting{{Key: "-compiler", Value: "gc"}, {Key: "vcs.revision", Value: "abc123"}, {Key: "DefaultGODEBUG", Value: "x509sha1=0"}})
	if rev != "abc123" || def != "x509sha1=0" {
		t.Fatalf("%q %q", rev, def)
	}
	if rev, def := buildInfoValues(nil); rev != "" || def != "" {
		t.Fatalf("%q %q", rev, def)
	}
}

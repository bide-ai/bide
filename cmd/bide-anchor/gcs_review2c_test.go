package main

import (
	"testing"

	"github.com/bide-ai/bide/anchor/gcs"
)

func TestReview2_WeakBucketExitCodes(t *testing.T) {
	for name, mod := range map[string]func(*gcs.BucketInfo){
		"not locked":    func(b *gcs.BucketInfo) { b.RetentionLocked = false },
		"no retention":  func(b *gcs.BucketInfo) { b.RetentionPeriod = 0 },
		"short":         func(b *gcs.BucketInfo) { b.RetentionPeriod = 1 },
		"no versioning": func(b *gcs.BucketInfo) { b.Versioning = false },
		"no UBLA":       func(b *gcs.BucketInfo) { b.UniformAccess = false },
		"no PAP":        func(b *gcs.BucketInfo) { b.PublicAccessPrevented = false },
	} {
		e := gcsSetup(t)
		e.publish(t, e.head(t, "r1", 1, "a"))
		e.fake.Configure(mod)
		args := []string{"-bucket", "b", "-project", e.proj, "-pubkey", e.pub, "-run", "r1"}
		cc, out, _ := e.do("check", args...)
		ec, _, errs := e.do("export", args...)
		t.Logf("%-14s check exit %d, export exit %d (%.90s)", name, cc, ec, errs)
		if cc != exitFindings {
			t.Errorf("%s: check exit %d, out %s", name, cc, out)
		}
		if ec != exitFindings {
			t.Errorf("%s: export exit %d", name, ec)
		}
	}
}

package main

import "testing"

// A listed generation that cannot be read is a finding (1) in export, not unknown (3): the exit class of
// ErrVanished must be pinned.
func TestReview2_VanishedGenerationExitsOneInExport(t *testing.T) {
	e := gcsSetup(t)
	e.publish(t, e.head(t, "r1", 1, "a"), e.head(t, "r1", 3, "c"))
	e.fake.VanishOnRead(e.fake.Names()[0])
	args := []string{"-bucket", "b", "-project", e.proj, "-pubkey", e.pub, "-run", "r1", "-force"}
	if code, _, errs := e.do("export", args...); code != exitFindings {
		t.Errorf("export -force with a vanished generation: exit %d, want %d: %s", code, exitFindings, errs)
	}
}

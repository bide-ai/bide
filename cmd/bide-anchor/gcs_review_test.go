package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/anchor/gcs"
)

func TestReview2_GCSOpenAndExitCodes(t *testing.T) {
	e := gcsSetup(t)
	e.publish(t, e.head(t, "r1", 1, "a"), e.head(t, "r1", 3, "c"))
	base := func(proj string) []string {
		return []string{"-bucket", "b", "-project", proj, "-pubkey", e.pub, "-run", "r1"}
	}
	// The bucket cannot be read at all (an outage, a denied identity): that is unknown, not a usage error.
	for name, err := range map[string]error{"unavailable": gcs.ErrUnavailable, "denied": gcs.ErrDenied, "reset": errors.New("connection reset")} {
		e.fake.FailNext("Bucket", err, 1)
		code, _, errs := e.do("check", base(e.proj)...)
		t.Logf("Bucket %s: exit %d: %.120s", name, code, strings.ReplaceAll(errs, "\n", " / "))
		if code != exitUnknown {
			t.Errorf("Bucket %s: exit %d, want %d (unknown)", name, code, exitUnknown)
		}
	}
	// A bucket owned by another project.
	code, _, errs := e.do("check", base("999")...)
	t.Logf("foreign project: exit %d: %.140s", code, strings.ReplaceAll(errs, "\n", " / "))
	// -project forms
	for _, p := range []string{"+123456789012", " 123456789012", "123456789012 ", "0", "00", "-5", "9223372036854775808", "18446744073709551616", "1e3", "0x10", ""} {
		code, _, _ := e.do("check", base(p)...)
		if code == 0 {
			t.Errorf("-project %q passed", p)
		}
		t.Logf("-project %q: exit %d", p, code)
	}
	// Leading zeros parse to the same number
	if code, _, _ := e.do("check", base("000"+e.proj)...); code != 0 {
		t.Logf("leading zeros: exit %d", code)
	}
	// a vanished generation: a finding (1) in check, not a pass
	names := e.fake.Names()
	e.fake.VanishOnRead(names[0])
	code, out, errs := e.do("check", base(e.proj)...)
	t.Logf("vanished (check): exit %d out %.160s err %.120s", code, strings.ReplaceAll(out, "\n", " / "), strings.ReplaceAll(errs, "\n", " / "))
	if code == 0 {
		t.Errorf("a vanished generation passes check")
	}
	code, _, errs = e.do("export", base(e.proj)...)
	t.Logf("vanished (export): exit %d %.120s", code, strings.ReplaceAll(errs, "\n", " / "))
	if code != exitFindings {
		t.Errorf("vanished export: exit %d", code)
	}
}

package main

import (
	"slices"
	"strings"
	"testing"
)

// The fixture has a gated test that the workflow's pattern matches, one it does not, a test that is not
// gated, a variable no workflow sets and a workflow variable no test checks: the command reports the
// three that are wrong and nothing else.
func TestCheckFindsTheMismatches(t *testing.T) {
	got, err := check("testdata/repo")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`BIDE_NOWORKFLOW_BROWSER is checked by TestUnsetVar but no workflow sets it, so no job runs them`,
		`TestTwoMissesThePattern needs BIDE_FAKE_BROWSER but the workflow's -run pattern "InChrome" does not match it: CI never runs it`,
		`a workflow sets BIDE_ORPHAN_BROWSER but no test checks it`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if slices.ContainsFunc(got, func(l string) bool {
		return strings.Contains(l, "TestOneInChrome") || strings.Contains(l, "TestNotGated")
	}) {
		t.Errorf("a test that is fine is reported:\n%s", strings.Join(got, "\n"))
	}
}

// This repository's browser-gated tests are the ones its Chrome workflows run.
func TestRepositoryBrowserTestsAreRunByTheirWorkflows(t *testing.T) {
	problems, err := check("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
	tests, err := gatedTests("../../..")
	if err != nil {
		t.Fatal(err)
	}
	// The scan is not vacuous: it finds the editor's tests, among others.
	for _, name := range []string{"TestRunPanelInChrome", "TestTypedModeInChrome"} {
		if !slices.Contains(tests["BIDE_EDIT_BROWSER"], name) {
			t.Errorf("%s is not found among the editor's browser tests %q: the scan is broken", name, tests["BIDE_EDIT_BROWSER"])
		}
	}
	for _, v := range []string{"BIDE_VIEW_BROWSER", "BIDE_APPROVE_BROWSER", "BIDE_MTLS_BROWSER"} {
		if len(tests[v]) == 0 {
			t.Errorf("no test found for %s", v)
		}
	}
}

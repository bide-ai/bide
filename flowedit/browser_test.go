package flowedit

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// browserFail fails the test in CI (where BIDE_EDIT_BROWSER=1 means the job installed everything),
// and skips it anywhere else, so a missing Chrome cannot make the check pass silently in CI.
func browserFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// smoke is what browser/smoke.mjs prints: what the page drew and what the browser did.
type smoke struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	Banner struct {
		Text     string `json:"text"`
		Role     string `json:"role"`
		Controls int    `json:"controls"`
	} `json:"banner"`
	BaselineLine    string   `json:"baselineLine"`
	Verdict         string   `json:"verdict"`
	Outline         []string `json:"outline"`
	GraphNodes      []string `json:"graphNodes"`
	SVGNamespace    string   `json:"svgNamespace"`
	Source          string   `json:"source"`
	Assertions      []string `json:"assertions"`
	InspectorBefore string   `json:"inspectorBefore"`
	ByPointer       string   `json:"byPointer"`
	Current         []string `json:"current"`
	Reached         string   `json:"reached"`
	ByKeyboard      string   `json:"byKeyboard"`
	ByKeyboardGraph string   `json:"byKeyboardGraph"`
	Live            string   `json:"live"`
	Console         []string `json:"console"`
	Failed          []string `json:"failed"`
	OffOrigin       []string `json:"offOrigin"`
}

// browserTools finds Chrome, node and the installed driver, or skips (fails, in CI).
func browserTools(t *testing.T) (node, dir, chrome string) {
	t.Helper()
	if os.Getenv("BIDE_EDIT_BROWSER") != "1" {
		t.Skip("set BIDE_EDIT_BROWSER=1 to drive the editor page in Chrome (the Editor (Chrome) job does)")
	}
	chrome = os.Getenv("CHROME_BIN")
	if chrome == "" {
		chrome, _ = exec.LookPath("google-chrome-stable")
	}
	if chrome == "" {
		browserFail(t, "Chrome is not installed (set CHROME_BIN, or install google-chrome-stable)")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		browserFail(t, "node is not installed")
	}
	dir, err = filepath.Abs("browser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "puppeteer-core")); err != nil {
		browserFail(t, "puppeteer-core is not installed in %s (npm ci --ignore-scripts)", dir)
	}
	return node, dir, chrome
}

// runScript serves s on a loopback port, runs browser/script in Node against its page in Chrome and
// decodes the one JSON line it prints into out. The script sees FILE, the path of the config.
func runScript(t *testing.T, script string, s *Server, out any) {
	t.Helper()
	node, dir, chrome := browserTools(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	cmd := exec.Command(node, script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CHROME_BIN="+chrome, "PAGE_URL="+ts.URL+"/#t="+s.Token(), "FILE="+s.path)
	raw, err := cmd.Output()
	if jerr := json.Unmarshal(raw, out); jerr != nil {
		t.Fatalf("the script printed no result (%v): %q", err, raw)
	}
}

// runSmoke drives the page of s in Chrome (browser/smoke.mjs).
func runSmoke(t *testing.T, s *Server) smoke {
	t.Helper()
	var out smoke
	runScript(t, "smoke.mjs", s, &out)
	if !out.OK {
		t.Fatalf("the script failed: %s\nconsole: %q\nfailed requests: %q", out.Error, out.Console, out.Failed)
	}
	return out
}

// checkBrowser is what holds for every page: the same nodes in config order in the graph and the
// outline, the graph drawn in the SVG namespace, selection by pointer and by keyboard alone, the
// live region telling the selection, and no console message, failed request or request off the
// origin (a Content-Security-Policy violation is a console error).
func checkBrowser(t *testing.T, out smoke, source string) {
	t.Helper()
	if got := strings.Join(out.Outline, ","); got != "validate,charge,receipt" {
		t.Errorf("outline %q, want the nodes in config order", got)
	}
	if got := strings.Join(out.GraphNodes, ","); got != "validate,charge,receipt" {
		t.Errorf("graph nodes %q", got)
	}
	if out.SVGNamespace != "http://www.w3.org/2000/svg" {
		t.Errorf("the graph is in namespace %q, so the browser would not draw it", out.SVGNamespace)
	}
	if out.Source != source {
		t.Errorf("source %q", out.Source)
	}
	if !strings.Contains(out.InspectorBefore, "Select a node") {
		t.Errorf("inspector before a selection: %q", out.InspectorBefore)
	}
	if out.ByPointer != "charge" || len(out.Current) != 1 || out.Current[0] != "charge" {
		t.Errorf("pointer selection: inspector %q, current %q", out.ByPointer, out.Current)
	}
	if out.Reached == "" || out.ByKeyboard != out.Reached {
		t.Errorf("keyboard selection: reached %q, inspector %q", out.Reached, out.ByKeyboard)
	}
	if out.ByKeyboardGraph != "validate" {
		t.Errorf("keyboard selection in the graph: %q", out.ByKeyboardGraph)
	}
	if !strings.HasPrefix(out.Live, "Selected ") {
		t.Errorf("the live region says %q after a selection", out.Live)
	}
	if len(out.Console) > 0 || len(out.Failed) > 0 || len(out.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q, failed requests %q, requests off the origin %q", out.Console, out.Failed, out.OffOrigin)
	}
}

// TestPageInChrome drives the page in real Chrome: it draws the verdict, the graph, the outline and
// the assertions the server computed, selects a node with the pointer and with the keyboard alone,
// and makes no request off its own origin, no failed request and no console message.
//
// It runs only with BIDE_EDIT_BROWSER=1 (the "Editor (Chrome)" job, .github/workflows/editor-chrome.yml,
// sets it, with CHROME_BIN and the puppeteer-core from browser/package-lock.json installed).
func TestPageInChrome(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	out := runSmoke(t, s)
	if out.Verdict != "ALL ASSERTIONS HOLD" {
		t.Errorf("verdict %q", out.Verdict)
	}
	if len(out.Assertions) != 2 || !strings.Contains(out.Assertions[0], "holds") {
		t.Errorf("assertions %q", out.Assertions)
	}
	if out.Banner.Text != "" || out.Banner.Role != "" {
		t.Errorf("a banner on a file that is not weaker than its baseline: %+v", out.Banner)
	}
	if !strings.Contains(out.BaselineLine, "not weaker than the file as it was when the editor opened it") {
		t.Errorf("baseline line %q", out.BaselineLine)
	}
	checkBrowser(t, out, testFlow)
}

// A file weaker than the committed one shows the banner the moment the page opens: with the
// classifier's words and items, announced as an alert, and with nothing in it that closes it.
func TestWeakenedBannerInChrome(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	p := filepath.Join(dir, "refund.yaml")
	weaker := strings.Replace(strings.Replace(weakFlow, "need: 2", "need: 1", 1), ">= 2 of 3", ">= 1 of 3", 1)
	write := func(text string) {
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(weakFlow)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "policy")
	write(weaker)
	s, err := newServer(t, p)
	if err != nil {
		t.Fatal(err)
	}
	out := runSmoke(t, s)
	for _, want := range []string{"APPROVAL GATE WEAKENED", "charge: approval 2 of 3 excluding the requester, times out after 24h -> 1 of 3", "assertion loosened", "git HEAD"} {
		if !strings.Contains(out.Banner.Text, want) {
			t.Errorf("the banner lacks %q:\n%s", want, out.Banner.Text)
		}
	}
	if out.Banner.Role != "alert" {
		t.Errorf("the banner's role is %q, want alert", out.Banner.Role)
	}
	if out.Banner.Controls != 0 {
		t.Errorf("the banner holds %d controls: it must not be dismissible", out.Banner.Controls)
	}
	if !strings.Contains(out.BaselineLine, "compared with git HEAD") {
		t.Errorf("baseline line %q", out.BaselineLine)
	}
	checkBrowser(t, out, weaker)
}

// TestEditAndSaveInChrome types a weakening edit into the source, sees the banner (with nothing in it
// that closes it), saves it through the dialog by keyboard, and has to confirm the weakening to do
// so; then changes the file behind the page's back, and the next save is refused and shows what is
// on disk. The file on disk is checked at each step.
func TestEditAndSaveInChrome(t *testing.T) {
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	var r struct {
		OK                   bool     `json:"ok"`
		Error                string   `json:"error"`
		StateBefore          string   `json:"stateBefore"`
		SaveDisabledBefore   bool     `json:"saveDisabledBefore"`
		Banner               string   `json:"banner"`
		BannerControls       int      `json:"bannerControls"`
		StateDirty           string   `json:"stateDirty"`
		SaveEnabled          bool     `json:"saveEnabled"`
		VerdictAfterEdit     string   `json:"verdictAfterEdit"`
		DialogTitle          string   `json:"dialogTitle"`
		DialogText           string   `json:"dialogText"`
		DiffLines            []string `json:"diffLines"`
		DoSaveDisabledBefore bool     `json:"doSaveDisabledBefore"`
		FocusInDialog        string   `json:"focusInDialog"`
		DoSaveDisabledAfter  bool     `json:"doSaveDisabledAfter"`
		StateAfterSave       string   `json:"stateAfterSave"`
		Disk1                string   `json:"disk1"`
		FocusAfterClose      string   `json:"focusAfterClose"`
		LiveAfterSave        string   `json:"liveAfterSave"`
		ConflictText         string   `json:"conflictText"`
		ConflictButtons      []string `json:"conflictButtons"`
		ConflictDiffLines    []string `json:"conflictDiffLines"`
		Disk2                string   `json:"disk2"`
		SourceAfterReload    string   `json:"sourceAfterReload"`
		StateAfterReload     string   `json:"stateAfterReload"`
		Console              []string `json:"console"`
		Failed               []string `json:"failed"`
		OffOrigin            []string `json:"offOrigin"`
	}
	runScript(t, "edit.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\nconsole: %q\nfailed requests: %q", r.Error, r.Console, r.Failed)
	}
	if len(r.Console) > 0 || len(r.Failed) > 0 || len(r.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q, failed requests %q, requests off the origin %q", r.Console, r.Failed, r.OffOrigin)
	}
	weaker := strings.Replace(weakFlow, "need: 2", "need: 1", 1)
	if r.StateBefore != "saved" || !r.SaveDisabledBefore {
		t.Errorf("before an edit: state %q, save disabled %v", r.StateBefore, r.SaveDisabledBefore)
	}
	if !strings.Contains(r.Banner, "APPROVAL GATE WEAKENED") || !strings.Contains(r.Banner, "charge: approval 2 of 3") || r.BannerControls != 0 {
		t.Errorf("banner %q with %d controls", r.Banner, r.BannerControls)
	}
	if r.StateDirty != "unsaved changes" || !r.SaveEnabled || !strings.Contains(r.VerdictAfterEdit, "POLICY VIOLATION") {
		t.Errorf("after the edit: state %q, save enabled %v, verdict %q", r.StateDirty, r.SaveEnabled, r.VerdictAfterEdit)
	}
	if !strings.HasPrefix(r.DialogTitle, "Save refund.yaml") || !strings.Contains(r.DialogText, "THIS WEAKENS THE POLICY") || !r.DoSaveDisabledBefore || r.DoSaveDisabledAfter || r.FocusInDialog != "confirm-weaken" {
		t.Errorf("dialog %q %q: Save disabled before %v after %v, focus %q", r.DialogTitle, r.DialogText, r.DoSaveDisabledBefore, r.DoSaveDisabledAfter, r.FocusInDialog)
	}
	if !slices.Contains(r.DiffLines, "-      need: 2") || !slices.Contains(r.DiffLines, "+      need: 1") {
		t.Errorf("diff lines %q", r.DiffLines)
	}
	if r.StateAfterSave != "saved" || r.Disk1 != weaker || r.FocusAfterClose != "source" || r.LiveAfterSave != "Saved refund.yaml" {
		t.Errorf("after the save: state %q, focus %q, live %q, disk %q", r.StateAfterSave, r.FocusAfterClose, r.LiveAfterSave, r.Disk1)
	}
	other := strings.Replace(weaker, "flow: refund", "flow: refund-other", 1)
	if !strings.Contains(r.ConflictText, "nothing was written") || r.Disk2 != other {
		t.Errorf("conflict: %q; disk %q", r.ConflictText, r.Disk2)
	}
	if len(r.ConflictButtons) != 2 || !strings.HasPrefix(r.ConflictButtons[1], "Reload from disk") || !slices.Contains(r.ConflictDiffLines, "-flow: refund-other") || !slices.Contains(r.ConflictDiffLines, "+flow: refund") {
		t.Errorf("conflict buttons %q, diff %q", r.ConflictButtons, r.ConflictDiffLines)
	}
	if r.SourceAfterReload != other || r.StateAfterReload != "saved" {
		t.Errorf("after the reload: source %q, state %q", r.SourceAfterReload, r.StateAfterReload)
	}
	if got, _ := os.ReadFile(p); string(got) != other {
		t.Errorf("the editor wrote over the other writer's file: %q", got)
	}
}

// TestFormsInChrome drives the inspector's forms: a node's safety, block and approval gate are
// edited by the controls a keyboard reaches, each change is a splice of the file's text (the diff
// is the edit), the check's answer for a gate draft is shown before it is applied, the weakened
// banner follows the text against the baseline, and undo and redo restore the exact bytes.
func TestFormsInChrome(t *testing.T) {
	// weakFlow with an assertion the gate satisfies with room to spare, so adding an approver keeps it true.
	formsFlow := strings.Replace(weakFlow, ">= 2 of 3", ">= 2 of 5", 1)
	s, _ := newTestServer(t, "refund.yaml", formsFlow)
	var r struct {
		OK                   bool     `json:"ok"`
		Error                string   `json:"error"`
		Original             string   `json:"original"`
		GateButton           string   `json:"gateButton"`
		NeedBefore           string   `json:"needBefore"`
		ApproversBefore      []string `json:"approversBefore"`
		SafetyBefore         string   `json:"safetyBefore"`
		ExcludeBefore        bool     `json:"excludeBefore"`
		TimeoutBefore        string   `json:"timeoutBefore"`
		AssertionDraft       string   `json:"assertionDraft"`
		Kofn                 string   `json:"kofn"`
		SourceBeforeApply    string   `json:"sourceBeforeApply"`
		AfterAddApprover     string   `json:"afterAddApprover"`
		BannerAfterAdd       string   `json:"bannerAfterAdd"`
		StateAfterAdd        string   `json:"stateAfterAdd"`
		FocusAfterApply      string   `json:"focusAfterApply"`
		UndoEnabled          bool     `json:"undoEnabled"`
		AfterNeed            string   `json:"afterNeed"`
		BannerAfterNeed      string   `json:"bannerAfterNeed"`
		RejectedDraft        string   `json:"rejectedDraft"`
		FeedbackClass        string   `json:"feedbackClass"`
		SourceAfterRejected  string   `json:"sourceAfterRejectedDraft"`
		AfterUndo            string   `json:"afterUndo"`
		RedoEnabled          bool     `json:"redoEnabled"`
		AfterRedo            string   `json:"afterRedo"`
		AfterSafety          string   `json:"afterSafety"`
		FocusAfterSafety     string   `json:"focusAfterSafety"`
		AfterClear           string   `json:"afterClear"`
		BannerAfterClear     string   `json:"bannerAfterClear"`
		GateButtonAfterClear string   `json:"gateButtonAfterClear"`
		AfterGateReceipt     string   `json:"afterGateReceipt"`
		AfterBlock           string   `json:"afterBlock"`
		RefusalShown         string   `json:"refusalShown"`
		RefusalLive          string   `json:"refusalLive"`
		SourceAfterRefusal   bool     `json:"sourceAfterRefusal"`
		Console              []string `json:"console"`
		Failed               []string `json:"failed"`
		OffOrigin            []string `json:"offOrigin"`
	}
	runScript(t, "forms.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\nconsole: %q\nfailed requests: %q", r.Error, r.Console, r.Failed)
	}
	// The one refused operation of the script is a 422, which the browser logs as a console error;
	// it is expected, exactly once, and nothing else is.
	const refused422 = "error: Failed to load resource: the server responded with a status of 422 (Unprocessable Entity)"
	if len(r.Console) != 1 || r.Console[0] != refused422 || len(r.Failed) > 0 || len(r.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q (want only the one 422 of the refused operation), failed requests %q, requests off the origin %q", r.Console, r.Failed, r.OffOrigin)
	}
	if r.Original != formsFlow || r.GateButton != "Apply gate" || r.NeedBefore != "2" || !slices.Equal(r.ApproversBefore, []string{"ops", "finance", "risk"}) || r.SafetyBefore != "side_effect" || !r.ExcludeBefore || r.TimeoutBefore != "24h" {
		t.Errorf("the form does not show the node's gate: need %q approvers %q safety %q exclude %v timeout %q button %q", r.NeedBefore, r.ApproversBefore, r.SafetyBefore, r.ExcludeBefore, r.TimeoutBefore, r.GateButton)
	}
	if !strings.Contains(r.AssertionDraft, `assert[0] "every path to charge passes approval >= 2 of 5" is violated`) {
		t.Errorf("a draft that breaks the assertion: %q", r.AssertionDraft)
	}
	if r.Kofn != "2 of 4 approvers must sign, never counting the person who started the run; the gate expires after 24h." || r.SourceBeforeApply != formsFlow {
		t.Errorf("the draft: %q (the source must not change before Apply)", r.Kofn)
	}
	addedApprover := strings.Replace(formsFlow, "[ops, finance, risk]", "[ops, finance, risk, audit]", 1)
	if r.AfterAddApprover != addedApprover {
		t.Errorf("after adding an approver:\n%s", r.AfterAddApprover)
	}
	if !strings.Contains(r.BannerAfterAdd, "APPROVAL GATE WEAKENED") || r.StateAfterAdd != "unsaved changes" || r.FocusAfterApply != "g-apply" || !r.UndoEnabled {
		t.Errorf("after adding: banner %q, state %q, focus %q, undo %v", r.BannerAfterAdd, r.StateAfterAdd, r.FocusAfterApply, r.UndoEnabled)
	}
	need3 := strings.Replace(addedApprover, "need: 2", "need: 3", 1)
	if r.AfterNeed != need3 || strings.TrimSpace(r.BannerAfterNeed) != "" {
		t.Errorf("after raising the need: banner %q\n%s", r.BannerAfterNeed, r.AfterNeed)
	}
	if !strings.Contains(r.RejectedDraft, "must be between 1 and 4") || !strings.Contains(r.FeedbackClass, "bad") || r.SourceAfterRejected != need3 {
		t.Errorf("the rejected draft: %q (%s); the source must not change", r.RejectedDraft, r.FeedbackClass)
	}
	if r.AfterUndo != formsFlow || !r.RedoEnabled || r.AfterRedo != addedApprover {
		t.Errorf("undo and redo do not restore the bytes: undo %q, redo %q", r.AfterUndo, r.AfterRedo)
	}
	idem := strings.Replace(formsFlow, "safety: side_effect", "safety: idempotent", 1)
	if r.AfterSafety != idem || r.FocusAfterSafety != "f-safety" {
		t.Errorf("after the safety: focus %q\n%s", r.FocusAfterSafety, r.AfterSafety)
	}
	cleared := strings.Replace(idem, "    approval:\n      need: 2\n      approvers: [ops, finance, risk]\n      exclude_principal: true\n      timeout: 24h\n", "", 1)
	if r.AfterClear != cleared || !strings.Contains(r.BannerAfterClear, "APPROVAL GATE WEAKENED") || r.GateButtonAfterClear != "Add gate" {
		t.Errorf("after removing the gate: banner %q, button %q\n%s", r.BannerAfterClear, r.GateButtonAfterClear, r.AfterClear)
	}
	if !strings.Contains(r.AfterGateReceipt, "  - {name: receipt, block: receipt, safety: idempotent, approval: {need: 1, approvers: [ops, audit2], exclude_principal: true}}\n") {
		t.Errorf("after gating receipt:\n%s", r.AfterGateReceipt)
	}
	if !strings.Contains(r.RefusalShown, "cannot be empty") || !strings.HasPrefix(r.RefusalLive, "Not applied: ") || !r.SourceAfterRefusal {
		t.Errorf("a refused operation: shown %q, announced %q, text unchanged %v", r.RefusalShown, r.RefusalLive, r.SourceAfterRefusal)
	}
	if !strings.Contains(r.AfterBlock, "  - {name: validate, block: validate_v2, safety: readonly}\n") {
		t.Errorf("after the block:\n%s", r.AfterBlock)
	}
}

// TestRacesInChrome: an answer that arrives late or fails. The author's typing is never overwritten by
// the late answer of an operation, a failed undo loses no history and says so, and the number the
// decisions-needed field shows is the number that is sent (a value that is not a whole number is
// refused with a message).
func TestRacesInChrome(t *testing.T) {
	formsFlow := strings.Replace(weakFlow, ">= 2 of 3", ">= 2 of 5", 1)
	s, _ := newTestServer(t, "refund.yaml", formsFlow)
	var raw map[string]json.RawMessage
	runScript(t, "races.mjs", s, &raw)
	get := func(key string, into any) {
		t.Helper()
		if err := json.Unmarshal(raw[key], into); err != nil {
			t.Fatalf("%s: %v (%s)", key, err, raw[key])
		}
	}
	var ok bool
	get("ok", &ok)
	var console, failed, pageErrors []string
	get("console", &console)
	get("failed", &failed)
	get("pageErrors", &pageErrors)
	if !ok {
		var e string
		get("error", &e)
		t.Fatalf("the script failed: %s\nconsole: %q", e, console)
	}
	if len(pageErrors) > 0 {
		t.Errorf("unhandled errors in the page: %q", pageErrors)
	}
	// The aborted report is the only failed request, and the browser logs it once.
	if len(failed) != 1 || !strings.HasPrefix(failed[0], "/api/report") || len(console) != 1 || !strings.Contains(console[0], "ERR_FAILED") {
		t.Errorf("console %q, failed requests %q: want only the aborted /api/report", console, failed)
	}
	var lateSource, lateMessage, lateLive string
	get("lateSource", &lateSource)
	get("lateMessage", &lateMessage)
	get("lateLive", &lateLive)
	if !strings.Contains(lateSource, "#TYPED MEANWHILE") || !strings.Contains(lateSource, "safety: side_effect\n    approval") || strings.Contains(lateSource, "safety: idempotent\n    approval") ||
		!strings.Contains(lateMessage, "text changed") || !strings.HasPrefix(lateLive, "Not applied: the text changed") {
		t.Errorf("a late answer: source %q, message %q, live %q", lateSource, lateMessage, lateLive)
	}
	var undoLive string
	var keptSource, undoEnabled, redoDisabled, oneStep bool
	get("failedUndoLive", &undoLive)
	get("failedUndoSource", &keptSource)
	get("undoStillEnabled", &undoEnabled)
	get("redoStillDisabled", &redoDisabled)
	get("undoneOneStep", &oneStep)
	if !strings.HasPrefix(undoLive, "Not undone") || !keptSource || !undoEnabled || !redoDisabled || !oneStep {
		t.Errorf("a failed undo: live %q, source kept %v, undo enabled %v, redo disabled %v, one step back %v", undoLive, keptSource, undoEnabled, redoDisabled, oneStep)
	}
	for _, bad := range []string{"1e3", "2.5", "", "-1"} {
		var v struct {
			Msg           string `json:"msg"`
			ApplyDisabled bool   `json:"applyDisabled"`
			Invalid       string `json:"invalid"`
		}
		get("needBad_"+bad, &v)
		if !strings.Contains(v.Msg, "whole number") || !v.ApplyDisabled || v.Invalid != "true" {
			t.Errorf("need %q: %+v", bad, v)
		}
	}
	var steps int
	get("undoSteps", &steps)
	if steps < 100 || steps > 101 {
		t.Errorf("undo went back %d steps after 105+ operations, want the cap of 100", steps)
	}
	var goodEnabled, unchanged bool
	get("needGoodApplyEnabled", &goodEnabled)
	get("sourceUnchanged", &unchanged)
	if !goodEnabled || !unchanged {
		t.Errorf("a good number: apply enabled %v, text unchanged %v", goodEnabled, unchanged)
	}
}

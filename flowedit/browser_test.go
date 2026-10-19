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
	BaselineLine string `json:"baselineLine"`
	Cue          struct {
		Sel, Other struct {
			Edge   float64 `json:"edge"`
			Weight int     `json:"weight"`
			Marker string  `json:"marker"`
		}
	} `json:"cue"`
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
	if out.Cue.Sel.Edge < 5 || out.Cue.Sel.Edge <= out.Cue.Other.Edge || out.Cue.Sel.Weight < 700 || out.Cue.Sel.Weight <= out.Cue.Other.Weight || out.Cue.Sel.Marker == "none" || out.Cue.Sel.Marker == "normal" || out.Cue.Other.Marker != "none" && out.Cue.Other.Marker != "normal" {
		t.Errorf("the selected outline item is told by color alone: %+v", out.Cue)
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
		BannerMutations      int      `json:"bannerMutationsWhileTyping"`
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
	if r.BannerMutations != 0 {
		t.Errorf("the banner was drawn again %d times while the text went on weakening the same way: an alert drawn again is spoken again", r.BannerMutations)
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
		KofnOne              string   `json:"kofnOne"`
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
	// A refused operation is a 200 with {"refused": reason}, which the browser does not log as a failed
	// request: the forms script refuses one operation (an empty block name) and the console stays empty.
	if len(r.Console) > 0 || len(r.Failed) > 0 || len(r.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q, failed requests %q, requests off the origin %q", r.Console, r.Failed, r.OffOrigin)
	}
	if r.Original != formsFlow || r.GateButton != "Apply gate" || r.NeedBefore != "2" || !slices.Equal(r.ApproversBefore, []string{"ops", "finance", "risk"}) || r.SafetyBefore != "side_effect" || !r.ExcludeBefore || r.TimeoutBefore != "24h" {
		t.Errorf("the form does not show the node's gate: need %q approvers %q safety %q exclude %v timeout %q button %q", r.NeedBefore, r.ApproversBefore, r.SafetyBefore, r.ExcludeBefore, r.TimeoutBefore, r.GateButton)
	}
	if !strings.Contains(r.AssertionDraft, `assert[0] "every path to charge passes approval >= 2 of 5" is violated`) {
		t.Errorf("a draft that breaks the assertion: %q", r.AssertionDraft)
	}
	if r.KofnOne != "1 of 1 approver must sign." {
		t.Errorf("one approver: %q", r.KofnOne)
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
	var fbMut int
	get("feedbackMutations", &fbMut)
	if fbMut > 1 {
		t.Errorf("the gate form's live region was rewritten %d times while typing a deadline whose verdict did not change", fbMut)
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

// TestAssertionsInChrome drives the assertions panel: an assertion is built from one of the six
// templates, its verdict is shown live before it is added (a violation with its counterexample, a
// duplicate refused), it is added, edited and removed as a splice of the file's text, and removing
// one shows the weakened banner.
func TestAssertionsInChrome(t *testing.T) {
	flow := assertFlow + "assert:\n  - every path to charge passes approval >= 2 of 3\n  - receipt reachable\n"
	s, _ := newTestServer(t, "refund.yaml", flow)
	var r struct {
		OK                      bool     `json:"ok"`
		Error                   string   `json:"error"`
		Rows                    []string `json:"rows"`
		Templates               []string `json:"templates"`
		SubmitInitial           string   `json:"submitInitial"`
		LiveViolated            string   `json:"liveViolated"`
		SourceBeforeAdd         string   `json:"sourceBeforeAdd"`
		AfterAdd                string   `json:"afterAdd"`
		RowsAfterAdd            []string `json:"rowsAfterAdd"`
		VerdictAfterAdd         string   `json:"verdictAfterAdd"`
		FocusAfterAdd           string   `json:"focusAfterAdd"`
		FocusAfterEditClick     string   `json:"focusAfterEditClick"`
		AfterRemoveSecond       string   `json:"afterRemoveSecond"`
		BannerAfterRemoveSecond string   `json:"bannerAfterRemoveSecond"`
		EditSubmit              string   `json:"editSubmit"`
		EditTemplate            string   `json:"editTemplate"`
		EditNode                string   `json:"editNode"`
		LiveEdit                string   `json:"liveEdit"`
		AfterEdit               string   `json:"afterEdit"`
		FocusAfterEdit          string   `json:"focusAfterEdit"`
		LiveDuplicate           string   `json:"liveDuplicate"`
		LiveApproval            string   `json:"liveApproval"`
		AfterRemove             string   `json:"afterRemove"`
		BannerAfterRemove       string   `json:"bannerAfterRemove"`
		RowsAfterRemove         []string `json:"rowsAfterRemove"`
		Console                 []string `json:"console"`
		Failed                  []string `json:"failed"`
		OffOrigin               []string `json:"offOrigin"`
	}
	runScript(t, "assertions.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\nconsole: %q\nfailed requests: %q", r.Error, r.Console, r.Failed)
	}
	if len(r.Console) > 0 || len(r.Failed) > 0 || len(r.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q, failed requests %q, requests off the origin %q", r.Console, r.Failed, r.OffOrigin)
	}
	if !slices.Equal(r.Rows, []string{"every path to charge passes approval >= 2 of 3", "receipt reachable"}) || !slices.Equal(r.Templates, assertionForms) || r.SubmitInitial != "Add assertion" {
		t.Errorf("rows %q, templates %q, submit %q", r.Rows, r.Templates, r.SubmitInitial)
	}
	if !strings.Contains(r.LiveViolated, "violated") || !strings.Contains(r.LiveViolated, "counterexample: validate → big spender → charge") || r.SourceBeforeAdd != flow {
		t.Errorf("the live verdict before adding: %q (the source must not change)", r.LiveViolated)
	}
	added := flow + "  - no side effect after validate\n"
	if r.AfterAdd != added || !slices.Equal(r.RowsAfterAdd, []string{"every path to charge passes approval >= 2 of 3", "receipt reachable", "no side effect after validate"}) || !strings.Contains(r.VerdictAfterAdd, "POLICY VIOLATION") || r.FocusAfterAdd != "a-template" {
		t.Errorf("after adding: verdict %q, focus %q\n%s", r.VerdictAfterAdd, r.FocusAfterAdd, r.AfterAdd)
	}
	if r.EditSubmit != "Save assertion" || r.EditTemplate != "no_side_effect" || r.EditNode != "validate" || !strings.Contains(r.LiveEdit, "holds") {
		t.Errorf("the edit form: %q %q %q, live %q", r.EditSubmit, r.EditTemplate, r.EditNode, r.LiveEdit)
	}
	edited := flow + "  - no side effect after charge\n"
	if r.FocusAfterEditClick != "a-template" {
		t.Errorf("focus after Edit: %q", r.FocusAfterEditClick)
	}
	if r.AfterRemoveSecond != strings.Replace(edited, "  - receipt reachable\n", "", 1) || !strings.Contains(r.BannerAfterRemoveSecond, "assertion removed: receipt reachable") {
		t.Errorf("after removing the second: banner %q\n%s", r.BannerAfterRemoveSecond, r.AfterRemoveSecond)
	}
	if r.AfterEdit != edited || r.FocusAfterEdit != "a-edit-2" {
		t.Errorf("after the edit: focus %q\n%s", r.FocusAfterEdit, r.AfterEdit)
	}
	if !strings.Contains(r.LiveDuplicate, "already has the assertion") {
		t.Errorf("a duplicate: %q", r.LiveDuplicate)
	}
	if !strings.Contains(r.LiveApproval, "every path to charge passes approval >= 2 of 3 excluding the requester") || !strings.Contains(r.LiveApproval, "violated") {
		t.Errorf("the approval form's verdict: %q", r.LiveApproval)
	}
	removed := strings.Replace(strings.Replace(edited, "  - receipt reachable\n", "", 1), "  - every path to charge passes approval >= 2 of 3\n", "", 1)
	if r.AfterRemove != removed || !strings.Contains(r.BannerAfterRemove, "SAFETY CHECK WEAKENED") || !strings.Contains(r.BannerAfterRemove, "assertion removed: every path to charge passes approval >= 2 of 3") {
		t.Errorf("after removing: banner %q\n%s", r.BannerAfterRemove, r.AfterRemove)
	}
	if !slices.Equal(r.RowsAfterRemove, []string{"no side effect after charge"}) {
		t.Errorf("rows after removing %q", r.RowsAfterRemove)
	}
}

// a11yFlow is a flow with a gate, assertions and a node with a space in its name, so the states the
// accessibility run visits have something in each pane.
func a11yFlow() string {
	return strings.Replace(weakFlow, ">= 2 of 3", ">= 2 of 5", 1)
}

// TestAccessibilityInChrome runs axe-core over the page in light and dark and in the states that
// matter, and fails on any violation. An automated check finds a fraction of the problems a person
// meets; the keyboard-only run below and a manual screen-reader pass are the gate (docs/guides/
// flow-editor.md, "Accessibility").
func TestAccessibilityInChrome(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", a11yFlow())
	var r struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Results map[string]struct {
			Violations []struct {
				ID     string   `json:"id"`
				Impact string   `json:"impact"`
				Help   string   `json:"help"`
				Nodes  []string `json:"nodes"`
			} `json:"violations"`
			Passes int `json:"passes"`
		} `json:"results"`
	}
	runScript(t, "a11y.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s", r.Error)
	}
	if len(r.Results) != 10 {
		t.Errorf("%d states were checked, want 10 (5 states in 2 themes)", len(r.Results))
	}
	for state, res := range r.Results {
		if res.Passes < 20 {
			t.Errorf("%s: only %d rules passed: the page was not checked", state, res.Passes)
		}
		for _, v := range res.Violations {
			t.Errorf("%s: %s (%s): %s at %q", state, v.ID, v.Impact, v.Help, v.Nodes)
		}
	}
}

// TestKeyboardOnlyInChrome is the whole path with the keyboard alone (browser/keyboard.mjs never
// uses the pointer): the outline is one Tab stop moved by the arrow keys, forms are reached by Tab,
// the focus returns to the control that was changed, Escape closes the dialog and gives the focus
// back, Tab stays inside it, and a weakening is saved after confirming it.
func TestKeyboardOnlyInChrome(t *testing.T) {
	s, p := newTestServer(t, "refund.yaml", a11yFlow())
	var r struct {
		OK                  bool           `json:"ok"`
		Error               string         `json:"error"`
		OutlineFirst        string         `json:"outlineFirst"`
		AfterArrow          string         `json:"afterArrow"`
		AfterArrowBack      string         `json:"afterArrowBack"`
		InspectorTitle      string         `json:"inspectorTitle"`
		OutlineTabStops     int            `json:"outlineTabStops"`
		FocusAfterSafety    string         `json:"focusAfterSafety"`
		AfterGate           string         `json:"afterGate"`
		FocusAfterGate      string         `json:"focusAfterGate"`
		FocusAfterAssertion string         `json:"focusAfterAssertion"`
		FocusAfterSkip      string         `json:"focusAfterSkip"`
		SkipLinkLabel       string         `json:"skipLinkLabel"`
		SkipVisible         bool           `json:"skipVisible"`
		DialogFocus         string         `json:"dialogFocus"`
		DialogTitle         string         `json:"dialogTitle"`
		WeakensShown        bool           `json:"weakensShown"`
		FocusAfterEscape    string         `json:"focusAfterEscape"`
		TabStaysInDialog    bool           `json:"tabStaysInDialog"`
		StateAfterSave      string         `json:"stateAfterSave"`
		FocusAfterSave      string         `json:"focusAfterSave"`
		Disk                string         `json:"disk"`
		Tabs                map[string]int `json:"tabs"`
		Console             []string       `json:"console"`
		Failed              []string       `json:"failed"`
		OffOrigin           []string       `json:"offOrigin"`
	}
	runScript(t, "keyboard.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\ntabs: %v\nconsole: %q", r.Error, r.Tabs, r.Console)
	}
	if len(r.Console) > 0 || len(r.Failed) > 0 || len(r.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q, failed requests %q, requests off the origin %q", r.Console, r.Failed, r.OffOrigin)
	}
	if r.OutlineFirst != "validate" || r.AfterArrow != "charge" || r.AfterArrowBack != "charge" || r.InspectorTitle != "charge" || r.OutlineTabStops != 1 {
		t.Errorf("the outline: first %q, after Down %q, after Down Up %q, inspector %q, tab stops %d", r.OutlineFirst, r.AfterArrow, r.AfterArrowBack, r.InspectorTitle, r.OutlineTabStops)
	}
	if r.FocusAfterSafety != "f-safety" || r.FocusAfterGate != "g-apply" || r.FocusAfterAssertion != "a-template" {
		t.Errorf("focus after a change: safety %q, gate %q, assertion %q", r.FocusAfterSafety, r.FocusAfterGate, r.FocusAfterAssertion)
	}
	if r.DialogFocus != "confirm-weaken" || !strings.HasPrefix(r.DialogTitle, "Save refund.yaml") || !r.WeakensShown || r.FocusAfterEscape != "save" || !r.TabStaysInDialog {
		t.Errorf("the dialog: focus %q, title %q, weakens shown %v, focus after Escape %q, Tab stays inside %v", r.DialogFocus, r.DialogTitle, r.WeakensShown, r.FocusAfterEscape, r.TabStaysInDialog)
	}
	if r.StateAfterSave != "saved" || r.FocusAfterSave != "source" {
		t.Errorf("after the save: state %q, focus %q", r.StateAfterSave, r.FocusAfterSave)
	}
	want := strings.Replace(a11yFlow(), "safety: side_effect", "safety: idempotent", 1)
	want = strings.Replace(want, "[ops, finance, risk]", "[ops, finance, risk, audit]", 1)
	want = strings.Replace(want, "timeout: 24h", "timeout: 72h", 1)
	want += "  - validate unreachable\n"
	if got, _ := os.ReadFile(p); string(got) != want || r.Disk != want {
		t.Errorf("the file on disk:\n%s\nwant\n%s", r.Disk, want)
	}
	// Forward moves are bounded at 40. The standard for reaching the toolbar from the assertion form is
	// the skip link: at most 5 key presses (Shift+Tab to the region's link, Enter) and the focus is on
	// Save. Walking back over the whole page with Shift+Tab to Undo and Save is bounded at 50 only as
	// a guard against the page growing a control at a time.
	if r.FocusAfterSkip != "save" || r.SkipLinkLabel != "Skip to toolbar" || !r.SkipVisible {
		t.Errorf("the skip link: label %q, visible on focus %v, focus after %q", r.SkipLinkLabel, r.SkipVisible, r.FocusAfterSkip)
	}
	for name, n := range r.Tabs {
		limit := 40
		switch name {
		case "Undo", "Save":
			limit = 50
		case "Skip to Save":
			limit = 5
		}
		if n > limit {
			t.Errorf("%d Tab presses to reach %s (limit %d)", n, name, limit)
		}
	}
}

// TestStructureInChrome: the structure controls by the keyboard: add a node, rename it, remove it, remove
// a wired node through the dialog that lists what else goes, write the entry, and see a rename of a
// gated node read as weakened (the gate check follows names).
func TestStructureInChrome(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", weakFlow)
	var r struct {
		OK                     bool     `json:"ok"`
		Error                  string   `json:"error"`
		Original               string   `json:"original"`
		AfterAdd               string   `json:"afterAdd"`
		FocusAfterAdd          string   `json:"focusAfterAdd"`
		InspectorAfterAdd      string   `json:"inspectorAfterAdd"`
		OutlineAfterAdd        []string `json:"outlineAfterAdd"`
		DupError               string   `json:"dupError"`
		DupTextSame            bool     `json:"dupTextSame"`
		AfterRename            string   `json:"afterRename"`
		FocusAfterRename       string   `json:"focusAfterRename"`
		InspectorAfterRename   string   `json:"inspectorAfterRename"`
		AfterRemoveLone        string   `json:"afterRemoveLone"`
		FocusAfterRemove       string   `json:"focusAfterRemove"`
		DialogOpenAfterLone    bool     `json:"dialogOpenAfterLone"`
		RemoveList             []string `json:"removeList"`
		DialogTitle            string   `json:"dialogTitle"`
		AfterCancel            string   `json:"afterCancel"`
		FocusAfterCancel       string   `json:"focusAfterCancel"`
		AfterRemoveWired       string   `json:"afterRemoveWired"`
		StateAfterRemove       string   `json:"stateAfterRemove"`
		AfterUndo              string   `json:"afterUndo"`
		EntryButton            string   `json:"entryButton"`
		AfterEntry             string   `json:"afterEntry"`
		EntryRefusal           string   `json:"entryRefusal"`
		DialogOpenAfterRefusal bool     `json:"dialogOpenAfterRefusal"`
		AfterGatedRename       string   `json:"afterGatedRename"`
		BannerAfterGatedRename string   `json:"bannerAfterGatedRename"`
		Console                []string `json:"console"`
		Failed                 []string `json:"failed"`
		OffOrigin              []string `json:"offOrigin"`
	}
	runScript(t, "structure.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\nconsole: %q\nfailed requests: %q", r.Error, r.Console, r.Failed)
	}
	if len(r.Console) > 0 || len(r.Failed) > 0 || len(r.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q, failed requests %q, requests off the origin %q", r.Console, r.Failed, r.OffOrigin)
	}
	added := strings.Replace(weakFlow, "  - {name: receipt, block: receipt, safety: idempotent}\n", "  - {name: receipt, block: receipt, safety: idempotent}\n  - {name: audit, block: audit_log}\n", 1)
	if r.AfterAdd != added || r.FocusAfterAdd != "f-name" || r.InspectorAfterAdd != "audit" || !slices.Equal(r.OutlineAfterAdd, []string{"validate", "charge", "receipt", "audit"}) {
		t.Errorf("after adding: focus %q, inspector %q, outline %q\n%s", r.FocusAfterAdd, r.InspectorAfterAdd, r.OutlineAfterAdd, r.AfterAdd)
	}
	if !strings.Contains(r.DupError, "already there") || !r.DupTextSame {
		t.Errorf("a duplicate name: %q, text unchanged %v", r.DupError, r.DupTextSame)
	}
	if r.AfterRename != strings.Replace(added, "name: audit,", "name: audit2,", 1) || r.FocusAfterRename != "f-name" || r.InspectorAfterRename != "audit2" {
		t.Errorf("after renaming: focus %q, inspector %q\n%s", r.FocusAfterRename, r.InspectorAfterRename, r.AfterRename)
	}
	if r.AfterRemoveLone != weakFlow || r.FocusAfterRemove != "an-name" || r.DialogOpenAfterLone {
		t.Errorf("removing a node nothing is wired to: focus %q, dialog open %v\n%s", r.FocusAfterRemove, r.DialogOpenAfterLone, r.AfterRemoveLone)
	}
	if r.DialogTitle != "Remove receipt?" || !slices.Equal(r.RemoveList, []string{"wiring 2: edge charge -> receipt"}) {
		t.Errorf("the removal dialog: %q, %q", r.DialogTitle, r.RemoveList)
	}
	if r.AfterCancel != weakFlow || r.FocusAfterCancel != "f-remove" {
		t.Errorf("after Cancel: focus %q, text unchanged %v", r.FocusAfterCancel, r.AfterCancel == weakFlow)
	}
	wired := strings.NewReplacer("  - {name: receipt, block: receipt, safety: idempotent}\n", "", "  - edge: [charge, receipt]\n", "").Replace(weakFlow)
	if r.AfterRemoveWired != wired || r.StateAfterRemove != "unsaved changes" || r.AfterUndo != weakFlow {
		t.Errorf("after removing a wired node (undo restores %v):\n%s", r.AfterUndo == weakFlow, r.AfterRemoveWired)
	}
	if r.EntryButton != "Make this the entry node" || r.AfterEntry != strings.Replace(weakFlow, "entry: validate", "entry: charge", 1) {
		t.Errorf("the entry: button %q\n%s", r.EntryButton, r.AfterEntry)
	}
	if !strings.Contains(r.EntryRefusal, "is the entry node") || r.DialogOpenAfterRefusal {
		t.Errorf("removing the entry node: %q, dialog open %v", r.EntryRefusal, r.DialogOpenAfterRefusal)
	}
	if !strings.Contains(r.AfterGatedRename, "name: charge2") || !strings.Contains(r.BannerAfterGatedRename, "WEAKENED") {
		t.Errorf("renaming a gated node: banner %q\n%s", r.BannerAfterGatedRename, r.AfterGatedRename)
	}
}

// TestNonPrintingNamesAreShownEscaped: plan refuses a name that does not print, so the server never sends
// one for a config; the page still writes such characters out (a typed-mode registry names a block in Go,
// which can be any string). The script has the page draw the real view of a config with each placeholder
// name replaced by a hostile one, walks every text node, title, aria-label, placeholder, alt and option
// in each state, and checks the page's helpers on every class. The editing fields and the source keep
// the raw text.
func TestNonPrintingNamesAreShownEscaped(t *testing.T) {
	src := `{"version":1,"flow":"flowZ","entry":"nodeA","nodes":[` +
		`{"name":"nodeA","block":"blockA","safety":"side_effect","approval":{"need":1,"approvers":["approvA"]}},` +
		`{"name":"nodeB","block":"b2"},{"name":"nodeC","block":"ok"}],` +
		`"wiring":[{"edge":["nodeA","nodeB"]},{"edge":["nodeB","nodeC"]}],` +
		`"assert":["every path to nodeC passes nodeA","nodeA reachable"]}` + "\n"
	s, _ := newTestServer(t, "f.json", src)
	var r struct {
		OK        bool     `json:"ok"`
		Error     string   `json:"error"`
		Helpers   []string `json:"helpers"`
		Nodes     []string `json:"nodes"`
		Title     string   `json:"title"`
		Warning   string   `json:"warning"`
		NameField string   `json:"nameField"`
		Hits      []struct {
			State string `json:"state"`
			Where string `json:"where"`
			Text  string `json:"text"`
			CP    string `json:"cp"`
		} `json:"hits"`
		Console []string `json:"console"`
	}
	runScript(t, "spoof.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\nconsole: %q", r.Error, r.Console)
	}
	if len(r.Helpers) > 0 {
		t.Errorf("the page's helpers leave characters raw or change plain text: %q", r.Helpers)
	}
	if len(r.Nodes) != 3 || r.Title != `ch\u{202E}gra` || r.NameField != "ch\u202egra" {
		t.Errorf("nodes %q, title %q, field %q", r.Nodes, r.Title, r.NameField)
	}
	for _, h := range r.Hits {
		t.Errorf("raw U+%s in %s (%s): %q", h.CP, h.Where, h.State, h.Text)
	}
	if !strings.Contains(r.Warning, "do not print") || len(r.Console) > 0 {
		t.Errorf("warning %q, console %q", r.Warning, r.Console)
	}
}

// TestMovesInChrome: reorder and the flow settings by the keyboard: a node up and down from the
// inspector, a wiring element from the wiring list, the flow's name and the documentation types, and a
// value the server refuses.
func TestMovesInChrome(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", weakFlow)
	var r struct {
		OK                   bool     `json:"ok"`
		Error                string   `json:"error"`
		Original             string   `json:"original"`
		OutlineBefore        []string `json:"outlineBefore"`
		Position             string   `json:"position"`
		UpLabel              string   `json:"upLabel"`
		DownDisabledAtEnd    bool     `json:"downDisabledAtEnd"`
		AfterUp              string   `json:"afterUp"`
		OutlineAfterUp       []string `json:"outlineAfterUp"`
		PositionAfterUp      string   `json:"positionAfterUp"`
		FocusAfterUp         string   `json:"focusAfterUp"`
		LiveAfterUp          string   `json:"liveAfterUp"`
		AfterDown            string   `json:"afterDown"`
		StateAfterDown       string   `json:"stateAfterDown"`
		FirstUpDisabled      bool     `json:"firstUpDisabled"`
		WiringItems          []string `json:"wiringItems"`
		WireStops            int      `json:"wireStops"`
		MoveBeforeSelect     bool     `json:"moveBeforeSelect"`
		WirePos              string   `json:"wirePos"`
		WireLabel            string   `json:"wireLabel"`
		UpDisabledAfter      bool     `json:"upDisabledAfter"`
		WirePosAfter         string   `json:"wirePosAfter"`
		SettingsOpenAtStart  bool     `json:"settingsOpenAtStart"`
		AfterWiring          string   `json:"afterWiring"`
		FocusAfterWiring     string   `json:"focusAfterWiring"`
		FirstUpDisabledAfter bool     `json:"firstUpDisabledAfter"`
		AfterFlow            string   `json:"afterFlow"`
		FocusAfterFlow       string   `json:"focusAfterFlow"`
		AfterIn              string   `json:"afterIn"`
		AfterInCleared       string   `json:"afterInCleared"`
		Refusal              string   `json:"refusal"`
		SourceAfterRefusal   string   `json:"sourceAfterRefusal"`
		Console              []string `json:"console"`
		Failed               []string `json:"failed"`
		OffOrigin            []string `json:"offOrigin"`
	}
	runScript(t, "moves.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\nconsole: %q\nfailed requests: %q", r.Error, r.Console, r.Failed)
	}
	if len(r.Console) > 0 || len(r.Failed) > 0 || len(r.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q, failed requests %q, requests off the origin %q", r.Console, r.Failed, r.OffOrigin)
	}
	receipt := "  - {name: receipt, block: receipt, safety: idempotent}\n"
	charge := "  - name: charge\n    block: charge\n    safety: side_effect\n    approval:\n      need: 2\n      approvers: [ops, finance, risk]\n      exclude_principal: true\n      timeout: 24h\n"
	if !strings.Contains(weakFlow, charge+receipt) {
		t.Fatal("the sample is not what the test expects")
	}
	if r.Position != "Position 3 of 3." || r.UpLabel != "Move up: receipt" || !r.DownDisabledAtEnd {
		t.Errorf("the move controls: %q %q down disabled %v", r.Position, r.UpLabel, r.DownDisabledAtEnd)
	}
	up := strings.Replace(weakFlow, charge+receipt, receipt+charge, 1)
	if r.AfterUp != up || !slices.Equal(r.OutlineAfterUp, []string{"validate", "receipt", "charge"}) || r.PositionAfterUp != "Position 2 of 3." || r.FocusAfterUp != "f-up" || !strings.Contains(r.LiveAfterUp, "receipt moved up") {
		t.Errorf("after moving up: position %q focus %q live %q outline %q\n%s", r.PositionAfterUp, r.FocusAfterUp, r.LiveAfterUp, r.OutlineAfterUp, r.AfterUp)
	}
	if r.AfterDown != weakFlow || r.StateAfterDown != "saved" {
		t.Errorf("down did not restore the text (state %q)\n%s", r.StateAfterDown, r.AfterDown)
	}
	if !r.FirstUpDisabled {
		t.Error("the first node's Move up is enabled")
	}
	if !slices.Equal(r.WiringItems, []string{"0", "1"}) || r.WireStops != 1 || !r.MoveBeforeSelect || r.WirePos != "Wiring element 2 of 2 is selected." || r.WireLabel != "Move up: wiring element 2" || r.SettingsOpenAtStart {
		t.Errorf("wiring list: items %q, tab stops %d, move disabled before a selection %v, %q, %q, settings open at start %v", r.WiringItems, r.WireStops, r.MoveBeforeSelect, r.WirePos, r.WireLabel, r.SettingsOpenAtStart)
	}
	wiring := "  - edge: [validate, charge]\n  - edge: [charge, receipt]\n"
	if r.AfterWiring != strings.Replace(weakFlow, wiring, "  - edge: [charge, receipt]\n  - edge: [validate, charge]\n", 1) || r.FocusAfterWiring != "w-down" || !r.UpDisabledAfter || r.WirePosAfter != "Wiring element 1 of 2 is selected." {
		t.Errorf("after moving wiring: focus %q\n%s", r.FocusAfterWiring, r.AfterWiring)
	}
	if !strings.Contains(r.AfterFlow, "flow: refund v2\n") || r.FocusAfterFlow != "fs-flow" || !strings.Contains(r.AfterIn, "in: Order\n") || strings.Contains(r.AfterInCleared, "in:") {
		t.Errorf("flow settings: focus %q\n%s\n%s\n%s", r.FocusAfterFlow, r.AfterFlow, r.AfterIn, r.AfterInCleared)
	}
	if !strings.Contains(r.Refusal, "cannot be empty") || r.SourceAfterRefusal != r.AfterInCleared {
		t.Errorf("a refused flow name: %q", r.Refusal)
	}
}

// TestWiringInChrome: edges, a switch, its arms and its else by the keyboard.
func TestWiringInChrome(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", weakFlow)
	var r struct {
		OK                bool     `json:"ok"`
		Error             string   `json:"error"`
		Original          string   `json:"original"`
		WithAudit         string   `json:"withAudit"`
		EdgesOutBefore    int      `json:"edgesOutBefore"`
		InLine            string   `json:"inLine"`
		AfterEdge         string   `json:"afterEdge"`
		FocusAfterEdge    string   `json:"focusAfterEdge"`
		OutsAfter         []string `json:"outsAfter"`
		LiveEdge          string   `json:"liveEdge"`
		AfterRemoveEdge   string   `json:"afterRemoveEdge"`
		DupEdge           string   `json:"dupEdge"`
		SwitchOffered     bool     `json:"switchOffered"`
		AfterSwitch       string   `json:"afterSwitch"`
		ArmsOne           int      `json:"armsOne"`
		RmDisabledOnly    bool     `json:"rmDisabledOnly"`
		AfterArm          string   `json:"afterArm"`
		AfterSetArm       string   `json:"afterSetArm"`
		AfterMoveArm      string   `json:"afterMoveArm"`
		FocusAfterMove    string   `json:"focusAfterMove"`
		AfterElse         string   `json:"afterElse"`
		AfterRemoveArm    string   `json:"afterRemoveArm"`
		LastRmDisabled    bool     `json:"lastRmDisabled"`
		AfterRemoveSwitch string   `json:"afterRemoveSwitch"`
		Console           []string `json:"console"`
		Failed            []string `json:"failed"`
		OffOrigin         []string `json:"offOrigin"`
	}
	runScript(t, "wiring.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\nconsole: %q\nfailed requests: %q", r.Error, r.Console, r.Failed)
	}
	if len(r.Console) > 0 || len(r.Failed) > 0 || len(r.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q, failed requests %q, requests off the origin %q", r.Console, r.Failed, r.OffOrigin)
	}
	if r.EdgesOutBefore != 0 || r.InLine != "Edges in: charge." {
		t.Errorf("receipt's connections: %d edges out, %q", r.EdgesOutBefore, r.InLine)
	}
	if !strings.Contains(r.AfterEdge, "  - edge: [charge, receipt]\n  - edge: [receipt, audit]\n") || r.FocusAfterEdge != "c-edge-to" || len(r.OutsAfter) != 1 || !strings.Contains(r.LiveEdge, "Edge added") {
		t.Errorf("after adding an edge: focus %q, outs %q, live %q\n%s", r.FocusAfterEdge, r.OutsAfter, r.LiveEdge, r.AfterEdge)
	}
	if r.AfterRemoveEdge != r.WithAudit {
		t.Errorf("removing the edge did not restore the text:\n%s", r.AfterRemoveEdge)
	}
	if !strings.Contains(r.DupEdge, "already there") {
		t.Errorf("a duplicate edge: %q", r.DupEdge)
	}
	if !r.SwitchOffered || !strings.Contains(r.AfterSwitch, "  - switch: audit\n    when: [{pred: big, to: receipt}]\n") || r.ArmsOne != 1 || !r.RmDisabledOnly {
		t.Errorf("a switch: %d arms, remove disabled %v\n%s", r.ArmsOne, r.RmDisabledOnly, r.AfterSwitch)
	}
	if !strings.Contains(r.AfterArm, "when: [{pred: big, to: receipt}, {pred: small, to: charge}]\n") {
		t.Errorf("an arm added:\n%s", r.AfterArm)
	}
	if !strings.Contains(r.AfterSetArm, "{pred: small, to: validate}") {
		t.Errorf("an arm changed:\n%s", r.AfterSetArm)
	}
	if !strings.Contains(r.AfterMoveArm, "when: [{pred: small, to: validate}, {pred: big, to: receipt}]\n") || r.FocusAfterMove != "c-arm-down-0" {
		t.Errorf("an arm moved: focus %q\n%s", r.FocusAfterMove, r.AfterMoveArm)
	}
	if !strings.Contains(r.AfterElse, "when: [{pred: small, to: validate}, {pred: big, to: receipt}]\n    else: charge\n") {
		t.Errorf("an else:\n%s", r.AfterElse)
	}
	if !strings.Contains(r.AfterRemoveArm, "when: [{pred: big, to: receipt}]\n") || !r.LastRmDisabled {
		t.Errorf("an arm removed: last remove disabled %v\n%s", r.LastRmDisabled, r.AfterRemoveArm)
	}
	if r.AfterRemoveSwitch != r.WithAudit {
		t.Errorf("removing the switch did not restore the text:\n%s", r.AfterRemoveSwitch)
	}
}

// TestJoinsInChrome: a join and a loop bound by the keyboard.
func TestJoinsInChrome(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", weakFlow)
	var r struct {
		OK                       bool     `json:"ok"`
		Error                    string   `json:"error"`
		AddJoinOpenAtStart       bool     `json:"addJoinOpenAtStart"`
		AfterAdd                 string   `json:"afterAdd"`
		FocusAfterAdd            string   `json:"focusAfterAdd"`
		InspectorTitle           string   `json:"inspectorTitle"`
		BlockFieldOnJoin         bool     `json:"blockFieldOnJoin"`
		RenameOnJoin             bool     `json:"renameOnJoin"`
		Inputs                   []string `json:"inputs"`
		RemoveInputDisabledAtTwo bool     `json:"removeInputDisabledAtTwo"`
		AfterAddInput            string   `json:"afterAddInput"`
		AfterMoveInput           string   `json:"afterMoveInput"`
		FocusAfterMoveInput      string   `json:"focusAfterMoveInput"`
		AfterRemoveInput         string   `json:"afterRemoveInput"`
		AfterMerge               string   `json:"afterMerge"`
		AfterGate                string   `json:"afterGate"`
		AfterJoinEdge            string   `json:"afterJoinEdge"`
		JoinOffersSwitch         bool     `json:"joinOffersSwitch"`
		LoopBefore               string   `json:"loopBefore"`
		NoExit                   string   `json:"noExit"`
		AfterLoop                string   `json:"afterLoop"`
		FocusAfterLoop           string   `json:"focusAfterLoop"`
		AfterLoopCleared         string   `json:"afterLoopCleared"`
		FractionLive             string   `json:"fractionLive"`
		DialogTitle              string   `json:"dialogTitle"`
		RemoveList               []string `json:"removeList"`
		AfterRemove              string   `json:"afterRemove"`
		Console                  []string `json:"console"`
		Failed                   []string `json:"failed"`
		OffOrigin                []string `json:"offOrigin"`
	}
	runScript(t, "joins.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\nconsole: %q\nfailed requests: %q", r.Error, r.Console, r.Failed)
	}
	if len(r.Console) > 0 || len(r.Failed) > 0 || len(r.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q, failed requests %q, requests off the origin %q", r.Console, r.Failed, r.OffOrigin)
	}
	if r.AddJoinOpenAtStart || !strings.Contains(r.AfterAdd, "  - join: both\n    inputs: [charge, receipt]\n    merge: merge_two\n") || r.FocusAfterAdd != "j-merge" || r.InspectorTitle != "both" {
		t.Errorf("after adding a join: open at start %v, focus %q, title %q\n%s", r.AddJoinOpenAtStart, r.FocusAfterAdd, r.InspectorTitle, r.AfterAdd)
	}
	if r.BlockFieldOnJoin || r.RenameOnJoin || !slices.Equal(r.Inputs, []string{"1. charge", "2. receipt"}) || r.RemoveInputDisabledAtTwo {
		t.Errorf("the join's inspector: block field %v, rename %v, inputs %q, remove disabled at two %v", r.BlockFieldOnJoin, r.RenameOnJoin, r.Inputs, r.RemoveInputDisabledAtTwo)
	}
	if !strings.Contains(r.AfterAddInput, "inputs: [charge, receipt, validate]") || !strings.Contains(r.AfterMoveInput, "inputs: [charge, validate, receipt]") || !strings.Contains(r.AfterRemoveInput, "inputs: [charge, receipt]") || r.FocusAfterMoveInput != "j-in-up-1" {
		t.Errorf("inputs: focus %q\n%s\n%s\n%s", r.FocusAfterMoveInput, r.AfterAddInput, r.AfterMoveInput, r.AfterRemoveInput)
	}
	if !strings.Contains(r.AfterMerge, "merge: merge_v2\n") {
		t.Errorf("the merge:\n%s", r.AfterMerge)
	}
	if !strings.Contains(r.AfterGate, "    merge: merge_v2\n    approval:\n      need: 1\n      approvers: [ops]\n") {
		t.Errorf("a gate on the join:\n%s", r.AfterGate)
	}
	if !strings.Contains(r.AfterJoinEdge, "  - edge: [both, validate]\n") || r.JoinOffersSwitch {
		t.Errorf("an edge out of the join (a switch offered: %v):\n%s", r.JoinOffersSwitch, r.AfterJoinEdge)
	}
	if !strings.Contains(r.NoExit, "no exit arm") {
		t.Errorf("a loop bound with no exit arm: %q", r.NoExit)
	}
	if r.LoopBefore != "0" || !strings.Contains(r.AfterLoop, "{pred: again, to: charge, loop_max: 3}") || r.FocusAfterLoop != "c-arm-max-0" || strings.Contains(r.AfterLoopCleared, "loop_max") {
		t.Errorf("a loop bound: before %q, focus %q\n%s\n%s", r.LoopBefore, r.FocusAfterLoop, r.AfterLoop, r.AfterLoopCleared)
	}
	if !strings.Contains(r.FractionLive, "whole number") {
		t.Errorf("a fractional bound: %q", r.FractionLive)
	}
	if r.DialogTitle != "Remove both?" || len(r.RemoveList) != 1 || !strings.Contains(r.RemoveList[0], "edge both -> validate") || strings.Contains(r.AfterRemove, "join: both") {
		t.Errorf("removing the join: title %q, list %q\n%s", r.DialogTitle, r.RemoveList, r.AfterRemove)
	}
}

// TestUndoCoversStructureEditsInChrome: Undo and Redo walk a sequence of every kind of structure edit
// byte for byte, the weakened banner follows each text, and the history survives a save.
func TestUndoCoversStructureEditsInChrome(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", weakFlow)
	var r struct {
		OK                     bool     `json:"ok"`
		Error                  string   `json:"error"`
		Steps                  int      `json:"steps"`
		BannerWhenWeakened     string   `json:"bannerWhenWeakened"`
		UndoMatches            []bool   `json:"undoMatches"`
		BannerAfterFirstUndo   string   `json:"bannerAfterFirstUndo"`
		UndoDisabledAtStart    bool     `json:"undoDisabledAtStart"`
		StateAtStart           string   `json:"stateAtStart"`
		Original               string   `json:"original"`
		RedoMatches            []bool   `json:"redoMatches"`
		RedoDisabledAtEnd      bool     `json:"redoDisabledAtEnd"`
		Disk                   string   `json:"disk"`
		Last                   string   `json:"last"`
		UndoEnabledAfterSave   bool     `json:"undoEnabledAfterSave"`
		StateAfterUndoPastSave string   `json:"stateAfterUndoPastSave"`
		DiskStill              bool     `json:"diskStill"`
		StateAfterRedoToSaved  string   `json:"stateAfterRedoToSaved"`
		Console                []string `json:"console"`
		Failed                 []string `json:"failed"`
		OffOrigin              []string `json:"offOrigin"`
	}
	runScript(t, "undo.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\nconsole: %q\nfailed requests: %q", r.Error, r.Console, r.Failed)
	}
	if len(r.Console) > 0 || len(r.Failed) > 0 || len(r.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q, failed requests %q, requests off the origin %q", r.Console, r.Failed, r.OffOrigin)
	}
	if r.Steps != 10 || len(r.UndoMatches) != 10 || len(r.RedoMatches) != 10 {
		t.Fatalf("steps %d, undos %d, redos %d", r.Steps, len(r.UndoMatches), len(r.RedoMatches))
	}
	for i, ok := range r.UndoMatches {
		if !ok {
			t.Errorf("undo %d did not give the earlier text and its banner", i+1)
		}
	}
	for i, ok := range r.RedoMatches {
		if !ok {
			t.Errorf("redo %d did not give the later text and its banner", i+1)
		}
	}
	if !strings.Contains(r.BannerWhenWeakened, "APPROVAL GATE WEAKENED") || strings.Contains(r.BannerAfterFirstUndo, "APPROVAL GATE WEAKENED") {
		t.Errorf("the banner: when weakened %q, after undoing the gate removal %q", r.BannerWhenWeakened, r.BannerAfterFirstUndo)
	}
	if !r.UndoDisabledAtStart || r.StateAtStart != "saved" || r.Original != weakFlow || !r.RedoDisabledAtEnd {
		t.Errorf("at the ends: undo disabled %v, state %q, redo disabled %v", r.UndoDisabledAtStart, r.StateAtStart, r.RedoDisabledAtEnd)
	}
	if r.Disk != r.Last || !r.UndoEnabledAfterSave || r.StateAfterUndoPastSave != "unsaved changes" || !r.DiskStill || r.StateAfterRedoToSaved != "saved" {
		t.Errorf("around a save: undo enabled %v, state after undo %q, file untouched %v, state after redo %q", r.UndoEnabledAfterSave, r.StateAfterUndoPastSave, r.DiskStill, r.StateAfterRedoToSaved)
	}
}

package flowedit

import (
	"slices"
	"strings"
	"testing"
)

// TestRemoveNodeTakesAssertionsInChrome: in the page, the removal dialog lists the assertion that names the
// node, the confirmed removal leaves none, the weakened banner and the save dialog name the removed
// assertion, and Undo brings it back. It runs only with BIDE_EDIT_BROWSER=1, as the other Chrome tests do.
func TestRemoveNodeTakesAssertionsInChrome(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", removeFlow)
	var r struct {
		OK              bool     `json:"ok"`
		Error           string   `json:"error"`
		Original        string   `json:"original"`
		RemoveList      []string `json:"removeList"`
		AfterRemove     string   `json:"afterRemove"`
		Banner          string   `json:"banner"`
		SaveDialog      string   `json:"saveDialog"`
		ConfirmBoxes    int      `json:"confirmBoxes"`
		AfterUndo       string   `json:"afterUndo"`
		BannerAfterUndo string   `json:"bannerAfterUndo"`
		Console         []string `json:"console"`
		Failed          []string `json:"failed"`
		OffOrigin       []string `json:"offOrigin"`
	}
	runScript(t, "removeassert.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\nconsole: %q\nfailed requests: %q", r.Error, r.Console, r.Failed)
	}
	if len(r.Console) > 0 || len(r.Failed) > 0 || len(r.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q, failed requests %q, requests off the origin %q", r.Console, r.Failed, r.OffOrigin)
	}
	const line = `assert 2: "audit reachable" is removed (removing an assertion weakens the policy)`
	if !slices.Contains(r.RemoveList, line) {
		t.Errorf("the removal dialog lists %q", r.RemoveList)
	}
	if strings.Contains(r.AfterRemove, "audit") || !strings.Contains(r.AfterRemove, "receipt reachable") {
		t.Errorf("after the removal:\n%s", r.AfterRemove)
	}
	if !strings.Contains(r.Banner, "WEAKENED") || !strings.Contains(r.Banner, "audit reachable") {
		t.Errorf("the banner %q does not name the removed assertion", r.Banner)
	}
	if !strings.Contains(r.SaveDialog, "THIS WEAKENS THE POLICY") || !strings.Contains(r.SaveDialog, "audit reachable") || r.ConfirmBoxes == 0 {
		t.Errorf("the save dialog (%d confirmations): %q", r.ConfirmBoxes, r.SaveDialog)
	}
	if r.AfterUndo != removeFlow || strings.TrimSpace(r.BannerAfterUndo) != "" {
		t.Errorf("undo: banner %q, text restored %v", r.BannerAfterUndo, r.AfterUndo == removeFlow)
	}
}

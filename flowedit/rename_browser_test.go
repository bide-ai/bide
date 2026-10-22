package flowedit

import (
	"strings"
	"testing"
)

// TestRenameInChromeIsNotWeakened: in the page, a rename of a gated node beside a block change leaves
// the banner quiet because the page carries the session's renames; clearing the gate shows the banner;
// Undo and Redo restore the renames each text had; and a save of the renamed text asks for no
// confirmation. It runs only with BIDE_EDIT_BROWSER=1, as the other Chrome tests do.
func TestRenameInChromeIsNotWeakened(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", renameFlow)
	type seen struct {
		Source   string `json:"source"`
		Banner   string `json:"banner"`
		Baseline string `json:"baseline"`
	}
	var r struct {
		OK           bool     `json:"ok"`
		Error        string   `json:"error"`
		Original     string   `json:"original"`
		States       []seen   `json:"states"`
		Undone       []seen   `json:"undone"`
		Redone       []seen   `json:"redone"`
		UndoneAgain  seen     `json:"undoneAgain"`
		ConfirmBoxes int      `json:"confirmBoxes"`
		AfterSave    seen     `json:"afterSave"`
		BackAtStart  bool     `json:"backAtStart"`
		TypedAtStart seen     `json:"typedAtStart"`
		Disk         string   `json:"disk"`
		Console      []string `json:"console"`
		Failed       []string `json:"failed"`
		OffOrigin    []string `json:"offOrigin"`
	}
	runScript(t, "rename.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\nconsole: %q\nfailed requests: %q", r.Error, r.Console, r.Failed)
	}
	if len(r.Console) > 0 || len(r.Failed) > 0 || len(r.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q, failed requests %q, requests off the origin %q", r.Console, r.Failed, r.OffOrigin)
	}
	const note = "Renames made in this session are matched to their baseline nodes: charge -> charge_card."
	quiet := func(what string, x seen, renamed bool) {
		t.Helper()
		if x.Banner != "" || strings.Contains(x.Baseline, "compared with") || !strings.Contains(x.Baseline, "not weaker than") {
			t.Errorf("%s: the banner %q, the baseline line %q: want no banner", what, x.Banner, x.Baseline)
		}
		if got := strings.Contains(x.Baseline, note); got != renamed {
			t.Errorf("%s: the baseline line %q: names the rename = %v, want %v", what, x.Baseline, got, renamed)
		}
	}
	if len(r.States) != 4 || len(r.Undone) != 3 || len(r.Redone) != 3 {
		t.Fatalf("states %d undone %d redone %d", len(r.States), len(r.Undone), len(r.Redone))
	}
	quiet("the opening text", r.States[0], false)
	quiet("after the rename", r.States[1], true)
	quiet("after the rename and the block change", r.States[2], true)
	if b := r.States[3].Banner; !strings.Contains(b, "WEAKENED") || !strings.Contains(b, "charge_card") {
		t.Errorf("the gate cleared on the renamed node: banner %q", b)
	}
	if !strings.Contains(r.States[2].Source, "name: charge_card") || !strings.Contains(r.States[2].Source, "block: charge_v2") {
		t.Errorf("the text after the block change:\n%s", r.States[2].Source)
	}
	// Undo walks back through the lists the texts had.
	quiet("undo of the gate", r.Undone[0], true)
	quiet("undo of the block", r.Undone[1], true)
	quiet("undo of the rename", r.Undone[2], false)
	if r.Undone[2].Source != renameFlow {
		t.Errorf("undo did not restore the opening text")
	}
	quiet("redo of the rename", r.Redone[0], true)
	quiet("redo of the block", r.Redone[1], true)
	if !strings.Contains(r.Redone[2].Banner, "WEAKENED") {
		t.Errorf("redo of the gate: banner %q", r.Redone[2].Banner)
	}
	quiet("undo of the gate again", r.UndoneAgain, true)
	if r.ConfirmBoxes != 0 {
		t.Errorf("a save of a renamed gated node asks for %d confirmations", r.ConfirmBoxes)
	}
	quiet("after the save", r.AfterSave, true)
	if !r.BackAtStart {
		t.Errorf("undo to the start did not give the opening text")
	}
	quiet("typing at the start, after undoing every rename", r.TypedAtStart, false)
	if r.Disk != r.UndoneAgain.Source {
		t.Errorf("the file written:\n%s", r.Disk)
	}
}

package approvecmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/taintfixture"
	"github.com/bide-ai/bide/internal/term"
)

// Probe: bide approve finds the escalation instance of a call as a gate of its own, shows the gate its pause
// note records, and refuses an approver of the other instance.
func TestApprove_DecidesTheEscalationInstanceByItsOwnGate(t *testing.T) {
	ctx := context.Background()
	f := taintfixture.New(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"own1"}})
	_ = f.Drive()
	if err := f.Submit("c2", "c2", "own1", true); err != nil {
		t.Fatal(err)
	}
	_ = f.Drive()

	esc, err := load(ctx, f.Journal, taintfixture.Run, "c2#taint")
	if err != nil {
		t.Fatalf("load(c2#taint): %v", err)
	}
	if esc.call.Name != "write" || esc.gate.Need != 2 || !slices.Equal(esc.gate.Approvers, []string{"esc1", "esc2"}) {
		t.Fatalf("the escalation gate = %s %d of %v, want write 2 of [esc1 esc2]", esc.call.Name, esc.gate.Need, esc.gate.Approvers)
	}
	for _, c := range []struct {
		as      string
		wantErr string
	}{{"esc1", ""}, {"esc2", ""}, {"own1", "not an approver"}, {"other", "not an approver"}} {
		err := esc.preflight(taintfixture.Run, c.as, false, f.Verifiers)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("preflight(%s) on the instance: %v", c.as, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("preflight(%s) on the instance = %v, want %q", c.as, err, c.wantErr)
		}
	}
	// The call's own gate, decided: it takes no more decisions, and its approver set is its own.
	own, err := load(ctx, f.Journal, taintfixture.Run, "c2")
	if err != nil {
		t.Fatalf("load(c2): %v", err)
	}
	if !slices.Equal(own.gate.Approvers, []string{"own1"}) {
		t.Fatalf("the call's gate approvers = %v", own.gate.Approvers)
	}
	if err := own.preflight(taintfixture.Run, "esc1", false, f.Verifiers); err == nil {
		t.Error("an escalation approver was accepted on the call's own gate")
	}
}

// Probe: bide approve -list shows the escalation instance as a gate of its own, with its own approvers,
// and the commands it prints name the instance and its approvers, not the call's.
func TestList_ShowsTheEscalationInstanceWithItsOwnGate(t *testing.T) {
	f := taintfixture.New(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"own1"}})
	_ = f.Drive()
	if err := f.Submit("c2", "c2", "own1", true); err != nil {
		t.Fatal(err)
	}
	_ = f.Drive()
	stub := Stores{Postgres: func(string, bool) (agent.Store, io.Closer, error) { return f.Journal.Store(), io.NopCloser(nil), nil }}
	var out, errb bytes.Buffer
	if code := Main([]string{"-postgres", "postgres://u@h/db", "-list", "-json"}, strings.NewReader(""), &out, &errb, stub); code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	var got []waiting
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if len(got) != 1 || got[0].Call != "c2#taint" || got[0].Tool != "write" || got[0].Need != 2 || !slices.Equal(got[0].Approvers, []string{"esc1", "esc2"}) {
		t.Fatalf("list = %+v, want the one waiting gate c2#taint of write, 2 of [esc1 esc2]", got)
	}
	if len(got[0].Why) != 1 || got[0].Why[0].Kind != "taint" || got[0].Why[0].Source != "crm" || !strings.HasPrefix(got[0].Why[0].Ref, "tool:read/c1") {
		t.Errorf("why = %+v, want the taint reason from crm read by tool read", got[0].Why)
	}
	for _, c := range got[0].Commands {
		if !strings.Contains(c, "c2#taint") || strings.Contains(c, "own1") {
			t.Errorf("command %q does not name the instance and its own approvers", c)
		}
	}
	if len(got[0].Commands) == 0 {
		t.Error("no commands printed for the instance")
	}
}

// Probe: the text of -list and the decision screen say why the escalation instance asks, and mark the reason as
// the application's record, not part of what is signed.
func TestApprove_ShowsWhyTheInstanceAsks(t *testing.T) {
	f := taintfixture.New(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"own1"}})
	_ = f.Drive()
	if err := f.Submit("c2", "c2", "own1", true); err != nil {
		t.Fatal(err)
	}
	_ = f.Drive()
	stub := Stores{Postgres: func(string, bool) (agent.Store, io.Closer, error) { return f.Journal.Store(), io.NopCloser(nil), nil }}
	var out, errb bytes.Buffer
	if code := Main([]string{"-postgres", "postgres://u@h/db", "-list", "-color", "never"}, strings.NewReader(""), &out, &errb, stub); code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "why: untrusted content from crm (read by tool read, call c1, journal position") {
		t.Errorf("the list does not say why:\n%s", out.String())
	}
	// The decision screen.
	esc, err := load(context.Background(), f.Journal, taintfixture.Run, "c2#taint")
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	var screen bytes.Buffer
	c := &cmd{out: &screen, st: term.For(&screen, term.Never), f: flags{as: "esc1", approverKeys: "keys"}}
	c.show(esc, f.Subject("c2#taint"), audit.Ed25519Signer{Priv: priv})
	for _, want := range []string{"why", "untrusted content from crm", "not part of what you sign"} {
		if !strings.Contains(screen.String(), want) {
			t.Errorf("the decision screen lacks %q:\n%s", want, screen.String())
		}
	}
	// The tool's own gate has no reason of its own to show.
	own, err := load(context.Background(), f.Journal, taintfixture.Run, "c2")
	if err != nil {
		t.Fatal(err)
	}
	var ownScreen bytes.Buffer
	c = &cmd{out: &ownScreen, st: term.For(&ownScreen, term.Never), f: flags{as: "own1", approverKeys: "keys"}}
	c.show(own, f.Subject("c2"), audit.Ed25519Signer{Priv: priv})
	if strings.Contains(ownScreen.String(), "untrusted content") {
		t.Errorf("the call's own gate shows the taint reason:\n%s", ownScreen.String())
	}
}

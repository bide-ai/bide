package runview

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/sample"
)

type stopRig struct {
	t     *testing.T
	store agent.Store
	j     *agent.Journal
	ctl   *agent.StopControl
	runs  []sample.Run
}

// newStopRig is a store of the demo runs with a basic control over it.
func newStopRig(t *testing.T) *stopRig {
	t.Helper()
	s := agent.NewMemStore()
	res, err := sample.Write(context.Background(), s, "")
	if err != nil {
		t.Fatal(err)
	}
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopBasic))
	if err != nil {
		t.Fatal(err)
	}
	return &stopRig{t: t, store: s, j: j, ctl: ctl, runs: res.Runs}
}

func (r *stopRig) set(scope agent.StopScope, reason string) agent.StopEntry {
	r.t.Helper()
	e, err := r.ctl.SetStop(context.Background(), agent.StopRequest{Scope: scope, Reason: reason, By: "ops", Host: "ops-1"})
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

func (r *stopRig) handler(feed *StopFeed) http.Handler {
	return Handler(Options{Source: JournalSource{J: r.j}, Stops: feed, Now: func() time.Time { return time.Now() }})
}

func global() agent.StopScope { return agent.StopScope{Kind: agent.StopGlobal} }

func TestStopBanner_AbsentWhenNothingToSay(t *testing.T) {
	r := newStopRig(t)
	h := r.handler(&StopFeed{Control: r.ctl})
	for _, p := range []string{"/", "/attention", "/run?id=" + r.runs[0].ID} {
		if code, body := get(t, h, p); code != 200 || strings.Contains(body, "stopbanner") {
			t.Fatalf("%s: %d, banner on a deployment with no stop", p, code)
		}
	}
	// A viewer given no control run is the viewer it always was.
	h = r.handler(nil)
	if code, body := get(t, h, "/stops.json"); code != 404 || strings.Contains(body, "stopbanner") {
		t.Fatalf("/stops.json without a feed: %d", code)
	}
}

func TestStopBanner_OnEveryPageEscapedAndFirst(t *testing.T) {
	r := newStopRig(t)
	r.set(global(), `model <b>misbehaving</b> & "quoted"`)
	h := r.handler(&StopFeed{Control: r.ctl})
	for _, p := range []string{"/", "/attention", "/run?id=" + r.runs[0].ID} {
		code, body := get(t, h, p)
		if code != 200 {
			t.Fatalf("%s: %d", p, code)
		}
		if strings.Count(body, `class="stopbanner sb-stop"`) != 1 || !strings.Contains(body, "STOPPED") {
			t.Fatalf("%s: want exactly one stop banner:\n%.600s", p, body)
		}
		if !strings.Contains(body, "model &lt;b&gt;misbehaving&lt;/b&gt; &amp; &#34;quoted&#34;") || strings.Contains(body, "<b>misbehaving") {
			t.Errorf("%s: the reason is not escaped", p)
		}
		if !strings.Contains(body, "basic mode, signatures not verified") || !strings.Contains(body, "asserted, not authenticated") {
			t.Errorf("%s: a basic viewer must say signatures are not verified", p)
		}
		if i, j := strings.Index(body, "<main>"), strings.Index(body, "stopbanner"); i < 0 || j < i || j > i+40 {
			t.Errorf("%s: the banner is not the first thing in <main> (main at %d, banner at %d)", p, i, j)
		}
	}
	// A page that is not about a store (a flow config) carries none.
	cfg := Handler(Options{Config: &Config{}})
	if _, body := get(t, cfg, "/"); strings.Contains(body, "stopbanner") {
		t.Fatal("the config page has a stop banner")
	}
}

// What a store supplied is cleaned: control and bidirectional characters cannot restyle the banner.
func TestStopBanner_CleansWhatTheStoreSupplied(t *testing.T) {
	st := agent.StopState{Mode: agent.StopBasic, Stops: []agent.StopEntry{{ID: strings.Repeat("a", 64), Scope: agent.StopScope{Kind: agent.StopTool, Name: "re‮fund"}, Reason: "x\x1b[31m red ⁦ y⁩\nz", By: "o‮ps", At: time.Now().UTC().Format(time.RFC3339Nano)}}}
	b := buildStopBanner(stopRead{st: st}, false, time.Now())
	for _, s := range []string{b.Active[0].Scope, b.Active[0].Reason, b.Active[0].By} {
		for _, bad := range []string{"‮", "⁦", "⁩", "\x1b", "\n"} {
			if strings.Contains(s, bad) {
				t.Errorf("%q still holds %q", s, bad)
			}
		}
	}
	long := buildStopBanner(stopRead{st: agent.StopState{Mode: agent.StopBasic, Stops: []agent.StopEntry{{ID: "x", Reason: strings.Repeat("r", 5000), At: "now"}}}}, false, time.Now())
	if len([]rune(long.Active[0].Reason)) > agent.MaxStopReason+3 {
		t.Errorf("a %d-rune reason", len([]rune(long.Active[0].Reason)))
	}
}

func TestStopBanner_BoundsTheListAndCountsTheRest(t *testing.T) {
	r := newStopRig(t)
	for i := 0; i < maxBannerStops+3; i++ {
		r.set(agent.StopScope{Kind: agent.StopTool, Name: "tool-" + string(rune('a'+i))}, "incident")
	}
	b, _ := Options{Stops: &StopFeed{Control: r.ctl}}.banner(context.Background())
	if len(b.Active) != maxBannerStops || b.MoreActive != 3 || !b.Stopped() {
		t.Fatalf("%d listed, %d more", len(b.Active), b.MoreActive)
	}
}

// failingLoad fails reads of the control run.
type failingLoad struct{ agent.Store }

func (f failingLoad) Load(ctx context.Context, run string, after int64) iter.Seq2[agent.Entry, error] {
	if run == agent.StopControlRun {
		return func(yield func(agent.Entry, error) bool) {
			yield(agent.Entry{}, errors.New("disk on fire: secret-detail"))
		}
	}
	return f.Store.Load(ctx, run, after)
}

// An unreadable control run is never "no stop": the banner says it cannot say, by class, with no detail.
func TestStopBanner_UnreadableControlRunIsSaid(t *testing.T) {
	r := newStopRig(t)
	r.set(global(), "incident")
	j, _ := agent.NewJournal(failingLoad{r.store})
	ctl, _ := agent.NewStopControl(j, agent.WithStopMode(agent.StopBasic))
	h := Handler(Options{Source: JournalSource{J: r.j}, Stops: &StopFeed{Control: ctl}})
	code, body := get(t, h, "/")
	if code != 200 || !strings.Contains(body, "could not be read") || !strings.Contains(body, "cannot say whether a stop is in force") {
		t.Fatalf("%d\n%.800s", code, body)
	}
	if strings.Contains(body, "secret-detail") {
		t.Fatal("the read error's detail is on the page")
	}
}

func auditedPolicy(t *testing.T, ids ...string) (agent.StopPolicy, map[string]audit.Ed25519Signer) {
	t.Helper()
	pubs := map[string]ed25519.PublicKey{}
	signers := map[string]audit.Ed25519Signer{}
	for _, id := range ids {
		pub, priv, _ := ed25519.GenerateKey(nil)
		pubs[id], signers[id] = pub, audit.Ed25519Signer{Priv: priv}
	}
	return agent.StopPolicy{DeploymentID: "prod-91c2", Setters: ids[:1], Lifters: ids, LiftQuorum: 1,
		Verifiers: func(id string) (agent.ApproverVerifier, bool) {
			p, ok := pubs[id]
			return audit.Ed25519Verifier{Pub: p}, ok
		}}, signers
}

func auditedControl(t *testing.T, j *agent.Journal, pol agent.StopPolicy) *agent.StopControl {
	t.Helper()
	ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(pol), agent.WithStopVerifier(audit.StopVerifierFromAudit))
	if err != nil {
		t.Fatal(err)
	}
	return ctl
}

// A viewer with no stop policy cannot check signed records: it says so, and says the stops are unknowable. A
// viewer given the policy shows the stop as signed, and shows a forged record as rejected, not as a stop.
func TestStopBanner_SignedDeploymentBasicAndAuditedViewers(t *testing.T) {
	r := newStopRig(t)
	pol, signers := auditedPolicy(t, "ops")
	actl := auditedControl(t, r.j, pol)
	if _, err := actl.SetStop(context.Background(), agent.StopRequest{Scope: global(), Reason: "incident", By: "ops", Signer: signers["ops"]}); err != nil {
		t.Fatal(err)
	}

	basic, _ := Options{Stops: &StopFeed{Control: r.ctl}}.banner(context.Background())
	if basic.Unknowable == "" || basic.Unverified == 0 || basic.Verified {
		t.Fatalf("a basic viewer on a signed deployment: %+v", basic)
	}
	_, body := get(t, r.handler(&StopFeed{Control: r.ctl}), "/")
	if !strings.Contains(body, "Stops unknowable") || !strings.Contains(body, "Signatures not verified") {
		t.Fatalf("the basic viewer's page must say so:\n%.900s", body)
	}

	aud, _ := Options{Stops: &StopFeed{Control: actl, Verified: true}}.banner(context.Background())
	if len(aud.Active) != 1 || !aud.Active[0].Signed || !aud.Verified || aud.Unknowable != "" || aud.RejectedTotal != 0 {
		t.Fatalf("an audited viewer: %+v", aud)
	}
	_, body = get(t, r.handler(&StopFeed{Control: actl, Verified: true}), "/")
	if !strings.Contains(body, "audited mode") || strings.Contains(body, "signatures not verified") || strings.Contains(body, "asserted, not authenticated") {
		t.Fatalf("the audited viewer's page:\n%.900s", body)
	}

	// An attacker with a policy of their own writes records into the control run: rejected, never a stop.
	evil, esigners := auditedPolicy(t, "mallory")
	ectl := auditedControl(t, r.j, evil)
	_, _ = ectl.SetStop(context.Background(), agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopTool, Name: "refund"}, Reason: "denial of service", By: "mallory", Signer: esigners["mallory"]})
	aud, _ = Options{Stops: &StopFeed{Control: actl, Verified: true}}.banner(context.Background())
	if len(aud.Active) != 1 || aud.RejectedTotal == 0 || len(aud.Rejected) == 0 {
		t.Fatalf("a forged stop must be rejected and listed, not active: %+v", aud)
	}
	if _, body := get(t, r.handler(&StopFeed{Control: actl, Verified: true}), "/"); !strings.Contains(body, "rejected and") {
		t.Fatal("the page does not show the rejected records")
	}
}

func TestStopsJSON(t *testing.T) {
	r := newStopRig(t)
	e := r.set(agent.StopScope{Kind: agent.StopTool, Name: "refund"}, "provider incident")
	code, body := get(t, r.handler(&StopFeed{Control: r.ctl}), "/stops.json")
	var b StopBanner
	if err := json.Unmarshal([]byte(body), &b); code != 200 || err != nil || len(b.Active) != 1 || b.Active[0].ID != e.ID || b.Active[0].Scope != "tool refund" || b.Mode != "basic" || b.Verified {
		t.Fatalf("%d %v %+v", code, err, b)
	}
}

// The control run is not a run: it is not listed, counted, or an item that needs attention.
func TestControlRunIsNotARun(t *testing.T) {
	r := newStopRig(t)
	before := listOf(t, r.handler(nil), "")
	r.set(global(), "incident")
	r.set(agent.StopScope{Kind: agent.StopTool, Name: "refund"}, "incident")
	for _, feed := range []*StopFeed{nil, {Control: r.ctl}} {
		h := r.handler(feed)
		l := listOf(t, h, "")
		if l.Listed != before.Listed || l.All != before.All || slices.Contains(ids(l), agent.StopControlRun) {
			t.Fatalf("the control run is listed: %d runs, was %d", l.Listed, before.Listed)
		}
		_, body := get(t, h, "/attention.json")
		if strings.Contains(body, agent.StopControlRun) {
			t.Fatal("the control run is in the Needs attention list")
		}
	}
	src := JournalSource{J: r.j}
	for name, fn := range map[string]func(context.Context) ([]string, error){"Runs": src.Runs, "Unfinished": src.Unfinished} {
		got, err := fn(context.Background())
		if err != nil || slices.Contains(got, agent.StopControlRun) {
			t.Errorf("%s lists the control run: %v %v", name, got, err)
		}
	}
}

func viewsOf(t *testing.T, r *stopRig) map[string]*View {
	t.Helper()
	out := map[string]*View{}
	for _, run := range r.runs {
		recs, err := r.j.History(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		out[run.ID] = Build(context.Background(), run.ID, recs, nil)
	}
	return out
}

func stateOf(t *testing.T, r *stopRig) agent.StopState {
	t.Helper()
	st, err := r.ctl.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A global stop holds every run that is neither over nor halted; a lifted stop holds nothing.
func TestHeldBy_GlobalAndLifted(t *testing.T) {
	r := newStopRig(t)
	e := r.set(global(), "incident")
	views := viewsOf(t, r)
	held, free := 0, 0
	for id, v := range views {
		h := heldBy(stateOf(t, r), v)
		switch {
		case ended(v.State) || v.State == "halted" || v.Kind == KindSteps:
			if len(h) != 0 {
				t.Errorf("%s (%s) is held by a stop though it cannot be driven by one", id, v.State)
			}
			free++
		default:
			if len(h) != 1 || h[0].May || h[0].Stop != e.ID {
				t.Errorf("%s (%s): %+v", id, v.State, h)
			}
			held++
		}
	}
	if held == 0 || free == 0 {
		t.Fatalf("the demo store must have both kinds of run: %d held, %d free", held, free)
	}
	if _, err := r.ctl.LiftStop(context.Background(), e.ID, agent.LiftRequest{By: "ops"}); err != nil {
		t.Fatal(err)
	}
	for id, v := range views {
		if h := heldBy(stateOf(t, r), v); len(h) != 0 {
			t.Errorf("%s is held by a lifted stop: %+v", id, h)
		}
	}
}

func TestHeldBy_FlowToolAndAgentScopes(t *testing.T) {
	r := newStopRig(t)
	views := viewsOf(t, r)
	var flowName, openTool string
	for _, v := range views {
		if !ended(v.State) && v.State != "halted" && v.Kind != KindSteps {
			if flowName == "" && v.Flow != "" {
				flowName = v.Flow
			}
			if openTool == "" && len(v.openTools) > 0 {
				openTool = v.openTools[0]
			}
		}
	}
	if flowName == "" || openTool == "" {
		t.Fatalf("the demo store has an unfinished flow run (%q) and an unfinished run with an open call (%q)", flowName, openTool)
	}
	r.set(agent.StopScope{Kind: agent.StopFlow, Name: flowName}, "flow incident")
	r.set(agent.StopScope{Kind: agent.StopTool, Name: openTool}, "tool incident")
	r.set(agent.StopScope{Kind: agent.StopTool, Name: "no-such-tool"}, "unrelated")
	r.set(agent.StopScope{Kind: agent.StopAgent, Name: "exec-agent@1"}, "agent incident")
	st := stateOf(t, r)
	for id, v := range views {
		h := heldBy(st, v)
		unfinished := !ended(v.State) && v.State != "halted" && v.Kind != KindSteps
		var flow, tool, ag, other bool
		for _, x := range h {
			switch x.Scope {
			case "flow " + flowName:
				flow = true
			case "tool " + openTool:
				tool = true
			case "agent exec-agent@1":
				ag = true
				if !x.May {
					t.Errorf("%s: an agent stop must be 'may', the journal cannot say who drives the run", id)
				}
			default:
				other = true
			}
		}
		if other {
			t.Errorf("%s: held by a stop that does not apply: %+v", id, h)
		}
		if flow != (unfinished && v.Flow == flowName) {
			t.Errorf("%s: flow stop held=%v for flow %q state %s", id, flow, v.Flow, v.State)
		}
		if tool != (unfinished && slices.Contains(v.openTools, openTool)) {
			t.Errorf("%s: tool stop held=%v, open tools %v", id, tool, v.openTools)
		}
		if ag != unfinished {
			t.Errorf("%s: agent stop 'may' = %v for a %s run", id, ag, v.State)
		}
	}
}

// The run list and the run page say which runs a stop holds, in JSON and in HTML; with no feed they say nothing.
func TestHeldByInTheRunListAndRunPage(t *testing.T) {
	r := newStopRig(t)
	r.set(global(), "incident")
	h := r.handler(&StopFeed{Control: r.ctl})
	l := listOf(t, h, "")
	held := 0
	var heldID string
	for _, row := range l.Rows {
		if len(row.Held) > 0 {
			held++
			heldID = row.ID
			if row.Group == "completed" || row.Group == "halted" {
				t.Errorf("%s (%s) is shown held", row.ID, row.Group)
			}
		}
	}
	if held == 0 {
		t.Fatal("no run is shown held under a global stop")
	}
	if v := viewOf(t, h, heldID); len(v.Held) != 1 {
		t.Fatalf("run.json held_by = %+v", v.Held)
	}
	if _, body := get(t, h, "/run?id="+heldID); !strings.Contains(body, "held by stop") {
		t.Fatal("the run page does not say the run is held")
	}
	if _, body := get(t, h, "/"); !strings.Contains(body, "held by stop") {
		t.Fatal("the run list does not say a run is held")
	}
	plain := listOf(t, r.handler(nil), "")
	for _, row := range plain.Rows {
		if len(row.Held) > 0 {
			t.Fatalf("%s: a viewer with no control run shows a hold", row.ID)
		}
	}
}

type noCloser struct{}

func (noCloser) Close() error { return nil }

// bide view -db -stop-policy: the flags build the feed, the served pages carry the banner, and the viewer
// judges the control run as an audited deployment does (a stop under another policy is rejected).
func TestMainServesTheStopBanner(t *testing.T) {
	r := newStopRig(t)
	pol, signers := auditedPolicy(t, "ops")
	actl := auditedControl(t, r.j, pol)
	if _, err := actl.SetStop(context.Background(), agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopTool, Name: "refund"}, Reason: "provider incident", By: "ops", Signer: signers["ops"]}); err != nil {
		t.Fatal(err)
	}
	doc, err := pol.Doc()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(doc)
	policy := filepath.Join(t.TempDir(), "stop-policy.json")
	if err := os.WriteFile(policy, b, 0o600); err != nil {
		t.Fatal(err)
	}
	stores := Stores{SQLite: func(string) (agent.Store, io.Closer, error) { return r.store, noCloser{}, nil }}
	db := filepath.Join(t.TempDir(), "x.db")

	var audited, plain, js string
	code, _, errs := serveMainStores(t, stores, []string{"-db", db, "-stop-policy", policy, "-addr", "127.0.0.1:0", "-color", "never"}, func(base string) {
		_, audited = fetch(t, base)
		_, js = fetch(t, base+"stops.json")
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(audited, "STOPPED") || !strings.Contains(audited, "tool refund") || !strings.Contains(audited, "audited mode") || strings.Contains(audited, "signatures not verified") {
		t.Fatalf("the audited viewer's run list:\n%.900s", audited)
	}
	var banner StopBanner
	if err := json.Unmarshal([]byte(js), &banner); err != nil || !banner.Verified || len(banner.Active) != 1 || !banner.Active[0].Signed {
		t.Fatalf("stops.json %v %+v", err, banner)
	}
	// Without the policy the same store reads as a basic one, and says so.
	code, _, errs = serveMainStores(t, stores, []string{"-db", db, "-addr", "127.0.0.1:0", "-color", "never"}, func(base string) { _, plain = fetch(t, base) })
	if code != 0 || !strings.Contains(plain, "Stops unknowable") || !strings.Contains(plain, "Signatures not verified") {
		t.Fatalf("the basic viewer on a signed deployment (exit %d, %s):\n%.900s", code, errs, plain)
	}
}

func TestViewStopFlagUsageErrors(t *testing.T) {
	in := newCLIInputs(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"root alone", []string{"-db", "x.db", "-stop-root", strings.Repeat("a", 64)}, "-stop-root needs -stop-policy"},
		{"with an export", []string{"-journal", in.journal, "-stop-policy", "p.json"}, "exported journal holds none"},
	} {
		var out, errb bytes.Buffer
		if code := Main(tc.args, &out, &errb, Stores{}); code != 2 || !strings.Contains(errb.String(), tc.want) {
			t.Errorf("%s: exit %d\n%s", tc.name, code, errb.String())
		}
	}
	var out, errb bytes.Buffer
	stores := Stores{SQLite: func(string) (agent.Store, io.Closer, error) { return agent.NewMemStore(), noCloser{}, nil }}
	if code := Main([]string{"-db", "x.db", "-stop-policy", filepath.Join(t.TempDir(), "none.json"), "-addr", "127.0.0.1:0"}, &out, &errb, stores); code != 1 || !strings.Contains(errb.String(), "none.json") {
		t.Errorf("a policy that does not exist: exit %d\n%s", code, errb.String())
	}
}

// openTools are the tools of calls the journal shows no result for, and only those.
func TestBuildOpenTools(t *testing.T) {
	recs := []agent.Record{
		{Name: "run:start", Kind: agent.StepValue, Result: json.RawMessage(`{"input":{"role":"user","parts":[{"type":"text","text":"go"}]}}`)},
		{Name: "m0", Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{}`)}, agent.ToolUse{ID: "c2", Name: "refund", Args: json.RawMessage(`{}`)}}}},
		{Name: "tool:c1", Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`"ok"`)},
	}
	v := Build(context.Background(), "r", recs, nil)
	if !slices.Equal(v.openTools, []string{"refund"}) {
		t.Fatalf("openTools = %v, want [refund]", v.openTools)
	}
}

// An audited viewer on a deployment that has not upgraded: plain stops are in force for the deployment's basic
// processes and ignored by audited ones. The banner says so instead of saying nothing is stopped.
func TestStopBanner_AuditedViewerOnABasicDeployment(t *testing.T) {
	r := newStopRig(t)
	r.set(global(), "incident")
	pol, _ := auditedPolicy(t, "ops")
	actl := auditedControl(t, r.j, pol)
	feed := &StopFeed{Control: actl, Plain: r.ctl, Verified: true}
	b, _ := Options{Stops: feed}.banner(context.Background())
	if b.PlainIgnored != 1 || b.Stopped() || !b.Show() {
		t.Fatalf("banner %+v", b)
	}
	_, body := get(t, r.handler(feed), "/")
	if !strings.Contains(body, "STOPPED FOR BASIC PROCESSES") || !strings.Contains(body, "ignored by audited ones") || !strings.Contains(body, `class="stopbanner sb-stop"`) {
		t.Fatalf("the page does not say it:\n%.900s", body)
	}
	_, js := get(t, r.handler(feed), "/stops.json")
	if !strings.Contains(js, `"plain_ignored": 1`) {
		t.Fatalf("stops.json: %s", js)
	}
	// An audited viewer with nothing plain in force says nothing about it.
	r2 := newStopRig(t)
	pol2, _ := auditedPolicy(t, "ops")
	b2, _ := Options{Stops: &StopFeed{Control: auditedControl(t, r2.j, pol2), Plain: r2.ctl, Verified: true}}.banner(context.Background())
	if b2.PlainIgnored != 0 || b2.Show() {
		t.Fatalf("banner %+v", b2)
	}
}

// The banner says when the control run was read: it is served again for a couple of seconds.
func TestStopBanner_SaysWhenItWasRead(t *testing.T) {
	r := newStopRig(t)
	r.set(global(), "incident")
	before := time.Now().Add(-time.Second)
	h := r.handler(&StopFeed{Control: r.ctl})
	_, body := get(t, h, "/")
	_, js := get(t, h, "/stops.json")
	var b StopBanner
	if err := json.Unmarshal([]byte(js), &b); err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339, b.ReadAt)
	if err != nil || at.Before(before.Truncate(time.Second)) || !strings.Contains(body, "read "+b.ReadAt) {
		t.Fatalf("read_at %q (%v)\n%s", b.ReadAt, err, body)
	}
}

// countingLister counts the runs a listing yields.
type countingLister struct {
	agent.Store
	yielded *int
}

func (c countingLister) Runs(ctx context.Context, f agent.RunFilter) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		l, _ := agent.Capability[agent.Lister](c.Store)
		for id, err := range l.Runs(ctx, f) {
			*c.yielded++
			if !yield(id, err) {
				return
			}
		}
	}
}

// Counting held runs lists at most maxHeldRead+1 runs, never the whole store, and gives up when its context
// ends instead of holding up a lift.
func TestHeldCount_IsBoundedAndHonorsItsContext(t *testing.T) {
	r := newStopRig(t)
	yielded := 0
	src := JournalSource{J: func() *agent.Journal { j, _ := agent.NewJournal(countingLister{r.store, &yielded}); return j }()}
	ids, err := src.UnfinishedLimit(context.Background(), 3)
	if err != nil || len(ids) != 3 || yielded > 4 {
		t.Fatalf("%d ids, %d yielded, %v: the listing must stop at the limit", len(ids), yielded, err)
	}
	if all, _ := src.UnfinishedLimit(context.Background(), 0); len(all) <= 3 {
		t.Fatalf("no limit lists everything: %d", len(all))
	}
	e := r.set(global(), "incident")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c := HeldCount(ctx, src, e); c.ListErr == "" || c.Held != 0 {
		t.Fatalf("a cancelled count must say so: %+v", c)
	}
	if c := HeldCount(context.Background(), src, e); c.ListErr != "" || c.Held == 0 {
		t.Fatalf("%+v", c)
	}
}

// A saga that is rolling back keeps compensating under a stop that does not block compensation: it is only
// "may be held". A stop that blocks compensation holds it.
func TestHeldBy_SagaRollback(t *testing.T) {
	r := newStopRig(t)
	recs := []agent.Record{
		{Name: "run:start", Kind: agent.StepValue, Result: json.RawMessage(`{"input":{"role":"user","parts":[{"type":"text","text":"go"}]},"saga":true}`)},
		{Name: "m0", Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "c1", Name: "charge", Args: json.RawMessage(`{}`)}, agent.ToolUse{ID: "c2", Name: "ship", Args: json.RawMessage(`{}`)}}}},
		{Name: "tool:c1", Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`"ok"`)},
		{Name: "saga:c2", Kind: agent.StepSagaFail, ToolUseID: "c2", Result: json.RawMessage(`"failed"`)},
	}
	v := Build(context.Background(), "saga-1", recs, nil)
	if !v.rollingBack {
		t.Fatalf("a saga with a failed step is rolling back: state %s", v.State)
	}
	plain := r.set(agent.StopScope{Kind: agent.StopFlow, Name: "x"}, "unused")
	_ = plain
	stGlobal := agent.StopState{Mode: agent.StopBasic, Stops: []agent.StopEntry{{ID: strings.Repeat("a", 64), Scope: global(), Compensation: agent.CompensationAllow}}}
	if h := heldBy(stGlobal, v); len(h) != 1 || !h[0].May {
		t.Fatalf("a stop that lets compensation continue: %+v", h)
	}
	stBlock := agent.StopState{Mode: agent.StopBasic, Stops: []agent.StopEntry{{ID: strings.Repeat("b", 64), Scope: global(), Compensation: agent.CompensationBlock}}}
	if h := heldBy(stBlock, v); len(h) != 1 || h[0].May {
		t.Fatalf("a stop that blocks compensation: %+v", h)
	}
	// Not a saga, or one that has not failed: held for certain.
	v2 := Build(context.Background(), "plain-1", recs[:3], nil)
	if v2.rollingBack {
		t.Fatal("a saga with no failed step is not rolling back")
	}
	if h := heldBy(stGlobal, v2); len(h) != 1 || h[0].May {
		t.Fatalf("%+v", h)
	}
}

// A plain stop under an audited viewer is enough for a banner by itself.
func TestStopBanner_PlainIgnoredAloneShowsABanner(t *testing.T) {
	b := buildStopBanner(stopRead{st: agent.StopState{Mode: agent.StopAudited}, plain: 2}, true, time.Now())
	if b.PlainIgnored != 2 || !b.Show() {
		t.Fatalf("%+v", b)
	}
}

// cancelOnLoad cancels a context the first time a run's records are read, as a deadline passing mid-count.
type cancelOnLoad struct {
	agent.Store
	cancel context.CancelFunc
}

func (c cancelOnLoad) Load(ctx context.Context, run string, after int64) iter.Seq2[agent.Entry, error] {
	c.cancel()
	return c.Store.Load(ctx, run, after)
}

func (c cancelOnLoad) Runs(ctx context.Context, f agent.RunFilter) iter.Seq2[string, error] {
	l, _ := agent.Capability[agent.Lister](c.Store)
	return l.Runs(ctx, f)
}

func TestHeldCount_CapAndDeadlineMidCount(t *testing.T) {
	r := newStopRig(t)
	e := r.set(global(), "incident")
	old := maxHeldRead
	maxHeldRead = 3
	defer func() { maxHeldRead = old }()
	if c := HeldCount(context.Background(), JournalSource{J: r.j}, e); !c.Truncated || c.Read != 3 {
		t.Fatalf("the cap: %+v", c)
	}
	maxHeldRead = old
	ctx, cancel := context.WithCancel(context.Background())
	j, _ := agent.NewJournal(cancelOnLoad{r.store, cancel})
	if c := HeldCount(ctx, JournalSource{J: j}, e); c.ListErr == "" || c.Read > 1 {
		t.Fatalf("a deadline mid-count must stop it: %+v", c)
	}
}

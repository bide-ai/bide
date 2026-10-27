package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/journalhook"
)

// The hooks by which package plan reaches flow taint (flow_taint_hooks.go): what a drive records at the run's
// start, what it compares on every drive, and what a node requires before it runs.

func hookDecls(mut func(*journalhook.FlowTaint)) *journalhook.FlowTaint {
	d := &journalhook.FlowTaint{
		Gate: &ApprovalPolicy{Need: 2, Approvers: []string{"a", "b", "c"}},
		Nodes: map[string]journalhook.FlowTaintNode{
			"fetch": {Untrusted: []string{"inbound-email"}},
			"look":  {Trusted: "our table"},
			"safe":  {Exempt: "reviewed"},
		},
	}
	if mut != nil {
		mut(d)
	}
	return d
}

func str(s string) *string { return &s }

// startFlowRun prepares and begins a flow run the way package plan does, with options for the input
// provenance, and returns the journal.
func startFlowRun(t *testing.T, d *journalhook.FlowTaint, untrusted *string) *Journal {
	t.Helper()
	ctx := context.Background()
	j := memJournal()
	start := RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: UserText("1"), Settings: RunSettings{UntrustedInput: untrusted}}
	got, err := flowTaintPrepare(ctx, j, "r1", start, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := beginRun(ctx, j, "r1", got.(RunStart)); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestFlowTaintPrepare(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	base := RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}
	// No policy: the start is unchanged.
	got, err := flowTaintPrepare(ctx, j, "r1", base, nil)
	if err != nil || got.(RunStart).taintDigest() != "" {
		t.Fatalf("no declarations: %v, digest %q", err, got.(RunStart).taintDigest())
	}
	// With a policy: the declarations are recorded, normalised.
	got, err = flowTaintPrepare(ctx, j, "r1", base, hookDecls(nil))
	if err != nil {
		t.Fatal(err)
	}
	decls, ok, err := got.(RunStart).flowTaintDecls()
	if err != nil || !ok || decls.Policy.Gate.Need != 2 || decls.Nodes["fetch"].Untrusted[0] != "inbound-email" || decls.Nodes["safe"].Exempt != "reviewed" {
		t.Fatalf("recorded declarations %+v, %v, %v", decls, ok, err)
	}
	if _, err := flowTaintPrepare(ctx, j, "r1", "not a start", hookDecls(nil)); !errors.Is(err, ErrConfig) {
		t.Errorf("a start that is not a RunStart: %v", err)
	}
	if _, err := flowTaintPrepare(ctx, j, "r1", base, &journalhook.FlowTaint{Gate: "x"}); !errors.Is(err, ErrConfig) {
		t.Errorf("a gate that is not a policy: %v", err)
	}
}

func TestFlowTaintPrepare_RequireProvenance(t *testing.T) {
	ctx := context.Background()
	req := hookDecls(func(d *journalhook.FlowTaint) { d.RequireProvenance = true })
	mk := func(un, tr *string) RunStart {
		return RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: UserText("1"), Settings: RunSettings{UntrustedInput: un, TrustedInput: tr}}
	}
	j := memJournal()
	// A new run that states neither is refused before anything is written.
	if _, err := flowTaintPrepare(ctx, j, "r1", mk(nil, nil), req); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "WithUntrustedInput") {
		t.Fatalf("a new run stating nothing: %v, want errInputProvenance", err)
	}
	if recs, _ := j.History(ctx, "r1"); len(recs) != 0 {
		t.Errorf("the refused drive wrote %d records", len(recs))
	}
	for what, s := range map[string]RunStart{"untrusted": mk(str("mail"), nil), "trusted": mk(nil, str("ours"))} {
		if _, err := flowTaintPrepare(ctx, j, "r2", s, req); err != nil {
			t.Errorf("a run stating its input %s: %v", what, err)
		}
	}
	// A run that began with a statement is resumed by a drive that states none.
	got, _ := flowTaintPrepare(ctx, j, "r3", mk(str("mail"), nil), req)
	if _, _, _, err := beginRun(ctx, j, "r3", got.(RunStart)); err != nil {
		t.Fatal(err)
	}
	if _, err := flowTaintPrepare(ctx, j, "r3", mk(nil, nil), req); err != nil {
		t.Errorf("a resume that states nothing: %v", err)
	}
	// Without RequireProvenance nothing is asked.
	if _, err := flowTaintPrepare(ctx, j, "r4", mk(nil, nil), hookDecls(nil)); err != nil {
		t.Errorf("no RequireProvenance: %v", err)
	}
}

func TestFlowTaintEnter(t *testing.T) {
	ctx := context.Background()
	under := startFlowRun(t, hookDecls(nil), nil)
	plain := startFlowRun(t, nil, nil)
	for what, c := range map[string]struct {
		j    *Journal
		d    *journalhook.FlowTaint
		want int
		err  bool
	}{
		"the same declarations":        {under, hookDecls(nil), journalhook.FlowTaintUnder, false},
		"no policy now: policyless":    {under, nil, journalhook.FlowTaintPolicyless, false},
		"a weaker gate":                {under, hookDecls(func(d *journalhook.FlowTaint) { d.Gate = &ApprovalPolicy{Need: 1, Approvers: []string{"a", "b", "c"}} }), 0, true},
		"a dropped source":             {under, hookDecls(func(d *journalhook.FlowTaint) { d.Nodes["fetch"] = journalhook.FlowTaintNode{} }), 0, true},
		"a new exemption":              {under, hookDecls(func(d *journalhook.FlowTaint) { d.Nodes["pay"] = journalhook.FlowTaintNode{Exempt: "x"} }), 0, true},
		"no policy then, none now":     {plain, nil, journalhook.FlowTaintNone, false},
		"no policy then, a policy now": {plain, hookDecls(nil), journalhook.FlowTaintNone, false},
	} {
		got, err := flowTaintEnter(ctx, c.j, "r1", c.d)
		if (err != nil) != c.err || err == nil && got != c.want {
			t.Errorf("%s: standing %d, err %v; want %d, err %v", what, got, err, c.want, c.err)
		}
		if err != nil && !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v is not ErrConfig", what, err)
		}
	}
	// A run with no recorded start has nothing to compare with.
	if _, err := flowTaintEnter(ctx, memJournal(), "none", hookDecls(nil)); !errors.Is(err, ErrStorage) {
		t.Errorf("no recorded start: %v, want ErrStorage", err)
	}
}

// A flow drive that states the input's provenance must state the one the run started with; one that states
// nothing runs under the recorded one.
func TestBeginRun_FlowInputProvenanceIsHeld(t *testing.T) {
	ctx := context.Background()
	j := startFlowRun(t, hookDecls(nil), str("mail"))
	mk := func(un, tr *string) RunStart {
		return RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: UserText("1"), Settings: RunSettings{UntrustedInput: un, TrustedInput: tr}}
	}
	for what, s := range map[string]RunStart{"another source": mk(str("web"), nil), "trusted instead": mk(nil, str("ours"))} {
		if _, _, _, err := beginRun(ctx, j, "r1", s); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v, want ErrConfig", what, err)
		}
	}
	for what, s := range map[string]RunStart{"the same source": mk(str("mail"), nil), "nothing": mk(nil, nil)} {
		if _, _, _, err := beginRun(ctx, j, "r1", s); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	// A run that began stating none refuses a drive that states one.
	j2 := startFlowRun(t, nil, nil)
	if _, _, _, err := beginRun(ctx, j2, "r1", mk(str("mail"), nil)); !errors.Is(err, ErrConfig) {
		t.Errorf("a source stated on a run that began with none: %v, want ErrConfig", err)
	}
}

func TestFlowRunProvenance(t *testing.T) {
	un, tr, err := flowRunProvenance([]any{WithUntrustedInput("mail")})
	if err != nil || un == nil || *un != "mail" || tr != nil {
		t.Fatalf("untrusted: %v %v %v", un, tr, err)
	}
	un, tr, err = flowRunProvenance([]any{WithTrustedInput("ours")})
	if err != nil || tr == nil || *tr != "ours" || un != nil {
		t.Fatalf("trusted: %v %v %v", un, tr, err)
	}
	if _, _, err := flowRunProvenance([]any{WithUntrustedInput("a"), WithTrustedInput("b")}); !errors.Is(err, ErrConfig) {
		t.Errorf("both: %v", err)
	}
	if _, _, err := flowRunProvenance([]any{WithUntrustedInput("")}); !errors.Is(err, ErrConfig) {
		t.Errorf("an empty source: %v", err)
	}
	// The options a flow run takes: identity, and the two provenance options; nothing else.
	if _, _, _, _, err := flowRunIdentity([]any{WithIdentity(Identity{Actor: "x"}), WithUntrustedInput("mail"), WithTrustedInput("x")}); err != nil {
		t.Errorf("flowRunIdentity with the three options: %v", err)
	}
	if _, _, _, _, err := flowRunIdentity([]any{WithMaxTurns(3)}); !errors.Is(err, ErrConfig) {
		t.Errorf("flowRunIdentity with a turn limit: %v, want ErrConfig", err)
	}
}

// ---- the node decision ----

func flowRec(name string) Record {
	return Record{Name: name, Kind: StepValue, Result: json.RawMessage(`1`)}
}

func put(t *testing.T, j *Journal, name string) {
	t.Helper()
	writeRaw(t, j, "r1", name, flowRec(name))
}

func nodeReasons(t *testing.T, j *Journal, key string) (gateReasonRecord, bool) {
	t.Helper()
	rec, ok, err := getGateReason(context.Background(), j, "r1", key)
	if err != nil {
		t.Fatal(err)
	}
	return rec, ok
}

func decide(j *Journal, key string, s Safety, standing int) (journalhook.FlowTaintDecision, error) {
	return flowTaintNode(context.Background(), j, "r1", key, s, standing)
}

func TestFlowTaintNode(t *testing.T) {
	side := Safety{}
	ro := Safety{ReadOnly: true}
	under := journalhook.FlowTaintUnder

	t.Run("no mark, no requirement and no record", func(t *testing.T) {
		j := startFlowRun(t, hookDecls(nil), nil)
		d, err := decide(j, "node:pay", side, under)
		if err != nil || d.Gate != nil {
			t.Fatalf("decision %+v, %v", d, err)
		}
		if _, ok := nodeReasons(t, j, "node:pay"); ok {
			t.Error("a gate-reason record was written for a node that read nothing untrusted")
		}
	})

	t.Run("after a source, the escalation gate; the record is written once", func(t *testing.T) {
		j := startFlowRun(t, hookDecls(nil), nil)
		put(t, j, "node:fetch")
		d, err := decide(j, "node:pay", side, under)
		g, _ := d.Gate.(*ApprovalPolicy)
		if err != nil || g == nil || g.Need != 2 {
			t.Fatalf("decision %+v, %v; want the 2-of-3 gate", d, err)
		}
		rec, ok := nodeReasons(t, j, "node:pay")
		if !ok || rec.Refused || len(rec.Reasons) != 1 || rec.Reasons[0].Source != "inbound-email" || rec.Gate == nil {
			t.Fatalf("record %+v, %v", rec, ok)
		}
		recs, _ := j.History(context.Background(), "r1")
		n := len(recs)
		if _, err := decide(j, "node:pay", side, under); err != nil {
			t.Fatal(err)
		}
		if recs, _ = j.History(context.Background(), "r1"); len(recs) != n {
			t.Errorf("the second decision wrote %d records", len(recs)-n)
		}
	})

	t.Run("an Idempotent node needs the gate too: it is not read-only", func(t *testing.T) {
		j := startFlowRun(t, hookDecls(nil), nil)
		put(t, j, "node:fetch")
		d, err := decide(j, "node:pay", Safety{Idempotent: true}, under)
		if err != nil || d.Gate == nil {
			t.Fatalf("decision %+v, %v", d, err)
		}
	})

	t.Run("a read-only node needs nothing and leaves nothing", func(t *testing.T) {
		j := startFlowRun(t, hookDecls(nil), nil)
		put(t, j, "node:fetch")
		d, err := decide(j, "node:look", ro, under)
		if err != nil || d.Gate != nil {
			t.Fatalf("decision %+v, %v", d, err)
		}
		if _, ok := nodeReasons(t, j, "node:look"); ok {
			t.Error("a record was written for a read-only node")
		}
	})

	t.Run("Refuse: the node is refused, the refusal is recorded, no node record is written, and it is refused again", func(t *testing.T) {
		j := startFlowRun(t, hookDecls(func(d *journalhook.FlowTaint) { d.Gate, d.Refuse = nil, true }), nil)
		put(t, j, "node:fetch")
		for i := 0; i < 2; i++ {
			_, err := decide(j, "node:pay", side, under)
			if !errors.Is(err, journalhook.ErrTaintRefused) || !strings.Contains(err.Error(), "inbound-email") {
				t.Fatalf("drive %d: %v, want the refusal naming the source", i, err)
			}
		}
		rec, ok := nodeReasons(t, j, "node:pay")
		if !ok || !rec.Refused {
			t.Fatalf("record %+v, %v; want a refused record", rec, ok)
		}
		if r, found, _ := j.Get(context.Background(), "r1", "node:pay"); found {
			t.Errorf("a refused node has a record: %+v", r)
		}
		recs, _ := j.History(context.Background(), "r1")
		for _, r := range recs {
			if strings.HasPrefix(r.Name, "approval") || strings.HasPrefix(r.Name, "attempt:") {
				t.Errorf("a refused node left %q", r.Name)
			}
		}
	})

	t.Run("an exempt node: no requirement, the reason is recorded", func(t *testing.T) {
		j := startFlowRun(t, hookDecls(nil), nil)
		put(t, j, "node:fetch")
		d, err := decide(j, "node:safe", side, under)
		if err != nil || d.Gate != nil {
			t.Fatalf("decision %+v, %v", d, err)
		}
		rec, ok := nodeReasons(t, j, "node:safe")
		if !ok || rec.Exempt != "reviewed" || rec.Refused || len(rec.Reasons) != 1 {
			t.Errorf("record %+v, %v", rec, ok)
		}
	})

	t.Run("a node that ran before the source is read at its own position", func(t *testing.T) {
		j := startFlowRun(t, hookDecls(nil), nil)
		put(t, j, "node:pay")   // ran first
		put(t, j, "node:fetch") // the source came later
		d, err := decide(j, "node:pay", side, under)
		if err != nil || d.Gate != nil {
			t.Fatalf("a replayed node is asked for a gate it never needed: %+v, %v", d, err)
		}
		if _, ok := nodeReasons(t, j, "node:pay"); ok {
			t.Error("a record was written for a node that ran before the source")
		}
	})

	t.Run("a loop iteration's key names its node", func(t *testing.T) {
		j := startFlowRun(t, hookDecls(nil), nil)
		put(t, j, "node:iter:0:fetch")
		d, err := decide(j, "node:iter:1:pay", side, under)
		if err != nil || d.Gate == nil {
			t.Fatalf("decision %+v, %v", d, err)
		}
		if _, ok := nodeReasons(t, j, "node:iter:1:pay"); !ok {
			t.Error("no record under the iteration's key")
		}
	})

	t.Run("the run's untrusted input taints the first node", func(t *testing.T) {
		j := startFlowRun(t, hookDecls(nil), str("mail-input"))
		d, err := decide(j, "node:pay", side, under)
		if err != nil || d.Gate == nil {
			t.Fatalf("decision %+v, %v", d, err)
		}
		rec, _ := nodeReasons(t, j, "node:pay")
		if len(rec.Reasons) != 1 || rec.Reasons[0].Source != "mail-input" || rec.Reasons[0].Ref != "input" {
			t.Errorf("reasons %+v", rec.Reasons)
		}
	})

	t.Run("not under taint: nothing, whatever was read", func(t *testing.T) {
		j := startFlowRun(t, nil, nil)
		put(t, j, "node:fetch")
		if d, err := decide(j, "node:pay", side, journalhook.FlowTaintNone); err != nil || d.Gate != nil {
			t.Fatalf("decision %+v, %v", d, err)
		}
	})

	t.Run("policyless: a side effect is refused as a config error, a read is not", func(t *testing.T) {
		j := startFlowRun(t, hookDecls(nil), nil)
		if _, err := decide(j, "node:pay", side, journalhook.FlowTaintPolicyless); !errors.Is(err, ErrConfig) || errors.Is(err, journalhook.ErrTaintRefused) {
			t.Errorf("a side effect: %v, want ErrConfig", err)
		}
		if d, err := decide(j, "node:look", ro, journalhook.FlowTaintPolicyless); err != nil || d.Gate != nil {
			t.Errorf("a read: %+v, %v", d, err)
		}
	})

	t.Run("a record held under another gate holds the node", func(t *testing.T) {
		j := startFlowRun(t, hookDecls(nil), nil)
		put(t, j, "node:fetch")
		if _, _, err := putGateReason(context.Background(), j, "r1", gateReasonRecord{Format: gateReasonFormat, ToolUseID: "node:pay",
			Reasons: []gateReason{{Kind: gateReasonTaint, Source: "inbound-email", Ref: "node:fetch", Pos: 1}}, PolicyDigest: "not-this-gate"}); err != nil {
			t.Fatal(err)
		}
		if _, err := decide(j, "node:pay", side, under); !errors.Is(err, ErrConfig) {
			t.Errorf("%v, want ErrConfig", err)
		}
	})

	t.Run("a planted record adds a requirement", func(t *testing.T) {
		j := startFlowRun(t, hookDecls(nil), nil)
		if _, _, err := putGateReason(context.Background(), j, "r1", gateReasonRecord{Format: gateReasonFormat, ToolUseID: "node:pay",
			Reasons: []gateReason{{Kind: gateReasonTaint, Source: "planted", Ref: "node:x", Pos: 1}}, PolicyDigest: gateDigest(&ApprovalPolicy{Need: 2, Approvers: []string{"a", "b", "c"}})}); err != nil {
			t.Fatal(err)
		}
		d, err := decide(j, "node:pay", side, under)
		if err != nil || d.Gate == nil {
			t.Errorf("a planted reason: %+v, %v; want the gate", d, err)
		}
	})

	t.Run("a name that is not a node key is refused", func(t *testing.T) {
		j := startFlowRun(t, hookDecls(nil), nil)
		for _, k := range []string{"node:pay:taint", "node:pay:step:x", "c1", ""} {
			if _, err := decide(j, k, side, under); !errors.Is(err, ErrConfig) {
				t.Errorf("%q: %v, want ErrConfig", k, err)
			}
		}
	})

	t.Run("a node and a recorded start that holds together is required", func(t *testing.T) {
		if _, err := decide(memJournal(), "node:pay", side, under); !errors.Is(err, ErrStorage) {
			t.Errorf("no recorded start: %v, want ErrStorage", err)
		}
	})
}

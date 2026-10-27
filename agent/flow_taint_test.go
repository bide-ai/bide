package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// The pure parts of flow taint (docs/design/flow-taint.md, A2 and A3): the declarations a flow run
// starts under and their digest, the check of a drive against them, the read set a node has at a point in the
// journal, the requirement that read set makes, and the records the engine will write. Nothing here is
// reached by a running flow yet.

func flowEscGate() *ApprovalPolicy {
	return &ApprovalPolicy{Need: 2, Approvers: []string{"a", "b", "c"}}
}

func baseDecls() flowTaintDecls {
	return flowTaintDecls{
		Policy: flowTaintPolicy{Gate: flowEscGate()},
		Nodes: map[string]flowNodeTaint{
			"fetch": {Untrusted: []string{"inbound-email"}},
			"look":  {Trusted: "our table"},
			"pay":   {},
		},
	}
}

func TestFlowTaintDecls_DigestCommitsToEverything(t *testing.T) {
	base := baseDecls()
	d0 := base.digest()
	if len(d0) != 64 || d0 != baseDecls().digest() {
		t.Fatalf("digest = %q, not stable", d0)
	}
	mut := map[string]func(d *flowTaintDecls){
		"the gate":            func(d *flowTaintDecls) { d.Policy.Gate = &ApprovalPolicy{Need: 1, Approvers: []string{"a", "b", "c"}} },
		"the gate's approver": func(d *flowTaintDecls) { d.Policy.Gate = &ApprovalPolicy{Need: 2, Approvers: []string{"a", "b", "d"}} },
		"Refuse instead":      func(d *flowTaintDecls) { d.Policy = flowTaintPolicy{Refuse: true} },
		"RequireProvenance":   func(d *flowTaintDecls) { d.Policy.RequireProvenance = true },
		"a source":            func(d *flowTaintDecls) { d.Nodes["fetch"] = flowNodeTaint{Untrusted: []string{"inbound-email", "web"}} },
		"another source":      func(d *flowTaintDecls) { d.Nodes["fetch"] = flowNodeTaint{Untrusted: []string{"crm"}} },
		"a dropped source":    func(d *flowTaintDecls) { d.Nodes["fetch"] = flowNodeTaint{} },
		"a trusted reason":    func(d *flowTaintDecls) { d.Nodes["look"] = flowNodeTaint{Trusted: "another reason"} },
		"an exemption":        func(d *flowTaintDecls) { d.Nodes["pay"] = flowNodeTaint{Exempt: "reviewed"} },
		"a declaring node":    func(d *flowTaintDecls) { d.Nodes["notify"] = flowNodeTaint{Untrusted: []string{"x"}} },
		"a renamed node":      func(d *flowTaintDecls) { d.Nodes["fetch2"] = d.Nodes["fetch"]; delete(d.Nodes, "fetch") },
	}
	for what, f := range mut {
		d := baseDecls()
		f(&d)
		if d.digest() == d0 {
			t.Errorf("the digest does not change with %s", what)
		}
	}
	// Order and repetition of sources do not matter; the declarations are normalised first.
	a, b := baseDecls(), baseDecls()
	a.Nodes["fetch"] = flowNodeTaint{Untrusted: []string{"web", "inbound-email", "web"}}
	b.Nodes["fetch"] = flowNodeTaint{Untrusted: []string{"inbound-email", "web"}}
	if a.digest() != b.digest() {
		t.Error("the digest depends on the order or repetition of sources")
	}
	// A node that declares nothing is the same as an absent node.
	c, e := baseDecls(), baseDecls()
	delete(e.Nodes, "pay")
	if c.digest() != e.digest() {
		t.Error("a node that declares nothing changes the digest")
	}
}

func TestFlowTaintDecls_RunStartRoundTrip(t *testing.T) {
	d := baseDecls()
	s := RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}.withFlowTaint(d)
	if s.taintDigest() != d.digest() {
		t.Errorf("the start records digest %q, want %q", s.taintDigest(), d.digest())
	}
	// Through the journal's JSON.
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back RunStart
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	got, ok, err := back.flowTaintDecls()
	if err != nil || !ok || got.digest() != d.digest() {
		t.Fatalf("flowTaintDecls = %+v, %v, %v", got, ok, err)
	}
	if got.Nodes["fetch"].Untrusted[0] != "inbound-email" || got.Policy.Gate.Need != 2 {
		t.Errorf("declarations read back as %+v", got)
	}
	// A start that records nothing reads as no declarations, not an error.
	if _, ok, err := (RunStart{}).flowTaintDecls(); ok || err != nil {
		t.Errorf("an empty start: %v, %v", ok, err)
	}
	// withFlowTaint does not change the start it was called on.
	orig := RunStart{Ext: map[string]json.RawMessage{"other": json.RawMessage(`1`)}}
	_ = orig.withFlowTaint(d)
	if _, has := orig.Ext[extTaintDigest]; has || len(orig.Ext) != 1 {
		t.Errorf("withFlowTaint changed its receiver's Ext: %v", orig.Ext)
	}
}

func TestFlowTaintDecls_ARecordThatDoesNotHoldTogetherIsRefused(t *testing.T) {
	good := RunStart{}.withFlowTaint(baseDecls())
	set := func(mut func(ext map[string]json.RawMessage)) RunStart {
		s := good
		s.Ext = map[string]json.RawMessage{}
		for k, v := range good.Ext {
			s.Ext[k] = v
		}
		mut(s.Ext)
		return s
	}
	tampered := strings.Replace(string(good.Ext[extTaintDeclarations]), "inbound-email", "nothing", 1)
	cases := map[string]RunStart{
		"declarations edited under the digest": set(func(e map[string]json.RawMessage) { e[extTaintDeclarations] = json.RawMessage(tampered) }),
		"a digest with no declarations":        set(func(e map[string]json.RawMessage) { delete(e, extTaintDeclarations) }),
		"declarations with no digest":          set(func(e map[string]json.RawMessage) { delete(e, extTaintDigest) }),
		"another format": set(func(e map[string]json.RawMessage) {
			e[extTaintDeclarations] = json.RawMessage(strings.Replace(string(good.Ext[extTaintDeclarations]), "v1", "v9", 1))
		}),
		"an unknown member": set(func(e map[string]json.RawMessage) {
			e[extTaintDeclarations] = json.RawMessage(strings.Replace(string(good.Ext[extTaintDeclarations]), `{"format"`, `{"future":1,"format"`, 1))
		}),
		"a duplicate member": set(func(e map[string]json.RawMessage) {
			e[extTaintDeclarations] = json.RawMessage(strings.Replace(string(good.Ext[extTaintDeclarations]), `{"format"`, `{"format":"x","format"`, 1))
		}),
		"not JSON":                      set(func(e map[string]json.RawMessage) { e[extTaintDeclarations] = json.RawMessage(`"x"`) }),
		"a digest that is not a string": set(func(e map[string]json.RawMessage) { e[extTaintDigest] = json.RawMessage(`3`) }),
	}
	for what, s := range cases {
		if _, ok, err := s.flowTaintDecls(); err == nil || !errors.Is(err, ErrStorage) {
			t.Errorf("%s: flowTaintDecls = ok %v, err %v; want an ErrStorage refusal", what, ok, err)
		}
	}
	// One member without the other is said so, not left to the later checks to trip over.
	for _, what := range []string{"a digest with no declarations", "declarations with no digest"} {
		if _, _, err := cases[what].flowTaintDecls(); err == nil || !strings.Contains(err.Error(), "without the other") {
			t.Errorf("%s: %v, want the message to say one member is recorded without the other", what, err)
		}
	}
}

func TestFlowTaintCheck(t *testing.T) {
	started := RunStart{Kind: RunKindFlow}.withFlowTaint(baseDecls())
	plain := RunStart{Kind: RunKindFlow}
	cur := baseDecls()
	weaker := baseDecls()
	weaker.Policy.Gate = &ApprovalPolicy{Need: 1, Approvers: []string{"a", "b", "c"}}
	dropped := baseDecls()
	dropped.Nodes["fetch"] = flowNodeTaint{}
	exempt := baseDecls()
	exempt.Nodes["pay"] = flowNodeTaint{Exempt: "reviewed"}
	refuse := baseDecls()
	refuse.Policy = flowTaintPolicy{Refuse: true}
	stricter := baseDecls()
	stricter.Policy.Gate = &ApprovalPolicy{Need: 3, Approvers: []string{"a", "b", "c"}}
	noPolicy := baseDecls()
	noPolicy.Policy = flowTaintPolicy{}

	type want struct {
		standing flowTaintStanding
		err      bool
	}
	for what, c := range map[string]struct {
		start   RunStart
		current *flowTaintDecls
		want    want
	}{
		"no policy then, no policy now":                 {plain, nil, want{flowNotUnderTaint, false}},
		"no policy then, a policy now: not retro-gated": {plain, &cur, want{flowNotUnderTaint, false}},
		"the same declarations":                         {started, &cur, want{flowUnderTaint, false}},
		"a weaker gate":                                 {started, &weaker, want{0, true}},
		"a stricter gate (exact, not allowed yet)":      {started, &stricter, want{0, true}},
		"a dropped source":                              {started, &dropped, want{0, true}},
		"a new exemption":                               {started, &exempt, want{0, true}},
		"Refuse for a gate":                             {started, &refuse, want{0, true}},
		"no policy now, a recorded one":                 {started, nil, want{flowPolicyless, false}},
		"declarations but no policy now":                {started, &noPolicy, want{flowPolicyless, false}},
	} {
		got, err := flowTaintCheck(c.start, c.current)
		if (err != nil) != c.want.err || err == nil && got != c.want.standing {
			t.Errorf("%s: standing %v, err %v; want %+v", what, got, err, c.want)
		}
		if err != nil {
			if !errors.Is(err, ErrConfig) {
				t.Errorf("%s: %v is not ErrConfig", what, err)
			}
			if !strings.Contains(err.Error(), started.taintDigest()[:12]) {
				t.Errorf("%s: %v does not name the recorded digest", what, err)
			}
		}
	}
	// A recorded start that does not hold together is a storage refusal, not a standing.
	bad := started
	bad.Ext = map[string]json.RawMessage{extTaintDigest: started.Ext[extTaintDigest]}
	if _, err := flowTaintCheck(bad, &cur); !errors.Is(err, ErrStorage) {
		t.Errorf("a start with a digest and no declarations: %v, want ErrStorage", err)
	}
}

// ---- the read set ----

func flowVal(name string, result string) Record {
	return Record{Name: name, Kind: StepValue, Result: json.RawMessage(result)}
}

func flowStartRecord(input *string) Record {
	b, _ := json.Marshal(RunStart{Kind: RunKindFlow, Settings: RunSettings{UntrustedInput: input}})
	return Record{Name: runStartStep, Kind: StepValue, Result: b}
}

func markSources(ms []taintMark) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Source+"@"+m.Ref)
	}
	return out
}

func TestFlowReadSet(t *testing.T) {
	d := baseDecls()
	mail := "mail-input"
	recs := []Record{
		flowStartRecord(nil),              // 0
		flowVal("flow:digest", `"x"`),     // 1
		flowVal("node:look", `1`),         // 2: a trusted node
		flowVal("node:fetch", `1`),        // 3: an untrusted source
		flowVal("node:iter:1:fetch", `1`), // 4: the same node in a loop
		flowVal("node:other", `1`),        // 5: a node that declares nothing
		flowVal("node:pay", `1`),          // 6: the node itself
	}
	for what, c := range map[string]struct {
		recs   []Record
		before int
		want   []string
	}{
		"nothing before any node":              {recs, 2, nil},
		"a trusted node leaves no mark":        {recs, 3, nil},
		"after the source":                     {recs, 4, []string{"inbound-email@node:fetch"}},
		"a loop iteration adds no new pair":    {recs, 6, []string{"inbound-email@node:fetch"}},
		"records after the point are not read": {recs, 3, nil},
	} {
		got, err := flowReadSet(c.recs, d, c.before, nil)
		if err != nil || !slices.Equal(markSources(got), c.want) {
			t.Errorf("%s: %v, %v; want %v", what, markSources(got), err, c.want)
		}
	}
	// The mark's position is where the source's first result is.
	got, _ := flowReadSet(recs, d, 6, nil)
	if len(got) != 1 || got[0].Pos != 3 {
		t.Errorf("marks %+v, want one at position 3", got)
	}
	// The run's input is a source when the start says so.
	withInput := append([]Record{flowStartRecord(&mail)}, recs[1:]...)
	got, err := flowReadSet(withInput, d, 2, nil)
	if err != nil || !slices.Equal(markSources(got), []string{"mail-input@input"}) {
		t.Errorf("input: %v, %v", markSources(got), err)
	}
	// Inherited marks (a parent's) come first and are kept.
	inh := []taintMark{{Source: "parent-feed", Ref: "inherited", Pos: 0}}
	got, _ = flowReadSet(recs, d, 3, inh)
	if !slices.Equal(markSources(got), []string{"parent-feed@inherited"}) {
		t.Errorf("inherited: %v", markSources(got))
	}
	// A node's declaration comes from the recorded declarations, by node name: a result of a node the
	// declarations do not know leaves no mark.
	got, _ = flowReadSet([]Record{flowVal("node:ghost", `1`)}, d, 1, nil)
	if len(got) != 0 {
		t.Errorf("an undeclared node left marks %v", markSources(got))
	}
	// A Step a node's body ran ("node:fetch:step:x") is not the node's result.
	got, _ = flowReadSet([]Record{flowVal("node:fetch:step:x", `1`)}, d, 1, nil)
	if len(got) != 0 {
		t.Errorf("a node's inner Step left marks %v", markSources(got))
	}
	// Two sources on one node are two marks.
	two := baseDecls()
	two.Nodes["fetch"] = flowNodeTaint{Untrusted: []string{"a", "b"}}
	got, _ = flowReadSet([]Record{flowVal("node:fetch", `1`)}, two, 1, nil)
	if !slices.Equal(markSources(got), []string{"a@node:fetch", "b@node:fetch"}) {
		t.Errorf("two sources: %v", markSources(got))
	}
}

func TestFlowReadSet_TaintMarkRecords(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	d := baseDecls()
	if _, err := putTaintMark(ctx, j, "r1", "node:look", "agent:triage"); err != nil {
		t.Fatal(err)
	}
	if _, err := putTaintMark(ctx, j, "r1", "node:look", "web:page"); err != nil {
		t.Fatal(err)
	}
	recs, err := j.History(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := flowReadSet(recs, d, len(recs), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"agent:triage@child:node:look", "web:page@child:node:look"}
	if !slices.Equal(markSources(got), want) {
		t.Errorf("marks %v, want %v", markSources(got), want)
	}
	// A record under the reserved prefix that does not read is an error, never a missing mark.
	bad := []Record{flowVal(taintMarkPrefix+"x", `"junk"`)}
	if _, err := flowReadSet(bad, d, 1, nil); !errors.Is(err, ErrStorage) {
		t.Errorf("an unreadable taint-mark record: %v, want ErrStorage", err)
	}
	// A mark record whose name and body disagree is unreadable too.
	b, _ := json.Marshal(taintMarkRecord{Format: taintMarkFormat, NodeKey: "node:look", Source: "agent:x"})
	mismatch := []Record{flowVal(taintMarkStep("node:look", "agent:y"), string(b))}
	if _, err := flowReadSet(mismatch, d, 1, nil); !errors.Is(err, ErrStorage) {
		t.Errorf("a mark whose name disagrees with its body: %v, want ErrStorage", err)
	}
}

func TestTaintMarkRecord(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	if got := taintMarkStep("node:iter:2:x", "agent:a:b"); got != taintMarkPrefix+encodeID("node:iter:2:x")+":"+encodeID("agent:a:b") {
		t.Errorf("taintMarkStep = %q", got)
	}
	// Two different sources of one node are two records; the same source twice is one.
	for _, src := range []string{"s1", "s2", "s1"} {
		if _, err := putTaintMark(ctx, j, "r1", "node:x", src); err != nil {
			t.Fatal(err)
		}
	}
	recs, _ := j.History(ctx, "r1")
	n := 0
	for _, r := range recs {
		if strings.HasPrefix(r.Name, taintMarkPrefix) {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d taint-mark records, want 2", n)
	}
	// The prefix is reserved: no Step can be named so.
	if !IsReservedStepName(taintMarkPrefix + "x") {
		t.Error("taint-mark: is not a reserved step name")
	}
	if _, err := putTaintMark(ctx, j, "r1", "not a node key", "s"); !errors.Is(err, ErrConfig) {
		t.Errorf("a mark for a name that is not a node key: %v, want ErrConfig", err)
	}
	if _, err := putTaintMark(ctx, j, "r1", "node:x", ""); !errors.Is(err, ErrConfig) {
		t.Errorf("a mark with no source: %v, want ErrConfig", err)
	}
}

// ---- the requirement ----

func flowMarksOf(srcs ...string) []taintMark {
	var out []taintMark
	for i, s := range srcs {
		out = append(out, taintMark{Source: s, Ref: "node:fetch", Pos: i + 3})
	}
	return out
}

func TestFlowRequirement(t *testing.T) {
	d := baseDecls()
	gate := func(r flowRequirement) bool { return r.Required && r.Gate != nil && !r.Refuse }
	// No marks, or a call that cannot have a side effect, or a node declared exempt: nothing is required.
	if r := flowRequirementFor(d, "pay", true, nil, nil); r.Required || len(r.Reasons) != 0 {
		t.Errorf("no marks: %+v", r)
	}
	if r := flowRequirementFor(d, "pay", false, flowMarksOf("s"), nil); r.Required {
		t.Errorf("a read-only node: %+v", r)
	}
	ex := baseDecls()
	ex.Nodes["pay"] = flowNodeTaint{Exempt: "reviewed"}
	r := flowRequirementFor(ex, "pay", true, flowMarksOf("s"), nil)
	if r.Required || r.Exempt != "reviewed" || len(r.Reasons) != 1 {
		t.Errorf("an exempt node: %+v, want the reason kept and nothing required", r)
	}
	// Marks on a side-effecting node under a Gate policy require the gate, with the reasons.
	r = flowRequirementFor(d, "pay", true, flowMarksOf("inbound-email"), nil)
	if !gate(r) || len(r.Reasons) != 1 || r.Reasons[0].Kind != gateReasonTaint || r.Reasons[0].Source != "inbound-email" || r.Gate.Need != 2 {
		t.Errorf("a tainted side effect: %+v", r)
	}
	// The gate is a copy.
	r.Gate.Need = 9
	if d.Policy.Gate.Need != 2 {
		t.Error("the requirement's gate shares the declarations' policy")
	}
	// Under Refuse the node is refused, not asked.
	rf := baseDecls()
	rf.Policy = flowTaintPolicy{Refuse: true}
	if r := flowRequirementFor(rf, "pay", true, flowMarksOf("s"), nil); !r.Required || !r.Refuse || r.Gate != nil {
		t.Errorf("Refuse: %+v", r)
	}
	// An exempt node is not refused either.
	rf.Nodes["pay"] = flowNodeTaint{Exempt: "reviewed"}
	if r := flowRequirementFor(rf, "pay", true, flowMarksOf("s"), nil); r.Required || r.Refuse {
		t.Errorf("Refuse with an exemption: %+v", r)
	}
}

// A record can only add: its taint reasons are a requirement even when the marks recompute to none, nothing
// in it removes one, and an exemption comes from the declarations, never from the record's text.
func TestFlowRequirement_ARecordOnlyAdds(t *testing.T) {
	d := baseDecls()
	rec := &gateReasonRecord{Format: gateReasonFormat, ToolUseID: "node:pay", Reasons: []gateReason{{Kind: gateReasonTaint, Source: "planted", Ref: "node:x", Pos: 1}}}
	r := flowRequirementFor(d, "pay", true, nil, rec)
	if !r.Required || len(r.Reasons) != 1 || r.Reasons[0].Source != "planted" {
		t.Errorf("a recorded reason with no recomputed marks: %+v, want it required", r)
	}
	// Recomputed and recorded reasons are both kept, each once.
	rec.Reasons = append(rec.Reasons, gateReason{Kind: gateReasonTaint, Source: "s", Ref: "node:fetch", Pos: 3})
	r = flowRequirementFor(d, "pay", true, flowMarksOf("s"), rec)
	if len(r.Reasons) != 2 {
		t.Errorf("reasons %+v, want the union of two", r.Reasons)
	}
	// An empty record removes nothing.
	empty := &gateReasonRecord{Format: gateReasonFormat, ToolUseID: "node:pay"}
	if r := flowRequirementFor(d, "pay", true, flowMarksOf("s"), empty); !r.Required {
		t.Errorf("an empty record removed a requirement: %+v", r)
	}
	// An exemption text in the record exempts nothing.
	fake := &gateReasonRecord{Format: gateReasonFormat, ToolUseID: "node:pay", Exempt: "trust me"}
	if r := flowRequirementFor(d, "pay", true, flowMarksOf("s"), fake); !r.Required || r.Exempt != "" {
		t.Errorf("a recorded exemption text exempted: %+v", r)
	}
	// A record that says the node was refused refuses it again, whatever the marks.
	refused := &gateReasonRecord{Format: gateReasonFormat, ToolUseID: "node:pay", Refused: true, Reasons: []gateReason{{Kind: gateReasonTaint, Source: "s", Ref: "node:fetch", Pos: 3}}}
	if r := flowRequirementFor(d, "pay", true, nil, refused); !r.Required || !r.Refuse {
		t.Errorf("a recorded refusal: %+v, want it refused again", r)
	}
	// A recorded reason on a read-only node, or an exempt one, still requires nothing there.
	if r := flowRequirementFor(d, "pay", false, nil, rec); r.Required {
		t.Errorf("a read-only node with a recorded reason: %+v", r)
	}
}

// A policy with neither a Gate nor Refuse is read as Refuse, never as no requirement, and Refuse wins over a
// Gate when a policy carries both.
func TestFlowRequirement_NoGateOrBothIsRefuse(t *testing.T) {
	for what, p := range map[string]flowTaintPolicy{
		"RequireProvenance alone": {RequireProvenance: true},
		"a Gate and Refuse":       {Gate: flowEscGate(), Refuse: true},
	} {
		d := baseDecls()
		d.Policy = p
		r := flowRequirementFor(d, "pay", true, flowMarksOf("s"), nil)
		if !r.Required || !r.Refuse || r.Gate != nil {
			t.Errorf("%s: %+v, want the node refused", what, r)
		}
	}
}

func TestGateReasonRecord_Refused(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	rec := gateReasonRecord{Format: gateReasonFormat, ToolUseID: "node:pay", Refused: true,
		Reasons: []gateReason{{Kind: gateReasonTaint, Source: "s", Ref: "node:fetch", Pos: 3}}, RecordedAt: "2026-10-27T00:00:00.000Z"}
	got, inserted, err := putGateReason(ctx, j, "r1", rec)
	if err != nil || !inserted || !got.Refused {
		t.Fatalf("putGateReason = %+v, %v, %v", got, inserted, err)
	}
	// Insert-once: a second record for the node, without the refusal, does not replace it.
	again := rec
	again.Refused = false
	got, inserted, err = putGateReason(ctx, j, "r1", again)
	if err != nil || inserted || !got.Refused {
		t.Fatalf("the second put = %+v, %v, %v; want the first record back", got, inserted, err)
	}
	if r, ok, err := getGateReason(ctx, j, "r1", "node:pay"); err != nil || !ok || !r.Refused {
		t.Fatalf("getGateReason = %+v, %v, %v", r, ok, err)
	}
	// A record with no refusal does not write the member.
	b, _ := json.Marshal(gateReasonRecord{Format: gateReasonFormat, ToolUseID: "c1"})
	if strings.Contains(string(b), "refused") {
		t.Errorf("an ordinary record carries the member: %s", b)
	}
}

func TestNodeNameOfKey(t *testing.T) {
	for key, want := range map[string]string{"node:x": "x", "node:iter:3:x": "x", "node:iter": "iter", "node:iter:0:iter": "iter"} {
		if got, ok := nodeNameOfKey(key); !ok || got != want {
			t.Errorf("nodeNameOfKey(%q) = %q, %v; want %q", key, got, ok, want)
		}
	}
	for _, key := range []string{"node:x:step:y", "node:x:taint", "x", "node:", ""} {
		if got, ok := nodeNameOfKey(key); ok {
			t.Errorf("nodeNameOfKey(%q) = %q, true; want not a node key", key, got)
		}
	}
}

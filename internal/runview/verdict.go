package runview

import (
	"fmt"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// Verdict is the answer a page opens with: one state in large type, a line under it, and a few key
// numbers. A state is never carried by color alone: Word and Icon say it too.
type Verdict struct {
	// Class styles the banner: good, bad, warn, wait or info.
	Class string
	// Word is the state in capitals ("VERIFIED"), Icon its glyph (a name of icons) and Line one
	// sentence of why.
	Word, Icon, Line string
	// Details are further lines the state calls out (a weakened gate's policy).
	Details []string
	Stats   []Stat
}

// Stat is one key number under the verdict.
type Stat struct {
	Value, Label string
	Sub          string // one line under the label
	Class        string // good, bad, warn or empty
}

func count(n int, singular string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %ss", n, singular)
}

// runVerdict picks the run page's verdict from the run's state, its gates and its audit proof:
//
//	halted                          HALTED (an operation's outcome is unknown: needs action)
//	waiting at an approval gate     WAITING FOR APPROVAL
//	aborted or cancelled            FAILED
//	completed, proof or signatures
//	  that do not verify            FAILED
//	completed, proof verifies       VERIFIED
//	completed, no proof given       COMPLETED
//	anything else                   IN PROGRESS (or NOT STARTED)
func runVerdict(v *View) Verdict {
	var d Verdict
	badSigs := false
	for _, g := range v.Gates {
		if !g.Single && strings.HasPrefix(g.Signatures, "signatures NOT") {
			badSigs = true
		}
	}
	switch {
	case v.State == "halted":
		ops := make([]string, len(v.Halts))
		for i, h := range v.Halts {
			ops[i] = h.ID
		}
		d = Verdict{Class: "bad", Word: "HALTED", Icon: "halt",
			Line: fmt.Sprintf("Outcome unknown for %s. A resume returns *agent.OutcomeUnknown until agent.ResolveHalt records it.", strings.Join(ops, ", "))}
	case v.State == "waiting":
		d = Verdict{Class: "wait", Word: "WAITING FOR APPROVAL", Icon: "wait", Line: "The run is paused at an approval gate and resumes when enough approvers decide."}
		for _, g := range v.Gates {
			if g.Waiting {
				d.Line = fmt.Sprintf("%s: %s", cmpName(g), g.Headline())
				if len(g.Pending) > 0 {
					d.Line += "; waiting on " + strings.Join(g.Pending, ", ")
				}
				break
			}
		}
	case v.State == string(agent.RunAborted) || v.State == string(agent.RunCancelled):
		d = Verdict{Class: "bad", Word: "FAILED", Icon: "x", Line: "The run " + v.State + ": " + v.Terminal}
		if v.Terminal == "" {
			d.Line = "The run " + v.State + "."
		}
	case v.State == string(agent.RunCompleted) && v.Proof != nil && !v.Proof.OK:
		d = Verdict{Class: "bad", Word: "FAILED", Icon: "x", Line: "The run completed, but its audit proof does not verify: " + v.Proof.Summary + "."}
	case v.State == string(agent.RunCompleted) && badSigs:
		d = Verdict{Class: "bad", Word: "FAILED", Icon: "x", Line: "The run completed, but an approval gate's signatures did not verify."}
	case v.State == string(agent.RunCompleted) && v.Proof != nil:
		d = Verdict{Class: "good", Word: "VERIFIED", Icon: "approval", Line: "Signatures and audit proof check out: " + strings.TrimPrefix(v.Proof.Summary, "verifies: ") + "."}
	case v.State == string(agent.RunCompleted):
		d = Verdict{Class: "info", Word: "COMPLETED", Icon: "ok", Line: "The run finished. No signed head or evidence package was given, so nothing is verified (-sth or -evidence, with -pubkey)."}
	case v.State == string(agent.RunStarted):
		d = Verdict{Class: "info", Word: "IN PROGRESS", Icon: "clock", Line: "The run has started and has no end marker yet."}
	default:
		d = Verdict{Class: "info", Word: "NOT STARTED", Icon: "idle", Line: "The journal holds no run:start for this run."}
	}

	tools, models, effects, reconciled := 0, 0, 0, 0
	for _, e := range v.Events {
		switch e.Class {
		case "tool":
			tools++
			for _, b := range e.Badges {
				if strings.HasPrefix(b, "side effect") {
					effects++
				}
				if strings.HasPrefix(b, "reconciled") {
					reconciled++
				}
			}
		case "model":
			models++
		}
	}
	d.Stats = []Stat{{Value: fmt.Sprint(v.Records), Label: "journal records", Sub: strings.TrimSpace(strings.Join([]string{kindText(v.Kind), v.Flow}, " "))}}
	if p := v.Proof; p != nil {
		st := Stat{Value: fmt.Sprintf("%d/%d", p.Covers, v.Records), Label: "records signed", Sub: "audit proof verifies", Class: "good"}
		if !p.OK {
			st.Sub, st.Class = "audit proof does NOT verify", "bad"
		}
		d.Stats = append(d.Stats, st)
	}
	if len(v.Gates) > 0 {
		g := v.Gates[0]
		st := Stat{Label: "approvals", Sub: g.SigLine()}
		switch {
		case g.Single:
			st.Value, st.Label = "1/1", "decision"
		default:
			st.Value = fmt.Sprintf("%d/%d", len(g.ApprovedBy), len(g.Approvers))
			st.Sub = strings.TrimPrefix(fmt.Sprintf("%d needed; %s", g.Need, g.SigLine()), "")
			if g.SigLine() == "" {
				st.Sub = fmt.Sprintf("%d needed", g.Need)
			}
		}
		st.Class = g.Class()
		if st.Class == "wait" {
			st.Class = "warn"
		}
		if len(v.Gates) > 1 {
			st.Sub = fmt.Sprintf("first of %d gates", len(v.Gates))
		}
		d.Stats = append(d.Stats, st)
	}
	if len(v.Halts) > 0 {
		d.Stats = append(d.Stats, Stat{Value: fmt.Sprint(len(v.Halts)), Label: "outcome unknown", Sub: "needs agent.ResolveHalt", Class: "bad"})
	}
	if v.Graph != nil {
		ran := 0
		for _, n := range v.Graph.Nodes {
			if n.Status != "not-reached" {
				ran++
			}
		}
		d.Stats = append(d.Stats, Stat{Value: fmt.Sprintf("%d/%d", ran, len(v.Graph.Nodes)), Label: "nodes reached", Sub: "of the declared flow"})
	}
	switch {
	case effects > 0:
		sub := fmt.Sprintf("of %s", count(tools, "tool result"))
		if reconciled > 0 {
			sub = fmt.Sprintf("%d reconciled by ResolveHalt", reconciled)
		}
		d.Stats = append(d.Stats, Stat{Value: fmt.Sprint(effects), Label: "side effect" + plural(effects), Sub: sub})
	case tools > 0:
		d.Stats = append(d.Stats, Stat{Value: fmt.Sprint(tools), Label: "tool result" + plural(tools), Sub: count(models, "model turn")})
	}
	if len(d.Stats) > 4 {
		d.Stats = d.Stats[:4]
	}
	return d
}

func cmpName(g Gate) string {
	if g.ToolName != "" {
		return g.ToolName
	}
	return g.ToolUseID
}

// configVerdict picks the config page's verdict: the violated assertions first, then the config's
// errors, then assertions that could not be decided; otherwise every assertion holds, or the config
// declares none.
func configVerdict(v *ConfigView) Verdict {
	var held, violated, other int
	for _, a := range v.Assertions {
		switch {
		case a.Holds:
			held++
		case a.Violated():
			violated++
		default:
			other++
		}
	}
	var d Verdict
	switch {
	case violated > 0:
		d = Verdict{Class: "bad", Word: fmt.Sprintf("%d POLICY VIOLATION%s", violated, strings.ToUpper(plural(violated))), Icon: "halt",
			Line: "An assertion fails on a path the flow declares. The counterexample is drawn in red."}
	case v.Errors > 0:
		d = Verdict{Class: "bad", Word: fmt.Sprintf("%d CONFIG ERROR%s", v.Errors, strings.ToUpper(plural(v.Errors))), Icon: "x",
			Line: "The config does not load. Each error is marked on its node or edge."}
	case other > 0:
		d = Verdict{Class: "warn", Word: fmt.Sprintf("%d ASSERTION%s UNDECIDED", other, strings.ToUpper(plural(other))), Icon: "unknown",
			Line: "An assertion could not be decided or read; the others hold."}
	case held > 0:
		d = Verdict{Class: "good", Word: "ALL ASSERTIONS HOLD", Icon: "approval",
			Line: fmt.Sprintf("%s hold on every path the flow declares.", count(held, "assertion"))}
	default:
		d = Verdict{Class: "info", Word: "NO ASSERTIONS", Icon: "node", Line: "The config declares none. Nothing is checked beyond its structure."}
	}
	if violated == 0 && v.Errors == 0 && v.Warnings > 0 {
		d.Line += fmt.Sprintf(" %s.", count(v.Warnings, "warning"))
	}
	nodes := 0
	if v.Graph != nil {
		for _, n := range v.Graph.Nodes {
			if n.Status != "undeclared" {
				nodes++
			}
		}
	}
	d.Stats = []Stat{
		{Value: fmt.Sprint(nodes), Label: "nodes", Sub: v.Flow},
		{Value: fmt.Sprint(v.Risk.SideEffects), Label: "side-effect steps", Sub: fmt.Sprintf("%d gated, %d ungated", v.Risk.Gated, v.Risk.Ungated), Class: map[bool]string{true: "warn", false: ""}[v.Risk.Ungated > 0]},
	}
	if n := len(v.Assertions); n > 0 {
		cl := "good"
		if held < n {
			cl = "bad"
		}
		d.Stats = append(d.Stats, Stat{Value: fmt.Sprintf("%d/%d", held, n), Label: "assertions hold", Sub: fmt.Sprintf("%d violated", violated), Class: cl})
	}
	d.Stats = append(d.Stats, Stat{Value: fmt.Sprint(len(v.Approvals)), Label: "approval gates", Sub: map[bool]string{true: "none declared", false: "declared on nodes"}[len(v.Approvals) == 0]})
	return d
}

// diffVerdict picks the diff page's verdict: a weakened approval gate first, always.
func diffVerdict(v *DiffView) Verdict {
	var d Verdict
	n := len(v.Changes)
	switch {
	case len(v.Weakened) > 0:
		d = Verdict{Class: "bad", Word: "APPROVAL GATE WEAKENED", Icon: "halt", Line: "Approval policy weakened",
			Details: v.Weakened}
	case n == 0:
		d = Verdict{Class: "good", Word: "NO CHANGES", Icon: "ok", Line: "The two configs declare the same flow."}
	case len(v.Strengthened) > 0:
		d = Verdict{Class: "good", Word: count(n, "change") + ", GATE STRENGTHENED", Icon: "approval", Line: "No approval gate was weakened."}
		d.Word = strings.ToUpper(d.Word)
	default:
		d = Verdict{Class: "warn", Word: strings.ToUpper(count(n, "change")), Icon: "tilde", Line: "No approval gate was weakened."}
	}
	c := v.Counts
	digest := Stat{Value: "n/a", Label: "digest", Sub: "not computed"}
	switch {
	case v.OldDigest != "" && v.NewDigest != "" && v.OldDigest == v.NewDigest:
		digest = Stat{Value: "same", Label: "digest", Sub: "runs paused under the old flow resume"}
	case v.OldDigest != "" && v.NewDigest != "":
		digest = Stat{Value: "new", Label: "digest", Sub: "paused runs cannot resume", Class: "warn"}
	}
	gate := Stat{Value: "0", Label: "gates weakened", Sub: fmt.Sprintf("%d strengthened", len(v.Strengthened))}
	if len(v.Weakened) > 0 {
		gate.Value, gate.Class = fmt.Sprint(len(v.Weakened)), "bad"
	}
	d.Stats = []Stat{
		gate,
		{Value: fmt.Sprintf("+%d −%d ~%d", c.NodesAdded, c.NodesRemoved, c.NodesChanged), Label: "nodes", Sub: "added, removed, changed"},
		{Value: fmt.Sprintf("+%d −%d ~%d", c.EdgesAdded, c.EdgesRemoved, c.EdgesChanged), Label: "edges and arms", Sub: "added, removed, changed"},
		digest,
	}
	return d
}

// Verdict is the run list's headline: what needs a person first.
func (l RunList) Verdict() Verdict {
	h, w, f := l.Counts["halted"], l.Counts["waiting"], l.Counts["failed"]
	var d Verdict
	switch {
	case h > 0:
		d = Verdict{Class: "bad", Word: fmt.Sprintf("%d HALTED", h), Icon: "halt", Line: "Runs whose outcome is unknown need an operator: agent.ResolveHalt."}
	case w > 0:
		d = Verdict{Class: "wait", Word: fmt.Sprintf("%d WAITING FOR APPROVAL", w), Icon: "wait", Line: "Runs paused at an approval gate."}
	case f > 0:
		d = Verdict{Class: "bad", Word: fmt.Sprintf("%d FAILED OR CANCELLED", f), Icon: "x", Line: "Runs that aborted or were cancelled."}
	case l.All == 0:
		d = Verdict{Class: "info", Word: "NO RUNS", Icon: "idle", Line: "Nothing to list."}
	default:
		d = Verdict{Class: "good", Word: "ALL CLEAR", Icon: "ok", Line: "No run is halted, waiting or failed."}
	}
	d.Stats = []Stat{
		{Value: fmt.Sprint(l.All), Label: "runs", Sub: "newest first"},
		{Value: fmt.Sprint(l.Counts["completed"]), Label: "completed", Class: "good"},
		{Value: fmt.Sprint(h + w), Label: "need a person", Sub: fmt.Sprintf("%d halted, %d waiting", h, w), Class: map[bool]string{true: "bad", false: ""}[h+w > 0]},
		{Value: fmt.Sprint(l.Counts["in-progress"]), Label: "in progress"},
	}
	return d
}

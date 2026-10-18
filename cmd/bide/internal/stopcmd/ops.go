package stopcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/runview"
)

// scopeSpecs are the scopes the flags name, in the order -all, -tool, -flow, -agent.
func (c *cmd) scopeSpecs() []agent.StopScope {
	var out []agent.StopScope
	if c.f.all {
		out = append(out, agent.StopScope{Kind: agent.StopGlobal})
	}
	for _, n := range c.f.tools {
		out = append(out, agent.StopScope{Kind: agent.StopTool, Name: n})
	}
	for _, n := range c.f.flows {
		out = append(out, agent.StopScope{Kind: agent.StopFlow, Name: n})
	}
	for _, n := range c.f.agents {
		out = append(out, agent.StopScope{Kind: agent.StopAgent, Name: n})
	}
	return out
}

func (c *cmd) compensation() string {
	if c.f.blockComp {
		return agent.CompensationBlock
	}
	return agent.CompensationAllow
}

func compWord(comp string) string {
	if comp == agent.CompensationBlock {
		return "held too (a saga's rollback waits for the lift)"
	}
	return "continues (a saga's rollback is not held)"
}

func (c *cmd) setStops(ctx context.Context, ctl *agent.StopControl, doc *agent.StopPolicyDoc, signer audit.Signer, by, host string) int {
	scopes := c.scopeSpecs()
	for _, sc := range scopes {
		if err := sc.Validate(); err != nil {
			return c.fail(exitUsage, "%v", err)
		}
	}
	if err := checkReason(c.f.reason); err != nil {
		return c.fail(exitUsage, "-reason: %v", err)
	}
	if err := c.preflight(ctx, ctl, doc, signer, "setter"); err != nil {
		return c.fail(exitFailed, "%v", err)
	}
	st, err := ctl.State(ctx)
	if err != nil {
		return c.errCode(err)
	}
	if err := c.guard(ctx, ctl, st, false); err != nil {
		return c.fail(exitFailed, "%v", err)
	}
	var todo []agent.StopScope
	for _, sc := range scopes {
		dup := false
		for _, a := range st.Active() {
			if a.Scope == sc && a.Compensation == c.compensation() {
				fmt.Fprintf(c.out, "already stopped: %s (stop %s, set by %s %s); nothing written\n", sc, short(a.ID), a.By, ago(a.At))
				dup = true
			}
		}
		if !dup {
			todo = append(todo, sc)
		}
	}
	if len(todo) == 0 {
		return exitOK
	}
	comp := c.compensation()
	for _, sc := range todo {
		fmt.Fprintf(c.out, "stop          %s\n", c.st.Bold(sc.String()))
	}
	fmt.Fprintf(c.out, "reason        %s\n", c.f.reason)
	fmt.Fprintf(c.out, "set by        %s@%s\n", by, host)
	fmt.Fprintf(c.out, "compensation  %s\n", compWord(comp))
	fmt.Fprintf(c.out, "mode          %s\n", c.modeLine(st))
	if c.f.stopPolicy == "" {
		fmt.Fprintf(c.out, "%s\n", c.st.Warn("this record is not signed: anyone with access to the journal can write or lift it"))
	}
	for _, sc := range todo {
		if sc.Kind == agent.StopAgent {
			fmt.Fprintf(c.out, "%s\n", c.st.Warn("note: an agent stop covers runs whose identity names the agent; a run with no identity is never covered. Use -all or -tool to cover those"))
			break
		}
	}
	if ok, usage := c.confirm("set "+countWord(len(todo), "stop"), false); !ok {
		if usage {
			return exitUsage
		}
		return c.fail(exitFailed, "not confirmed: nothing was written")
	}
	for i, sc := range todo {
		e, err := ctl.SetStop(ctx, agent.StopRequest{Scope: sc, Reason: c.f.reason, Compensation: comp, By: by, Host: host, Signer: signer})
		// The control run's write is once-only per scope: a command that lost the insert to another operator's
		// stop of the same scope (or found it in force) reports that stop and writes nothing.
		if dup, ok := errors.AsType[*agent.AlreadyStoppedError](err); ok {
			fmt.Fprintf(c.out, "already stopped: %s (stop %s, set by %s %s); nothing written\n", sc, short(dup.Entry.ID), dup.Entry.By, ago(dup.Entry.At))
			continue
		}
		if err == nil {
			err = c.landed(ctx, ctl, st, e.ID)
		}
		if err != nil {
			if i > 0 {
				fmt.Fprintf(c.errw, "%d of %d stops were set before this failure; -list shows them\n", i, len(todo))
			}
			return c.errCode(err)
		}
		fmt.Fprintf(c.out, "stop %s in force: %s\n", c.st.Bold(e.ID), sc)
	}
	return exitOK
}

func countWord(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return fmt.Sprintf("%d %ss", n, w)
}

// checkReason mirrors the record's own limits so a bad reason is a usage error before anything is shown or
// signed; the record validates it again.
func checkReason(r string) error {
	probe := agent.StopRecord{Format: agent.StopFormat, Kind: "lift", Mode: agent.StopBasic, StopID: strings.Repeat("0", 64), SetDigest: strings.Repeat("0", 64), Reason: r, At: time.Now().UTC().Format(time.RFC3339Nano), By: "x"}
	if len(r) > agent.MaxStopReason {
		return fmt.Errorf("longer than %d bytes", agent.MaxStopReason)
	}
	for _, ch := range r {
		if ch < 0x20 || ch == 0x7f || (ch >= 0x80 && ch < 0xa0) || ch == 0x2028 || ch == 0x2029 || (ch >= 0x202a && ch <= 0x202e) || (ch >= 0x2066 && ch <= 0x2069) || ch == 0xfffd {
			return errors.New("a control or bidirectional character, or invalid UTF-8")
		}
	}
	_ = probe
	return nil
}

// resolve finds the one stop id (or a prefix of it) names.
func resolve(st agent.StopState, id string) (agent.StopEntry, error) {
	var hit []agent.StopEntry
	for _, s := range st.Stops {
		if s.ID == id {
			return s, nil
		}
		if strings.HasPrefix(s.ID, id) {
			hit = append(hit, s)
		}
	}
	switch len(hit) {
	case 1:
		return hit[0], nil
	case 0:
		return agent.StopEntry{}, fmt.Errorf("no accepted stop %q (-list shows them; a rejected record is not a stop)", id)
	}
	return agent.StopEntry{}, fmt.Errorf("%q names %d stops: give more of the ID", id, len(hit))
}

func (c *cmd) liftStop(ctx context.Context, ctl *agent.StopControl, j *agent.Journal, doc *agent.StopPolicyDoc, signer audit.Signer, by, host string) int {
	if err := checkReason(c.f.reason); err != nil {
		return c.fail(exitUsage, "-reason: %v", err)
	}
	if err := c.preflight(ctx, ctl, doc, signer, "lifter"); err != nil {
		return c.fail(exitFailed, "%v", err)
	}
	st, err := ctl.State(ctx)
	if err != nil {
		return c.errCode(err)
	}
	e, err := resolve(st, c.f.lift)
	if err != nil {
		return c.fail(exitFailed, "%v", err)
	}
	if err := c.guard(ctx, ctl, st, false); err != nil {
		return c.fail(exitFailed, "%v", err)
	}
	if e.Lifted {
		fmt.Fprintf(c.out, "stop %s is already lifted (by %s); nothing written\n", short(e.ID), strings.Join(e.LiftedBy, ", "))
		return exitOK
	}
	c.printStop(e, false)
	c.printHeld(ctx, j, e)
	fmt.Fprintf(c.out, "lift reason   %s\n", c.f.reason)
	fmt.Fprintf(c.out, "lifted by     %s@%s\n", by, host)
	fmt.Fprintf(c.out, "mode          %s\n", c.modeLine(st))
	if st.Mode == agent.StopBasic && e.Signed {
		return c.fail(exitFailed, "stop %s was set under a signed record: a plain lift does not lift it, and a record with no effect would only litter the control run. Give -stop-policy, -as and -key; nothing was written", short(e.ID))
	}
	if ok, usage := c.confirm("lift stop "+short(e.ID), true); !ok {
		if usage {
			return exitUsage
		}
		return c.fail(exitFailed, "not confirmed: nothing was written")
	}
	e, err = ctl.LiftStop(ctx, e.ID, agent.LiftRequest{Reason: c.f.reason, By: by, Host: host, Signer: signer})
	if err != nil {
		return c.writeErr(ctx, ctl, err)
	}
	if e.Lifted {
		fmt.Fprintf(c.out, "stop %s lifted: runs held by it carry on when their recovery drives them\n", c.st.Bold(e.ID))
	} else {
		fmt.Fprintf(c.out, "your lift of stop %s is recorded: %d of %d lifts held, %d more needed; the stop is still in force\n", c.st.Bold(short(e.ID)), e.LiftsHeld, e.LiftsNeeded, max(0, e.LiftsNeeded-e.LiftsHeld))
	}
	return exitOK
}

// writeErr is errCode for a record that is not a stop: the trail is written first, so a failure after it
// landed leaves the record on the trail and not in force. That is exit 5, found by asking the control what
// waits on the trail.
func (c *cmd) writeErr(ctx context.Context, ctl *agent.StopControl, err error) int {
	if _, ok := errors.AsType[*agent.StopTrailError](err); !ok && c.f.stopPolicy != "" {
		if pend, perr := ctl.PendingRecords(ctx); perr == nil {
			for _, p := range pend {
				if !p.Superseded && p.Record.By == c.f.as {
					return c.fail(exitHalfDone, "%v\nyour record is on the stop trail but not in force; run %s -apply-pending (same -db, -stop-policy) to apply it", err, c.src.Prog)
				}
			}
		}
	}
	return c.errCode(err)
}

func (c *cmd) printStop(e agent.StopEntry, brief bool) {
	fmt.Fprintf(c.out, "stop          %s\n", c.st.Bold(e.ID))
	fmt.Fprintf(c.out, "scope         %s\n", e.Scope)
	fmt.Fprintf(c.out, "reason        %s\n", e.Reason)
	signed := "not signed (asserted, not authenticated)"
	if e.Signed {
		signed = "signed"
	}
	fmt.Fprintf(c.out, "set by        %s@%s, %s, %s (%s)\n", e.By, e.Host, ago(e.At), e.At, signed)
	fmt.Fprintf(c.out, "compensation  %s\n", compWord(e.Compensation))
	if e.AdoptedFrom != "" {
		fmt.Fprintf(c.out, "adopted from  plain record %s\n", short(e.AdoptedFrom))
	}
	if e.Lifted {
		fmt.Fprintf(c.out, "state         lifted by %s\n", strings.Join(e.LiftedBy, ", "))
		return
	}
	fmt.Fprintf(c.out, "state         in force; %d of %d lifts held\n", e.LiftsHeld, e.LiftsNeeded)
}

func (c *cmd) reauditTrail(ctx context.Context, ctl *agent.StopControl) int {
	if st, err := ctl.State(ctx); err != nil {
		return c.errCode(err)
	} else if err := c.guard(ctx, ctl, st, false); err != nil {
		return c.fail(exitFailed, "%v", err)
	}
	n, err := ctl.Reaudit(ctx)
	if err != nil {
		return c.errCode(err)
	}
	fmt.Fprintf(c.out, "stop trail: appended %s the control run had and the trail lacked\n", countWord(n, "record"))
	return exitOK
}

func (c *cmd) applyPending(ctx context.Context, ctl *agent.StopControl, signer audit.Signer, by, host string) int {
	if err := checkReason(c.f.reason); err != nil {
		return c.fail(exitUsage, "-reason: %v", err)
	}
	if st, err := ctl.State(ctx); err != nil {
		return c.errCode(err)
	} else if err := c.guard(ctx, ctl, st, false); err != nil {
		return c.fail(exitFailed, "%v", err)
	}
	pend, err := ctl.PendingRecords(ctx)
	if err != nil {
		return c.errCode(err)
	}
	if len(pend) == 0 {
		fmt.Fprintln(c.out, "nothing is pending: every signed record on the trail that is not a stop is in the control run")
		return exitOK
	}
	apply := 0
	for _, p := range pend {
		r := p.Record
		state := "WILL APPLY"
		if p.Superseded {
			state = "SUPERSEDED (a later record is already in force: left as it is)"
		} else {
			apply++
		}
		fmt.Fprintf(c.out, "%-10s signed by %s, written %s ago: %s", r.Kind, r.By, age(p.Age), c.st.Bold(short(agent.StopDigest(r))))
		if r.StopID != "" {
			fmt.Fprintf(c.out, " (stop %s)", short(r.StopID))
		}
		fmt.Fprintf(c.out, "  %s\n", state)
		if r.Reason != "" {
			fmt.Fprintf(c.out, "           reason: %s\n", r.Reason)
		}
	}
	if apply == 0 {
		fmt.Fprintln(c.out, "nothing to apply: every pending record is superseded")
		return exitOK
	}
	fmt.Fprintf(c.out, "A signature cannot be revoked: applying makes each record effective however old it is. Your apply record names them, signed by %s.\n", by)
	if ok, usage := c.confirm("apply the pending records", true); !ok {
		if usage {
			return exitUsage
		}
		return c.fail(exitFailed, "not confirmed: nothing was written")
	}
	applied, superseded, err := ctl.ApplyPending(ctx, agent.ApplyRequest{By: by, Host: host, Reason: c.f.reason, Signer: signer})
	if err != nil {
		if applied > 0 {
			fmt.Fprintf(c.errw, "%d record(s) were applied before this failure; -list shows the rest\n", applied)
		}
		return c.errCode(err)
	}
	fmt.Fprintf(c.out, "applied %d, left %d superseded\n", applied, superseded)
	return exitOK
}

func (c *cmd) changeMode(ctx context.Context, ctl *agent.StopControl, o Opened, signer audit.Signer, by, host string) int {
	if err := checkReason(c.f.reason); err != nil {
		return c.fail(exitUsage, "-reason: %v", err)
	}
	to := agent.StopAudited
	if c.f.downgrade {
		to = agent.StopBasic
	}
	st, err := ctl.State(ctx)
	if err != nil {
		return c.errCode(err)
	}
	if err := c.guard(ctx, ctl, st, to == agent.StopAudited); err != nil {
		return c.fail(exitFailed, "%v", err)
	}
	if to == agent.StopBasic && len(st.Active()) > 0 {
		return c.fail(exitFailed, "%d stop(s) are active: lift them before leaving audited mode (an operator must not lift a stop by leaving audited mode)", len(st.Active()))
	}
	if to == agent.StopAudited {
		j, err := agent.NewJournal(o.Store)
		if err != nil {
			return c.errCode(err)
		}
		bctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopBasic))
		if err != nil {
			return c.errCode(err)
		}
		bs, err := bctl.State(ctx)
		if err != nil {
			return c.errCode(err)
		}
		for _, p := range bs.Active() {
			fmt.Fprintf(c.out, "adopt         %s (plain stop %s by %s): set again, signed by %s\n", p.Scope, short(p.ID), p.By, by)
		}
		fmt.Fprintf(c.out, "Roll the policy file out to every process first; a process left basic holds side-effect calls it cannot judge.\n")
	} else {
		fmt.Fprintf(c.out, "Leaving audited mode is a configuration rollout; this records the change on the trail, and the viewer shows the period as unaudited.\n")
	}
	fmt.Fprintf(c.out, "record        change of mode to %s, signed by %s\n", to, by)
	if ok, usage := c.confirm("record the change of mode to "+string(to), true); !ok {
		if usage {
			return exitUsage
		}
		return c.fail(exitFailed, "not confirmed: nothing was written")
	}
	if _, err := ctl.ChangeMode(ctx, agent.ModeRequest{To: to, Reason: c.f.reason, By: by, Host: host, Signer: signer}); err != nil {
		return c.writeErr(ctx, ctl, err)
	}
	fmt.Fprintf(c.out, "recorded: the deployment is %s from the processes' next start with this configuration\n", to)
	return exitOK
}

type jsonScope struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

type jsonStop struct {
	ID           string    `json:"id"`
	Scope        jsonScope `json:"scope"`
	Reason       string    `json:"reason"`
	Compensation string    `json:"compensation"`
	By           string    `json:"by"`
	Host         string    `json:"host,omitempty"`
	At           string    `json:"at"`
	Signed       bool      `json:"signed"`
	Active       bool      `json:"active"`
	LiftedBy     []string  `json:"lifted_by,omitempty"`
	LiftsHeld    int       `json:"lifts_held"`
	LiftsNeeded  int       `json:"lifts_needed"`
	AdoptedFrom  string    `json:"adopted_from,omitempty"`
}

type jsonRejected struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type jsonPending struct {
	Kind       string `json:"kind"`
	Digest     string `json:"digest"`
	By         string `json:"by"`
	AgeSeconds int64  `json:"age_seconds"`
	StopID     string `json:"stop_id,omitempty"`
	Superseded bool   `json:"superseded"`
}

// jsonState is bide stop -list -json: a stable schema.
type jsonState struct {
	Mode             string         `json:"mode"`
	PolicyDigest     string         `json:"policy_digest,omitempty"`
	PolicyTransition int            `json:"policy_transitions"`
	Unknowable       string         `json:"unknowable,omitempty"`
	Unverified       int            `json:"unverified"`
	UnverifiedActive int            `json:"unverified_active"`
	Stops            []jsonStop     `json:"stops"`
	Rejected         []jsonRejected `json:"rejected"`
	RejectedTotal    int            `json:"rejected_total"`
	Pending          []jsonPending  `json:"pending"`
}

func (c *cmd) state(ctx context.Context, o Opened) (agent.StopState, []agent.PendingStop, int) {
	ctl, _, err := c.control(o)
	if err != nil {
		return agent.StopState{}, nil, c.errCode(err)
	}
	st, err := ctl.State(ctx)
	if err != nil {
		return st, nil, c.errCode(err)
	}
	var pend []agent.PendingStop
	if c.f.stopPolicy != "" && o.Events != nil {
		if pend, err = ctl.PendingRecords(ctx); err != nil {
			return st, nil, c.errCode(err)
		}
	}
	return st, pend, exitOK
}

// printState is -list (and the list a bare command shows).
func (c *cmd) printState(ctx context.Context, o Opened, asList bool) int {
	st, pend, code := c.state(ctx, o)
	if code != exitOK {
		return code
	}
	if asList && c.f.asJSON {
		js := jsonState{Mode: string(st.Mode), PolicyDigest: st.PolicyDigest, PolicyTransition: st.PolicySeq, Unverified: st.Unverified, UnverifiedActive: st.UnverifiedActive,
			Stops: []jsonStop{}, Rejected: []jsonRejected{}, RejectedTotal: st.RejectedTotal, Pending: []jsonPending{}}
		if err := st.UnknowableErr(); err != nil {
			js.Unknowable = err.Error()
		}
		for _, s := range st.Stops {
			js.Stops = append(js.Stops, jsonStop{ID: s.ID, Scope: jsonScope{Kind: string(s.Scope.Kind), Name: s.Scope.Name}, Reason: s.Reason, Compensation: s.Compensation, By: s.By, Host: s.Host, At: s.At,
				Signed: s.Signed, Active: !s.Lifted, LiftedBy: s.LiftedBy, LiftsHeld: s.LiftsHeld, LiftsNeeded: s.LiftsNeeded, AdoptedFrom: s.AdoptedFrom})
		}
		for _, r := range st.Rejected {
			js.Rejected = append(js.Rejected, jsonRejected{Name: r.Name, Reason: r.Reason})
		}
		for _, p := range pend {
			js.Pending = append(js.Pending, jsonPending{Kind: p.Record.Kind, Digest: agent.StopDigest(p.Record), By: p.Record.By, AgeSeconds: int64(p.Age.Seconds()), StopID: p.Record.StopID, Superseded: p.Superseded})
		}
		b, err := json.MarshalIndent(js, "", "  ")
		if err != nil {
			return c.fail(exitFailed, "%v", err)
		}
		fmt.Fprintf(c.out, "%s\n", b)
		return exitOK
	}
	fmt.Fprintf(c.out, "mode  %s\n", c.modeLine(st))
	if err := st.UnknowableErr(); err != nil {
		fmt.Fprintf(c.out, "%s\n", c.st.Fail("STOPS UNKNOWABLE: "+err.Error()+"; side-effect calls a stop could cover are held until this is fixed"))
	}
	if st.Unverified > 0 {
		fmt.Fprintf(c.out, "%s\n", c.st.Warn(fmt.Sprintf("signatures NOT verified: %d signed record(s) in the control run cannot be checked without -stop-policy", st.Unverified)))
	}
	act := st.Active()
	if len(act) == 0 {
		fmt.Fprintln(c.out, "no stop is active")
	}
	for _, s := range act {
		fmt.Fprintln(c.out)
		c.printStop(s, true)
	}
	if n := len(st.Stops) - len(act); n > 0 {
		fmt.Fprintf(c.out, "\n%d earlier stop(s) lifted (-show <id> for one)\n", n)
	}
	if st.RejectedTotal > 0 {
		fmt.Fprintf(c.out, "\n%s\n", c.st.Warn(fmt.Sprintf("%d record(s) in the control run were rejected and have no effect:", st.RejectedTotal)))
		for _, r := range st.Rejected {
			fmt.Fprintf(c.out, "  %s: %s\n", r.Name, r.Reason)
		}
	}
	if len(pend) > 0 {
		fmt.Fprintf(c.out, "\n%d signed record(s) on the trail never took effect (UNAPPLIED or SUPERSEDED): %s -apply-pending shows each\n", len(pend), c.src.Prog)
	}
	return exitOK
}

func (c *cmd) showStop(ctx context.Context, o Opened) int {
	ctl, _, err := c.control(o)
	if err != nil {
		return c.errCode(err)
	}
	st, err := ctl.State(ctx)
	if err != nil {
		return c.errCode(err)
	}
	e, err := resolve(st, c.f.show)
	if err != nil {
		return c.fail(exitFailed, "%v", err)
	}
	fmt.Fprintf(c.out, "mode          %s\n", c.modeLine(st))
	c.printStop(e, false)
	recs, err := ctl.StopRecords(ctx)
	if err != nil {
		return c.errCode(err)
	}
	for _, r := range recs {
		if r.Kind == "lift" && r.StopID == e.ID {
			fmt.Fprintf(c.out, "lift record   by %s@%s, %s: %s\n", r.By, r.Host, ago(r.At), r.Reason)
		}
	}
	return exitOK
}

// guard refuses, before anything is written, a write the deployment's processes would not honor. Each case
// was a record the command could write and report as in force while nothing was stopped:
//
//   - a plain record on a deployment that is audited: its audited processes ignore plain records. (A
//     deployment that moved back to basic, by a signed -downgrade, is basic again.)
//   - an audited write whose policy the store's chain does not reach (the wrong root, a fork): the record
//     would be rejected by the processes that hold the real policy, and a competing root would be written.
//   - the first audited write of a deployment with plain stops in force: the audited view ignores them, so
//     they would vanish from it while basic processes still enforce them. -upgrade adopts them first.
func (c *cmd) guard(ctx context.Context, ctl *agent.StopControl, st agent.StopState, upgrading bool) error {
	recs, err := ctl.StopRecords(ctx)
	if err != nil {
		return err
	}
	signed, backToBasic := false, false
	for _, r := range recs {
		signed = signed || r.Signed()
		if r.Kind == "mode" {
			backToBasic = r.To == agent.StopBasic
		}
	}
	if c.f.stopPolicy == "" {
		if signed && !backToBasic {
			return errors.New("this deployment is audited: its processes ignore a plain record, so nothing would be stopped or lifted. Give -stop-policy, -as and -key; nothing was written")
		}
		return nil
	}
	if st.ChainErr != "" {
		return fmt.Errorf("the store's policy chain does not reach this policy file (%s): give the deployment's root policy, or -stop-root; nothing was written", st.ChainErr)
	}
	if err := st.UnknowableErr(); err != nil {
		return fmt.Errorf("%v; nothing was written", err)
	}
	if !signed && !upgrading && c.j != nil {
		bctl, err := agent.NewStopControl(c.j, agent.WithStopMode(agent.StopBasic))
		if err != nil {
			return err
		}
		bs, err := bctl.State(ctx)
		if err != nil {
			return err
		}
		if n := len(bs.Active()); n > 0 {
			return fmt.Errorf("%s in force were set plain, and an audited record would hide them from audited processes while basic ones still enforce them: run -upgrade first, which adopts them; nothing was written", countWord(n, "stop"))
		}
	}
	return nil
}

// landed reports whether the record the control just wrote counts in the policy's view: a stop is in force,
// and no new record was rejected. A record that is written and ignored is a failure, not a success.
func (c *cmd) landed(ctx context.Context, ctl *agent.StopControl, before agent.StopState, id string) error {
	after, err := ctl.State(ctx)
	if err != nil {
		return err
	}
	if after.RejectedTotal > before.RejectedTotal {
		return fmt.Errorf("the record was written but the control run rejected it (%d rejected record(s)): %w", after.RejectedTotal-before.RejectedTotal, agent.ErrStopRejected)
	}
	if id != "" {
		for _, s := range after.Active() {
			if s.ID == id {
				return nil
			}
		}
		return fmt.Errorf("the stop was written but is not in force in the policy's view: %w", agent.ErrStopRejected)
	}
	return nil
}

// heldTimeout bounds the count of held runs: it is a courtesy before the prompt and must not hold up a lift.
var heldTimeout = 5 * time.Second

// printHeld says how many unfinished runs the stop holds: what the lift lets carry on.
func (c *cmd) printHeld(ctx context.Context, j *agent.Journal, e agent.StopEntry) {
	cctx, cancel := context.WithTimeout(ctx, heldTimeout)
	defer cancel()
	n := runview.HeldCount(cctx, runview.JournalSource{J: j}, e)
	switch {
	case n.Incomplete:
		fmt.Fprintf(c.out, "runs held     at least %d held (count incomplete: %s after %s read)\n", n.Held, n.ListErr, countWord(n.Read, "unfinished run"))
		return
	case n.ListErr != "":
		fmt.Fprintf(c.out, "runs held     not counted: the runs could not be listed (%s)\n", n.ListErr)
		return
	case !runview.JournalSource{J: j}.CanList():
		fmt.Fprintf(c.out, "runs held     not counted: this store cannot list its runs\n")
		return
	}
	line := fmt.Sprintf("%d held", n.Held)
	if n.May > 0 {
		line += fmt.Sprintf(", %d may be held (an agent stop covers runs by the agent identity that drives them; the journal does not record it)", n.May)
	}
	line += fmt.Sprintf(" of %s read", countWord(n.Read, "unfinished run"))
	if n.Unread > 0 {
		line += fmt.Sprintf(", %d could not be read", n.Unread)
	}
	if n.Truncated {
		line += "; more unfinished runs exist and were not read"
	}
	fmt.Fprintf(c.out, "runs held     %s\n", line)
}

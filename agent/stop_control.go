package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// StopSigner signs a stop record in audited mode. audit.Signer implements it, so a key file, an HSM or
// a KMS key signs a stop exactly as it signs an approval.
type StopSigner interface {
	Alg() Alg
	Sign(message []byte) ([]byte, error)
}

// StopOption configures NewStopControl.
type StopOption interface{ applyStop(*StopControl) error }

type stopOption func(*StopControl) error

func (f stopOption) applyStop(c *StopControl) error { return f(c) }

// WithStopMode declares the mode of this process: StopBasic, or StopAudited (which needs
// WithStopPolicy). The mode is the process's own configuration, never something the store says, and
// it must be declared: a control that reads a control run holding records and has no declared mode
// fails with ErrConfig, so a process cannot be silently basic or audited by accident.
func WithStopMode(m StopMode) StopOption {
	return stopOption(func(c *StopControl) error {
		if m != StopBasic && m != StopAudited {
			return fmt.Errorf("WithStopMode(%q): %w", m, ErrConfig)
		}
		c.mode = m
		return nil
	})
}

// WithStopPolicy gives the control the audited-mode policy: the trust root, the policy this process
// pins (normally the one the deployment started audited mode with). It is validated here and again
// whenever State uses it. The later policies are the control run's own, by signed transitions.
func WithStopPolicy(p StopPolicy) StopOption {
	return stopOption(func(c *StopControl) error {
		if err := p.Validate(); err != nil {
			return err
		}
		c.policy = &p
		return nil
	})
}

// WithStopVerifier gives the control the means to build the verifiers of a policy a transition
// carries (audit.NewVerifier). Without it a control cannot adopt a policy transition: it reports it
// as rejected, and the policy in force stays the one it was given.
func WithStopVerifier(nv StopVerifierFunc) StopOption {
	return stopOption(func(c *StopControl) error { c.nv = nv; return nil })
}

// WithStopClock sets the clock that stamps a record's At (default time.Now).
func WithStopClock(now func() time.Time) StopOption {
	return stopOption(func(c *StopControl) error {
		if now == nil {
			return fmt.Errorf("WithStopClock: nil clock: %w", ErrConfig)
		}
		c.now = now
		return nil
	})
}

// StopControl reads and writes the control run: the stops, lifts, mode and policy records of a
// deployment. It is the one place that decides which records count (see State), so every reader, the
// engine's check points included, sees the same effective stops.
type StopControl struct {
	j      *Journal
	policy *StopPolicy
	nv     StopVerifierFunc
	mode   StopMode
	now    func() time.Time
}

// NewStopControl returns the control over j's store.
func NewStopControl(j *Journal, opts ...StopOption) (*StopControl, error) {
	if j == nil {
		return nil, fmt.Errorf("NewStopControl: nil journal: %w", ErrConfig)
	}
	c := &StopControl{j: j, now: time.Now}
	for _, o := range opts {
		if o == nil {
			return nil, fmt.Errorf("NewStopControl: nil option: %w", ErrConfig)
		}
		if err := o.applyStop(c); err != nil {
			return nil, err
		}
	}
	switch {
	case c.mode == StopAudited && c.policy == nil:
		return nil, fmt.Errorf("NewStopControl: audited mode needs WithStopPolicy: %w", ErrConfig)
	case c.mode == StopBasic && c.policy != nil:
		return nil, fmt.Errorf("NewStopControl: basic mode takes no WithStopPolicy: %w", ErrConfig)
	case c.mode == "" && c.policy != nil:
		c.mode = "" // declared at State, once the control run holds records
	}
	return c, nil
}

// Mode is the mode the control runs under: the declared one, else audited with a policy and basic
// without (an undeclared mode is refused as soon as the control run holds a record).
func (c *StopControl) Mode() StopMode {
	switch {
	case c.mode != "":
		return c.mode
	case c.policy != nil:
		return StopAudited
	}
	return StopBasic
}

// ErrStopsUnknowable is returned by StopState.UnknowableErr when a control genuinely cannot judge which
// stops are in force: a basic process that holds an active stop set under a signed record (it lacks the
// deployment's policy), or an audited process whose pin the stored chain does not reach, or whose chain
// forks or competes. The engine treats it as "stops unknowable" and fails closed for side-effect calls
// that a stop could cover; a stop or a lift can still be written. A forged record is not a cause: in an
// audited control with its pin on the chain it is only Rejected.
var ErrStopsUnknowable = errors.New("agent: stops unknowable: the control run holds signed-looking records this process cannot verify")

// maxRejected bounds StopState.Rejected.
const maxRejected = 100

// StopEntry is one stop as State reports it.
type StopEntry struct {
	ID           string
	Scope        StopScope
	Reason       string
	Compensation string
	By, Host, At string
	// Signed is true for a stop whose set record was signed; in a basic control it was not checked.
	Signed bool
	// Lifted is true once the lift rule in force is met; a lifted stop never becomes active again.
	Lifted bool
	// LiftedBy names the lifters whose records counted. LiftsHeld and LiftsNeeded say how far an
	// active stop is from being lifted.
	// AdoptedFrom is the digest of the plain record this stop re-states (a move to audited mode).
	AdoptedFrom            string
	LiftedBy               []string
	LiftsHeld, LiftsNeeded int
	digest                 string
	liftSet                map[string]bool
}

// Rejection is a record in the control run that has no effect, and why.
type Rejection struct{ Name, Reason string }

// StopState is the effective state of the control run for one control.
type StopState struct {
	// Mode is the control's mode (its configuration).
	Mode StopMode
	// PolicyDigest and PolicySeq name the policy in force after the policy transitions that took
	// effect (an audited control; zero when none did).
	PolicyDigest string
	PolicySeq    int
	// ChainErr, when set, says why this audited control cannot trust the chain it walked: the pinned
	// policy is not on it, two roots or two successors of one policy compete, or a rollback. The
	// stops are unknowable (UnknowableErr); a stop is never silently dropped.
	ChainErr string
	// UnverifiedActive counts, in a basic control, the active stops set under a signed record: it
	// cannot check the lift rule they were set under.
	UnverifiedActive int
	// Stops holds every accepted stop in the order set.
	Stops []StopEntry
	// Rejected lists the first maxRejected records with no effect and why; RejectedTotal counts them
	// all, so a flood of garbage does not grow the result.
	Rejected      []Rejection
	RejectedTotal int
	// Unverified counts, in a basic control, the signed records it cannot check because it has no policy
	// (the deployment may have moved to audited mode without this process): say so loudly. An audited
	// control does not count here; its forgeries are in Rejected.
	Unverified int
}

// UnknowableErr returns ErrStopsUnknowable when the control cannot judge which stops are in force (see
// the error). The stops it can see are in State. The engine fails closed on it for side-effect calls a
// stop could cover.
func (s StopState) UnknowableErr() error {
	switch {
	case s.ChainErr != "":
		return fmt.Errorf("%s: %w", s.ChainErr, ErrStopsUnknowable)
	case s.Mode == StopBasic && s.UnverifiedActive > 0:
		return fmt.Errorf("%d active stop(s) were set under signed records this basic process cannot check (is it missing the deployment's stop policy?): %w", s.UnverifiedActive, ErrStopsUnknowable)
	}
	return nil
}

// Active returns the stops that are not lifted, in the order they were set.
func (s StopState) Active() []StopEntry {
	var out []StopEntry
	for _, st := range s.Stops {
		if !st.Lifted {
			out = append(out, st)
		}
	}
	return out
}

// StopQuery is what a check point asks: which stop, if any, covers this. Fill the fields the point
// knows (a call knows its Tool, the flow and agent it runs under); a field left empty matches no stop
// of that scope. Compensation is true when the thing asked about is a compensation.
type StopQuery struct {
	Tool, Flow, Agent string
	Compensation      bool
}

// Covers reports whether s covers q: a global stop covers everything; a tool, flow or agent stop
// covers a query that names exactly its name. A compensation is covered only by a stop that blocks it.
func (s StopEntry) Covers(q StopQuery) bool {
	if s.Lifted || q.Compensation && s.Compensation != CompensationBlock {
		return false
	}
	switch s.Scope.Kind {
	case StopGlobal:
		return true
	case StopTool:
		return q.Tool != "" && q.Tool == s.Scope.Name
	case StopFlow:
		return q.Flow != "" && q.Flow == s.Scope.Name
	case StopAgent:
		return q.Agent != "" && q.Agent == s.Scope.Name
	}
	return false
}

// Covering returns the first active stop that covers q.
func (s StopState) Covering(q StopQuery) (StopEntry, bool) {
	for _, st := range s.Stops {
		if st.Covers(q) {
			return st, true
		}
	}
	return StopEntry{}, false
}

// protocol:stop begin ApplyC EffC

// State folds the control run into the effective state. Which records count is decided here and
// nowhere else, and nothing the store holds can make it fail: a record that does not check is
// Rejected, never an error. (An error is the control's own configuration: a policy that no longer
// validates, or a mode that was not declared.)
//
//   - Records are read in the store's order. A stop is active from its set record until the lift
//     rule is met at a lift's position; it never becomes active again.
//   - A basic control (no policy) trusts the store: any well-formed set record is a stop and one lift
//     lifts it, except that a stop set under a signed record (an audited deployment's) is lifted only by
//     a signed lift, never by a plain one. Signed records it cannot check are counted in Unverified,
//     and an active stop set under a signed record makes the stops unknowable (UnknowableErr): the
//     process is probably a basic one on an audited deployment.
//   - An audited control accepts a set only if it is signed by a setter, and counts a lift only if it
//     is signed by a lifter, of the policy in force at that record's position and for this
//     deployment: LiftQuorum distinct lifters, one not the setter with LiftNeedsOther. A plain or
//     unsigned record is ignored.
//   - The policy in force starts at the deployment's root record (the first one that verifies under
//     its own policy) and advances by transitions: a "policy" record counts when it names the next
//     number and the digest in force and LiftQuorum distinct lifters of the policy in force have signed
//     it. The control's own policy is a pin: it must be on that chain (the root itself, normally, or
//     a later policy the walk reaches), else the stops are unknowable. A stop set under an earlier
//     policy stays in force. Forks and competing roots make the stops unknowable too.
//   - "mode" records are evidence of a change of mode and nothing else: the mode is configuration.
func (c *StopControl) State(ctx context.Context) (StopState, error) {
	audited := c.Mode() == StopAudited
	var pin StopPolicy
	if audited {
		if err := c.policy.Validate(); err != nil {
			return StopState{}, err // the control's own policy changed under it
		}
		pin = *c.policy
	}
	recs, err := c.j.History(ctx, StopControlRun)
	if err != nil {
		return StopState{}, err
	}
	if c.mode == "" && len(recs) > 0 {
		return StopState{}, fmt.Errorf("the control run holds records and this process has not declared its stop mode (WithStopMode): %w", ErrConfig)
	}
	if !audited {
		st, _ := c.foldFrom(recs, false, pin, "")
		return st, nil
	}
	pinDigest := pin.Digest()
	// The chain starts at a root record that verifies under its own policy: the one that is the pin
	// if there is one, else the first. A competing root is only Rejected (anyone can write a
	// self-signed one); the pin decides which chain is this deployment's.
	var roots []StopPolicy
	hasRootRecord := false
	for _, rec := range recs {
		if rec.Kind != StepValue || !strings.HasPrefix(rec.Name, "@stop/root/") {
			continue
		}
		r, err := decodeStopRecord(rec.Result)
		if err != nil || r.Kind != "root" || stopRecordStep(r) != rec.Name {
			continue
		}
		if c.nv == nil && StopPolicyDigest(*r.Policy) != pinDigest {
			hasRootRecord = true // may be genuine, and this process cannot build it: see below
			continue
		}
		rp, err := c.policyOf(*r.Policy, pin, pinDigest)
		if err != nil || !(rp.verify(r, "lifter") || rp.verify(r, "setter")) {
			continue
		}
		if !slices.ContainsFunc(roots, func(o StopPolicy) bool { return o.Digest() == rp.Digest() }) {
			roots = append(roots, rp)
		}
	}
	// The root that is the pin first, then the others in store order: the first whose chain reaches the pin
	// is the deployment's.
	slices.SortStableFunc(roots, func(x, y StopPolicy) int {
		return map[bool]int{true: 0, false: 1}[x.Digest() == pinDigest] - map[bool]int{true: 0, false: 1}[y.Digest() == pinDigest]
	})
	for _, root := range roots {
		if st, reached := c.foldFrom(recs, true, root, root.Digest()); reached {
			return st, nil
		}
	}
	st, _ := c.foldFrom(recs, true, pin, pinDigest)
	switch {
	case len(roots) > 0:
		// No stored root's chain reaches the pin: this process's policy is not this deployment's, or the
		// roots are not. Judged under the pin; the stops are unknowable.
		st.ChainErr = "the pinned policy is not on the store's chain (configure the deployment's root policy, or one the chain reaches)"
	case hasRootRecord:
		st.ChainErr = "the control run has a root policy this process cannot build (give it a verifier constructor, WithStopVerifier) or pin the root policy"
	}
	return st, nil
}

// foldFrom folds recs starting from the policy start (the root, or the pin when there is none), and
// reports whether the control's own policy is on the chain it walked.
func (c *StopControl) foldFrom(recs []Record, audited bool, start StopPolicy, startDigest string) (StopState, bool) {
	cur := start
	st := StopState{Mode: c.Mode()}
	reject := func(name, why string) {
		st.RejectedTotal++
		if len(st.Rejected) < maxRejected {
			st.Rejected = append(st.Rejected, Rejection{name, why})
		}
	}
	broken := func(why string) {
		if st.ChainErr == "" {
			st.ChainErr = why
		}
	}
	chain := []string{}
	var prevPol StopPolicy
	var prevDigest string
	forks := map[string]map[string]StopRecord{}
	if audited {
		st.PolicyDigest = startDigest
		chain = append(chain, startDigest)
	}
	unverified := func(r StopRecord) {
		if !audited && r.Signed() {
			st.Unverified++
		}
	}
	byID := map[string]int{}
	approvals := map[string]map[string]StopRecord{} // policy transition number -> signer -> approval
	for _, rec := range recs {
		if rec.Kind != StepValue || !strings.HasPrefix(rec.Name, "@stop/") {
			continue
		}
		r, err := decodeStopRecord(rec.Result)
		if err != nil || stopRecordStep(r) != rec.Name {
			reject(rec.Name, "malformed")
			continue
		}
		switch r.Kind {
		case "root":
			if !audited {
				unverified(r)
				reject(rec.Name, "not checked: this process has no stop policy")
			} else if r.Policy != nil && StopPolicyDigest(*r.Policy) != startDigest {
				reject(rec.Name, "a root policy other than the one this chain starts from")
			}
		case "set":
			if _, dup := byID[r.StopID]; dup {
				continue
			}
			if audited && !cur.verify(r, "setter") {
				reject(rec.Name, "not signed by an enrolled setter of the policy in force, for this deployment")
				unverified(r)
				continue
			}
			if !audited {
				unverified(r)
			}
			byID[r.StopID] = len(st.Stops)
			st.Stops = append(st.Stops, StopEntry{ID: r.StopID, Scope: *r.Scope, Reason: r.Reason, Compensation: r.Compensation, By: r.By, Host: r.Host, At: r.At,
				AdoptedFrom: r.AdoptedFrom, Signed: r.Signed(), digest: StopDigest(r), liftSet: map[string]bool{}})
		case "lift":
			i, ok := byID[r.StopID]
			if !ok || st.Stops[i].digest != r.SetDigest {
				reject(rec.Name, "it names no accepted stop")
				continue
			}
			s := &st.Stops[i]
			if s.Lifted {
				continue
			}
			if !audited {
				if s.Signed && !r.Signed() {
					reject(rec.Name, "a stop set under a signed record is not lifted by a plain one")
					continue
				}
				unverified(r)
				s.Lifted, s.LiftedBy = true, []string{r.By}
				continue
			}
			if !cur.verify(r, "lifter") {
				reject(rec.Name, "not signed by an enrolled lifter of the policy in force, for this deployment")
				unverified(r)
				continue
			}
			s.liftSet[r.By] = true
			countLifts(s, cur)
		case "mode":
			switch {
			case !audited:
				unverified(r)
				if r.Signed() {
					reject(rec.Name, "not checked: this process has no stop policy")
				}
			case !cur.verify(r, "lifter") && !cur.verify(r, "setter"):
				reject(rec.Name, "not signed by an enrolled approver of the policy in force, for this deployment")
				unverified(r)
			}
		case "policy":
			if !audited {
				unverified(r)
				reject(rec.Name, "not checked: this process has no stop policy")
				continue
			}
			if r.Seq == st.PolicySeq && st.PolicySeq > 0 && r.PrevDigest == prevDigest && prevPol.verify(r, "lifter") && StopPolicyDigest(*r.Policy) != st.PolicyDigest {
				// Another successor of the policy that was replaced: a fork, once its quorum signs it.
				d := StopPolicyDigest(*r.Policy)
				if forks[d] == nil {
					forks[d] = map[string]StopRecord{}
				}
				forks[d][r.By] = r
				if len(forks[d]) >= prevPol.LiftQuorum {
					broken(fmt.Sprintf("the policy chain forks at transition %d: a quorum of the earlier policy also approved a different successor", r.Seq))
				}
				reject(rec.Name, "an approval of another successor of the policy already replaced")
				continue
			}
			if !cur.verify(r, "lifter") {
				reject(rec.Name, "not signed by a lifter of the policy in force, for this deployment")
				unverified(r)
				continue
			}
			if r.Seq != st.PolicySeq+1 || r.PrevDigest != st.PolicyDigest || r.Policy.DeploymentID != cur.DeploymentID {
				reject(rec.Name, "not an approval of the next transition from the policy in force, for this deployment")
				continue
			}
			key := fmt.Sprint(r.Seq)
			if approvals[key] == nil {
				approvals[key] = map[string]StopRecord{}
			}
			g := approvals[key]
			g[r.By] = r
			agree := 0
			for _, a := range g {
				if StopPolicyDigest(*a.Policy) == StopPolicyDigest(*r.Policy) {
					agree++
				}
			}
			if agree < cur.LiftQuorum {
				continue // the lifters approved different policies
			}
			next, err := c.policyOf(*r.Policy, StopPolicy{}, "")
			if err != nil {
				reject(rec.Name, "the new policy does not validate here: "+err.Error())
				delete(approvals, key)
				continue
			}
			if slices.Contains(chain, next.Digest()) {
				broken(fmt.Sprintf("the policy chain rolls back at transition %d: a quorum approved a policy that was already in force", r.Seq))
				delete(approvals, key)
				continue
			}
			prevPol, prevDigest = cur, st.PolicyDigest
			cur, st.PolicyDigest, st.PolicySeq = next, next.Digest(), r.Seq
			chain = append(chain, st.PolicyDigest)
			for i := range st.Stops {
				if !st.Stops[i].Lifted {
					countLifts(&st.Stops[i], cur)
				}
			}
		}
	}
	reached := !audited || slices.Contains(chain, c.policy.Digest())
	for i := range st.Stops {
		s := &st.Stops[i]
		if !s.Lifted {
			s.LiftsNeeded = 1
			if audited {
				s.LiftsNeeded = cur.LiftQuorum
			} else if s.Signed {
				st.UnverifiedActive++
			}
			s.LiftsHeld = len(enrolledLifters(s, cur, audited))
		}
	}
	return st, reached
}

// policyOf builds the policy a document names: the pin itself when the digest is its own (no
// verifier constructor needed), else through the control's verifier constructor.
func (c *StopControl) policyOf(d StopPolicyDoc, pin StopPolicy, pinDigest string) (StopPolicy, error) {
	if pinDigest != "" && StopPolicyDigest(d) == pinDigest {
		return pin, nil
	}
	return PolicyFromDoc(d, c.nv)
}

// enrolledLifters returns the lifters among those who approved s's lift that the policy in force
// still enrols.
func enrolledLifters(s *StopEntry, p StopPolicy, audited bool) []string {
	if !audited {
		return nil
	}
	var out []string
	for id := range s.liftSet {
		if slices.Contains(p.Lifters, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// countLifts marks s lifted when the lift rule of p is met by the lifters counted so far that p
// still enrols.
func countLifts(s *StopEntry, p StopPolicy) {
	by := enrolledLifters(s, p, true)
	if len(by) < p.LiftQuorum {
		return
	}
	other := false
	for _, id := range by {
		other = other || id != s.By
	}
	if p.LiftNeedsOther && !other {
		return
	}
	s.Lifted, s.LiftedBy = true, by
}

// protocol:stop end

// StopRequest is what SetStop takes. A signing control needs Signer and By must be an enrolled
// setter; a basic control ignores Signer. By and Host are recorded as given in a basic control.
type StopRequest struct {
	Scope        StopScope
	Reason       string
	Compensation string // CompensationAllow (the default) or CompensationBlock
	By, Host     string
	Signer       StopSigner
	// AdoptedFrom is the digest of the plain stop this one re-states (ChangeMode sets it).
	AdoptedFrom string
}

// LiftRequest is what LiftStop takes: one lifter's approval. An audited control needs Signer and By
// must be an enrolled lifter; the stop is lifted when the quorum is met.
type LiftRequest struct {
	Reason   string
	By, Host string
	Signer   StopSigner
}

// ModeRequest is what ChangeMode takes: one signer's evidence record of a change of mode.
type ModeRequest struct {
	To       StopMode
	Reason   string
	By, Host string
	Signer   StopSigner
}

// SetStop sets a stop and returns it as State reports it. The record counts from the moment it is
// committed; the call fails with ErrStopRejected if the fold does not accept it (a policy transition
// raced it, so its signer is no longer enrolled: set again).
func (c *StopControl) SetStop(ctx context.Context, req StopRequest) (StopEntry, error) {
	if err := req.Scope.Validate(); err != nil {
		return StopEntry{}, err
	}
	comp := req.Compensation
	if comp == "" {
		comp = CompensationAllow
	}
	st, err := c.State(ctx)
	if err != nil {
		return StopEntry{}, err
	}
	r := StopRecord{Format: StopFormat, Kind: "set", Mode: st.Mode, Scope: &req.Scope, Compensation: comp, Reason: req.Reason, By: req.By, Host: req.Host, AdoptedFrom: req.AdoptedFrom}
	if err := c.write(ctx, st, &r, req.Signer, true); err != nil {
		return StopEntry{}, err
	}
	return c.find(ctx, r.StopID, stopRecordStep(r))
}

// LiftStop adds one lifter's approval of lifting stop id and returns the stop as State then reports
// it (Lifted when the rule is met, else LiftsHeld of LiftsNeeded).
func (c *StopControl) LiftStop(ctx context.Context, id string, req LiftRequest) (StopEntry, error) {
	st, err := c.State(ctx)
	if err != nil {
		return StopEntry{}, err
	}
	var target *StopEntry
	for i := range st.Stops {
		if st.Stops[i].ID == id {
			target = &st.Stops[i]
		}
	}
	if target == nil {
		return StopEntry{}, fmt.Errorf("no accepted stop %q: %w", id, ErrStopRejected)
	}
	r := StopRecord{Format: StopFormat, Kind: "lift", Mode: st.Mode, StopID: id, SetDigest: target.digest, Reason: req.Reason, By: req.By, Host: req.Host}
	if err := c.write(ctx, st, &r, req.Signer, false); err != nil {
		return StopEntry{}, err
	}
	return c.find(ctx, id, stopRecordStep(r))
}

// ChangeMode records that the deployment changed mode: evidence for history and the audit trail,
// which changes nothing a process does (the mode is each process's configuration, rolled out
// by the operator). It needs a signing control (a policy and a signer).
//
//   - To audited, the stops a basic view holds that are active are adopted: the control sets each
//     again, signed (reason prefixed "adopted:"), because an audited control ignores unsigned stops;
//     a deployment moved to audited mode mid-stop stays stopped. The plain originals stay in the
//     log and are still in force for any process that is still basic.
//   - To basic, the control refuses while a stop is active (an operator must not lift a stop by
//     leaving audited mode), though no store-side rule can enforce it for another process's
//     configuration: the audit trail records it.
func (c *StopControl) ChangeMode(ctx context.Context, req ModeRequest) (StopState, error) {
	if c.policy == nil || req.Signer == nil {
		return StopState{}, fmt.Errorf("a record of a change of mode is signed: the control needs a stop policy and a signer: %w", ErrConfig)
	}
	if req.To != StopAudited && req.To != StopBasic {
		return StopState{}, fmt.Errorf("mode %q: %w", req.To, ErrConfig)
	}
	st, err := c.State(ctx)
	if err != nil {
		return st, err
	}
	if req.To == StopBasic && len(st.Active()) > 0 {
		return st, fmt.Errorf("%d stop(s) are active: lift them before leaving audited mode: %w", len(st.Active()), ErrConfig)
	}
	if req.To == StopAudited {
		basic := &StopControl{j: c.j, now: c.now, mode: StopBasic}
		bs, err := basic.State(ctx)
		if err != nil {
			return st, err
		}
		for _, p := range bs.Active() {
			covered := false
			for _, a := range st.Active() {
				covered = covered || a.Scope == p.Scope && a.Compensation == p.Compensation
			}
			if covered {
				continue
			}
			if _, err := c.SetStop(ctx, StopRequest{Scope: p.Scope, Reason: "adopted: " + p.Reason, Compensation: p.Compensation, By: req.By, Host: req.Host, Signer: req.Signer, AdoptedFrom: p.digest}); err != nil {
				return st, fmt.Errorf("adopt the stop %s: %w", p.ID, err)
			}
		}
	}
	n := 1
	recs, _ := c.j.History(ctx, StopControlRun)
	for _, rec := range recs {
		if strings.HasPrefix(rec.Name, "@stop/mode/") {
			n++
		}
	}
	r := StopRecord{Format: StopFormat, Kind: "mode", Mode: StopAudited, To: req.To, Seq: n, Reason: req.Reason, By: req.By, Host: req.Host}
	if err := c.write(ctx, st, &r, req.Signer, false); err != nil {
		return StopState{}, err
	}
	return c.State(ctx)
}

// RotatePolicy adds one lifter's approval of replacing the policy in force with next, and returns the
// state afterwards (PolicySeq has advanced once a quorum of the policy in force has approved the same
// document). Stops stay in force and new ones can be set throughout.
func (c *StopControl) RotatePolicy(ctx context.Context, next StopPolicy, req LiftRequest) (StopState, error) {
	if c.policy == nil || req.Signer == nil {
		return StopState{}, fmt.Errorf("a policy transition is signed: the control needs a stop policy and a signer: %w", ErrConfig)
	}
	doc, err := next.Doc()
	if err != nil {
		return StopState{}, err
	}
	if doc.DeploymentID != c.policy.DeploymentID {
		return StopState{}, fmt.Errorf("a policy transition keeps the deployment ID: %w", ErrConfig)
	}
	st, err := c.State(ctx)
	if err != nil {
		return st, err
	}
	r := StopRecord{Format: StopFormat, Kind: "policy", Mode: StopAudited, Seq: st.PolicySeq + 1, PrevDigest: st.PolicyDigest, Policy: &doc, Reason: req.Reason, By: req.By, Host: req.Host}
	if err := c.write(ctx, st, &r, req.Signer, false); err != nil {
		return StopState{}, err
	}
	after, err := c.State(ctx)
	if err != nil {
		return after, err
	}
	name := stopRecordStep(r)
	for _, rj := range after.Rejected {
		if rj.Name == name {
			return after, fmt.Errorf("the record %s was written but has no effect: %s: %w", name, rj.Reason, ErrStopRejected)
		}
	}
	return after, nil
}

// ensureRoot writes the deployment's root record (the first policy, signed) if no stored root's chain
// reaches this process's pin: every audited deployment is walkable from its first audited write on.
func (c *StopControl) ensureRoot(ctx context.Context, st StopState, by, host, reason string, signer StopSigner) error {
	recs, err := c.j.History(ctx, StopControlRun)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if strings.HasPrefix(rec.Name, "@stop/root/") && st.ChainErr == "" {
			return nil // a root whose chain reaches this process's pin is already there
		}
	}
	doc, err := c.policy.Doc()
	if err != nil {
		return err
	}
	root := StopRecord{Format: StopFormat, Kind: "root", Mode: StopAudited, Policy: &doc, Reason: reason, By: by, Host: host}
	return c.write(ctx, st, &root, signer, false)
}

// protocol:stop begin Write

// write completes r (time, policy digest, ID, signature) and inserts it. An audited control signs.
func (c *StopControl) write(ctx context.Context, st StopState, r *StopRecord, signer StopSigner, isSet bool) error {
	if c.policy != nil && r.Kind != "root" && signer != nil {
		if err := c.ensureRoot(ctx, st, r.By, r.Host, "root of the deployment's policy chain", signer); err != nil {
			return err
		}
	}
	r.At = c.now().UTC().Format(time.RFC3339Nano)
	if c.policy != nil {
		if signer == nil {
			return fmt.Errorf("an audited control signs every record: a signer is required: %w", ErrConfig)
		}
		r.Mode, r.Alg, r.PolicyDigest, r.Deployment = StopAudited, signer.Alg(), st.PolicyDigest, c.policy.DeploymentID
	} else if r.Kind != "set" && r.Kind != "lift" {
		return fmt.Errorf("a basic control writes only stops and lifts: %w", ErrConfig)
	} else {
		r.Mode = StopBasic
	}
	if isSet {
		r.StopID = StopIDOf(*r)
	}
	if r.Mode == StopAudited {
		sig, err := signer.Sign(StopSigningBytes(*r))
		if err != nil {
			return fmt.Errorf("sign the stop record: %w", err) // a signer's error never carries key material (see audit.Signer)
		}
		r.Signature = hex.EncodeToString(sig)
	}
	if err := r.validate(); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode the stop record: %w (%w)", err, ErrStorage)
	}
	_, _, err = c.j.putNew(ctx, StopControlRun, stopRecordStep(*r), Record{Kind: StepValue, Result: b})
	return err
}

// protocol:stop end

// find returns the stop id as State reports it, and fails with ErrStopRejected if the record name just
// written was not accepted (it is listed in Rejected) or the stop is not there.
func (c *StopControl) find(ctx context.Context, id, name string) (StopEntry, error) {
	st, err := c.State(ctx)
	if err != nil {
		return StopEntry{}, err
	}
	for _, rj := range st.Rejected {
		if rj.Name == name {
			return StopEntry{}, fmt.Errorf("the record %s was written but has no effect: %s: %w", name, rj.Reason, ErrStopRejected)
		}
	}
	for _, s := range st.Stops {
		if s.ID == id {
			return s, nil
		}
	}
	return StopEntry{}, fmt.Errorf("the record %s was written but the stop %s is not in force: %w", name, id, ErrStopRejected)
}

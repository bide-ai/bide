package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// This file is the record vocabulary of the kill switch: the stop, the lift and the mode records that
// make up the control run (see StopControl and docs/design/kill-switch.md). A record is canonical
// JSON. In audited mode it carries a signature over its canonical bytes, under a domain-separation
// label, by an enrolled key; in basic mode it is plain.

// StopFormat names the format of a stop record.
const StopFormat = "bide.stop/1"

// StopControlRun is the run ID of the control run: the journal run in which a store holds the stop,
// lift and mode records. The engine reserves it, so no user run collides with it, and a recovery pass
// does not drive it.
const StopControlRun = "@stop"

// MaxStopReason bounds the reason of a stop, a lift or a mode change, in bytes.
const MaxStopReason = 1024

// maxStopName bounds a scope's name and an operator's name, in bytes.
const maxStopName = 256

// StopScopeKind says what a stop covers.
type StopScopeKind string

const (
	// StopGlobal covers every run.
	StopGlobal StopScopeKind = "global"
	// StopTool covers the calls of the tool with exactly the scope's name.
	StopTool StopScopeKind = "tool"
	// StopFlow covers the runs and nodes of the plan flow with exactly the scope's name.
	StopFlow StopScopeKind = "flow"
	// StopAgent covers the runs of the agent whose Identity.Actor is exactly the scope's name. A run with
	// no identity has no Actor and is never covered by an agent stop: give every agent an identity
	// (WithIdentity) before relying on an agent stop, and use a tool or global stop otherwise.
	StopAgent StopScopeKind = "agent"
)

// StopScope is what a stop covers: a kind and, except for StopGlobal, an exact name. There are no
// patterns: a mistyped pattern that matches too much is an outage, and one that matches too little is
// a stop that does not stop.
type StopScope struct {
	Kind StopScopeKind `json:"kind"`
	Name string        `json:"name,omitempty"`
}

// String is the scope as a person reads it: "global", "tool refund".
func (s StopScope) String() string {
	if s.Kind == StopGlobal {
		return "global"
	}
	return string(s.Kind) + " " + s.Name
}

// Validate reports whether s names a scope: a known kind, no name for StopGlobal, and for every other
// kind a name of valid UTF-8, with no control or bidirectional character, at most 256 bytes.
func (s StopScope) Validate() error {
	switch s.Kind {
	case StopGlobal:
		if s.Name != "" {
			return fmt.Errorf("stop scope %q: a global stop has no name: %w", s.Kind, ErrConfig)
		}
		return nil
	case StopTool, StopFlow, StopAgent:
		if s.Name == "" {
			return fmt.Errorf("stop scope %q: a name is required: %w", s.Kind, ErrConfig)
		}
		return checkStopText("stop scope name", s.Name, maxStopName)
	}
	return fmt.Errorf("stop scope kind %q is not global, tool, flow or agent: %w", s.Kind, ErrConfig)
}

// StopMode is the mode a deployment's stops run under: StopBasic (the default) or StopAudited.
type StopMode string

const (
	// StopBasic needs no keys: anyone who can write the store sets and lifts a stop, and the records
	// are plain and unsigned. It trusts store access completely and offers no tamper evidence.
	StopBasic StopMode = "basic"
	// StopAudited signs every record with an enrolled key, lifts by a quorum and records to an
	// anchored trail (package audit).
	StopAudited StopMode = "audited"
)

// Compensation says whether a stop also holds the compensation (the rollback) of the calls it covers.
const (
	CompensationAllow = "allow" // the default: a stop pauses forward calls, a rollback continues
	CompensationBlock = "block" // a covered call's compensation pauses too
)

// StopRecord is one record of the control run. Kind says which fields are used: "set" a stop, "lift"
// one lifter's approval of lifting it, "mode" evidence that a deployment changed mode (it never
// changes what a process does: the mode is the process's configuration), "policy" one lifter's
// approval of replacing the stop policy (a transition applies once a lift quorum of the policy in
// force has approved it), "root" the first policy of the deployment, signed by one of its own
// approvers (the walk of the chain starts from the first root a process can verify).
type StopRecord struct {
	Format string `json:"format"`
	Kind   string `json:"kind"`
	// Mode is the mode the record was written under. A record written under StopBasic is ignored once
	// the deployment is audited.
	Mode StopMode `json:"mode"`
	// Deployment is the deployment ID of the policy the record was signed under (audited mode): it is in
	// the signed body, so a record cannot be replayed into another deployment.
	Deployment string `json:"deployment,omitempty"`
	// AdoptedFrom, on a set record, is the digest (StopDigest) of the plain set record a signed stop
	// re-states when a deployment moves to audited mode, so the history stays traceable.
	AdoptedFrom string `json:"adopted_from,omitempty"`
	// StopID is the stop a set record makes (see StopIDOf) and the stop a lift record lifts.
	StopID string `json:"stop_id,omitempty"`
	// SetDigest, on a lift, is the digest of the set record it lifts (StopDigest), so a lift cannot be
	// replayed against another stop.
	SetDigest    string     `json:"set_digest,omitempty"`
	Scope        *StopScope `json:"scope,omitempty"`
	Compensation string     `json:"compensation,omitempty"`
	// Gen, on a set record, is its generation: one more than the greatest generation of any set of the same
	// scope and compensation the writer read, from 1. The record is named by its scope key and Gen
	// (stopRecordStep), and an insert is once-only, so two writers that chose the same generation
	// collide and one fails, re-reads, and finds the stop in force. A record written before generations
	// existed has Gen 0 and is named by its stop ID; it is read as before.
	Gen int `json:"gen,omitempty"`
	// To, on a mode record, is the mode being changed to; Seq the change's number (from 1).
	To  StopMode `json:"to,omitempty"`
	Seq int      `json:"seq,omitempty"`
	// Policy, on a "policy" record, is the policy that replaces the one in force (PrevDigest names it);
	// Seq is the transition's number, from 1. Each approval of a transition carries the whole document.
	Policy     *StopPolicyDoc `json:"policy,omitempty"`
	PrevDigest string         `json:"prev_digest,omitempty"`
	// Applied, on an "apply" record, are the digests (StopDigest) of the pending trail records its signer applied
	// to the control run (StopControl.ApplyPending): who applied what, and when, is itself a signed record.
	Applied []string `json:"applied,omitempty"`
	// Reason is the operator's text, at most MaxStopReason bytes. It holds no secret: it is shown in
	// logs, notifications and the viewer.
	Reason string `json:"reason,omitempty"`
	// At is when the writer wrote the record, by its own clock, in RFC 3339 with nanoseconds. It is
	// informational: nothing's effect depends on it.
	At string `json:"at"`
	// By is the operator: in audited mode the enrolled approver id, whose signature authenticates it;
	// in basic mode a name as given, which is a claim. Host is the writer's host name, a claim too.
	By   string `json:"by"`
	Host string `json:"host,omitempty"`
	// PolicyDigest is the digest of the StopPolicy the writer signed under (audited mode): evidence. A
	// record is accepted by its signature under the policy in force at its position in the log.
	PolicyDigest string `json:"policy_digest,omitempty"`
	Alg          Alg    `json:"alg,omitempty"`
	// Signature is the hex signature over StopSigningBytes (audited mode).
	Signature string `json:"signature,omitempty"`
}

// Signed reports whether r carries a signature.
func (r StopRecord) Signed() bool { return r.Signature != "" }

// body is r's canonical bytes with its signature removed (and, for a set record's identity, its
// StopID too).
func (r StopRecord) body(withID bool) []byte {
	r.Signature = ""
	if !withID && r.Kind == "set" {
		r.StopID = ""
	}
	b, _ := json.Marshal(r) // a struct of strings and ints: cannot fail
	c, err := canonicalJSON(string(b))
	if err != nil {
		return b
	}
	return []byte(c)
}

// StopIDOf returns the ID of the stop a set record makes: the hex SHA-256 of its canonical bytes
// without StopID and Signature. Nobody chooses it, so two different stops cannot share one.
func StopIDOf(r StopRecord) string {
	sum := sha256.Sum256(r.body(false))
	return hex.EncodeToString(sum[:])
}

// StopDigest returns the hex SHA-256 of the canonical bytes of the whole record r, signature included:
// what a lift's SetDigest names.
func StopDigest(r StopRecord) string {
	r2 := r
	b, _ := json.Marshal(r2)
	c, err := canonicalJSON(string(b))
	if err != nil {
		c = string(b)
	}
	sum := sha256.Sum256([]byte(c))
	return hex.EncodeToString(sum[:])
}

// stopLabel is the domain-separation label of a stop signature.
const stopLabel = "bide-stop/1\n"

// StopSigningBytes returns the bytes a signature of r covers: the label, the record's kind and its
// canonical body (StopID included, Signature excluded).
func StopSigningBytes(r StopRecord) []byte {
	return append([]byte(stopLabel+r.Kind+"\n"), r.body(true)...)
}

// checkStopText refuses text that is not valid UTF-8, holds a control, format or bidirectional
// character, or is longer than max bytes.
func checkStopText(what, s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("%s is %d bytes, over %d: %w", what, len(s), max, ErrConfig)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8: %w", what, ErrConfig)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%s holds a control or invisible character: %w", what, ErrConfig)
		}
	}
	return nil
}

// Validate reports whether r is a well-formed record of its kind (not whether it is authorised): its
// format, mode, time, operator, reason, signature presence, and the fields its kind needs.
func (r StopRecord) Validate() error { return r.validate() }

// validate is Validate.
func (r StopRecord) validate() error {
	if r.Format != StopFormat {
		return fmt.Errorf("stop record format %q is not %q: %w", r.Format, StopFormat, ErrInvalidStop)
	}
	if r.Mode != StopBasic && r.Mode != StopAudited {
		return fmt.Errorf("stop record mode %q: %w", r.Mode, ErrInvalidStop)
	}
	if _, err := time.Parse(time.RFC3339Nano, r.At); err != nil {
		return fmt.Errorf("stop record time %q: %w", r.At, ErrInvalidStop)
	}
	if r.By == "" || checkStopText("stop record operator", r.By, maxStopName) != nil || checkStopText("stop record host", r.Host, maxStopName) != nil {
		return fmt.Errorf("stop record operator or host: %w", ErrInvalidStop)
	}
	if err := checkStopText("stop record reason", r.Reason, MaxStopReason); err != nil {
		return fmt.Errorf("%v: %w", err, ErrInvalidStop)
	}
	if (r.Mode == StopAudited) != r.Signed() {
		return fmt.Errorf("stop record: a record is signed exactly when it is audited: %w", ErrInvalidStop)
	}
	if r.Gen < 0 || r.Kind != "set" && r.Gen != 0 {
		return fmt.Errorf("stop record: a generation belongs to a set record and is not negative: %w", ErrInvalidStop)
	}
	switch r.Kind {
	case "set":
		if r.Scope == nil || r.Scope.Validate() != nil || r.StopID != StopIDOf(r) {
			return fmt.Errorf("stop record: a set record needs a valid scope and its own ID: %w", ErrInvalidStop)
		}
		if r.Compensation != CompensationAllow && r.Compensation != CompensationBlock {
			return fmt.Errorf("stop record: compensation %q: %w", r.Compensation, ErrInvalidStop)
		}
	case "lift":
		if r.StopID == "" || r.SetDigest == "" {
			return fmt.Errorf("stop record: a lift names its stop and the set record's digest: %w", ErrInvalidStop)
		}
	case "mode":
		if r.Seq < 1 || r.To != StopBasic && r.To != StopAudited {
			return fmt.Errorf("stop record: a mode change needs a number and a mode: %w", ErrInvalidStop)
		}
	case "apply":
		if len(r.Applied) == 0 || r.Mode != StopAudited {
			return fmt.Errorf("stop record: an apply record names the records it applied and is signed: %w", ErrInvalidStop)
		}
	case "root":
		if r.Policy == nil || r.Mode != StopAudited {
			return fmt.Errorf("stop record: a root record carries the first policy and is signed: %w", ErrInvalidStop)
		}
	case "policy":
		if r.Seq < 1 || r.PrevDigest == "" || r.Policy == nil || r.Mode != StopAudited {
			return fmt.Errorf("stop record: a policy transition needs a number, the digest it replaces, the new policy and a signature: %w", ErrInvalidStop)
		}
	default:
		return fmt.Errorf("stop record kind %q: %w", r.Kind, ErrInvalidStop)
	}
	return nil
}

// stopRecordStep is the journal name of r's entry. It is derived from the record's whole content (a set
// record's ID covers it; a lift's or a mode record's name ends in a digest of the record), so two
// operators never contend for a position, a retry of the very same record is a no-op, and a writer
// cannot squat a name with a forged record to keep the real one out (a name taken by a record that
// does not verify costs nothing: the real record has another name).
func stopRecordStep(r StopRecord) string {
	d := StopDigest(r)[:16]
	switch r.Kind {
	case "set":
		if r.Gen > 0 && r.Scope != nil {
			return setNamePrefix(*r.Scope, r.Compensation) + strconv.Itoa(r.Gen)
		}
		return "@stop/set/" + r.StopID
	case "lift":
		return "@stop/lift/" + r.StopID + "/" + encodeID(r.By) + "/" + d
	}
	if r.Kind == "apply" {
		return "@stop/apply/" + encodeID(r.By) + "/" + d
	}
	if r.Kind == "root" {
		return "@stop/root/" + StopPolicyDigest(*r.Policy) + "/" + encodeID(r.By) + "/" + d
	}
	if r.Kind == "policy" {
		return fmt.Sprintf("@stop/policy/%010d/%s/%s", r.Seq, encodeID(r.By), d)
	}
	return fmt.Sprintf("@stop/mode/%010d/%s/%s", r.Seq, encodeID(r.By), d)
}

// setNamePrefix is the start of the journal names of the sets of one scope and compensation: the digest of
// the two, and the generation follows. A set that has no generation is named by its stop ID, which holds
// no "/", so the two forms never collide.
func setNamePrefix(scope StopScope, compensation string) string {
	sum := sha256.Sum256([]byte(string(scope.Kind) + "\x00" + scope.Name + "\x00" + compensation))
	return "@stop/set/" + hex.EncodeToString(sum[:]) + "/"
}

// decodeStopRecord decodes a control-run entry's result strictly.
func decodeStopRecord(b []byte) (StopRecord, error) {
	var r StopRecord
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil || dec.More() {
		return StopRecord{}, fmt.Errorf("stop record does not decode: %w", ErrInvalidStop)
	}
	return r, r.validate()
}

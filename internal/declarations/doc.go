// Package declarations is the report on an agent's taint and cap declarations
// (docs/design/untrusted-content-and-amount-caps.md, sections 5.4 and 9). A host dumps the declarations of
// its agent as a document; the package reads one back strictly, recomputes the digest a run's run:start
// records (taint_policy_digest) from it, lists findings about one document, and classifies the difference
// between two as weakened, strengthened or changed. It reads no clock, store or configuration.
package declarations

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/bide-ai/bide/internal/strictjson"
)

// Format tags a document. A document of another format is refused, never read as a version it is not.
const Format = "bide.declarations.v1"

// digestFormat tags the declarations the digest commits to (agent.taintDeclarationsFormat).
const digestFormat = "bide.taint-declarations.v1"

// Document is the declarations of one agent.
type Document struct {
	Format string `json:"format"`
	// Digest is the taint_policy_digest a run started by this agent records, "" for an agent with no taint
	// policy (no run it starts is under taint).
	Digest string `json:"digest"`
	// Policy is the agent's taint policy, nil for none.
	Policy *Policy `json:"policy,omitempty"`
	// Tools is every tool the agent registers, by name.
	Tools []Tool `json:"tools"`
}

// Policy is the taint policy: the escalation gate a tainted turn's side effects need, or Refuse.
type Policy struct {
	Gate   *Gate `json:"gate,omitempty"`
	Refuse bool  `json:"refuse,omitempty"`
}

// Gate is an approval policy as the digest sees it. The approvers' keys are not part of it.
type Gate struct {
	Single           bool     `json:"single,omitempty"`
	Need             int      `json:"need,omitempty"`
	Approvers        []string `json:"approvers,omitempty"`
	ExcludePrincipal bool     `json:"exclude_principal,omitempty"`
	TimeoutMS        int64    `json:"timeout_ms,omitempty"`
}

// Tool is one registered tool. Untrusted, Exempt and Caps are what the digest covers; Effect (the tool can
// have a side effect) and OwnGate (its own approval gate) are for the findings and the diff only.
type Tool struct {
	Name      string   `json:"name"`
	Effect    bool     `json:"effect,omitempty"`
	OwnGate   *Gate    `json:"own_gate,omitempty"`
	Untrusted string   `json:"untrusted,omitempty"`
	Exempt    string   `json:"exempt,omitempty"`
	Caps      []string `json:"caps,omitempty"`
}

// Decode reads a document strictly: one JSON value, no unknown member (a document written by a newer
// version is refused, not read with the parts this version knows), the right format, tools sorted by name
// without a repeat, and a digest that is the digest of what the document declares.
func Decode(r io.Reader) (Document, error) {
	const limit = 16 << 20
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return Document{}, fmt.Errorf("read the declarations: %w", err)
	}
	if len(raw) > limit {
		return Document{}, fmt.Errorf("the declarations are over %d bytes", limit)
	}
	// strictjson refuses what encoding/json reads as something other than it shows: a duplicate member
	// (the last would win), a member spelled in another case ("FORMAT" for "format"), an unknown member,
	// data after the value, and invalid UTF-8.
	var d Document
	if err := strictjson.Unmarshal(raw, &d, nil); err != nil {
		return Document{}, fmt.Errorf("the declarations do not read: %w", err)
	}
	if d.Format != Format {
		return Document{}, fmt.Errorf("the declarations are format %q, this version reads %q", d.Format, Format)
	}
	if !slices.IsSortedFunc(d.Tools, func(a, b Tool) int { return compareStrings(a.Name, b.Name) }) {
		return Document{}, fmt.Errorf("the declarations list tools out of order")
	}
	for i := 1; i < len(d.Tools); i++ {
		if d.Tools[i].Name == d.Tools[i-1].Name {
			return Document{}, fmt.Errorf("the declarations list tool %q twice", d.Tools[i].Name)
		}
	}
	if want := d.Recompute(); d.Digest != want {
		return Document{}, fmt.Errorf("the declarations' digest %q is not the digest of what they declare (%q)", d.Digest, want)
	}
	return d, nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// wire is the gate's JSON as agent.ApprovalPolicy encodes it, which is what the digest hashes.
func (g *Gate) wire() json.RawMessage {
	if g == nil {
		return nil
	}
	if g.Single {
		if g.TimeoutMS != 0 {
			return mustJSON(struct {
				Single    bool  `json:"single"`
				TimeoutMS int64 `json:"timeout_ms,omitempty"`
			}{true, g.TimeoutMS})
		}
		return json.RawMessage(`{"single":true}`)
	}
	return mustJSON(struct {
		Need             int      `json:"need"`
		Approvers        []string `json:"approvers,omitempty"`
		ExcludePrincipal bool     `json:"exclude_principal,omitempty"`
		TimeoutMS        int64    `json:"timeout_ms,omitempty"`
	}{g.Need, g.Approvers, g.ExcludePrincipal, g.TimeoutMS})
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil { // plain values always marshal
		panic(fmt.Sprintf("declarations: encode: %v", err))
	}
	return b
}

// Recompute is the digest of what the document declares: the same bytes agent.Agent hashes for
// taint_policy_digest, "" when the document has no policy.
func (d Document) Recompute() string {
	if d.Policy == nil {
		return ""
	}
	type toolDecl struct {
		Untrusted string   `json:"untrusted,omitempty"`
		Exempt    string   `json:"exempt,omitempty"`
		Caps      []string `json:"caps,omitempty"`
	}
	covered := struct {
		Format string              `json:"format"`
		Gate   json.RawMessage     `json:"gate,omitempty"`
		Refuse bool                `json:"refuse,omitempty"`
		Tools  map[string]toolDecl `json:"tools,omitempty"`
	}{Format: digestFormat, Gate: d.Policy.Gate.wire(), Refuse: d.Policy.Refuse}
	for _, t := range d.Tools {
		if t.Untrusted != "" || t.Exempt != "" || len(t.Caps) > 0 {
			if covered.Tools == nil {
				covered.Tools = map[string]toolDecl{}
			}
			covered.Tools[t.Name] = toolDecl{Untrusted: t.Untrusted, Exempt: t.Exempt, Caps: t.Caps}
		}
	}
	sum := sha256.Sum256(mustJSON(covered))
	return hex.EncodeToString(sum[:])
}

// Encode writes d as indented JSON with the characters of a cap line (">", "->") as written, and a final
// newline: the form a host commits and a reviewer reads.
func Encode(w io.Writer, d Document) error {
	if d.Tools == nil {
		d.Tools = []Tool{}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

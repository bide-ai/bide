package doctor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/bide-ai/bide/internal/declarations"
	"github.com/bide-ai/bide/internal/strictjson"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/plan"
)

// declFile is a declarations file read by kind: an agent's document (format bide.declarations.v1), or a flow's,
// the plan.RegistryDescription of its blocks (version 2 carries the taint declarations).
type declFile struct {
	agent *declarations.Document
	flow  *declarations.FlowDocument
}

func (f declFile) kind() string {
	if f.flow != nil {
		return "flow"
	}
	return "agent"
}

// maxDeclFile is the most a declarations file may hold, as declarations.Decode bounds a document.
const maxDeclFile = 16 << 20

// readDeclarations reads the declarations file strictly. A file with a "format" member is an agent's document;
// one with "version" and "blocks" members is a flow's description; anything else is read as an agent's
// document, which refuses it.
func readDeclarations(path string) (declFile, error) {
	fail := func(err error) (declFile, error) { return declFile{}, fmt.Errorf("%s: %w", path, err) }
	f, err := os.Open(path)
	if err != nil {
		return declFile{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxDeclFile+1))
	if err != nil {
		return fail(err)
	}
	if len(raw) > maxDeclFile {
		return fail(fmt.Errorf("the declarations are over %d bytes", maxDeclFile))
	}
	var members map[string]json.RawMessage
	_ = json.Unmarshal(raw, &members) // a file that is not an object is refused by the reader below
	_, hasFormat := members["format"]
	_, hasVersion := members["version"]
	_, hasBlocks := members["blocks"]
	if !hasFormat && hasVersion && hasBlocks {
		d, err := readFlowDescription(raw)
		if err != nil {
			return fail(err)
		}
		return declFile{flow: &d}, nil
	}
	d, err := declarations.Decode(bytes.NewReader(raw))
	if err != nil {
		return fail(err)
	}
	return declFile{agent: &d}, nil
}

// readFlowDescription reads a plan.RegistryDescription strictly and converts it.
func readFlowDescription(raw []byte) (declarations.FlowDocument, error) {
	var d plan.RegistryDescription
	if err := strictjson.Unmarshal(raw, &d, nil); err != nil {
		return declarations.FlowDocument{}, fmt.Errorf("the description does not read: %w", err)
	}
	if err := d.Validate(); err != nil {
		return declarations.FlowDocument{}, err
	}
	return flowDocument(d), nil
}

// flowDocument is the declarations of a valid description.
func flowDocument(d plan.RegistryDescription) declarations.FlowDocument {
	doc := declarations.FlowDocument{TaintKnown: d.TaintDeclared}
	if p := d.TaintPolicy; p != nil {
		doc.Policy = &declarations.Policy{Gate: gateDoc(p.Gate), Refuse: p.Refuse, RequireProvenance: p.RequireProvenance}
	}
	for _, b := range d.Blocks {
		doc.Blocks = append(doc.Blocks, declarations.FlowBlock{
			Name: b.Name, Effect: b.Safety != "readonly", Idempotent: b.Safety == "idempotent", OwnGate: gateDoc(b.Gate),
			Untrusted: b.Untrusted, Trusted: b.Trusted, Exempt: b.TaintExempt,
		})
	}
	for _, m := range d.Merges {
		doc.Blocks = append(doc.Blocks, declarations.FlowBlock{
			Name: m.Name, Merge: true, Effect: m.Safety != "readonly", Idempotent: m.Safety == "idempotent", OwnGate: gateDoc(m.Gate),
			Untrusted: m.Untrusted, Trusted: m.Trusted, Exempt: m.TaintExempt,
		})
	}
	return doc
}

// gateDoc is a described gate as the report holds it. Validate has checked the timeout parses.
func gateDoc(g *plan.GateDescription) *declarations.Gate {
	if g == nil {
		return nil
	}
	out := &declarations.Gate{Single: g.Single, Need: g.Need, Approvers: slices.Clone(g.Approvers), ExcludePrincipal: g.ExcludePrincipal}
	if g.Timeout != "" {
		if t, err := time.ParseDuration(g.Timeout); err == nil {
			out.TimeoutMS = t.Milliseconds()
		}
	}
	return out
}

// policyLine is how a policy is said in the header.
func policyLine(p *declarations.Policy, none string) string {
	switch {
	case p == nil:
		return none
	case p.Refuse:
		return "taint policy: refuse a tainted turn's side effects"
	}
	return "taint policy: an escalation gate"
}

// reportDeclarations prints the findings about the declarations in the file and returns the exit status:
// 0 for a document that reads (a finding is a note, not a failure), 1 for one that does not.
func reportDeclarations(stdout, stderr io.Writer, st term.Style, path string) int {
	f, err := readDeclarations(path)
	if err != nil {
		fmt.Fprintf(stderr, "%sbide doctor: %v\n", st.ErrorPrefix(), err)
		return 1
	}
	var findings []declarations.Finding
	switch {
	case f.flow != nil:
		d := f.flow
		header := "taint policy not known (the description carries no taint declarations)"
		if d.TaintKnown {
			header = policyLine(d.Policy, "taint policy not given to the description")
		}
		fmt.Fprintf(stdout, "flow  %s  %d block(s)\n", header, len(d.Blocks))
		findings = d.Findings()
	default:
		d := f.agent
		fmt.Fprintf(stdout, "%s  %d tool(s)  digest %s\n", policyLine(d.Policy, "no taint policy"), len(d.Tools), orNone(d.Digest))
		findings = d.Findings()
	}
	for _, fd := range findings {
		fmt.Fprintf(stdout, "%s  %-18s %s\n", st.Warn("note"), fd.Subject, fd.Message)
	}
	if len(findings) == 0 {
		fmt.Fprintf(stdout, "%s  nothing to look at\n", st.OK("ok  "))
	}
	return 0
}

// reportDiff prints the changes from the old document to the new and returns the exit status: 1 when
// anything is weakened, or when either document does not read or the two do not agree with their digests;
// 0 otherwise.
func reportDiff(stdout, stderr io.Writer, st term.Style, oldPath, newPath string) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "%sbide doctor: %v\n", st.ErrorPrefix(), err)
		return 1
	}
	oldDoc, err := readDeclarations(oldPath)
	if err != nil {
		return fail(err)
	}
	newDoc, err := readDeclarations(newPath)
	if err != nil {
		return fail(err)
	}
	var changes []declarations.Change
	switch {
	case oldDoc.kind() != newDoc.kind():
		return fail(fmt.Errorf("%s is %s declarations and %s is %s declarations: they cannot be compared", oldPath, oldDoc.kind(), newPath, newDoc.kind()))
	case oldDoc.flow != nil:
		changes = declarations.FlowDiff(*oldDoc.flow, *newDoc.flow)
		fmt.Fprintln(stdout, "flow declarations")
	default:
		changes, err = declarations.Diff(*oldDoc.agent, *newDoc.agent)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "digest %s -> %s\n", orNone(oldDoc.agent.Digest), orNone(newDoc.agent.Digest))
	}
	if len(changes) == 0 {
		fmt.Fprintf(stdout, "%s  no difference\n", st.OK("ok  "))
		return 0
	}
	weakened := 0
	for _, class := range []declarations.Class{declarations.Weakened, declarations.Changed, declarations.Strengthened} {
		for _, c := range changes {
			if c.Class != class {
				continue
			}
			label := st.Dim("changed     ")
			switch class {
			case declarations.Weakened:
				weakened++
				label = st.Fail("WEAKENED    ")
			case declarations.Strengthened:
				label = st.OK("strengthened")
			}
			note := ""
			if !c.Covered && oldDoc.flow == nil { // a flow description has no digest
				note = st.Dim("  (not in the digest)")
			}
			fmt.Fprintf(stdout, "%s  %s: %s%s\n", label, c.Subject, c.What, note)
		}
	}
	if weakened > 0 {
		fmt.Fprintf(stdout, "\n%d change(s) weaken the declarations\n", weakened)
		return 1
	}
	return 0
}

func orNone(digest string) string {
	if digest == "" {
		return "(none)"
	}
	return digest[:min(12, len(digest))]
}

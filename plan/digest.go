package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strconv"
	"strings"
)

// flowDigestStep is the reserved journal name under which Run records the frozen
// flow's topology digest as the FIRST durable step of a run (see Flow.Run). It is
// an internal record of the run, not a declared node; conform recognises it and
// verifies the journaled digest equals the current flow's Digest() rather than
// treating it as a divergence.
const flowDigestStep = "flow:digest"

// Digest returns a deterministic SHA-256 hash (hex-encoded) of this frozen flow's
// DECLARED topology: the flow name, every node's name+kind+input type+output type,
// every edge, and every Switch and its ordered arms. It is a stable fingerprint of
// the diagram, computed by canonicalizing the spec in insertion order (never by map
// iteration), so two builds of the same topology produce the same digest and any
// change to the topology (a renamed or retyped node, an added or reordered edge, a
// changed arm) produces a different digest.
//
// Run journals this digest under the reserved step name flow:digest, so the audit
// layer's Merkle tree and signed tree head cover it. That makes it offline-verifiable
// that a run followed THIS declared topology: an auditor proves the flow:digest
// record is included under a signed tree head and checks the proven digest equals
// the declared flow's Digest(). See docs/guides/flows.md, "Cryptographic conformance".
//
// The digest commits to topology, not to node bodies: it does not hash the arbitrary
// Go inside a Step, only its declared shape (name and I/O types). This mirrors
// Conform, which is declaration-vs-journal, not value verification.
func (f *Flow[In, Out]) Digest() string { return f.core.digest() }

// digest computes the topology digest over the frozen builderCore. It serializes the
// spec into a canonical, insertion-ordered, unambiguously delimited byte string and
// hashes it with SHA-256. Determinism comes from iterating the insertion-ordered
// nodes, edges, and branches slices (never a map) and from length-independent field
// separators, so no two distinct topologies can canonicalize to the same bytes.
func (c *builderCore) digest() string {
	var b strings.Builder

	// Domain tag and version, so this hash use cannot collide with another and the
	// scheme can evolve without silently matching an old digest.
	b.WriteString("go-agents.plan.topology.v1\n")

	// The flow name and its pinned boundary types anchor the digest to this flow.
	writeField(&b, "flow", c.flowName)
	writeField(&b, "in", typeName(c.inType))
	writeField(&b, "out", typeName(c.outType))
	writeField(&b, "entry", c.entry)

	// Nodes in insertion order: name, kind, input type, output type. Each field is
	// length-prefixed so no combination of names and types can be confused for another.
	b.WriteString("nodes\n")
	for _, n := range c.nodes {
		writeField(&b, "n.name", n.name)
		writeField(&b, "n.kind", strconv.Itoa(int(n.kind)))
		writeField(&b, "n.in", typeName(n.inType))
		writeField(&b, "n.out", typeName(n.outType))
		// A Join fans in several producers; its ordered inputs and their port types are
		// part of the topology (a differently-ordered or differently-typed merge is a
		// distinct shape), so commit to them in declared order. Non-join nodes have no
		// joinInputs, so this adds nothing to their canonical bytes.
		for i, src := range n.joinInputs {
			writeField(&b, "n.join.in", src)
			var portType reflect.Type
			if i < len(n.joinInTypes) {
				portType = n.joinInTypes[i]
			}
			writeField(&b, "n.join.intype", typeName(portType))
		}
	}

	// Edges in insertion order: producer -> consumer, by name.
	b.WriteString("edges\n")
	for _, e := range c.edges {
		writeField(&b, "e.from", e.from)
		writeField(&b, "e.to", e.to)
	}

	// Branches in insertion order: the switched-over node and its ordered arms. Each
	// arm records whether it is the Else fallback and its target, in declared order,
	// so reordering arms changes the digest.
	b.WriteString("branches\n")
	for _, br := range c.branches {
		writeField(&b, "b.over", br.over)
		for _, a := range br.arms {
			writeField(&b, "a.else", strconv.FormatBool(a.isElse))
			writeField(&b, "a.target", a.target)
		}
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// writeField appends one canonical, unambiguously delimited field to b: the field
// key, then the byte length of the value, then the value itself, each newline
// separated. Length-prefixing the value means no value (a name containing a newline,
// say) can be mistaken for a following field, so distinct topologies never share an
// encoding.
func writeField(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteByte('\n')
	b.WriteString(strconv.Itoa(len(value)))
	b.WriteByte('\n')
	b.WriteString(value)
	b.WriteByte('\n')
}

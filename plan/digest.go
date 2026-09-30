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
// DECLARED topology: the flow name, every node's name, block, kind, input type and output
// type (each type with its full package path), every edge, and every Switch and its ordered
// arms with their predicate names. This is the v2 digest (domain tag
// "bide.plan.topology.v2"); DigestV1 is the earlier one. It is a stable fingerprint of
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
// The digest commits to topology and to the NAMES of the behaviour it wires, not to node
// bodies: it hashes which registered block a node runs and which registered predicate an
// arm tests (for a flow built in Go, the names given with BlockName and Arm.Named; a node's
// block defaults to its own name and an unnamed predicate commits to none), but not the Go
// inside a block or predicate. This mirrors Conform, which is declaration-vs-journal, not
// value verification.
//
// Run refuses to resume a run whose journaled digest is not this one, including a run
// journaled under v1: v1 does not commit to block or predicate names, so it cannot show the
// run started under this flow rather than one that runs another block or predicate.
func (f *Flow[In, Out]) Digest() string { return f.core.digest() }

// DigestV1 returns the v1 topology digest (domain tag "bide.plan.topology.v1"), which
// Run recorded before v2. It does not commit to block or predicate names and writes types
// without their package paths. Use it only to check a proof of a flow:digest record
// journaled by an earlier version; Run and Conform accept only Digest.
func (f *Flow[In, Out]) DigestV1() string { return f.core.digestV1() }

// digest computes the v2 topology digest over the frozen builderCore. It serializes the
// spec into a canonical, insertion-ordered, unambiguously delimited byte string and hashes
// it with SHA-256. Determinism comes from iterating the insertion-ordered nodes, edges, and
// branches slices (never a map) and from length-prefixed fields, so no two distinct
// topologies can canonicalize to the same bytes.
//
// v2 adds to v1 each node's block name, each arm's predicate name, and types written with
// their full package path (see canonicalType), so a flow that runs another block, tests
// another predicate, or takes a type of the same name from another package is a different
// flow.
func (c *builderCore) digest() string {
	var b strings.Builder
	b.WriteString(topologyDigestV2 + "\n")

	writeField(&b, "flow", c.flowName)
	writeField(&b, "in", canonicalType(c.inType))
	writeField(&b, "out", canonicalType(c.outType))
	writeField(&b, "entry", c.entry)

	b.WriteString("nodes\n")
	for _, n := range c.nodes {
		writeField(&b, "n.name", n.name)
		writeField(&b, "n.block", n.block)
		writeField(&b, "n.kind", strconv.Itoa(int(n.kind)))
		writeField(&b, "n.in", canonicalType(n.inType))
		writeField(&b, "n.out", canonicalType(n.outType))
		for i, src := range n.joinInputs {
			writeField(&b, "n.join.in", src)
			var portType reflect.Type
			if i < len(n.joinInTypes) {
				portType = n.joinInTypes[i]
			}
			writeField(&b, "n.join.intype", canonicalType(portType))
		}
	}

	b.WriteString("edges\n")
	for _, e := range c.edges {
		writeField(&b, "e.from", e.from)
		writeField(&b, "e.to", e.to)
	}

	b.WriteString("branches\n")
	for _, br := range c.branches {
		writeField(&b, "b.over", br.over)
		for _, a := range br.arms {
			writeField(&b, "a.else", strconv.FormatBool(a.isElse))
			writeField(&b, "a.pred", a.predName)
			writeField(&b, "a.target", a.target)
			writeField(&b, "a.loopback", strconv.FormatBool(a.loopBack))
			writeField(&b, "a.loopmax", strconv.Itoa(a.loopMax))
		}
	}

	b.WriteString("loops\n")
	for _, lp := range c.loops {
		writeField(&b, "l.head", lp.head)
		writeField(&b, "l.over", lp.over)
		writeField(&b, "l.max", strconv.Itoa(lp.max))
		for _, name := range lp.body {
			writeField(&b, "l.body", name)
		}
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// Domain tags of the two digest versions. The tag is the first line hashed, so a v1 and a
// v2 digest of any two flows never collide.
const (
	topologyDigestV1 = "bide.plan.topology.v1"
	topologyDigestV2 = "bide.plan.topology.v2"
)

// canonicalType writes t with every named type qualified by its full package path, so two
// types that print alike (a/model.Req and b/model.Req are both "model.Req" to
// reflect.Type.String) are written differently. A named type is its package path, '.', and
// its name (a generic instantiation's name already spells its type arguments with their
// paths); a predeclared type is its name; an unnamed type is spelled out from its parts.
func canonicalType(t reflect.Type) string {
	if t == nil {
		return "?"
	}
	if t.Name() != "" {
		if t.PkgPath() == "" {
			return t.Name()
		}
		return t.PkgPath() + "." + t.Name()
	}
	switch t.Kind() {
	case reflect.Pointer:
		return "*" + canonicalType(t.Elem())
	case reflect.Slice:
		return "[]" + canonicalType(t.Elem())
	case reflect.Array:
		return "[" + strconv.Itoa(t.Len()) + "]" + canonicalType(t.Elem())
	case reflect.Map:
		return "map[" + canonicalType(t.Key()) + "]" + canonicalType(t.Elem())
	case reflect.Chan:
		switch t.ChanDir() {
		case reflect.RecvDir:
			return "<-chan " + canonicalType(t.Elem())
		case reflect.SendDir:
			return "chan<- " + canonicalType(t.Elem())
		}
		return "chan " + canonicalType(t.Elem())
	case reflect.Struct:
		var b strings.Builder
		b.WriteString("struct{")
		for i := range t.NumField() {
			f := t.Field(i)
			if i > 0 {
				b.WriteString("; ")
			}
			if f.Anonymous {
				b.WriteString("embedded ")
			}
			b.WriteString(qualifiedName(f.PkgPath, f.Name))
			b.WriteByte(' ')
			b.WriteString(canonicalType(f.Type))
			if f.Tag != "" {
				b.WriteByte(' ')
				b.WriteString(strconv.Quote(string(f.Tag)))
			}
		}
		b.WriteString("}")
		return b.String()
	case reflect.Func:
		var b strings.Builder
		b.WriteString("func(")
		for i := range t.NumIn() {
			if i > 0 {
				b.WriteString(", ")
			}
			if t.IsVariadic() && i == t.NumIn()-1 {
				b.WriteString("..." + canonicalType(t.In(i).Elem()))
				continue
			}
			b.WriteString(canonicalType(t.In(i)))
		}
		b.WriteString(") (")
		for i := range t.NumOut() {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(canonicalType(t.Out(i)))
		}
		b.WriteString(")")
		return b.String()
	case reflect.Interface:
		var b strings.Builder
		b.WriteString("interface{")
		for i := range t.NumMethod() {
			m := t.Method(i)
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(qualifiedName(m.PkgPath, m.Name))
			b.WriteByte(' ')
			b.WriteString(canonicalType(m.Type))
		}
		b.WriteString("}")
		return b.String()
	}
	return t.String()
}

// qualifiedName is a field or method name, qualified by its package path when it is
// unexported (two packages' unexported names are distinct identifiers).
func qualifiedName(pkgPath, name string) string {
	if pkgPath == "" {
		return name
	}
	return pkgPath + "." + name
}

// digestV1 computes the v1 topology digest over the frozen builderCore. It is kept
// unchanged so a journal written before v2 can still be checked (see DigestV1). It serializes the
// spec into a canonical, insertion-ordered, unambiguously delimited byte string and
// hashes it with SHA-256. Determinism comes from iterating the insertion-ordered
// nodes, edges, and branches slices (never a map) and from length-independent field
// separators, so no two distinct topologies can canonicalize to the same bytes.
func (c *builderCore) digestV1() string {
	var b strings.Builder

	// Domain tag and version, so this hash use cannot collide with another and the
	// scheme can evolve without silently matching an old digest.
	b.WriteString(topologyDigestV1 + "\n")

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
	// arm records whether it is the Else fallback, its target, and whether it is a
	// bounded back-edge with its iteration bound, in declared order, so reordering arms
	// or turning an arm into a loop (or changing its bound) changes the digest.
	b.WriteString("branches\n")
	for _, br := range c.branches {
		writeField(&b, "b.over", br.over)
		for _, a := range br.arms {
			writeField(&b, "a.else", strconv.FormatBool(a.isElse))
			writeField(&b, "a.target", a.target)
			// Commit to the loop back-edge and its bound so a looped flow's digest is
			// distinct from the acyclic one and shifts if the bound changes. This is the
			// STRUCTURE only; the runtime iteration count is never hashed (it is runtime,
			// like Safety). A non-loop arm writes loopback=false and max 0, so an acyclic
			// flow's digest is unchanged by this addition only if it has no loop arms.
			writeField(&b, "a.loopback", strconv.FormatBool(a.loopBack))
			writeField(&b, "a.loopmax", strconv.Itoa(a.loopMax))
		}
	}

	// Loops in insertion order: the derived loop head, switch, ordered body region, and
	// bound. This binds the digest to the resolved loop structure (not just the arm
	// flags), so two flows with the same nodes/edges/arms but a different loop region are
	// distinct. Empty for an acyclic flow, so its canonical bytes are unchanged.
	b.WriteString("loops\n")
	for _, lp := range c.loops {
		writeField(&b, "l.head", lp.head)
		writeField(&b, "l.over", lp.over)
		writeField(&b, "l.max", strconv.Itoa(lp.max))
		for _, name := range lp.body {
			writeField(&b, "l.body", name)
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

package rfc3161

import (
	"encoding/asn1"
	"math/big"
)

// A node is one DER element: the standard library's RawValue, which aliases the input (parsing
// copies and allocates nothing from a length field; a length beyond the input is an error).
type node = asn1.RawValue

func isUniversal(n node, tag int) bool { return n.Class == asn1.ClassUniversal && n.Tag == tag }
func isSeq(n node) bool                { return isUniversal(n, asn1.TagSequence) && n.IsCompound }
func isSet(n node) bool                { return isUniversal(n, asn1.TagSet) && n.IsCompound }
func isOID(n node) bool                { return isUniversal(n, asn1.TagOID) && !n.IsCompound }
func isOctets(n node) bool             { return isUniversal(n, asn1.TagOctetString) && !n.IsCompound }
func isInt(n node) bool                { return isUniversal(n, asn1.TagInteger) && !n.IsCompound }
func isCtx(n node, tag int, compound bool) bool {
	return n.Class == asn1.ClassContextSpecific && n.Tag == tag && n.IsCompound == compound
}

// parseNode reads the first element of b and returns the rest. Any encoding error of the standard
// library's DER reader (indefinite length, non-minimal length or tag, truncation) is not-der.
func parseNode(b []byte) (node, []byte, *Refusal) {
	var n node
	rest, err := asn1.Unmarshal(b, &n)
	if err != nil {
		return node{}, nil, refuse(ReasonNotDER, "%v", err)
	}
	return n, rest, nil
}

// kids returns the elements inside a constructed node. Every byte of the body must belong to one.
func kids(n node) ([]node, *Refusal) {
	var out []node
	b := n.Bytes
	for len(b) > 0 {
		c, rest, r := parseNode(b)
		if r != nil {
			return nil, r
		}
		out = append(out, c)
		b = rest
	}
	return out, nil
}

// only returns the single element inside a constructed node (an EXPLICIT tag, for example).
func only(n node, what string) (node, *Refusal) {
	c, rest, r := parseNode(n.Bytes)
	if r != nil {
		return node{}, r
	}
	if len(rest) != 0 {
		return node{}, refuse(ReasonMalformed, "%s holds more than one element", what)
	}
	return c, nil
}

func oidOf(n node, what string) (asn1.ObjectIdentifier, *Refusal) {
	if !isOID(n) {
		return nil, refuse(ReasonMalformed, "%s is not an OBJECT IDENTIFIER", what)
	}
	var o asn1.ObjectIdentifier
	if _, err := asn1.Unmarshal(n.FullBytes, &o); err != nil {
		return nil, refuse(ReasonMalformed, "%s: %v", what, err)
	}
	return o, nil
}

func bigOf(n node, what string) (*big.Int, *Refusal) {
	if !isInt(n) {
		return nil, refuse(ReasonMalformed, "%s is not an INTEGER", what)
	}
	v := new(big.Int)
	if _, err := asn1.Unmarshal(n.FullBytes, &v); err != nil {
		return nil, refuse(ReasonNotDER, "%s: %v", what, err)
	}
	return v, nil
}

// smallOf reads a small INTEGER.
func smallOf(n node, what string) (int64, *Refusal) {
	v, r := bigOf(n, what)
	if r != nil {
		return 0, r
	}
	if !v.IsInt64() {
		return 0, refuse(ReasonMalformed, "%s is out of range", what)
	}
	return v.Int64(), nil
}

// implicitInt reads the content of an IMPLICIT-tagged INTEGER (DER minimal two's complement).
func implicitInt(n node, what string) (*big.Int, *Refusal) {
	b := n.Bytes
	if len(b) == 0 || len(b) > 8 {
		return nil, refuse(ReasonMalformed, "%s has %d content bytes", what, len(b))
	}
	if len(b) > 1 && (b[0] == 0 && b[1]&0x80 == 0 || b[0] == 0xff && b[1]&0x80 != 0) {
		return nil, refuse(ReasonNotDER, "%s is not minimally encoded", what)
	}
	v := new(big.Int).SetBytes(b)
	if b[0]&0x80 != 0 {
		v.Sub(v, new(big.Int).Lsh(big.NewInt(1), uint(8*len(b))))
	}
	return v, nil
}

// wellFormed walks a DER element and everything inside it, refusing BER forms (a constructed
// universal type other than SEQUENCE and SET) and nesting deeper than maxDepth. It is how ignored
// fields are still held to DER.
func wellFormed(n node, depth int) *Refusal {
	if depth > maxDepth {
		return refuse(ReasonTooDeep, "nesting deeper than %d", maxDepth)
	}
	if !n.IsCompound {
		return nil
	}
	if n.Class == asn1.ClassUniversal && n.Tag != asn1.TagSequence && n.Tag != asn1.TagSet {
		return refuse(ReasonNotDER, "constructed universal type %d", n.Tag)
	}
	cs, r := kids(n)
	if r != nil {
		return r
	}
	for _, c := range cs {
		if r := wellFormed(c, depth+1); r != nil {
			return r
		}
	}
	return nil
}

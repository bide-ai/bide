package runview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"html/template"
	"strings"
	"time"

	"github.com/bide-ai/bide/audit"
)

// MerklePath is one record's climb to the signed root: its leaf hash, the sibling hash at each
// level of the RFC 6962 audit path and the node it combines into, ending at the root the signed
// tree head commits to. The page draws it as a small diagram. It is built from the proof the
// audit package verified (audit.ProofBundle), so the diagram shows a path that holds.
type MerklePath struct {
	Seq   int    // the record's 1-based position in the journal
	Name  string // its journal name
	Size  int    // the journal length the signed root commits to
	Leaf  string // the record's leaf hash, hex
	Steps []MerkleStep
	Root  string // the signed root, hex
	// Climbed reports that hashing the path from the leaf gave Root, which the page checks again
	// here for the intermediate hashes it shows; the proof itself was verified by audit.
	Climbed  bool
	Alg      string
	SignedAt time.Time
}

// MerkleStep is one level: the sibling hash, which side it is on, and the node they make.
type MerkleStep struct {
	Sibling string
	Left    bool // the sibling is the left child
	Node    string
}

// short is the first 8 hex digits of a hash, enough to tell hashes apart on a diagram.
func short8(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// nodeHash is the RFC 6962 interior node hash: SHA-256(0x01 || left || right).
func nodeHash(l, r []byte) []byte {
	h := sha256.New()
	h.Write([]byte{0x01})
	h.Write(l)
	h.Write(r)
	return h.Sum(nil)
}

// merklePath reads the climb out of a verified bundle, following the RFC 9162 verification of an
// inclusion proof to know which side each sibling is on.
func merklePath(b audit.ProofBundle, name string) *MerklePath {
	m := &MerklePath{Seq: b.Inclusion.Index + 1, Name: name, Size: b.Inclusion.Size, Alg: string(b.STH.Alg),
		Root: hex.EncodeToString(b.STH.Root), SignedAt: headTime(b.STH)}
	leaf := audit.JournalLeafHash(b.RecordBytes)
	m.Leaf = hex.EncodeToString(leaf)
	fn, sn := b.Inclusion.Index, b.Inclusion.Size-1
	r := leaf
	ok := true
	for _, p := range b.Inclusion.Path {
		if sn == 0 {
			ok = false
			break
		}
		st := MerkleStep{Sibling: hex.EncodeToString(p)}
		if fn&1 == 1 || fn == sn {
			r, st.Left = nodeHash(p, r), true
			for fn&1 == 0 && fn != 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			r = nodeHash(r, p)
		}
		fn >>= 1
		sn >>= 1
		st.Node = hex.EncodeToString(r)
		m.Steps = append(m.Steps, st)
	}
	m.Climbed = ok && sn == 0 && bytes.Equal(r, b.STH.Root)
	return m
}

// Diagram is the path drawn as an SVG, or empty when it did not climb to the root (the page then
// shows no diagram rather than one that does not hold).
func (m *MerklePath) Diagram() template.HTML {
	if m == nil || !m.Climbed {
		return ""
	}
	return template.HTML(merkleSVG(m)) // built by merkleSVG, every label escaped
}

// Caption says what the diagram shows.
func (m *MerklePath) Caption() string {
	return fmt.Sprintf("Record %d of %d (%s) hashed up %d level%s to the root the signed head commits to. Verified by the audit package: the head's signature, and the path from this record to its root.",
		m.Seq, m.Size, m.Name, len(m.Steps), plural(len(m.Steps)))
}

// merkleSVG draws the climb bottom to top: the record's leaf at the foot, a sibling hash beside
// each node on the path, and the signed root at the top.
func merkleSVG(m *MerklePath) string {
	const (
		w, rowH = 660, 78 // w leaves room for the signed time beside the root, on the side with no sibling
		cx      = w / 2
		nodeW   = 230
		nodeH   = 46
		sibW    = 170
		sibH    = 46
		sibOff  = 210
		top     = 24
	)
	n := len(m.Steps)
	h := top + n*rowH + nodeH + 28
	y := func(level int) int { return top + (n-level)*rowH } // the top of the box at a level; level n is the root
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="merkle" viewBox="0 0 %d %d" width="%d" height="%d" role="group" aria-label="audit path: record %d to the signed root in %d levels">`, w, h, w, h, m.Seq, n)
	b.WriteString(`<defs>`)
	for _, mk := range []struct{ id, cls string }{{"mk-arrow", "arrowhead taken"}, {"mk-sib", "arrowhead"}} {
		fmt.Fprintf(&b, `<marker id="%s" viewBox="0 0 10 10" refX="9" refY="5" markerUnits="userSpaceOnUse" markerWidth="11" markerHeight="11" orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" class="%s"/></marker>`, mk.id, mk.cls)
	}
	b.WriteString(iconDefs("gi-", "approval"))
	b.WriteString(`</defs>`)
	box := func(cls string, x, yy, bw, bh int, cap, hash string) {
		fmt.Fprintf(&b, `<g class="mk %s"><rect class="box" x="%d" y="%d" width="%d" height="%d" rx="10"/>`, cls, x, yy, bw, bh)
		fmt.Fprintf(&b, `<text class="mkcap" x="%d" y="%d">%s</text><text class="mkhash" x="%d" y="%d">%s</text>`, x+bw/2, yy+16, html.EscapeString(cap), x+bw/2, yy+bh-9, html.EscapeString(hash))
		fmt.Fprintf(&b, `<title>%s</title></g>`, html.EscapeString(cap+" "+hash))
	}
	// Edges first, so the boxes sit on them: up the path, and each sibling into its node.
	for i, st := range m.Steps {
		fromY, toY := y(i), y(i+1)
		fmt.Fprintf(&b, `<path class="edge taken" d="M%d,%d L%d,%d" marker-end="url(#mk-arrow)"/>`, cx, fromY, cx, toY+nodeH+3)
		sign := 1
		if st.Left {
			sign = -1
		}
		sx, ex, ey := cx+sign*(sibOff-sibW/2), cx+sign*(nodeW/2+3), toY+nodeH/2
		fmt.Fprintf(&b, `<path class="edge" d="M%d,%d C%d,%d %d,%d %d,%d" marker-end="url(#mk-sib)"/>`, sx, ey, sx+sign*-20, ey, ex+sign*30, ey, ex, ey)
	}
	if n == 0 {
		box("leaf", cx-nodeW/2, y(0), nodeW, nodeH, fmt.Sprintf("record %d · %s", m.Seq, clip(m.Name, 20)), "leaf is the root "+short8(m.Leaf))
	} else {
		box("leaf", cx-nodeW/2, y(0), nodeW, nodeH, fmt.Sprintf("record %d · %s", m.Seq, clip(m.Name, 20)), "leaf "+short8(m.Leaf))
	}
	for i, st := range m.Steps {
		sx := cx - sibOff - sibW/2
		if !st.Left {
			sx = cx + sibOff - sibW/2
		}
		yy := y(i + 1)
		box("sib", sx, yy+(nodeH-sibH)/2, sibW, sibH, "sibling", short8(st.Sibling))
		if i+1 < n {
			box("node", cx-nodeW/2, yy, nodeW, nodeH, fmt.Sprintf("level %d", i+1), short8(st.Node))
		}
	}
	if n > 0 {
		yy := y(n)
		fmt.Fprintf(&b, `<g class="mk root"><rect class="box" x="%d" y="%d" width="%d" height="%d" rx="12"/><use class="nicon" href="#gi-approval" x="%d" y="%d" width="24" height="24"/>`,
			cx-nodeW/2-22, yy-4, nodeW+44, nodeH+8, cx-nodeW/2-10, yy+(nodeH-24)/2)
		fmt.Fprintf(&b, `<text class="mkcap" x="%d" y="%d">signed root · %s</text><text class="mkhash" x="%d" y="%d">%s</text>`, cx+12, yy+16, html.EscapeString(m.Alg), cx+12, yy+nodeH-9, html.EscapeString(short8(m.Root)))
		if !m.SignedAt.IsZero() {
			// Beside the root, opposite the last sibling, which sits in the root's row.
			lx, anchor := cx+nodeW/2+28, "start"
			if !m.Steps[n-1].Left {
				lx, anchor = cx-nodeW/2-28, "end"
			}
			fmt.Fprintf(&b, `<text class="mksub" x="%d" y="%d" text-anchor="%s">signed</text><text class="mksub" x="%d" y="%d" text-anchor="%s">%s</text>`, lx, yy+nodeH/2-2, anchor, lx, yy+nodeH/2+14, anchor, html.EscapeString(m.SignedAt.Format("2006-01-02 15:04:05 UTC")))
		}
		fmt.Fprintf(&b, `<title>signed root %s</title></g>`, html.EscapeString(m.Root))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

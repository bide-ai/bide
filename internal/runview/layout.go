package runview

import (
	"fmt"
	"slices"
	"sort"
)

// The graph layout is a layered one, written here and depending on nothing:
//
//  1. rank: each node's row is the length of its longest forward path from the entry (loop-back
//     arms and any edge that would not point down are set aside);
//  2. long edges: an edge that skips rows passes through a dummy node in each row it crosses, so
//     it is routed through the gaps beside the nodes, not behind them;
//  3. order: sweeps of the barycenter heuristic order each row to cut crossings, keeping the best
//     order seen;
//  4. place: each row is pulled toward the mean of its neighbors' centers, with the least-squares
//     placement that keeps the order and the gap (an isotonic regression), sweeping down and up;
//  5. route: edges leave and enter on spread ports and bend with vertical tangents; loop-back arcs
//     run down the right side, outside every node of the rows they span.
//
// Every step is deterministic: ties break on the declared order.
const (
	nodeW  = 228
	gapX   = 44 // between two nodes of a row
	gapDum = 14 // beside a dummy node (a long edge passing)
	gapY   = 68 // between rows
	pad    = 28
	dummyW = 10
	badgeW = 52 // a gate badge sits on a node's top right corner and sticks out by half of it
)

// lnode is a real node or a dummy in a row.
type lnode struct {
	real int // index into Graph.Nodes, or -1 for a dummy
	w    int
	cx   float64
	rank int
}

// extraLines is how many text lines under the node's two (name, and kind with status or risk) it
// needs: a crash line, a gate line, a signature line.
func extraLines(n GraphNode) int {
	switch {
	case n.Gate != "":
		return 2
	case n.Crash != "":
		return 1
	}
	return 0
}

// layout places the graph's nodes in rows, routes its edges and draws it.
func layout(g *Graph, entry string) {
	extra := 0
	for _, n := range g.Nodes {
		extra = max(extra, extraLines(n))
	}
	g.nw, g.nh, g.gy = nodeW, 56+18*extra, gapY-6*extra
	place(g, entry)
	g.SVG = svg(g)
}

// place sets every node's x, y and rank, every edge's path and label position, and the graph's size.
func place(g *Graph, entry string) {
	n := len(g.Nodes)
	idx := make(map[string]int, n)
	for i, nd := range g.Nodes {
		idx[nd.Name] = i
	}
	rank := make([]int, n)
	// Relax forward edges until stable: a DAG of n nodes settles in n passes.
	for range g.Nodes {
		changed := false
		for _, e := range g.Edges {
			f, okf := idx[e.From]
			t, okt := idx[e.To]
			if e.Back || !okf || !okt || e.To == entry {
				continue
			}
			if rank[t] < rank[f]+1 && rank[f]+1 < n {
				rank[t], changed = rank[f]+1, true
			}
		}
		if !changed {
			break
		}
	}
	for i := range g.Nodes {
		g.Nodes[i].rank = rank[i]
	}

	// Rows of real nodes and of the dummies of long edges.
	var ln []lnode
	for i := range g.Nodes {
		ln = append(ln, lnode{real: i, w: g.nw, rank: rank[i]})
	}
	chains := make([][]int, len(g.Edges)) // the lnode path of each forward edge, nil for a back edge
	for ei, e := range g.Edges {
		f, okf := idx[e.From]
		t, okt := idx[e.To]
		if !okf || !okt || e.Back || rank[t] <= rank[f] {
			continue
		}
		c := []int{f}
		for r := rank[f] + 1; r < rank[t]; r++ {
			ln = append(ln, lnode{real: -1, w: dummyW, rank: r})
			c = append(c, len(ln)-1)
		}
		chains[ei] = append(c, t)
	}
	maxRank := 0
	for _, v := range ln {
		maxRank = max(maxRank, v.rank)
	}
	g.ranks = maxRank + 1
	rows := make([][]int, maxRank+1)
	for v := range ln {
		rows[ln[v].rank] = append(rows[ln[v].rank], v)
	}
	up := make([][]int, len(ln))
	down := make([][]int, len(ln))
	for _, c := range chains {
		for k := 0; k+1 < len(c); k++ {
			down[c[k]] = append(down[c[k]], c[k+1])
			up[c[k+1]] = append(up[c[k+1]], c[k])
		}
	}
	orderRows(rows, ln, up, down)
	placeX(rows, ln, up, down)

	// Absolute positions: the leftmost box at pad.
	left := 1e9
	for _, v := range ln {
		left = min(left, v.cx-float64(v.w)/2)
	}
	shift := float64(pad) - left
	for v := range ln {
		ln[v].cx += shift
	}
	nh, gy := g.nh, g.gy
	rowY := func(r int) int { return pad + r*(nh+gy) }
	width := 0
	for v := range ln {
		if ln[v].real >= 0 {
			nd := &g.Nodes[ln[v].real]
			nd.x, nd.y = int(ln[v].cx)-g.nw/2, rowY(ln[v].rank)
		}
		width = max(width, int(ln[v].cx)+ln[v].w/2)
	}
	g.Height = pad*2 + g.ranks*nh + (g.ranks-1)*gy + 16 // 16: the last row's notes

	route(g, idx, ln, chains, rowY)
	g.Width = width + pad + badgeW/2
	for _, e := range g.Edges {
		if e.arc {
			g.Width = max(g.Width, e.ax+pad)
		}
		if e.Label != "" {
			w, _ := labelSize(e.Label)
			g.Width = max(g.Width, e.lx+w/2+pad)
		}
	}
}

// orderRows orders each row to cut edge crossings: barycenter sweeps down and up, the best order kept.
func orderRows(rows [][]int, ln []lnode, up, down [][]int) {
	pos := make([]int, len(ln))
	setPos := func() {
		for _, row := range rows {
			for i, v := range row {
				pos[v] = i
			}
		}
	}
	setPos()
	cross := func() int {
		total := 0
		for r := 0; r+1 < len(rows); r++ {
			type pair struct{ a, b int }
			var es []pair
			for _, u := range rows[r] {
				for _, w := range down[u] {
					es = append(es, pair{pos[u], pos[w]})
				}
			}
			for i := range es {
				for j := i + 1; j < len(es); j++ {
					if (es[i].a-es[j].a)*(es[i].b-es[j].b) < 0 {
						total++
					}
				}
			}
		}
		return total
	}
	sweep := func(r int, nbr [][]int) {
		row := rows[r]
		bary := make(map[int]float64, len(row))
		for _, v := range row {
			if len(nbr[v]) == 0 {
				bary[v] = float64(pos[v])
				continue
			}
			s := 0.0
			for _, w := range nbr[v] {
				s += float64(pos[w])
			}
			bary[v] = s / float64(len(nbr[v]))
		}
		sort.SliceStable(row, func(i, j int) bool { return bary[row[i]] < bary[row[j]] })
		for i, v := range row {
			pos[v] = i
		}
	}
	best, bestRows := cross(), cloneRows(rows)
	if len(ln) > 400 {
		return // a graph this size is laid out in the order it is declared
	}
	for it := 0; it < 8 && best > 0; it++ {
		if it%2 == 0 {
			for r := 1; r < len(rows); r++ {
				sweep(r, up)
			}
		} else {
			for r := len(rows) - 2; r >= 0; r-- {
				sweep(r, down)
			}
		}
		if c := cross(); c < best {
			best, bestRows = c, cloneRows(rows)
		}
	}
	for r := range rows {
		copy(rows[r], bestRows[r])
	}
}

func cloneRows(rows [][]int) [][]int {
	out := make([][]int, len(rows))
	for i, r := range rows {
		out[i] = slices.Clone(r)
	}
	return out
}

// placeX sets every node's center: packed, then pulled toward its neighbors' mean, row by row.
func placeX(rows [][]int, ln []lnode, up, down [][]int) {
	gap := func(a, b int) float64 {
		g := gapX
		if ln[a].real < 0 || ln[b].real < 0 {
			g = gapDum
		}
		return float64(ln[a].w+ln[b].w)/2 + float64(g)
	}
	for _, row := range rows {
		x := 0.0
		for i, v := range row {
			if i > 0 {
				x += gap(row[i-1], v)
			}
			ln[v].cx = x
		}
		for _, v := range row {
			ln[v].cx -= x / 2
		}
	}
	pull := func(r int, nbr [][]int) {
		row := rows[r]
		want := make([]float64, len(row))
		for i, v := range row {
			want[i] = ln[v].cx
			if len(nbr[v]) > 0 {
				s := 0.0
				for _, w := range nbr[v] {
					s += ln[w].cx
				}
				want[i] = s / float64(len(nbr[v]))
			}
		}
		// Least squares with the order and the gaps kept: with o the gaps summed from the left,
		// x_i - o_i must not decrease, and the nearest such sequence to want_i - o_i is the
		// pool-adjacent-violators fit.
		off := make([]float64, len(row))
		for i := 1; i < len(row); i++ {
			off[i] = off[i-1] + gap(row[i-1], row[i])
		}
		type block struct {
			sum float64
			n   int
		}
		var st []block
		for i := range row {
			st = append(st, block{want[i] - off[i], 1})
			for len(st) > 1 {
				a, b := st[len(st)-2], st[len(st)-1]
				if a.sum/float64(a.n) <= b.sum/float64(b.n) {
					break
				}
				st = append(st[:len(st)-2], block{a.sum + b.sum, a.n + b.n})
			}
		}
		i := 0
		for _, b := range st {
			for k := 0; k < b.n; k++ {
				ln[row[i]].cx = b.sum/float64(b.n) + off[i]
				i++
			}
		}
	}
	for it := 0; it < 6; it++ {
		for r := 1; r < len(rows); r++ {
			pull(r, up)
		}
		for r := len(rows) - 2; r >= 0; r-- {
			pull(r, down)
		}
	}
}

type pt struct{ x, y int }

// route sets each edge's path and label position. A forward edge leaves its source's bottom and
// enters its target's top on ports spread across the box, passing straight through the dummy of
// each row it crosses; a loop-back arc leaves the right side and runs outside the rows it spans.
func route(g *Graph, idx map[string]int, ln []lnode, chains [][]int, rowY func(int) int) {
	nh := g.nh
	cx := func(v int) int { return int(ln[v].cx) }
	// Ports: the out-edges of a node sorted by where each goes next, the in-edges by where each
	// came from, spread across the middle of the box.
	type end struct{ edge, other int }
	outs := map[int][]end{}
	ins := map[int][]end{}
	for ei, c := range chains {
		if c == nil {
			continue
		}
		outs[c[0]] = append(outs[c[0]], end{ei, cx(c[1])})
		ins[c[len(c)-1]] = append(ins[c[len(c)-1]], end{ei, cx(c[len(c)-2])})
	}
	port := func(m map[int][]end, node int) map[int]int {
		es := m[node]
		sort.SliceStable(es, func(i, j int) bool { return es[i].other < es[j].other })
		p := map[int]int{}
		w := g.nw
		for i, e := range es {
			p[e.edge] = cx(node) - w/2 + w*(i+1)/(len(es)+1)
		}
		return p
	}
	outPort := map[int]int{}
	inPort := map[int]int{}
	for v := range outs {
		for e, x := range port(outs, v) {
			outPort[e] = x
		}
	}
	for v := range ins {
		for e, x := range port(ins, v) {
			inPort[e] = x
		}
	}
	var boxes []rect // the labels placed, to keep two apart
	label := func(e *GraphEdge, x, y int) {
		w, h := labelSize(e.Label)
		r := rect{x - w/2, y - h/2, w, h}
		for try := 0; try < 6; try++ {
			if !slices.ContainsFunc(boxes, func(o rect) bool { return o.overlaps(r) }) {
				break
			}
			r.y += 16 * (1 - 2*(try%2)) * (try/2 + 1) // 16 down, 16 up, 32 down...
		}
		boxes = append(boxes, r)
		e.lx, e.ly = r.x+r.w/2, r.y+r.h/2
	}
	var spans [][2]int
	for ei := range g.Edges {
		e := &g.Edges[ei]
		f, okf := idx[e.From]
		t, okt := idx[e.To]
		if !okf || !okt {
			continue
		}
		if c := chains[ei]; c != nil {
			pts := []pt{{outPort[ei], g.Nodes[f].y + nh}}
			for _, d := range c[1 : len(c)-1] {
				y := rowY(ln[d].rank)
				pts = append(pts, pt{cx(d), y}, pt{cx(d), y + nh})
			}
			pts = append(pts, pt{inPort[ei], g.Nodes[t].y - 2})
			e.d = curve(pts)
			if e.Label != "" || e.Problem != "" {
				label(e, (pts[0].x+pts[1].x)/2, (pts[0].y+pts[1].y)/2)
			}
			continue
		}
		// A loop-back arc: out of the right side, around outside the rows it spans.
		lo, hi := g.Nodes[t].rank, g.Nodes[f].rank
		if lo > hi {
			lo, hi = hi, lo
		}
		right := 0
		for v := range ln {
			if ln[v].rank >= lo && ln[v].rank <= hi {
				right = max(right, cx(v)+ln[v].w/2)
			}
		}
		k := 0
		for _, s := range spans {
			if s[0] <= hi && lo <= s[1] {
				k++
			}
		}
		spans = append(spans, [2]int{lo, hi})
		arc := right + 34 + 18*k
		a, z := g.Nodes[f], g.Nodes[t]
		x1, y1 := a.x+g.nw, a.y+nh/2
		x2, y2 := z.x+g.nw+2, z.y+nh/2
		if y1 == y2 {
			y1, y2 = y1-nh/5, y2+nh/5
		}
		e.d = fmt.Sprintf("M%d,%d C%d,%d %d,%d %d,%d", x1, y1, arc, y1, arc, y2, x2, y2)
		e.arc, e.ax = true, arc
		if e.Label != "" || e.Problem != "" {
			w, _ := labelSize(e.Label)
			label(e, arc-w/2-4, (y1+y2)/2) // inside the arc
		}
	}
}

// curve draws pts as a chain of segments: a straight one between two points of one x, and an S
// bend with vertical tangents otherwise.
func curve(pts []pt) string {
	s := fmt.Sprintf("M%d,%d", pts[0].x, pts[0].y)
	for i := 1; i < len(pts); i++ {
		p, q := pts[i-1], pts[i]
		if p.x == q.x {
			s += fmt.Sprintf(" L%d,%d", q.x, q.y)
			continue
		}
		m := (p.y + q.y) / 2
		s += fmt.Sprintf(" C%d,%d %d,%d %d,%d", p.x, m, q.x, m, q.x, q.y)
	}
	return s
}

type rect struct{ x, y, w, h int }

func (a rect) overlaps(b rect) bool {
	return a.x < b.x+b.w && b.x < a.x+a.w && a.y < b.y+b.h && b.y < a.y+a.h
}

// labelSize estimates an edge label's pill: 12.5px text is about 7px a character.
func labelSize(s string) (w, h int) { return 7*len([]rune(s)) + 16, 20 }

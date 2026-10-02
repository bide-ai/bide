package main

import (
	"fmt"
	"strings"
	"testing"

	gsm "github.com/blackwell-systems/gsm"
)

// TestSignalsCommute checks the order independence the example claims, exhaustively: from every
// state the signal events reach (a breadth-first search from the initial state), any two signal
// events, on the same line or on different lines, applied in either order, leave every line in the
// same state. Each line's signal takes each of the 3 levels independently, so there are 27.
func TestSignalsCommute(t *testing.T) {
	m, regs, _ := buildSafetyMesh()
	type ev struct {
		reg         *gsm.Registry
		line, event string
	}
	var evs []ev
	for i, r := range regs {
		for _, lv := range signalLevels {
			evs = append(evs, ev{r, fmt.Sprintf("line%d", i+1), "signal_" + lv})
		}
	}
	show := func(fs gsm.FedState) string {
		var b strings.Builder
		for i, r := range regs {
			fmt.Fprintf(&b, "line%d%v ", i+1, m.Of(fs, r))
		}
		return b.String()
	}
	seen := map[string]bool{show(m.NewState()): true}
	queue := []gsm.FedState{m.NewState()}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, a := range evs {
			next := m.Apply(s, a.reg, a.event)
			if k := show(next); !seen[k] {
				seen[k] = true
				queue = append(queue, next)
			}
			for _, b := range evs {
				ab := m.Apply(next, b.reg, b.event)
				ba := m.Apply(m.Apply(s, b.reg, b.event), a.reg, a.event)
				if show(ab) != show(ba) {
					t.Fatalf("from %s: %s.%s then %s.%s gives %s; the other order gives %s",
						show(s), a.line, a.event, b.line, b.event, show(ab), show(ba))
				}
			}
		}
	}
	if len(seen) != 27 {
		t.Fatalf("reached %d states, want 27 (3 levels for each of 3 lines)", len(seen))
	}
}

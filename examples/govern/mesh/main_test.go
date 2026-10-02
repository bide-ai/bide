package main

import (
	"fmt"
	"strings"
	"testing"

	gsm "github.com/blackwell-systems/gsm"
)

// TestSignalsCommute checks the order independence the example claims: any two signal events, on
// the same line or on different lines, applied in either order from the initial state or from any
// state one signal away, leave every line in the same state.
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
	starts := []gsm.FedState{m.NewState()}
	for _, e := range evs {
		starts = append(starts, m.Apply(m.NewState(), e.reg, e.event))
	}
	for _, s := range starts {
		for _, a := range evs {
			for _, b := range evs {
				ab := m.Apply(m.Apply(s, a.reg, a.event), b.reg, b.event)
				ba := m.Apply(m.Apply(s, b.reg, b.event), a.reg, a.event)
				if show(ab) != show(ba) {
					t.Fatalf("from %s: %s.%s then %s.%s gives %s; the other order gives %s",
						show(s), a.line, a.event, b.line, b.event, show(ab), show(ba))
				}
			}
		}
	}
}

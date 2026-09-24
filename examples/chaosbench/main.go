// Command chaosbench runs the crash-injection benchmark: it drives a non-idempotent side
// effect through an exhaustive crash-point sweep plus randomized multi-crash schedules, and
// reports whether the side effect ever fired more than once. A durable, side-effect-safe
// loop passes; an at-least-once loop double-fires.
//
// Add another SDK by implementing chaos.System for it (a small adapter wiring that SDK to
// do one non-idempotent side effect against a fault-injectable store) and adding it here.
package main

import (
	"fmt"

	"github.com/dayna/go-agents/chaos"
)

func main() {
	fmt.Println("chaos benchmark — a non-idempotent side effect under crash injection")
	fmt.Println("invariant: a crash must NEVER double-fire the side effect (at-most-once)")
	fmt.Println()

	systems := []struct {
		name string
		sys  chaos.System
	}{
		{"go-agents", chaos.GoAgents()},
		{"naive-loop", chaos.NaiveReference()}, // the at-least-once baseline (expected to fail)
		// Add competitor adapters here: {"langchaingo", langchaingoAdapter{}}, ...
	}
	for _, s := range systems {
		fmt.Println("  " + chaos.Verify(s.name, s.sys, 1000).String())
	}
}

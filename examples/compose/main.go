// Command compose demonstrates compositional construction: a "pricing" subsystem is defined
// and verified on its own, then embedded as a reusable unit into a larger "storefront"
// system and connected to an order registry. A pricing upgrade then propagates through the
// embedded subsystem and across the embed boundary. Run: go run ./examples/compose
package main

import (
	"context"
	"fmt"

	gsm "github.com/blackwell-systems/gsm"
	"github.com/dayna/go-agents/govern"
)

func main() {
	ctx := context.Background()

	// Step 1: a reusable subsystem — pricing tier drives a catalog badge — verified alone.
	pricing := gsm.NewRegistry("pricing")
	tier := pricing.Enum("tier", "free", "pro")
	pricing.Event("upgrade").Writes(tier).
		Guard(func(s gsm.State) bool { return s.Get(tier) == "free" }).
		Apply(func(s gsm.State) gsm.State { return s.Set(tier, "pro") }).Add()

	catalog := gsm.NewRegistry("catalog")
	badge := catalog.Enum("badge", "none", "star")
	badgeOf := map[string]string{"free": "none", "pro": "star"}

	sub := gsm.NewFederation("pricing-subsystem").
		Morphism(pricing, catalog).Shared(badge).
		Map(func(srcNF, d gsm.State) gsm.State { return d.Set(badge, badgeOf[srcNF.Get(tier)]) }).Add()
	if _, _, err := sub.Build(); err != nil { // verified in isolation
		panic(err)
	}

	fmt.Println("== Compositional construction ==")
	fmt.Println("Step 1: define & verify the 'pricing' subsystem on its own ............ ok")

	// Step 2: embed the verified subsystem into a larger storefront and add an order registry.
	order := gsm.NewRegistry("order")
	perk := order.Enum("perk", "basic", "premium")
	perkOf := map[string]string{"none": "basic", "star": "premium"}

	m, _, err := gsm.NewFederation("storefront").
		Embed(sub).
		Morphism(catalog, order).Shared(perk).
		Map(func(srcNF, d gsm.State) gsm.State { return d.Set(perk, perkOf[srcNF.Get(badge)]) }).Add().
		Build()
	if err != nil {
		panic(err)
	}
	fmt.Printf("Step 2: embed it + connect an 'order' registry ....................... %d-component system\n\n", len(m.Registries()))

	// Step 3: run it through a durable governor; a pricing upgrade propagates end to end.
	g, _ := govern.NewFederated(ctx, m, govern.NewMemEventLog(), "acct-42", m.NewState())
	trace := func(label string) {
		st := g.State()
		fmt.Printf("%-22s tier=%-5s badge=%-5s perk=%s\n", label,
			m.Of(st, pricing).Get(tier), m.Of(st, catalog).Get(badge), m.Of(st, order).Get(perk))
	}
	trace("initial:")
	g.Apply(ctx, "pricing", "upgrade")
	trace("after upgrade:")
	fmt.Println("→ upgrade propagated: pricing → catalog (inside the embedded subsystem) → order (across the embed boundary).")
	fmt.Println("\nThe subsystem was verified once and reused as a black box — the compositional-collapse result.")
}

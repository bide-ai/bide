// Command compose demonstrates compositional construction: a multi-registry "fulfillment"
// subsystem (inventory → shipping, with an internal morphism) is defined and verified on its
// own, then embedded — as a reusable black box — into TWO different larger systems: a
// customer-facing storefront and an ops dashboard. Each connects the subsystem to its own
// boundary registry. The point: verify a subsystem once, reuse it in many systems.
//
// Run: go run ./examples/compose
package main

import (
	"context"
	"fmt"

	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

// buildFulfillment is the reusable subsystem: two internal registries connected by a morphism.
// Higher stock lets shipping expedite. Returned so outer systems can wire to its registries.
func buildFulfillment() (sub *gsm.Federation, inventory, shipping *gsm.Registry, stock, mode gsm.Var) {
	inventory = gsm.NewRegistry("inventory")
	stock = inventory.Enum("stock", "low", "high")
	inventory.Event("restock").Writes(stock).
		Apply(func(s gsm.State) gsm.State { return s.Set(stock, "high") }).Add()

	shipping = gsm.NewRegistry("shipping")
	mode = shipping.Enum("mode", "standard", "expedited")
	modeOf := map[string]string{"low": "standard", "high": "expedited"}
	sub = gsm.NewFederation("fulfillment").
		Morphism(inventory, shipping).Shared(mode).
		Map(func(srcNF, d gsm.State) gsm.State { return d.Set(mode, modeOf[srcNF.Get(stock)]) }).Add()
	return
}

func main() {
	ctx := context.Background()

	fmt.Println("== Compositional construction: verify a subsystem once, reuse it ==")

	// Step 1: define & verify the multi-registry subsystem in isolation.
	sub, inventory, shipping, stock, mode := buildFulfillment()
	if _, _, err := sub.Build(); err != nil {
		panic(err)
	}
	fmt.Println("Step 1: define & verify the 'fulfillment' subsystem (inventory → shipping) ... ok")

	// Step 2a: embed it into a customer-facing storefront (adds an order/ETA registry).
	order := gsm.NewRegistry("order")
	eta := order.Enum("eta", "7-day", "2-day")
	etaOf := map[string]string{"standard": "7-day", "expedited": "2-day"}
	storefront, _, err := gsm.NewFederation("storefront").
		Embed(sub).
		Morphism(shipping, order).Shared(eta).
		Map(func(srcNF, d gsm.State) gsm.State { return d.Set(eta, etaOf[srcNF.Get(mode)]) }).Add().
		Build()
	if err != nil {
		panic(err)
	}

	// Step 2b: embed the SAME verified subsystem into an ops dashboard (adds a reorder alert).
	ops := gsm.NewRegistry("ops")
	alert := ops.Enum("reorder", "needed", "ok")
	alertOf := map[string]string{"low": "needed", "high": "ok"}
	dashboard, _, err := gsm.NewFederation("ops-dashboard").
		Embed(sub).
		Morphism(inventory, ops).Shared(alert).
		Map(func(srcNF, d gsm.State) gsm.State { return d.Set(alert, alertOf[srcNF.Get(stock)]) }).Add().
		Build()
	if err != nil {
		panic(err)
	}
	fmt.Printf("Step 2: embed it into TWO systems — storefront (%d comps) and ops-dashboard (%d comps)\n\n",
		len(storefront.Registries()), len(dashboard.Registries()))

	// Step 3: run each system through its own durable governor; one restock, two views.
	gStore, _ := govern.NewFederated(ctx, storefront, govern.NewMemEventLog(), "store", storefront.NewState())
	gOps, _ := govern.NewFederated(ctx, dashboard, govern.NewMemEventLog(), "ops", dashboard.NewState())

	fmt.Printf("storefront  before restock: stock=%s shipping=%s order-eta=%s\n",
		storefront.Of(gStore.State(), inventory).Get(stock), storefront.Of(gStore.State(), shipping).Get(mode), storefront.Of(gStore.State(), order).Get(eta))
	fmt.Printf("ops         before restock: stock=%s shipping=%s reorder=%s\n",
		dashboard.Of(gOps.State(), inventory).Get(stock), dashboard.Of(gOps.State(), shipping).Get(mode), dashboard.Of(gOps.State(), ops).Get(alert))

	gStore.Apply(ctx, "inventory", "restock")
	gOps.Apply(ctx, "inventory", "restock")

	fmt.Printf("storefront  after restock:  stock=%s shipping=%s order-eta=%s\n",
		storefront.Of(gStore.State(), inventory).Get(stock), storefront.Of(gStore.State(), shipping).Get(mode), storefront.Of(gStore.State(), order).Get(eta))
	fmt.Printf("ops         after restock:  stock=%s shipping=%s reorder=%s\n",
		dashboard.Of(gOps.State(), inventory).Get(stock), dashboard.Of(gOps.State(), shipping).Get(mode), dashboard.Of(gOps.State(), ops).Get(alert))

	fmt.Println("\n→ the same verified 'fulfillment' subsystem drives both systems; restock propagates")
	fmt.Println("  inventory → shipping (inside the subsystem) → each system's own boundary registry.")
	fmt.Println("(Note: reusing a subsystem twice within ONE system would need registry namespacing,")
	fmt.Println(" which Embed does not yet provide — reuse here is across separate systems.)")
}

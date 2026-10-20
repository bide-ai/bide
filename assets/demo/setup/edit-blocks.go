//go:build ignore

// edit-blocks.go writes the blocks description that bide edit -blocks reads (assets/demo/shots-edit-typed.sh):
// what a program's plan.Registry registers, as json.Marshal of Registry.Describe writes it. The blocks are
// the refund flow's (validate, charge, receipt) and four the flow does not use, so the palette has rows
// that are not placed. Run it from cmd/bide, which carries the modules it imports:
//
//	go run ../../assets/demo/setup/edit-blocks.go -out blocks.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

// Order is what the refund flow passes from step to step.
type Order struct{ ID string }

func main() {
	out := flag.String("out", "blocks.json", "where to write the description")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "edit-blocks.go:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	reg := plan.NewRegistry()
	order := func(_ context.Context, o Order) (Order, error) { return o, nil }
	for _, err := range []error{
		reg.RegisterStep("validate", order, plan.ReadOnly()),
		reg.RegisterStep("charge", order),
		reg.RegisterStep("receipt", func(_ context.Context, o Order) (string, error) { return o.ID, nil }, plan.Idempotent()),
		reg.RegisterStep("notify", func(_ context.Context, s string) (string, error) { return strings.ToUpper(s), nil }, plan.Idempotent()),
		reg.RegisterStep("score", func(_ context.Context, o Order) (int, error) { return len(o.ID), nil }, plan.ReadOnly()),
		reg.RegisterStep("manual-review", order, plan.Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}})),
		reg.RegisterPredicate("large", func(o Order) bool { return len(o.ID) > 3 }),
	} {
		if err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(reg.Describe(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, b, 0o600)
}

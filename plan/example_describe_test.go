package plan_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

type order struct{ ID int }
type receipt struct{ Status string }

func ExampleRegistry_Describe() {
	reg := plan.NewRegistry()
	_ = reg.RegisterStep("price", func(_ context.Context, o order) (order, error) { return o, nil }, plan.ReadOnly())
	_ = reg.RegisterStep("refund", func(_ context.Context, o order) (receipt, error) { return receipt{"paid"}, nil },
		plan.Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance"}}))
	_ = reg.RegisterPredicate("large", func(o order) bool { return o.ID > 100 })

	// A program writes this to a file for an editor or a check to read without the program.
	out, _ := json.MarshalIndent(reg.Describe(), "", "  ")
	fmt.Println(string(out))
	// Output:
	// {
	//   "version": 1,
	//   "blocks": [
	//     {
	//       "name": "price",
	//       "kind": "step",
	//       "in": "github.com/bide-ai/bide/plan_test.order",
	//       "out": "github.com/bide-ai/bide/plan_test.order",
	//       "safety": "readonly"
	//     },
	//     {
	//       "name": "refund",
	//       "kind": "step",
	//       "in": "github.com/bide-ai/bide/plan_test.order",
	//       "out": "github.com/bide-ai/bide/plan_test.receipt",
	//       "safety": "side_effect",
	//       "gate": {
	//         "need": 2,
	//         "approvers": [
	//           "ops",
	//           "finance"
	//         ]
	//       }
	//     }
	//   ],
	//   "predicates": [
	//     {
	//       "name": "large",
	//       "switched": "github.com/bide-ai/bide/plan_test.order"
	//     }
	//   ],
	//   "merges": []
	// }
}

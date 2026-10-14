package approvals_test

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/approvals"
)

// Mount the intake on the application's server. The journal is the application's own, so an
// audit.AuditedStore around its store signs each decision as it is written.
func ExampleNew() {
	journal := agenttest.MemJournal()
	keys := func(id string) (agent.ApproverVerifier, bool) { return nil, false } // the enrolled keys

	policies := map[string]*agent.ApprovalPolicy{
		"charge": {Need: 2, Approvers: []string{"ops", "finance", "risk"}}, // the policy the tool was built with
	}
	h, err := approvals.New(journal, keys,
		approvals.WithGatePolicy(func(tool string) (*agent.ApprovalPolicy, bool) { p, ok := policies[tool]; return p, ok }),
		approvals.OnRecorded(func(ctx context.Context, r approvals.Recorded) {
			if r.Decided {
				// Re-run r.RootRunID now, or leave it to agent.RecoverLoop.
			}
		}),
		approvals.WithAuth(func(req *http.Request, a approvals.Access) error {
			if req.TLS == nil || len(req.TLS.PeerCertificates) == 0 {
				return approvals.ErrUnauthenticated // require a client certificate
			}
			return nil
		}))
	if err != nil {
		fmt.Println(err)
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/approvals/", http.StripPrefix("/approvals", h))
	fmt.Println("mounted")
	// Output: mounted
}

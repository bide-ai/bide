package storetest

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// The PR's own tenantStore (keys by a tenant in the context), checked with CheckWrapper's default
// contexts, which is how the docs present the call. Neither default context carries the tenant
// key, so both map to tenant "" and the A1 violation is not detected.
func TestCheckWrapperDefaultContextsMissTenantStore(t *testing.T) {
	if f := check(func(s agent.Store) agent.Store { return tenantStore{s} }); len(f) == 0 {
		t.Fatal("CheckWrapper with its default contexts passed a wrapper whose keys depend on the context")
	}
}

// With the caller's contexts it is detected (control).
func TestCheckWrapperCallerContextsCatchTenantStore(t *testing.T) {
	a := context.WithValue(context.Background(), tenantKey{}, "A")
	b := context.WithValue(context.Background(), tenantKey{}, "B")
	if f := check(func(s agent.Store) agent.Store { return tenantStore{s} }, a, b); len(f) == 0 {
		t.Fatal("not detected")
	}
}

// One context only: nothing is compared at all.
func TestCheckWrapperOneContextChecksNothing(t *testing.T) {
	a := context.WithValue(context.Background(), tenantKey{}, "A")
	if f := check(func(s agent.Store) agent.Store { return tenantStore{s} }, a); len(f) == 0 {
		t.Fatal("CheckWrapper given one context passed a wrapper whose keys depend on the context")
	}
}
